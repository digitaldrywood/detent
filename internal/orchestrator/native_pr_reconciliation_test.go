package orchestrator

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestNativeLinkedPRMetadataDoesNotReconcileWorkflow(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, prState, ci string
	}{
		{"unproduced CI", "OPEN", ""},
		{"failed external CI", "OPEN", "failure"},
		{"merged external PR", "MERGED", "success"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
			cfg := normalizeConfig(Config{
				AutoPromote:    AutoPromoteConfig{Enabled: true},
				ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
				TerminalStates: []string{"Done", "Cancelled"},
			})
			issue := autoPromoteTickIssue("release-delivery", nil, &connector.PullRequest{
				Number: 42, URL: "https://github.test/example-org/example-project/pull/42",
				State: tt.prState, CIStatus: tt.ci, MergeableState: "clean", MergedAt: &now,
			})
			issue.State = "Todo"
			tracker := &nativeWorkflowConnector{autoPromoteTickConnector: &autoPromoteTickConnector{
				stateIssues: []connector.Issue{issue},
			}}
			orch := &Orchestrator{cfg: cfg, connector: tracker}
			state := newState(cfg)
			state.Retry[issue.ID] = Retry{}
			transitioned := orch.reconcileStaleLinkedPullRequestIssues(t.Context(), &state, []connector.Issue{issue}, now)
			if len(transitioned) != 0 || len(tracker.updates) != 0 || len(tracker.comments) != 0 {
				t.Fatalf("external PR metadata changed native workflow: transitions=%v updates=%v comments=%v", transitioned, tracker.updates, tracker.comments)
			}
			if _, retained := state.Retry[issue.ID]; !retained {
				t.Fatal("external PR reconciliation cleared native dispatch memory")
			}
		})
	}
}
