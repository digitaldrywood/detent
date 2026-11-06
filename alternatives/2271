package hubserver

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type tenantRunCleanupPredecessor struct {
	attempt   leasestore.Attempt
	event     eventstore.Event
	committed map[int64]eventstore.Event
}

func readTenantRunCleanupPredecessor(ctx context.Context, r tenantstore.Reader, scope nativeScope, request tracker.NativeRunEvent) (*tenantRunCleanupPredecessor, error) {
	if request.Data.Sequence <= 1 {
		return nil, nil
	}
	attempt, err := r.Attempt(ctx, request.Data.AttemptID)
	if errors.Is(err, corestore.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if attempt.LeaseID != string(request.Data.LeaseID) || attempt.FencingToken != int64(request.Data.FencingToken) || attempt.RunID != request.Data.RunID {
		return nil, leasestore.ErrAttemptConflict
	}
	event, err := r.LatestRunEvent(ctx, scope.project, attempt.ID)
	if err != nil {
		return nil, err
	}
	previous := &tenantRunCleanupPredecessor{attempt: attempt, event: event, committed: map[int64]eventstore.Event{}}
	reader, ok := r.(nativeRunEventReader)
	if !ok {
		return nil, nativeRunEventStorageUnavailable()
	}
	for _, prefix := range request.PriorEvents {
		if prefix.Data.Sequence > attempt.Sequence {
			continue
		}
		row, err := reader.RunEvent(ctx, scope.project, event.WorkItemID, nativeAttemptHistoryID(prefix.Type, prefix.Data))
		if errors.Is(err, corestore.ErrNotFound) {
			return nil, leasestore.ErrAttemptConflict
		}
		if err != nil {
			return nil, err
		}
		if row.OrganizationID != scope.organization || row.ProjectID != scope.project || row.WorkItemID != event.WorkItemID || row.Type != prefix.Type {
			return nil, artifact.ErrIntegrity
		}
		previous.committed[prefix.Data.Sequence] = row
	}
	return previous, nil
}

func readTenantRunCleanupData(ctx context.Context, content *eventstore.Content, previous *tenantRunCleanupPredecessor) (*tracker.NativeRunData, map[int64]*tracker.NativeRunData, error) {
	if previous == nil {
		return nil, nil, nil
	}
	rows := map[int64]eventstore.Event{}
	for sequence, row := range previous.committed {
		rows[sequence] = row
	}
	rows[previous.attempt.Sequence] = previous.event
	runs := map[int64]*tracker.NativeRunData{}
	for sequence, row := range rows {
		event, err := content.Event(ctx, row)
		if err != nil {
			return nil, nil, err
		}
		run := event.Data.Run
		if run == nil || run.AttemptID != previous.attempt.ID || run.RunID != previous.attempt.RunID || run.LeaseID != tracker.LeaseID(previous.attempt.LeaseID) || run.FencingToken != tracker.FencingToken(previous.attempt.FencingToken) || run.Sequence != sequence {
			return nil, nil, artifact.ErrIntegrity
		}
		runs[sequence] = run
	}
	committed := map[int64]*tracker.NativeRunData{}
	for sequence := range previous.committed {
		committed[sequence] = runs[sequence]
	}
	return runs[previous.attempt.Sequence], committed, nil
}

func preserveTenantRunCleanup(previous *tracker.NativeRunData, current *tracker.NativeRunData) error {
	if previous == nil || current.Sequence < previous.Sequence {
		return nil
	}
	if current.AttemptID != previous.AttemptID || current.RunID != previous.RunID || current.LeaseID != previous.LeaseID || current.FencingToken != previous.FencingToken || current.Sequence != previous.Sequence && current.Sequence != previous.Sequence+1 || current.Identity == nil || previous.Identity == nil || *current.Identity != *previous.Identity {
		return leasestore.ErrAttemptConflict
	}
	if previous.Runtime == nil || previous.Runtime.WorkerLaunch == nil || previous.Runtime.WorkerLaunch.PID <= 0 || previous.Runtime.WorkerLaunch.ProcessStart.IsZero() {
		return nil
	}
	if current.Runtime == nil {
		return leasestore.ErrAttemptConflict
	}
	if current.Runtime.WorkerLaunch != nil {
		if current.Runtime.WorkerLaunch.PID <= 0 || current.Runtime.WorkerLaunch.ProcessStart.IsZero() {
			return leasestore.ErrAttemptConflict
		}
		return nil
	}
	runtime := *current.Runtime
	launch := *previous.Runtime.WorkerLaunch
	runtime.WorkerLaunch = &launch
	current.Runtime = &runtime
	return nil
}
