package hubserver

import (
	"context"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type nativeRunReplaySnapshot struct {
	rows     []eventstore.Event
	attempts []leasestore.Attempt
}

func readNativeRunReplaySnapshot(ctx context.Context, r tenantstore.Reader, scope nativeScope, item string, events []tracker.NativeRunEvent) (*nativeRunReplaySnapshot, error) {
	rows := make([]eventstore.Event, len(events))
	attempts := make([]leasestore.Attempt, len(events))
	reader, ok := r.(nativeRunEventReader)
	if !ok {
		return nil, nativeRunEventStorageUnavailable()
	}
	for i, event := range events {
		row, err := reader.RunEvent(ctx, scope.project, tracker.NativeWorkItemID(item), nativeAttemptHistoryID(event.Type, event.Data))
		if err != nil {
			return nil, err
		}
		if row.OrganizationID != scope.organization || row.ProjectID != scope.project || row.WorkItemID != tracker.NativeWorkItemID(item) || row.Type != event.Type {
			return nil, artifact.ErrIntegrity
		}
		attempt, err := r.Attempt(ctx, event.Data.AttemptID)
		if err != nil {
			return nil, err
		}
		if attempt.RunID != event.Data.RunID || attempt.LeaseID != string(event.Data.LeaseID) || attempt.FencingToken != int64(event.Data.FencingToken) || attempt.Sequence < event.Data.Sequence {
			return nil, artifact.ErrIntegrity
		}
		attempts[i] = attempt
		rows[i] = row
	}
	return &nativeRunReplaySnapshot{rows: rows, attempts: attempts}, nil
}
