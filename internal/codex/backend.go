package codex

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

var ErrMissingAppServer = errors.New("codex app-server is required")

const terminalWaitInstructions = "For known long-running terminal commands, set the command wait to about 55 seconds and the enclosing functions.exec yield horizon to at least 60 seconds, with headroom beyond the command wait. Do not use 1-second yields for builds, test suites, CI checks, or an already-running managed session. Reuse the existing command session and wait on it instead of repeatedly running ps, pgrep, or tail probes unless there is evidence the session is stuck."

const dynamicToolTurnInstructions = "You are Detent's board operator assistant. Use only the provided Detent tools for board, fleet, telemetry, activity, and operator actions. Never use shell, filesystem, network, MCP, browser, delegation, or configuration tools. Mutating tools only create proposals; tell the operator that confirmation is required and never claim a proposal already executed."

type AgentBackend struct {
	client  *AppServer
	options Options
}

var _ runner.AgentResumeVerifier = (*AgentBackend)(nil)
var _ runner.AgentToolBackend = (*AgentBackend)(nil)

func NewAgentBackend(client *AppServer, options Options) (*AgentBackend, error) {
	if client == nil {
		return nil, ErrMissingAppServer
	}
	return &AgentBackend{
		client:  client,
		options: options,
	}, nil
}

func (b *AgentBackend) RunTurn(
	ctx context.Context,
	req runner.AgentTurnRequest,
	onUpdate runner.AgentUpdateHandler,
) (runner.AgentTurnResult, error) {
	return b.runTurn(ctx, req, nil, nil, onUpdate)
}

func (b *AgentBackend) RunTurnWithTools(
	ctx context.Context,
	req runner.AgentTurnRequest,
	tools []runner.AgentTool,
	toolHandler runner.AgentToolHandler,
	onUpdate runner.AgentUpdateHandler,
) (runner.AgentTurnResult, error) {
	dynamicTools := make([]DynamicTool, 0, len(tools))
	for _, tool := range tools {
		dynamicTools = append(dynamicTools, DynamicTool{
			Type:        "function",
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: tool.InputSchema,
		})
	}
	var dynamicHandler DynamicToolHandler
	if toolHandler != nil {
		dynamicHandler = func(ctx context.Context, call DynamicToolCall) (DynamicToolResult, error) {
			result, err := toolHandler(ctx, runner.AgentToolCall{Name: call.Name, Arguments: call.Arguments})
			return DynamicToolResult{Content: result.Content, Success: result.Success}, err
		}
	}
	return b.runTurn(ctx, req, dynamicTools, dynamicHandler, onUpdate)
}

func (b *AgentBackend) runTurn(
	ctx context.Context,
	req runner.AgentTurnRequest,
	tools []DynamicTool,
	toolHandler DynamicToolHandler,
	onUpdate runner.AgentUpdateHandler,
) (runner.AgentTurnResult, error) {
	ctx = withWorkerTempDir(ctx, req.TempDir)
	ctx = withWorkerEnvironment(ctx, req.Environment)
	restricted := req.ReadOnly || len(tools) > 0
	result, err := b.client.RunTurn(ctx, RunTurnRequest{
		Workspace:               req.Workspace,
		Prompt:                  req.Prompt,
		ResumeThreadID:          req.Resume.ThreadID,
		DeveloperInstructions:   toolTurnInstructions(tools, req.ToolInstructions),
		ApprovalPolicy:          approvalPolicy(b.options.ApprovalPolicy, restricted),
		MCPElicitationPolicy:    mcpElicitationPolicy(b.options.DeliverableElicitationAllowlist, req, restricted),
		ThreadSandbox:           threadSandbox(b.options.ThreadSandbox, restricted),
		TurnSandboxPolicy:       turnSandboxPolicy(b.options.ThreadSandbox, b.options.TurnSandboxPolicy, req.ExtraWritableRoots, restricted),
		Model:                   req.Model,
		ModelProvider:           req.ModelProvider,
		ServiceTier:             req.ServiceTier,
		ReasoningEffort:         req.ReasoningEffort,
		TurnTimeout:             req.TurnTimeout,
		StallTimeout:            b.options.StallTimeout,
		DynamicTools:            tools,
		ToolHandler:             toolHandler,
		RequireSubscriptionAuth: req.RequireSubscriptionAuth,
	}, func(update Update) error {
		if onUpdate == nil {
			return nil
		}
		return onUpdate(agentUpdateFromCodex(update))
	})
	if err != nil {
		if errors.Is(err, ErrSubscriptionAuthRequired) {
			return runner.AgentTurnResult{
				ThreadID:           result.ThreadID,
				TurnID:             result.TurnID,
				SessionID:          result.SessionID,
				AuthenticationMode: securityaudit.AuthenticationRejected,
			}, fmt.Errorf("%w: %w", runner.ErrSubscriptionAuthRequired, err)
		}
		if errors.Is(err, ErrTransportClose) && result.ThreadID != "" && result.TurnID != "" {
			return runner.AgentTurnResult{
				ThreadID:           result.ThreadID,
				TurnID:             result.TurnID,
				SessionID:          result.SessionID,
				AuthenticationMode: result.AuthenticationMode,
			}, runner.NewAgentTurnCleanupError(err)
		}
		return runner.AgentTurnResult{}, err
	}
	return runner.AgentTurnResult{
		ThreadID:           result.ThreadID,
		TurnID:             result.TurnID,
		SessionID:          result.SessionID,
		AuthenticationMode: result.AuthenticationMode,
	}, nil
}

