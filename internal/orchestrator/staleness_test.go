package orchestrator

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/selector"
	"github.com/digitaldrywood/detent/internal/staleness"
	"github.com/digitaldrywood/detent/internal/store"
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

func TestRefreshStalenessWarningLifecycle(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		run  func(*testing.T, *Orchestrator, *State, store.Store)
	}{
		{
			name: "human gate reminder fires once and rearms",
			run: func(t *testing.T, orch *Orchestrator, state *State, _ store.Store) {
				enteredAt := now.Add(-4 * time.Hour)
				issue := connector.Issue{ID: "issue-human", Identifier: "corp#74", State: "Human Review"}
				state.BoardIssues = []connector.Issue{issue}
				state.laneEntries[workflowLaneEntryKey(issue)] = enteredAt

				orch.refreshStalenessWarnings(t.Context(), state, nil, now)
				if len(state.StalenessWarnings) != 1 {
					t.Fatalf("first refresh warnings = %#v, want one reminder", state.StalenessWarnings)
				}
				orch.refreshStalenessWarnings(t.Context(), state, nil, now.Add(time.Minute))
				if len(state.StalenessWarnings) != 0 {
					t.Fatalf("second refresh warnings = %#v, want reminder silenced", state.StalenessWarnings)
				}
				orch.refreshStalenessWarnings(t.Context(), state, nil, now.Add(2*time.Hour))
				if len(state.StalenessWarnings) != 1 {
					t.Fatalf("rearmed refresh warnings = %#v, want one reminder", state.StalenessWarnings)
				}
			},
		},
		{
			name: "acknowledgement survives recompute and clears on lane reentry",
			run: func(t *testing.T, orch *Orchestrator, state *State, backend store.Store) {
				firstEntry := now.Add(-4 * time.Hour)
				issue := connector.Issue{ID: "issue-ack", Identifier: "detent#1926", State: "Merging"}
				state.BoardIssues = []connector.Issue{issue}
				key := workflowLaneEntryKey(issue)
				state.laneEntries[key] = firstEntry

				orch.refreshStalenessWarnings(t.Context(), state, nil, now)
				if len(state.StalenessWarnings) != 1 {
					t.Fatalf("first refresh warnings = %#v, want one", state.StalenessWarnings)
				}
				var warningID string
				for id := range state.StalenessWarnings {
					warningID = id
				}
				if err := backend.AcknowledgeStalenessWarning(t.Context(), "detent", warningID, now); err != nil {
					t.Fatalf("AcknowledgeStalenessWarning() error = %v", err)
				}
				orch.refreshStalenessWarnings(t.Context(), state, nil, now.Add(time.Minute))
				if len(state.StalenessWarnings) != 0 {
					t.Fatalf("acknowledged warnings = %#v, want none", state.StalenessWarnings)
				}

				state.laneEntries[key] = now.Add(-3 * time.Hour)
				orch.refreshStalenessWarnings(t.Context(), state, nil, now.Add(time.Minute))
				if len(state.StalenessWarnings) != 1 {
					t.Fatalf("new-entry warnings = %#v, want new condition", state.StalenessWarnings)
				}
				if _, exists := state.StalenessWarnings[warningID]; exists {
					t.Fatalf("new lane entry reused acknowledged warning ID %q", warningID)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend, err := store.Open(t.Context(), store.Config{Path: filepath.Join(t.TempDir(), "detent.db")})
			if err != nil {
				t.Fatalf("store.Open() error = %v", err)
			}
			t.Cleanup(func() { _ = backend.Close() })
			cfg := normalizeConfig(Config{
				Staleness: staleness.Config{
					Enabled:        true,
					HumanGateRearm: 2 * time.Hour,
					Lanes: []staleness.LaneThreshold{
						{State: "Human Review", Threshold: 2 * time.Hour, HumanGate: true},
						{State: "Merging", Threshold: 2 * time.Hour},
					},
				},
			})
			cfg.Project.ID = "detent"
			orch := &Orchestrator{cfg: cfg, stalenessWarningStore: backend}
			state := newState(cfg)
			tt.run(t, orch, &state, backend)
		})
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

func TestRecordedStalenessPark(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		issue   connector.Issue
		blocked *Blocked
		want    bool
	}{
		{name: "non-blocked issue", issue: connector.Issue{ID: "1", State: "Todo"}},
		{name: "cause-less blocked issue", issue: connector.Issue{ID: "2", State: "Blocked"}},
		{name: "operator park cause", issue: connector.Issue{ID: "3", State: "Blocked"}, blocked: &Blocked{Reason: "operator parked pending review"}, want: true},
		{name: "sticky breaker cause", issue: connector.Issue{ID: "4", State: "Blocked"}, blocked: &Blocked{Recovery: &workflowLaneBlockedRecoveryMetadata{Cause: "project failure breaker"}}, want: true},
		{name: "tracked dependency", issue: connector.Issue{ID: "5", State: "Blocked", BlockedBy: []connector.BlockedRef{{Identifier: "detent#1"}}}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := State{Blocked: map[string]Blocked{}}
			if tt.blocked != nil {
				state.Blocked[tt.issue.ID] = *tt.blocked
			}
			if got := recordedStalenessPark(tt.issue, &state); got != tt.want {
				t.Fatalf("recordedStalenessPark() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestRefreshStalenessWarningsProviderRateWindowPacing(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 7, 15, 0, 0, 0, time.UTC)
	enteredAt := now.Add(-13 * time.Hour)
	candidate := dispatchTestIssue("issue-paced", "Todo")
	candidate.StageUpdatedAt = &enteredAt

	tests := []struct {
		name       string
		completion *time.Time
		wantKind   string
	}{
		{
			name:       "paced but progressing",
			completion: timePointer(now.Add(-time.Hour)),
		},
		{
			name:     "paced and stalled",
			wantKind: staleness.KindProjectLiveness,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := normalizeConfig(Config{
				BillingMode:         workflowconfig.BillingModeSubscription,
				MaxConcurrentAgents: 10,
				ActiveStates:        []string{"Todo"},
				TerminalStates:      []string{"Done"},
				Staleness: staleness.Config{
					Enabled:                       true,
					NoCompletionThreshold:         12 * time.Hour,
					RepeatedDecisionCount:         20,
					RepeatedDecisionWindow:        24 * time.Hour,
					RepeatedDecisionBenignReasons: []string{dispatchSkipRateWindowBackpressure},
				},
			})
			cfg.Project.ID = "detent"
			orch := &Orchestrator{cfg: cfg}
			state := newState(cfg)
			state.RateLimits = providerRateLimits(50, 100)
			for index := range 5 {
				id := fmt.Sprintf("running-%d", index)
				state.Running[id] = Running{Issue: dispatchTestIssue(id, "Todo")}
			}
			if tt.completion != nil {
				state.Completed["completed"] = Completed{CompletedAt: *tt.completion}
			}
			for index := range 20 {
				state.SchedulerDecisions = append(state.SchedulerDecisions, telemetry.SchedulerDecision{
					IssueID:    candidate.ID,
					Identifier: candidate.Identifier,
					Result:     "skipped",
					Reason:     dispatchSkipRateWindowBackpressure,
					DecisionAt: now.Add(-time.Hour).Add(time.Duration(index) * 3 * time.Minute),
				})
			}

			orch.refreshStalenessWarnings(t.Context(), &state, []connector.Issue{candidate}, now)
			if tt.wantKind == "" {
				if len(state.StalenessWarnings) != 0 {
					t.Fatalf("staleness warnings = %#v, want none", state.StalenessWarnings)
				}
				return
			}
			if len(state.StalenessWarnings) != 1 {
				t.Fatalf("staleness warnings = %#v, want one %s warning", state.StalenessWarnings, tt.wantKind)
			}
			for _, warning := range state.StalenessWarnings {
				if warning.Warning.Kind != tt.wantKind {
					t.Fatalf("warning kind = %q, want %q", warning.Warning.Kind, tt.wantKind)
				}
			}
		})
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

	got := stalenessDecisions(decisions, stalenessDecisionIssueIndex(&state, nil), state.laneEntries)
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
