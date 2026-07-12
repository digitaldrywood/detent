package orchestrator

import (
	"context"
	"log/slog"
	"time"

	"github.com/digitaldrywood/detent/internal/backendcapacity"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func (o *Orchestrator) handleTransientOverload(
	ctx context.Context,
	state *State,
	event runpkg.Completion,
	running Running,
	overloadErr *backendcapacity.Error,
) {
	releaseBackendCapacityProbe(state, running)
	o.completeDurableWorkAttempt(
		ctx,
		state,
		running,
		event.CompletedAt,
		store.WorkAttemptTerminalFailure,
		backendcapacity.TransientOverloadErrorClass,
		event.Err.Error(),
		"waiting",
		"retrying after transient provider overload",
	)
	if workspaceIssueTerminal(running.Issue, o.cfg.TerminalStates) {
		o.releaseClaim(state, running.Issue.ID)
		return
	}
	attempt := event.RetryAttempt
	if attempt < 1 {
		attempt = running.Attempt
	}
	if attempt < 1 {
		attempt = 1
	}
	delay := event.RetryDelay
	if delay <= 0 {
		delay = o.cfg.OverloadRetryDelay
	}
	if delay <= 0 {
		delay = defaultOverloadRetryDelay
	}
	o.scheduleRetryAfter(
		state,
		running.Issue,
		attempt,
		event.CompletedAt,
		delay,
		string(backendcapacity.ErrorTypeTransientOverload),
		running.WorkerHost,
	)
	retryAt := event.CompletedAt.Add(delay)
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      event.CompletedAt,
		Event:   "transient_overload_retry",
		Message: "transient provider overload; retrying affected issue at " + retryAt.Format(time.RFC3339),
	})
	if o.logger != nil {
		o.logger.Log(
			ctx,
			slog.LevelInfo,
			"transient overload retry scheduled",
			"reason", backendcapacity.ErrorTypeTransientOverload,
			"backend_id", overloadErr.Scope.BackendID,
			"backend_kind", overloadErr.Scope.BackendKind,
			"provider", overloadErr.Scope.Provider,
			"issue_id", running.Issue.ID,
			"attempt", attempt,
			"retry_at", retryAt,
			"error", event.Err,
		)
	}
}
