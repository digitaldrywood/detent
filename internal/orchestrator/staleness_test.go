package orchestrator

import (
	"context"
	"slices"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/selector"
	"github.com/digitaldrywood/detent/internal/staleness"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestRefreshStalenessWarningsEmitsAndDeliversOncePerActiveCondition(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 30, 15, 0, 0, 0, time.UTC)
	enteredAt := now.Add(-3 * time.Hour)
	issue := connector.Issue{
		ID:             "issue-1574",
		Identifier:     "digitaldrywood/detent#1574",
		State:          "Merging",
		StageUpdatedAt: &enteredAt,
	}
	cfg := normalizeConfig(Config{
		Staleness: staleness.Config{
			Enabled: true,
			Lanes: []staleness.LaneThreshold{{
				State:     "Merging",
				Threshold: 2 * time.Hour,
			}},
		},
	})
	cfg.Project.ID = "detent"
	deliveries := 0
	orch := &Orchestrator{
		cfg: cfg,
		newStalenessNotifier: func(staleness.DeliveryConfig) (staleness.Notifier, error) {
			return stalenessNotifierFunc(func(context.Context, staleness.Warning) error {
				deliveries++
				return nil
			}), nil
		},
	}
	orch.cfg.StalenessDelivery.WebhookURL = "https://example.test/warnings"
	state := newState(cfg)
	state.BoardIssues = []connector.Issue{issue}
	state.laneEntries[workflowLaneEntryKey(issue)] = enteredAt

	orch.refreshStalenessWarnings(t.Context(), &state, nil, now)
	orch.refreshStalenessWarnings(t.Context(), &state, nil, now.Add(time.Minute))

	if len(state.StalenessWarnings) != 1 {
		t.Fatalf("staleness warnings = %#v, want one active warning", state.StalenessWarnings)
	}
	if deliveries != 1 {
		t.Fatalf("webhook deliveries = %d, want 1", deliveries)
	}
	events := 0
	for _, event := range state.RecentEvents {
		if event.Event == "fleet_staleness_warning" {
			events++
		}
	}
	if events != 1 {
		t.Fatalf("fleet_staleness_warning events = %d, want 1", events)
	}
}

func TestDeliverStalenessWarningsBoundsWorkPerTick(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 30, 15, 0, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{
		Staleness: staleness.Config{
			Enabled: true,
			Lanes: []staleness.LaneThreshold{{
				State:     "Merging",
				Threshold: time.Hour,
			}},
		},
	})
	cfg.Project.ID = "detent"
	deliveries := 0
	orch := &Orchestrator{
		cfg: cfg,
		newStalenessNotifier: func(staleness.DeliveryConfig) (staleness.Notifier, error) {
			return stalenessNotifierFunc(func(context.Context, staleness.Warning) error {
				deliveries++
				return nil
			}), nil
		},
	}
	orch.cfg.StalenessDelivery.WebhookURL = "https://example.test/warnings"
	state := newState(cfg)
	for _, id := range []string{"issue-a", "issue-b"} {
		enteredAt := now.Add(-2 * time.Hour)
		issue := connector.Issue{ID: id, Identifier: id, State: "Merging", StageUpdatedAt: &enteredAt}
		state.BoardIssues = append(state.BoardIssues, issue)
		state.laneEntries[workflowLaneEntryKey(issue)] = enteredAt
	}

	orch.refreshStalenessWarnings(t.Context(), &state, nil, now)
	if deliveries != 1 {
		t.Fatalf("first tick webhook deliveries = %d, want 1", deliveries)
	}
	orch.refreshStalenessWarnings(t.Context(), &state, nil, now.Add(time.Minute))
	if deliveries != 2 {
		t.Fatalf("second tick webhook deliveries = %d, want 2", deliveries)
	}
}

