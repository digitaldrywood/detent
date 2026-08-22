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
	"github.com/digitaldrywood/detent/internal/observability"
	"github.com/digitaldrywood/detent/internal/provenance"
	"github.com/digitaldrywood/detent/internal/selector"
	"github.com/digitaldrywood/detent/internal/staleness"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestRefreshStalenessWarningsRecordsDiagnosticWithoutDelivery(t *testing.T) {
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
	if deliveries != 0 {
		t.Fatalf("webhook deliveries = %d, want 0", deliveries)
	}
	events := 0
	for _, event := range state.RecentEvents {
		if event.Event == "fleet_observability_condition" {
			events++
		}
	}
	if events != 1 {
		t.Fatalf("fleet_observability_condition events = %d, want 1", events)
	}
}

func TestRefreshStalenessWarningLifecycle(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		run  func(*testing.T, *Orchestrator, *State, store.Store)
	}{
		{
			name: "human review age remains visible as a review queue condition",
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
				if len(state.StalenessWarnings) != 1 {
					t.Fatalf("second refresh warnings = %#v, want one diagnostic condition", state.StalenessWarnings)
				}
				orch.refreshStalenessWarnings(t.Context(), state, nil, now.Add(2*time.Hour))
				if len(state.StalenessWarnings) != 1 {
					t.Fatalf("aged refresh warnings = %#v, want one diagnostic condition", state.StalenessWarnings)
				}
				for _, warning := range state.StalenessWarnings {
					if warning.Warning.Class != observability.ClassReviewQueue || !warning.DeliveredAt.IsZero() || warning.DeliveryAttempts != 0 {
						t.Fatalf("review queue condition = %#v, want undelivered review queue class", warning)
					}
				}
			},
		},
		{
			name: "human review condition survives a sparse refresh cadence",
			run: func(t *testing.T, orch *Orchestrator, state *State, _ store.Store) {
				enteredAt := now.Add(-4 * time.Hour)
				issue := connector.Issue{ID: "issue-human", Identifier: "corp#74", State: "Human Review"}
				state.BoardIssues = []connector.Issue{issue}
				state.laneEntries[workflowLaneEntryKey(issue)] = enteredAt

				orch.refreshStalenessWarnings(t.Context(), state, nil, now)
				orch.refreshStalenessWarnings(t.Context(), state, nil, now.Add(2*time.Hour))
				if len(state.StalenessWarnings) != 1 {
					t.Fatalf("aged refresh warnings = %#v, want one diagnostic condition", state.StalenessWarnings)
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
		state.StalenessWarnings[id] = StalenessWarning{
			Warning:    staleness.Warning{ID: id, Class: observability.ClassFault, IssueID: id},
			DetectedAt: now,
			Visible:    true,
		}
	}

	orch.deliverStalenessWarnings(t.Context(), &state, now)
	if deliveries != 1 {
		t.Fatalf("first tick webhook deliveries = %d, want 1", deliveries)
	}
	orch.deliverStalenessWarnings(t.Context(), &state, now.Add(time.Minute))
	if deliveries != 2 {
		t.Fatalf("second tick webhook deliveries = %d, want 2", deliveries)
	}
}

func TestHumanGateConditionNeverDelivers(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	backend, err := store.Open(t.Context(), store.Config{Path: filepath.Join(t.TempDir(), "detent.db")})
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	cfg := normalizeConfig(Config{Staleness: staleness.Config{
		Enabled:        true,
		HumanGateRearm: 24 * time.Hour,
		Lanes:          []staleness.LaneThreshold{{State: "Human Review", Threshold: time.Hour, HumanGate: true}},
	}})
	cfg.Project.ID = "detent"
	attempts := 0
	orch := &Orchestrator{
		cfg:                   cfg,
		stalenessWarningStore: backend,
		newStalenessNotifier: func(staleness.DeliveryConfig) (staleness.Notifier, error) {
			return stalenessNotifierFunc(func(context.Context, staleness.Warning) error {
				attempts++
				return nil
			}), nil
		},
	}
	orch.cfg.StalenessDelivery.WebhookURL = "https://example.test/warnings"
	state := newState(cfg)
	issue := connector.Issue{ID: "issue-human", Identifier: "corp#74", State: "Human Review"}
	state.BoardIssues = []connector.Issue{issue}
	state.laneEntries[workflowLaneEntryKey(issue)] = now.Add(-2 * time.Hour)

	orch.refreshStalenessWarnings(t.Context(), &state, nil, now)
	states, err := backend.ListStalenessWarningStates(t.Context(), "detent")
	if err != nil {
		t.Fatalf("ListStalenessWarningStates() error = %v", err)
	}
	if len(states) != 0 {
		t.Fatalf("warning states after failed delivery = %#v, want no reminder marker", states)
	}
	orch.refreshStalenessWarnings(t.Context(), &state, nil, now.Add(stalenessDeliveryRetryBase))
	if attempts != 0 {
		t.Fatalf("webhook attempts = %d, want none", attempts)
	}
	states, err = backend.ListStalenessWarningStates(t.Context(), "detent")
	if err != nil {
		t.Fatalf("ListStalenessWarningStates() after success error = %v", err)
	}
	if len(states) != 0 {
		t.Fatalf("warning states after diagnostic refresh = %#v, want none", states)
	}
}

func TestQueuedHumanGateConditionsNeverDeliver(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{Staleness: staleness.Config{
		Enabled:        true,
		HumanGateRearm: 24 * time.Hour,
		Lanes:          []staleness.LaneThreshold{{State: "Human Review", Threshold: time.Hour, HumanGate: true}},
	}})
	cfg.Project.ID = "detent"
	delivered := []string{}
	orch := &Orchestrator{
		cfg: cfg,
		newStalenessNotifier: func(staleness.DeliveryConfig) (staleness.Notifier, error) {
			return stalenessNotifierFunc(func(_ context.Context, warning staleness.Warning) error {
				delivered = append(delivered, warning.IssueID)
				return nil
			}), nil
		},
	}
	orch.cfg.StalenessDelivery.WebhookURL = "https://example.test/warnings"
	state := newState(cfg)
	for _, id := range []string{"issue-a", "issue-b"} {
		issue := connector.Issue{ID: id, Identifier: id, State: "Human Review"}
		state.BoardIssues = append(state.BoardIssues, issue)
		state.laneEntries[workflowLaneEntryKey(issue)] = now.Add(-2 * time.Hour)
	}

	orch.refreshStalenessWarnings(t.Context(), &state, nil, now)
	orch.refreshStalenessWarnings(t.Context(), &state, nil, now.Add(time.Minute))
	if len(delivered) != 0 {
		t.Fatalf("delivered warnings = %v, want none", delivered)
	}
	if len(state.StalenessWarnings) != 2 {
		t.Fatalf("diagnostic conditions = %#v, want both review queue items", state.StalenessWarnings)
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
		name        string
		issue       connector.Issue
		blocked     *Blocked
		attribution provenance.Attribution
		want        bool
	}{
		{name: "non-blocked issue", issue: connector.Issue{ID: "1", State: "Todo"}},
		{name: "cause-less blocked issue", issue: connector.Issue{ID: "2", State: "Blocked"}},
		{name: "generic blocked reason", issue: connector.Issue{ID: "3", State: "Blocked"}, blocked: &Blocked{Reason: "waiting for dependency"}},
		{name: "operator stop cause", issue: connector.Issue{ID: "4", State: "Blocked"}, blocked: &Blocked{Source: BlockedSourceOperatorStop, StopReason: "operator parked pending review"}, want: true},
		{name: "sticky breaker cause", issue: connector.Issue{ID: "5", State: "Blocked"}, blocked: &Blocked{Reason: dispatchLoopDetectedReason}, want: true},
		{name: "tracked dependency", issue: connector.Issue{ID: "6", State: "Blocked", BlockedBy: []connector.BlockedRef{{Identifier: "detent#1"}}}},
		{
			name:        "authenticated human park with cause",
			issue:       connector.Issue{ID: "7", State: "Blocked", BlockerReason: "operator parked pending decision"},
			attribution: provenance.AttributionFromSource(provenance.SourceHumanSession, provenance.Actor{Login: "operator", Kind: "User"}),
			want:        true,
		},
		{
			name:        "authenticated human move without cause",
			issue:       connector.Issue{ID: "8", State: "Blocked"},
			attribution: provenance.AttributionFromSource(provenance.SourceHumanSession, provenance.Actor{Login: "operator", Kind: "User"}),
		},
		{
			name:    "human-owned recovery park",
			issue:   connector.Issue{ID: "9", State: "Blocked"},
			blocked: &Blocked{Recovery: &workflowLaneBlockedRecoveryMetadata{Owner: blockedRecoveryOwnerHuman, Cause: "waiting for operator approval"}},
			want:    true,
		},
		{
			name:    "orchestrator-owned recovery",
			issue:   connector.Issue{ID: "10", State: "Blocked"},
			blocked: &Blocked{Recovery: &workflowLaneBlockedRecoveryMetadata{Owner: blockedRecoveryOwnerOrchestrator, Cause: "waiting for config fingerprint"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := State{Blocked: map[string]Blocked{}, laneProvenance: map[string]provenance.Attribution{}}
			if tt.blocked != nil {
				state.Blocked[tt.issue.ID] = *tt.blocked
			}
			if tt.attribution.Origin != "" {
				state.laneProvenance[workflowLaneEntryKey(tt.issue)] = tt.attribution
			}
			_, got := recordedStalenessPark(tt.issue, &state, nil)
			if got != tt.want {
				t.Fatalf("recordedStalenessPark() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestStaleRecordedParkCause(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	recordedAt := now.Add(-6 * time.Hour)
	expiredAt := now.Add(-time.Hour)
	tests := []struct {
		name    string
		issue   connector.Issue
		blocked Blocked
		want    bool
		detail  string
	}{
		{
			name:    "holding evidence remains quiet",
			issue:   connector.Issue{ID: "1", State: "Blocked"},
			blocked: Blocked{BlockerEvidence: []telemetry.BlockerEvidence{{Status: "holds", RecordedAt: &recordedAt}}},
		},
		{
			name:    "expired holding evidence needs review",
			issue:   connector.Issue{ID: "2", State: "Blocked"},
			blocked: Blocked{BlockerEvidence: []telemetry.BlockerEvidence{{Status: "holds", Detail: "predicate expired", RecordedAt: &recordedAt, ExpiresAt: &expiredAt}}},
			want:    true,
			detail:  "predicate expired",
		},
		{
			name:    "cleared evidence needs review",
			issue:   connector.Issue{ID: "3", State: "Blocked"},
			blocked: Blocked{BlockerEvidence: []telemetry.BlockerEvidence{{Status: blockerEvidenceStatusCleared, Detail: "dependency merged", RecordedAt: &recordedAt}}},
			want:    true,
			detail:  "dependency merged",
		},
		{
			name:   "resolved tracked dependency needs review",
			issue:  connector.Issue{ID: "4", State: "Blocked", BlockedBy: []connector.BlockedRef{{Identifier: "detent#1", State: "Done"}}},
			want:   true,
			detail: "the recorded dependencies no longer block this item",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := State{Blocked: map[string]Blocked{tt.issue.ID: tt.blocked}}
			_, got, detail, _ := staleRecordedParkCause(tt.issue, &state, recordedAt, "operator park", []string{"done"}, now)
			if got != tt.want {
				t.Fatalf("staleRecordedParkCause() stale = %t, want %t", got, tt.want)
			}
			if detail != tt.detail {
				t.Fatalf("staleRecordedParkCause() detail = %q, want %q", detail, tt.detail)
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
