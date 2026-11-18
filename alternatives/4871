package runner

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	sessionmodel "github.com/digitaldrywood/detent/internal/runnersession/model"
	"github.com/digitaldrywood/detent/internal/store"
)

func (r *Runner) sessionFinalization(sessionID int64, issue connector.Issue, startedAt, finishedAt time.Time, result RunResult, model, backendKind string, turns int64, turnResult AgentTurnResult, resumedFromSessionID int64) *sessionmodel.Finalization {
	if result.FinalState == "" {
		result.FinalState = FinalStateCompleted
	}
	requestedModel := strings.TrimSpace(model)
	resolvedModel := effectiveModel(result.RuntimeIdentity.ResolvedModel.Value, result.Model)
	usageModel := effectiveModel(resolvedModel, requestedModel)
	actualCostUSD := r.usageCostUSD(usageModel, result.Tokens.InputTokens, result.Tokens.CachedInputTokens, result.Tokens.OutputTokens, backendKind)
	projectedCostUSD, projectionOvershootUSD := budgetProjectionCosts(result.budgetProjection, actualCostUSD)

	value := &sessionmodel.Finalization{
		SessionID: sessionID,
		Finish: store.SessionFinish{
			CompletedAt:           finishedAt,
			Turns:                 turns,
			TurnStartRefused:      result.TurnStartRefused,
			InputTokens:           result.Tokens.InputTokens,
			CachedInputTokens:     result.Tokens.CachedInputTokens,
			OutputTokens:          result.Tokens.OutputTokens,
			ReasoningOutputTokens: result.Tokens.ReasoningOutputTokens,
			TotalTokens:           result.Tokens.TotalTokens,
			ModelContextWindow:    result.Tokens.ModelContextWindow,
			RuntimeSeconds:        int64(math.Round(result.Tokens.RuntimeSeconds)),
			FinalState:            result.FinalState,
			Model:                 resolvedModel,
			ProviderThreadID:      turnResult.ThreadID,
			ProviderSessionID:     turnResult.SessionID,
			ResumedFromSessionID:  resumedFromSessionID,
			RuntimeIdentity:       result.RuntimeIdentity},
		Usage: store.UsageEvent{
			ProjectID:              r.projectID,
			SessionID:              sessionID,
			IssueID:                issue.ID,
			Identifier:             issue.Identifier,
			PRNumber:               pullRequestNumber(issue),
			Model:                  usageModel,
			InputTokens:            result.Tokens.InputTokens,
			CachedInputTokens:      result.Tokens.CachedInputTokens,
			OutputTokens:           result.Tokens.OutputTokens,
			ReasoningOutputTokens:  result.Tokens.ReasoningOutputTokens,
			TotalTokens:            result.Tokens.TotalTokens,
			ModelContextWindow:     result.Tokens.ModelContextWindow,
			CostUSD:                actualCostUSD,
			Compute:                result.Compute,
			ProjectedCostUSD:       projectedCostUSD,
			ProjectionOvershootUSD: projectionOvershootUSD,
			RuntimeSeconds:         int64(math.Round(result.Tokens.RuntimeSeconds)),
			StartedAt:              startedAt,
			FinishedAt:             finishedAt,
			Outcome:                result.FinalState},
	}
	if _, ok := r.store.(workflowPhaseStore); ok {
		value.Phase = &store.WorkflowPhaseEvent{
			ProjectID:             r.projectID,
			SessionID:             sessionID,
			IssueID:               issue.ID,
			Identifier:            issue.Identifier,
			IssueURL:              issue.URL,
			PRNumber:              pullRequestNumber(issue),
			PhaseType:             store.WorkflowPhaseTypeAgentSession,
			PhaseName:             "agent_active",
			Status:                result.FinalState,
			StartedAt:             startedAt,
			FinishedAt:            finishedAt,
			DurationSeconds:       int64(math.Round(result.Tokens.RuntimeSeconds)),
			Turns:                 1,
			InputTokens:           result.Tokens.InputTokens,
			CachedInputTokens:     result.Tokens.CachedInputTokens,
			OutputTokens:          result.Tokens.OutputTokens,
			ReasoningOutputTokens: result.Tokens.ReasoningOutputTokens,
			TotalTokens:           result.Tokens.TotalTokens,
			ModelContextWindow:    result.Tokens.ModelContextWindow,
			EndpointFamily:        strings.TrimSpace(backendKind)}
	}
	return value
}

type sessionFinalizationStore interface {
	ReconcileSessionFinalization(context.Context, int64, *sessionmodel.Finalization) error
}

func (r *Runner) reconcileSessionFinalization(ctx context.Context, execution Execution, id int64, value *sessionmodel.Finalization) error {
	owner, ok := r.store.(sessionFinalizationStore)
	if !ok || id <= 0 {
		return errors.New("session finalization owner unavailable")
	}
	ctx = bindSessionExecution(ctx, r.store, execution)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := owner.ReconcileSessionFinalization(ctx, id, value); err != nil {
		return fmt.Errorf("reconcile session finalization: %w", err)
	}
	return nil
}
