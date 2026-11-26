package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/runner/runworkspace"

	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/serviceapi"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type DependencyReasonGenerator func(context.Context, string) (string, error)

type DependencyReasonExecution interface {
	SetDependencyReasonGenerator(DependencyReasonGenerator)
}

type dependencyReasonGeneratorKey struct{}

func WithDependencyReasonGenerator(ctx context.Context, generate DependencyReasonGenerator) context.Context {
	return context.WithValue(ctx, dependencyReasonGeneratorKey{}, generate)
}

func DependencyReasonGeneratorFromContext(ctx context.Context) DependencyReasonGenerator {
	generate, ok := ctx.Value(dependencyReasonGeneratorKey{}).(DependencyReasonGenerator)
	if !ok {
		return nil
	}
	return generate
}

func (r *Runner) dependencyReasonGenerator(backend AgentBackend, template AgentTurnRequest, execution Execution, backendKind string) DependencyReasonGenerator {
	return r.textGenerator(backend, template, execution, backendKind, 560, "Return only the requested sentence. Do not call tools or read files.")
}

func (r *Runner) textGenerator(backend AgentBackend, template AgentTurnRequest, execution Execution, backendKind string, maxBytes int, instructions string) DependencyReasonGenerator {
	return func(ctx context.Context, prompt string) (textResult string, returnErr error) {
		scratchRoot := template.TempDir
		if scratchRoot == "" {
			scratchRoot = os.TempDir()
		}
		scratchRoot, err := filepath.EvalSymlinks(scratchRoot)
		if err != nil {
			return "", fmt.Errorf("resolve text turn scratch root: %w", err)
		}
		scratch, err := os.MkdirTemp(scratchRoot, "detent-reason-")
		if err != nil {
			return "", fmt.Errorf("prepare dependency reason scratch: %w", err)
		}
		defer func() {
			if !UnretiredProviderSession(returnErr) && !errors.Is(returnErr, ErrWorkerProcessReap) {
				returnErr = errors.Join(returnErr, os.RemoveAll(scratch))
			}
		}()
		request := template
		request.Workspace = scratch
		request.TempDir = scratch
		request.Environment = r.withGoBudget(ctx, scratch, procgroup.Environment{Variables: serviceapi.RestrictedEnvironment()})
		request.ExtraWritableRoots = nil
		request.ExtraNetworkDomains = nil
		request.AllowLocalBinding = false
		request.Attachments = nil
		request.ConversationControl = nil
		request.Prompt = prompt
		request.ReadOnly = true
		request.TextOnly = true
		request.SupplementalTools = false
		request.Resume = AgentResume{}
		request.MaxTurns = 1
		request.ToolInstructions = instructions
		return r.generateTextTurn(ctx, backend, request, execution, backendKind, maxBytes, nil)
	}
}

func (r *Runner) generateTextTurn(ctx context.Context, backend AgentBackend, request AgentTurnRequest, execution Execution, backendKind string, maxBytes int, onUpdate AgentUpdateHandler) (string, error) {
	ctx, finish := (AgentProcessRequest{Workspace: request.Workspace, TempDir: request.TempDir}).OwnProviderLaunch(ctx)
	var text strings.Builder
	lastItem := ""
	usage := RunResult{Model: request.Model}
	if usage.Model == "" {
		usage.Model = "provider_default"
	}
	normalizeUsage := NewTokenUsageNormalizer(false)
	result, err, cleanupErr := runAgentBackendTurnWithTools(ctx, backend, request, nil, nil, func(_ context.Context, update AgentUpdate) error {
		if update.Type == AgentUpdateTokenUsage {
			update.Tokens = normalizeUsage(update.Tokens)
			usage.Tokens = TokenTotals{InputTokens: update.Tokens.InputTokens, CachedInputTokens: update.Tokens.CachedInputTokens, OutputTokens: update.Tokens.OutputTokens}
		}
		if update.Billing != "" {
			usage.BillingMode = update.Billing
		}
		if update.Type == AgentUpdateMessageDelta {
			key := update.ItemID
			if key == "" {
				key = update.TurnID
			}
			if key != lastItem {
				text.Reset()
				lastItem = key
			}
			if text.Len()+len(update.Delta) > maxBytes {
				return errors.New("generated text exceeds limit")
			}
			text.WriteString(update.Delta)
		}
		if onUpdate != nil {
			return onUpdate(update)
		}
		return nil
	})
	usage.ReportedCostMicros, usage.CostSource = result.ReportedCostMicros, result.CostSource
	usageCtx, cancelUsage := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	usageErr := reportTurnUsage(usageCtx, execution, usage, request.Model, backendKind, r.usageCostUSD, r.logger)
	cancelUsage()
	if err := errors.Join(finish(errors.Join(err, cleanupErr)), usageErr); err != nil {
		return "", err
	}
	return strings.TrimSpace(text.String()), nil
}

func (r *Runner) installDependencyReasonGenerator(ctx context.Context, req RunRequest) context.Context {
	source, ok := req.Execution.(DependencyReasonExecution)
	if !ok {
		return ctx
	}
	attempt := workspace.AttemptWorkspaceFromContext(ctx)
	owner := workspace.ValidationProcessOwnerFromContext(ctx)
	generate := func(ctx context.Context, prompt string) (string, error) {
		ctx = workspace.WithAttemptWorkspace(ctx, attempt)
		ctx = workspace.WithValidationProcessOwner(ctx, owner)
		workflow, runtime, _, _ := r.runtimeSnapshot()
		role := runtime.effectiveRunRole(req.executionRole())
		selection, backend, backendConfig, err := runtime.selectRequestBackend(req, runworkspace.SelectorContext(req.SelectorContext, workflow), role)
		if err != nil {
			return "", err
		}
		modelProvider, serviceTier, effort := agentTurnIdentityOptions(backendConfig)
		request := AgentTurnRequest{Model: effectiveModel("", selection.Model, runtime.defaultModelForRole(role)), ModelProvider: modelProvider, ServiceTier: serviceTier, ReasoningEffort: effort, MaxDuration: durationFromMillis(workflow.Config.Agent.MaxTurnDurationMS), MaxRSSBytes: r.maxAgentRSSBytes, RSSPollInterval: r.rssPollInterval, processRSS: r.processRSS}
		return r.dependencyReasonGenerator(backend, request, req.Execution, backendConfig.Kind)(ctx, prompt)
	}
	source.SetDependencyReasonGenerator(generate)
	return WithDependencyReasonGenerator(ctx, generate)
}
