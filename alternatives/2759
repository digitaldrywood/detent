package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/serviceapi"
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
		for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
			if value := os.Getenv(key); scratchRoot == "" && value != "" {
				scratchRoot = value
				break
			}
		}
		if scratchRoot == "" {
			return "", errors.New("dependency reason worker scratch is unavailable")
		}
		scratch, err := os.MkdirTemp(scratchRoot, "detent-reason-")
		if err != nil {
			return "", fmt.Errorf("prepare dependency reason scratch: %w", err)
		}
		defer func() { returnErr = errors.Join(returnErr, os.RemoveAll(scratch)) }()
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
		return r.generateTextTurn(ctx, backend, request, execution, backendKind, maxBytes, nil, nil)
	}
}

func (r *Runner) generateTextTurn(ctx context.Context, backend AgentBackend, request AgentTurnRequest, execution Execution, backendKind string, maxBytes int, tools []AgentTool, handler AgentToolHandler) (string, error) {
	var text strings.Builder
	lastItem := ""
	usage := RunResult{Model: request.Model}
	if usage.Model == "" {
		usage.Model = "provider_default"
	}
	result, err, cleanupErr := runAgentBackendTurnWithTools(ctx, backend, request, tools, handler, func(_ context.Context, update AgentUpdate) error {
		if update.Type == AgentUpdateTokenUsage {
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
		return nil
	})
	usage.ReportedCostMicros, usage.CostSource = result.ReportedCostMicros, result.CostSource
	usageCtx, cancelUsage := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	reportTurnUsage(usageCtx, execution, usage, request.Model, backendKind, r.usageCostUSD, r.logger)
	cancelUsage()
	if err := errors.Join(err, cleanupErr); err != nil {
		return "", err
	}
	return strings.TrimSpace(text.String()), nil
}

func (r *Runner) installDependencyReasonGenerator(ctx context.Context, req RunRequest) context.Context {
	source, ok := req.Execution.(DependencyReasonExecution)
	if !ok {
		return ctx
	}
	generate := func(ctx context.Context, prompt string) (string, error) {
		workflow, runtime, _, _ := r.runtimeSnapshot()
		role := runtime.effectiveRunRole(runRole(req.Mode, req.Issue))
		selection, backend, backendConfig, err := runtime.selectRequestBackend(req, selectorContext(req.SelectorContext, workflow), role)
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
