package orchestrator

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestClosedLabelSnapshotsExcludedFromDispatch(t *testing.T) {
	for _, lane := range []string{"Todo", "In Progress", "Rework", "Merging"} {
		for _, reason := range []string{"completed", "not_planned", ""} {
			t.Run(lane+"/"+reason, func(t *testing.T) {
				tracker := &statusReconcileConnector{}
				orch := newStatusReconcileOrchestrator(tracker)
				orch.cfg.ActiveStates = []string{lane}
				issue := statusReconcileIssue("issue-2813", lane, true, reason)
				issue.AssignedToWorker = true
				issue.Fields["fixture"] = "hydrated"
				state := newState(orch.cfg)
				orch.dispatchReadyIssues(t.Context(), &state, []connector.Issue{issue}, time.Now())
				if len(state.SchedulerDecisions) != 0 || len(state.Running) != 0 || state.DispatchStatus.CandidateCount != 0 {
					t.Fatalf("closed snapshot entered dispatch: decisions=%+v status=%+v", state.SchedulerDecisions, state.DispatchStatus)
				}
			})
		}
	}
}
