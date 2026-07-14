package codex

import (
	"context"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

var ErrMissingAppServer = errors.New("codex app-server is required")

type AgentBackend struct {
	client  *AppServer
	options Options
}

var _ runner.AgentResumeVerifier = (*AgentBackend)(nil)

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
	ctx = withWorkerTempDir(ctx, req.TempDir)
	result, err := b.client.RunTurn(ctx, RunTurnRequest{
		Workspace:         req.Workspace,
		Prompt:            req.Prompt,
		ResumeThreadID:    req.Resume.ThreadID,
		ApprovalPolicy:    b.options.ApprovalPolicy,
		ThreadSandbox:     b.options.ThreadSandbox,
		TurnSandboxPolicy: turnSandboxPolicyForWorkspace(b.options.ThreadSandbox, b.options.TurnSandboxPolicy, req.ExtraWritableRoots),
		Model:             req.Model,
		ModelProvider:     req.ModelProvider,
		ServiceTier:       req.ServiceTier,
		ReasoningEffort:   req.ReasoningEffort,
		TurnTimeout:       req.TurnTimeout,
	}, func(update Update) error {
		if onUpdate == nil {
			return nil
		}
		return onUpdate(agentUpdateFromCodex(update))
	})
	if err != nil {
		if errors.Is(err, ErrTransportClose) && result.ThreadID != "" && result.TurnID != "" {
			return runner.AgentTurnResult{
				ThreadID:  result.ThreadID,
				TurnID:    result.TurnID,
				SessionID: result.SessionID,
			}, runner.NewAgentTurnCleanupError(err)
		}
		return runner.AgentTurnResult{}, err
	}
	return runner.AgentTurnResult{
		ThreadID:  result.ThreadID,
		TurnID:    result.TurnID,
		SessionID: result.SessionID,
	}, nil
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
	return runner.AgentUpdate{
		Type:                runner.AgentUpdateType(update.Type),
		Method:              update.Method,
		ProcessIdentity:     update.ProcessIdentity,
		WorkerProcess:       update.WorkerProcess,
		ThreadID:            update.ThreadID,
		TurnID:              update.TurnID,
		ProviderSessionID:   providerSessionID,
		ItemID:              update.ItemID,
		Tool:                update.Tool,
		Delta:               update.Delta,
		Status:              update.Status,
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
			ModelContextWindow:    update.Tokens.ModelContextWindow,
		},
		RateLimits: rateLimitsFromCodex(update.RateLimits),
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
