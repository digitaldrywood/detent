package orchestrator

import (
	"context"
	"errors"
	"time"

	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func (o *Orchestrator) handleModelPermitDeferred(
	ctx context.Context,
	state *State,
	event runpkg.Completion,
	running Running,
) bool {
	if !errors.Is(event.Err, runpkg.ErrModelPermitUnavailable) {
		return false
	}
	releaseBackendCapacityProbe(state, running)
	releaseDispatchRecoveryAdmission(state, event.IssueID)
	releaseProjectFailureBreakerCanary(state, event.IssueID)
	o.completeDurableWorkAttempt(
		ctx,
		state,
		running,
		event.CompletedAt,
		store.WorkAttemptTerminalCapacity,
		dispatchSkipRateWindowBackpressure,
		event.Err.Error(),
		"deferred",
		"merge fallback waiting for provider model capacity",
	)
	delay := o.cfg.ContinuationRetryDelay
	if delay < 0 {
		delay = 0
	}
	completedAt := event.CompletedAt
	if completedAt.IsZero() {
		completedAt = time.Now()
	}
	state.Retry[event.IssueID] = Retry{
		Issue:         cloneIssue(running.Issue),
		Attempt:       max(1, running.Attempt),
		DueAt:         completedAt.Add(delay),
		Error:         dispatchSkipRateWindowBackpressure,
		WorkerHost:    running.WorkerHost,
		RetryMode:     event.Request.RetryMode,
		ResumeState:   event.Request.ResumeState,
		MergePrecheck: cloneMergePrecheck(event.Result.MergePrecheck),
	}
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      completedAt,
		Event:   "merge_fallback_deferred",
		Message: "deferred merge fallback for " + issueLabel(running.Issue) + " until provider model capacity is available",
	})
	return true
}
