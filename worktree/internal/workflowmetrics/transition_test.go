package workflowmetrics

import (
	"context"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestRecordLaneTransitionRecordsExitAndEntry(t *testing.T) {
	t.Parallel()

	startedAt := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	transitionAt := startedAt.Add(5 * time.Minute)
	recorder := &transitionRecorder{}
	err := RecordLaneTransition(context.Background(), recorder, LaneTransition{
		ProjectID: "detent",
		Issue: connector.Issue{
			ID:             "issue-1",
			Identifier:     "DD-1",
			State:          "Backlog",
			StageUpdatedAt: &startedAt,
		},
		TargetState:  "Todo",
		At:           transitionAt,
		Reason:       "routine",
		MetadataJSON: `{"provenance":{"origin":"routine"}}`,
	})
	if err != nil {
		t.Fatalf("RecordLaneTransition() error = %v", err)
	}
	if len(recorder.events) != 2 {
		t.Fatalf("events = %#v, want exit and entry", recorder.events)
	}
	exit, enter := recorder.events[0], recorder.events[1]
	if exit.Status != "exited" || exit.PhaseName != "Backlog" || exit.DurationSeconds != 300 {
		t.Fatalf("exit event = %#v", exit)
	}
	if enter.Status != "entered" || enter.PhaseName != "Todo" || enter.PreviousPhaseName != "Backlog" ||
		enter.Reason != "routine" || enter.MetadataJSON != `{"provenance":{"origin":"routine"}}` {
		t.Fatalf("enter event = %#v", enter)
	}
}

type transitionRecorder struct {
	events []PhaseEvent
}

func (r *transitionRecorder) RecordWorkflowPhaseEvent(_ context.Context, event PhaseEvent) (int64, error) {
	r.events = append(r.events, event)
	return int64(len(r.events)), nil
}
