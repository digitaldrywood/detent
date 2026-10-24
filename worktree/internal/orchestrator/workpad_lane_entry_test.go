package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/scheduler"
)

type laneEntryConnector struct {
	*implementProgressConnector
	entry connector.IssueStateTransition
	found bool
	err   error
}

func (c *laneEntryConnector) CurrentLaneEntry(context.Context, connector.Issue) (connector.IssueStateTransition, bool, error) {
	return c.entry, c.found, c.err
}

func TestTrackerLaneEntrySupersedesStaleWorkpadHold(t *testing.T) {
	t.Parallel()
	recorded := time.Date(2026, 10, 8, 9, 41, 0, 0, time.UTC)
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name         string
		laneReader   bool
		entered      time.Time
		found        bool
		err          error
		action       string
		wantDispatch bool
	}{
		{name: "lane move after the hold releases it", laneReader: true, entered: recorded.Add(8 * time.Hour), found: true, action: "manual takeover hold", wantDispatch: true},
		{name: "lane move after a human-owned blocker releases it", laneReader: true, entered: recorded.Add(8 * time.Hour), found: true, wantDispatch: true},
		{name: "hold recorded after the lane move still holds", laneReader: true, entered: recorded.Add(-time.Hour), found: true, action: "manual takeover hold"},
		{name: "unknown lane entry keeps the hold", laneReader: true, action: "manual takeover hold"},
		{name: "lane entry read failure keeps the hold", laneReader: true, entered: recorded.Add(8 * time.Hour), found: true, err: errors.New("history unavailable"), action: "manual takeover hold"},
		{name: "tracker without lane history keeps the hold", action: "manual takeover hold"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			issue := implementProgressIssueWithoutPR()
			issue.State = "Todo"
			issue.AssignedToWorker = true
			body := implementProgressWorkpadComment("", tt.action)
			if tt.action == "" {
				body = "## Codex Workpad\n\n```detent-status\nschema: 1\nstatus: blocked\nblockers:\n  - reason: Approve the hardware check.\n    owner: human\nhuman_action: null\n```"
			}
			issue.Comments = []connector.IssueComment{{Body: body, CreatedAt: &recorded}}
			base := &implementProgressConnector{refreshed: issue, hydrated: issue}
			var tracker connector.Connector = base
			if tt.laneReader {
				tracker = &laneEntryConnector{implementProgressConnector: base, entry: connector.IssueStateTransition{EnteredAt: tt.entered}, found: tt.found, err: tt.err}
			}
			cfg := normalizeConfig(Config{
				Project:             scheduler.ProjectCandidate{ID: "detent"},
				MaxConcurrentAgents: 1,
				ActiveStates:        []string{"Todo", "In Progress", "Rework"},
				ObservedStates:      []string{"Blocked", "Human Review"},
				TerminalStates:      []string{"Done"},
			})
			orch := &Orchestrator{cfg: cfg, connector: tracker}
			state := newState(cfg)
			decision := orch.liveDispatchPlanner(t.Context(), nil).dispatchableIssueDecision(issue, &state, false, now, "")
			if tt.wantDispatch {
				if !decision.dispatchable {
					t.Fatalf("decision = %+v, want dispatchable after the tracker lane move", decision)
				}
				return
			}
			if decision.dispatchable || decision.reason != dispatchSkipBlockedByDependency {
				t.Fatalf("decision = %+v, want %q", decision, dispatchSkipBlockedByDependency)
			}
		})
	}
}