func TestStalenessDispatchableItemsUsesDispatchPlannerEligibility(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 30, 15, 0, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{
		MaxConcurrentAgents: 1,
		ActiveStates:        []string{"Todo"},
		TerminalStates:      []string{"Done"},
		Authorization: selector.Selector{
			Labels: selector.Labels{Include: []string{"authorized"}},
		},
	})
	orch := &Orchestrator{cfg: cfg}
	state := newState(cfg)
	unauthorized := connector.Issue{
		ID:               "issue-a",
		Identifier:       "issue-a",
		Title:            "Unauthorized issue",
		State:            "Todo",
		AssignedToWorker: true,
	}
	authorized := connector.Issue{
		ID:               "issue-b",
		Identifier:       "issue-b",
		Title:            "Authorized issue",
		State:            "Todo",
		Labels:           []string{"authorized"},
		AssignedToWorker: true,
	}

	items := orch.stalenessDispatchableItems([]connector.Issue{unauthorized, authorized}, &state, now)
	if len(items) != 1 || items[0].ID != authorized.ID {
		t.Fatalf("dispatchable staleness items = %#v, want only authorized issue", items)
	}
}

func TestConfigFromWorkflowIncludesStalenessDecisionPolicy(t *testing.T) {
	t.Parallel()
	workflow := workflowconfig.Default()
	workflow.Observability.Staleness.RepeatedDecisionBenignReasons = []string{"planned_maintenance"}
	workflow.Tracker.TerminalStates = []string{"Done", "Cancelled"}

	got := ConfigFromWorkflow(workflow).Staleness
	if !slices.Equal(got.RepeatedDecisionBenignReasons, []string{"planned_maintenance"}) {
		t.Fatalf("RepeatedDecisionBenignReasons = %#v, want configured reason", got.RepeatedDecisionBenignReasons)
	}
	if !slices.Equal(got.TerminalStates, workflow.Tracker.TerminalStates) {
		t.Fatalf("TerminalStates = %#v, want %#v", got.TerminalStates, workflow.Tracker.TerminalStates)
	}
}

func TestStalenessDecisionsCarryCurrentIssueState(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 30, 15, 0, 0, 0, time.UTC)
	state := State{
		BoardIssues: []connector.Issue{
			{
				ID:     "issue-closed",
				State:  "Done",
				Closed: true,
			},
			{
				ID:    "issue-merged",
				State: "Merging",
				PullRequest: &connector.PullRequest{
					State: "MERGED",
				},
			},
		},
		Completed: map[string]Completed{
			"issue-stale-completed": {
				Issue: connector.Issue{
					ID:    "issue-stale-completed",
					State: "Human Review",
				},
			},
		},
	}
	decisions := []telemetry.SchedulerDecision{
		{IssueID: "issue-closed", Result: "skipped", Reason: "merge_slot_revoked", DecisionAt: now},
		{IssueID: "issue-merged", Reason: "merge_slot_revoked", DecisionAt: now},
		{IssueID: "issue-stale-completed", Reason: "merge_slot_revoked", DecisionAt: now},
		{IssueID: "issue-gone", Reason: "merge_slot_revoked", DecisionAt: now},
	}

	got := stalenessDecisions(decisions, stalenessDecisionIssueIndex(&state, nil))
	if len(got) != len(decisions) {
		t.Fatalf("stalenessDecisions() = %#v, want %d decisions", got, len(decisions))
	}
	if got[0].CurrentState != "Done" || !got[0].Closed {
		t.Fatalf("closed decision = %#v, want current Done and closed", got[0])
	}
	if got[0].Result != "skipped" {
		t.Fatalf("closed decision result = %q, want skipped", got[0].Result)
	}
	if got[1].CurrentState != "Merging" || !got[1].Merged {
		t.Fatalf("merged decision = %#v, want current Merging and merged", got[1])
	}
	for _, index := range []int{2, 3} {
		if got[index].CurrentState != "" || got[index].Closed || got[index].Merged {
			t.Fatalf("historical decision = %#v, want no current item state", got[index])
		}
	}
}

type stalenessNotifierFunc func(context.Context, staleness.Warning) error

func (fn stalenessNotifierFunc) Notify(ctx context.Context, warning staleness.Warning) error {
	return fn(ctx, warning)
}
