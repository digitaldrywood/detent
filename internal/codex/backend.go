package codex

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

var ErrMissingAppServer = errors.New("codex app-server is required")

type AgentBackend struct {
	client *AppServer
}

func NewAgentBackend(client *AppServer) (*AgentBackend, error) {
	if client == nil {
		return nil, ErrMissingAppServer
	}
	return &AgentBackend{client: client}, nil
}

func (b *AgentBackend) RunTurn(
	ctx context.Context,
	req runner.AgentTurnRequest,
	onUpdate runner.AgentUpdateHandler,
) (runner.AgentTurnResult, error) {
	result, err := b.client.RunTurn(ctx, RunTurnRequest{
		Workspace:         req.Workspace,
		Prompt:            req.Prompt,
		ApprovalPolicy:    req.ApprovalPolicy,
		ThreadSandbox:     req.ThreadSandbox,
		TurnSandboxPolicy: req.TurnSandboxPolicy,
		Model:             req.Model,
		ModelProvider:     req.ModelProvider,
		ServiceTier:       req.ServiceTier,
	}, func(update Update) error {
		if onUpdate == nil {
			return nil
		}
		return onUpdate(agentUpdateFromCodex(update))
	})
	if err != nil {
		return runner.AgentTurnResult{}, err
	}
	return runner.AgentTurnResult{
		ThreadID:  result.ThreadID,
		TurnID:    result.TurnID,
		SessionID: result.SessionID,
	}, nil
}

func agentUpdateFromCodex(update Update) runner.AgentUpdate {
	return runner.AgentUpdate{
		Type:            runner.AgentUpdateType(update.Type),
		Method:          update.Method,
		ProcessIdentity: update.ProcessIdentity,
		ThreadID:        update.ThreadID,
		TurnID:          update.TurnID,
		ItemID:          update.ItemID,
		Delta:           update.Delta,
		Status:          update.Status,
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
	return &telemetry.RateLimits{
		LimitID:   snapshot.LimitID,
		LimitName: snapshot.LimitName,
		Primary:   rateLimitBucketFromCodex(snapshot.Primary),
		Secondary: rateLimitBucketFromCodex(snapshot.Secondary),
		Credits:   creditsBucketFromCodex(snapshot.Credits),
	}
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
