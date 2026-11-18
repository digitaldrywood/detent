package nativechange

import (
	"context"
	"net/http"
	"slices"
	"strconv"

	"github.com/digitaldrywood/detent/internal/hubclient/checkpoint"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (e *nativeExecution) flushObservations(ctx context.Context) error {
	if err := e.flush(ctx); err != nil {
		return err
	}
	if len(e.observations) == 0 {
		return nil
	}
	last := len(e.observations) - 1
	event := e.observations[last]
	event.PriorEvents = slices.Clone(e.observations[:last])
	e.pending = &event
	return e.flush(ctx)
}

func (e *nativeExecution) receiptAttempt(ctx context.Context) (tracker.NativeAttempt, error) {
	var attempt tracker.NativeAttempt
	path, err := nativeItemPath(e.claim.lease.WorkItemID)
	if err != nil {
		return attempt, err
	}
	client := e.claim.source.NativeClient()
	if err := client.Transport().Request(ctx, http.MethodGet, client.Scope()+path+"/attempts/"+e.data.AttemptID, nil, &attempt); err != nil {
		return attempt, e.executionError(ctx, err)
	}
	if !checkpoint.SameRun(e.data, attempt.NativeRunData) || attempt.ClaimReleasedAt != nil || attempt.Outcome != "" {
		return attempt, runner.ErrExecutionAuthorityUnavailable
	}
	return attempt, nil
}

func (e *nativeExecution) reconcileObservation(ctx context.Context, observation *tracker.NativeRunEvent) error {
	attempt, err := e.receiptAttempt(ctx)
	if err != nil {
		return err
	}
	first := observation.Data.Sequence
	if len(observation.PriorEvents) != 0 {
		first = observation.PriorEvents[0].Data.Sequence
	}
	if attempt.Sequence != first-1 && attempt.Sequence != observation.Data.Sequence {
		return runner.ErrExecutionAuthorityUnavailable
	}
	e.pending.Data.Sequence = attempt.Sequence + 1
	e.pending.IdempotencyKey = attempt.AttemptID + ":" + strconv.FormatInt(e.pending.Data.Sequence, 10) + ":receipt"
	return nil
}