func approvalPolicy(configured any, restricted bool) any {
	if restricted {
		return "never"
	}
	return configured
}

func mcpElicitationPolicy(allowlist []MCPElicitationRule, req runner.AgentTurnRequest, restricted bool) MCPElicitationPolicy {
	if restricted {
		return MCPElicitationPolicy{}
	}
	return MCPElicitationPolicy{
		DeliverableKind: strings.TrimSpace(req.DeliverableKind),
		Repository:      strings.TrimSpace(req.DeliverableRepository),
		IssueRepository: strings.TrimSpace(req.IssueRepository),
		Allowlist:       append([]MCPElicitationRule(nil), allowlist...),
	}
}

func threadSandbox(configured string, restricted bool) string {
	if restricted {
		return "read-only"
	}
	return configured
}

func turnSandboxPolicy(threadSandbox string, configured any, roots []string, restricted bool) any {
	if restricted {
		return nil
	}
	return turnSandboxPolicyForWorkspace(threadSandbox, configured, roots)
}

func toolTurnInstructions(tools []DynamicTool, override string) string {
	if override = strings.TrimSpace(override); override != "" {
		return override
	}
	if len(tools) == 0 {
		return terminalWaitInstructions
	}
	return dynamicToolTurnInstructions
}

func (b *AgentBackend) VerifyResume(ctx context.Context, resume runner.AgentResume) error {
	if strings.TrimSpace(resume.ThreadID) == "" {
		return runner.ErrAgentResumeUnsupported
	}
	return b.client.VerifyThread(ctx, resume.ThreadID)
}

func (b *AgentBackend) ListModels(ctx context.Context) ([]runner.AgentModel, error) {
	models, err := b.client.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]runner.AgentModel, 0, len(models))
	for _, model := range models {
		out = append(out, runner.AgentModel{
			ID:                        model.ID,
			Model:                     model.Model,
			Default:                   model.Default,
			Upgrade:                   model.Upgrade,
			SupportedReasoningEfforts: append([]string(nil), model.SupportedReasoningEfforts...),
		})
	}
	return out, nil
}

func (b *AgentBackend) DefaultModel(ctx context.Context, workspace string) (string, error) {
	return b.client.DefaultModel(ctx, workspace)
}

