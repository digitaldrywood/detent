package cli

import (
	"context"

	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/hub"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

type issueExplanationSnapshotSource struct {
	snapshots *hub.Hub[telemetry.Snapshot]
}

func (s issueExplanationSnapshotSource) Snapshot(ctx context.Context) (explain.SnapshotObservation, error) {
	if err := ctx.Err(); err != nil {
		return explain.SnapshotObservation{}, err
	}
	snapshot, ok := s.snapshots.Latest()
	if !ok {
		return explain.SnapshotObservation{State: explain.SourceUnavailable}, nil
	}
	return explain.SnapshotObservation{Snapshot: snapshot}, nil
}

func newIssueExplainer(snapshots *hub.Hub[telemetry.Snapshot], backend store.Store) *explain.Service {
	var scheduler explain.SchedulerReader
	if reader, ok := backend.(store.IssueSchedulerDecisionStore); ok {
		scheduler = reader
	}
	var sessions explain.SessionReader
	if reader, ok := backend.(store.ActivityStore); ok {
		sessions = reader
	}
	return explain.New(explain.Dependencies{
		Snapshots: issueExplanationSnapshotSource{snapshots: snapshots},
		Workflow:  backend,
		Attempts:  backend,
		Scheduler: scheduler,
		Sessions:  sessions,
		Admission: backend,
	})
}