func agentUpdateFromCodex(update Update) runner.AgentUpdate {
	identity := update.RuntimeIdentity
	if identity.IsZero() && update.Model != "" {
		identity = agentidentity.RuntimeUpdate(update.Model, "", "", "", time.Time{})
	}
	providerSessionID := ""
	if update.ThreadID != "" && update.TurnID != "" {
		providerSessionID = update.ThreadID + "-" + update.TurnID
	}
	threadTotal := runner.AgentTokenCounts{
		InputTokens:           update.Tokens.InputTokens,
		CachedInputTokens:     update.Tokens.CachedInputTokens,
		OutputTokens:          update.Tokens.OutputTokens,
		ReasoningOutputTokens: update.Tokens.ReasoningOutputTokens,
		TotalTokens:           update.Tokens.TotalTokens,
	}
	return runner.AgentUpdate{
		Type:                runner.AgentUpdateType(update.Type),
		Method:              update.Method,
		ProcessIdentity:     update.ProcessIdentity,
		WorkerProcess:       update.WorkerProcess,
		ThreadID:            update.ThreadID,
		TurnID:              update.TurnID,
		AuxiliaryTurn:       update.AuxiliaryTurn,
		ProviderSessionID:   providerSessionID,
		ItemID:              update.ItemID,
		Tool:                update.Tool,
		Command:             update.Command,
		Delta:               update.Delta,
		Status:              update.Status,
		ExitCode:            update.ExitCode,
		Model:               update.Model,
		RuntimeIdentity:     identity,
		BackendErrorBody:    update.BackendErrorBody,
		BackendErrorMessage: update.BackendErrorMessage,
		Tokens: runner.AgentTokenUsage{
			InputTokens:           update.Tokens.InputTokens,
			CachedInputTokens:     update.Tokens.CachedInputTokens,
			OutputTokens:          update.Tokens.OutputTokens,
			ReasoningOutputTokens: update.Tokens.ReasoningOutputTokens,
			TotalTokens:           update.Tokens.TotalTokens,
			ThreadTotal:           &threadTotal,
			Last:                  agentTokenCountsFromCodex(update.Tokens.Last),
			ModelContextWindow:    update.Tokens.ModelContextWindow,
		},
		RateLimits: rateLimitsFromCodex(update.RateLimits),
	}
}

func agentTokenCountsFromCodex(tokens *TokenUsageBreakdown) *runner.AgentTokenCounts {
	if tokens == nil {
		return nil
	}
	return &runner.AgentTokenCounts{
		InputTokens:           tokens.InputTokens,
		CachedInputTokens:     tokens.CachedInputTokens,
		OutputTokens:          tokens.OutputTokens,
		ReasoningOutputTokens: tokens.ReasoningOutputTokens,
		TotalTokens:           tokens.TotalTokens,
	}
}

func rateLimitsFromCodex(snapshot *RateLimitSnapshot) *telemetry.RateLimits {
	if snapshot == nil {
		return nil
	}
	limits := &telemetry.RateLimits{
		LimitID:     snapshot.LimitID,
		LimitName:   snapshot.LimitName,
		ReachedType: snapshot.RateLimitReachedType,
		Primary:     rateLimitBucketFromCodex(snapshot.Primary),
		Secondary:   rateLimitBucketFromCodex(snapshot.Secondary),
		Credits:     creditsBucketFromCodex(snapshot.Credits),
	}
	observedAt := time.Now().UTC()
	for _, bucket := range []*telemetry.RateLimitBucket{limits.Primary, limits.Secondary} {
		if bucket != nil {
			bucket.ObservedAt = &observedAt
		}
	}
	if strings.TrimSpace(snapshot.RateLimitReachedType) != "" {
		bucket := limits.Primary
		if strings.Contains(strings.ToLower(snapshot.RateLimitReachedType), "secondary") {
			bucket = limits.Secondary
		}
		if bucket != nil {
			bucket.Status = telemetry.RateLimitStatusExhausted
		}
	}
	return limits
}

func rateLimitBucketFromCodex(window *RateLimitWindow) *telemetry.RateLimitBucket {
	if window == nil {
		return nil
	}

	used := min(max(int64(math.Round(window.UsedPercent)), 0), 100)

	bucket := &telemetry.RateLimitBucket{
		Limit:     100,
		Used:      used,
		Remaining: 100 - used,
	}
	if window.ResetsAt != nil {
		resetAt := time.Unix(*window.ResetsAt, 0).UTC()
		bucket.ResetAt = &resetAt
	}
	return bucket
}

func creditsBucketFromCodex(credits *CreditsSnapshot) *telemetry.RateLimitBucket {
	if credits == nil {
		return nil
	}
	return &telemetry.RateLimitBucket{
		HasCredits: credits.HasCredits,
		Unlimited:  credits.Unlimited,
		Balance:    credits.Balance,
	}
}
