package staleness

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestEvaluate(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 30, 15, 0, 0, 0, time.UTC)
	cfg := Config{
		Enabled:           true,
		LaneReentryWindow: 7 * 24 * time.Hour,
		Lanes: []LaneThreshold{
			{State: "Human Review", Threshold: 48 * time.Hour, HumanGate: true},
			{State: "Blocked", Threshold: 24 * time.Hour},
		},
		NoCompletionThreshold:  12 * time.Hour,
		NoMergeThreshold:       6 * time.Hour,
		RepeatedDecisionCount:  3,
		RepeatedDecisionWindow: 24 * time.Hour,
	}

	tests := []struct {
		name      string
		input     Input
		wantKinds []string
		wantHuman bool
	}{
		{
			name: "lane threshold emits one human reminder",
			input: Input{
				ProjectID: "detent",
				Items: []Item{
					{ID: "41", State: "Human Review", EnteredAt: now.Add(-72 * time.Hour)},
					{ID: "41", State: "Human Review", EnteredAt: now.Add(-72 * time.Hour)},
				},
			},
			wantKinds: []string{KindLaneAging},
			wantHuman: true,
		},
		{
			name: "recorded park stays quiet",
			input: Input{
				ProjectID: "detent",
				Items: []Item{{
					ID:           "42",
					State:        "Blocked",
					EnteredAt:    now.Add(-72 * time.Hour),
					RecordedPark: true,
				}},
			},
		},
		{
			name: "stale recorded park cause warns",
			input: Input{
				ProjectID: "detent",
				Items: []Item{{
					ID:              "43",
					State:           "Blocked",
					EnteredAt:       now.Add(-72 * time.Hour),
					RecordedPark:    true,
					ParkCauseKey:    "config-a",
					ParkCauseStale:  true,
					ParkCauseDetail: "the recorded config fingerprint no longer matches",
					ParkCauseSince:  now.Add(-time.Hour),
				}},
			},
			wantKinds: []string{KindParkCauseStale},
		},
		{
			name: "rolling lane reentry warns without restoring lifetime residency",
			input: Input{
				ProjectID: "detent",
				Items: []Item{{
					ID:        "44",
					State:     "Human Review",
					EnteredAt: now.Add(-20 * time.Hour),
					LaneVisits: []LaneVisit{
						{State: "Human Review", EnteredAt: now.Add(-6 * 24 * time.Hour), ExitedAt: now.Add(-4 * 24 * time.Hour)},
						{State: "Human Review", EnteredAt: now.Add(-3 * 24 * time.Hour), ExitedAt: now.Add(-2 * 24 * time.Hour)},
					},
				}},
			},
			wantKinds: []string{KindLaneReentry},
			wantHuman: true,
		},
		{
			name: "dispatchable queue without completions warns",
			input: Input{
				ProjectID:    "detent",
				Dispatchable: []Item{{ID: "1", EnteredAt: now.Add(-13 * time.Hour)}},
			},
			wantKinds: []string{KindProjectLiveness},
		},
		{
			name: "empty queue without completions stays quiet",
			input: Input{
				ProjectID: "detent",
			},
		},
		{
			name: "recently populated queue stays quiet",
			input: Input{
				ProjectID:    "detent",
				Dispatchable: []Item{{ID: "1", EnteredAt: now.Add(-time.Hour)}},
				Completions:  []Completion{{At: now.Add(-48 * time.Hour)}},
			},
		},
		{
			name: "merge queue without success warns",
			input: Input{
				ProjectID:  "detent",
				MergeQueue: []Item{{ID: "2", EnteredAt: now.Add(-7 * time.Hour)}},
			},
			wantKinds: []string{KindMergeLiveness},
		},
		{
			name: "repeated identical decisions warn",
			input: Input{
				ProjectID: "detent",
				Decisions: []Decision{
					{IssueID: "3", CurrentState: "Merging", Result: "skipped", Reason: "merge_slot_revoked", At: now.Add(-3 * time.Hour)},
					{IssueID: "3", CurrentState: "Merging", Result: "skipped", Reason: "merge_slot_revoked", At: now.Add(-2 * time.Hour)},
					{IssueID: "3", CurrentState: "Merging", Result: "skipped", Reason: "merge_slot_revoked", At: now.Add(-time.Hour)},
					{IssueID: "3", CurrentState: "Merging", Result: "skipped", Reason: "other", At: now.Add(-time.Hour)},
				},
			},
			wantKinds: []string{KindRepeatedDecision},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Evaluate(cfg, tt.input, now)
			if len(got) != len(tt.wantKinds) {
				t.Fatalf("Evaluate() warnings = %#v, want kinds %v", got, tt.wantKinds)
			}
			for index, kind := range tt.wantKinds {
				if got[index].Kind != kind {
					t.Fatalf("Evaluate()[%d].Kind = %q, want %q", index, got[index].Kind, kind)
				}
			}
			if len(got) > 0 && got[0].WaitingOnHuman != tt.wantHuman {
				t.Fatalf("Evaluate()[0].WaitingOnHuman = %t, want %t", got[0].WaitingOnHuman, tt.wantHuman)
			}
		})
	}
}

func TestEvaluateLaneReentryUsesBoundedCumulativeResidency(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 21, 15, 0, 0, 0, time.UTC)
	cfg := Config{
		Enabled:           true,
		LaneReentryWindow: 7 * 24 * time.Hour,
		Lanes:             []LaneThreshold{{State: "Human Review", Threshold: 72 * time.Hour, HumanGate: true}},
	}
	tests := []struct {
		name     string
		item     Item
		wantKind string
	}{
		{
			name: "recent round trips accumulate",
			item: Item{ID: "74", State: "Human Review", EnteredAt: now.Add(-20 * time.Hour), LaneVisits: []LaneVisit{
				{State: "Human Review", EnteredAt: now.Add(-6 * 24 * time.Hour), ExitedAt: now.Add(-4 * 24 * time.Hour)},
				{State: "Human Review", EnteredAt: now.Add(-3 * 24 * time.Hour), ExitedAt: now.Add(-2 * 24 * time.Hour)},
			}},
			wantKind: KindLaneReentry,
		},
		{
			name: "old residency falls outside rolling window",
			item: Item{ID: "74", State: "Human Review", EnteredAt: now.Add(-20 * time.Hour), LaneVisits: []LaneVisit{
				{State: "Human Review", EnteredAt: now.Add(-14 * 24 * time.Hour), ExitedAt: now.Add(-10 * 24 * time.Hour)},
			}},
		},
		{
			name:     "current continuous residency uses lane aging",
			item:     Item{ID: "74", State: "Human Review", EnteredAt: now.Add(-72 * time.Hour)},
			wantKind: KindLaneAging,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Evaluate(cfg, Input{ProjectID: "detent", Items: []Item{tt.item}}, now)
			if tt.wantKind == "" {
				if len(got) != 0 {
					t.Fatalf("Evaluate() = %#v, want no warning", got)
				}
				return
			}
			if len(got) != 1 || got[0].Kind != tt.wantKind {
				t.Fatalf("Evaluate() = %#v, want one %s warning", got, tt.wantKind)
			}
		})
	}
}

func TestWarningIdentityChangesWithCondition(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)

	tests := []struct {
		name   string
		first  Input
		second Input
	}{
		{
			name:   "lane reentry",
			first:  Input{ProjectID: "detent", Items: []Item{{ID: "74", State: "Human Review", EnteredAt: now.Add(-96 * time.Hour)}}},
			second: Input{ProjectID: "detent", Items: []Item{{ID: "74", State: "Human Review", EnteredAt: now.Add(-80 * time.Hour)}}},
		},
		{
			name: "repeated decision lane change",
			first: Input{ProjectID: "detent", Decisions: []Decision{
				{IssueID: "1926", CurrentState: "Todo", LaneEnteredAt: now.Add(-4 * time.Hour), Result: "skipped", Reason: "dispatch_loop", At: now.Add(-3 * time.Hour)},
				{IssueID: "1926", CurrentState: "Todo", LaneEnteredAt: now.Add(-4 * time.Hour), Result: "skipped", Reason: "dispatch_loop", At: now.Add(-2 * time.Hour)},
			}},
			second: Input{ProjectID: "detent", Decisions: []Decision{
				{IssueID: "1926", CurrentState: "Rework", LaneEnteredAt: now.Add(-90 * time.Minute), Result: "skipped", Reason: "dispatch_loop", At: now.Add(-time.Hour)},
				{IssueID: "1926", CurrentState: "Rework", LaneEnteredAt: now.Add(-90 * time.Minute), Result: "skipped", Reason: "dispatch_loop", At: now.Add(-30 * time.Minute)},
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{
				Enabled:                true,
				Lanes:                  []LaneThreshold{{State: "Human Review", Threshold: time.Hour}},
				RepeatedDecisionCount:  2,
				RepeatedDecisionWindow: 24 * time.Hour,
			}
			first := Evaluate(cfg, tt.first, now)
			second := Evaluate(cfg, tt.second, now)
			if len(first) != 1 || len(second) != 1 {
				t.Fatalf("warning counts = %d and %d, want one each", len(first), len(second))
			}
			if first[0].ID == second[0].ID {
				t.Fatalf("warning ID remained %q after condition changed", first[0].ID)
			}
		})
	}
}

func TestEvaluateRepeatedDecisions(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 30, 15, 0, 0, 0, time.UTC)
	baseConfig := Config{
		Enabled:                true,
		RepeatedDecisionCount:  20,
		RepeatedDecisionWindow: 24 * time.Hour,
		RepeatedDecisionBenignReasons: []string{
			"already_running",
			"blocked_by_dependency",
			"github_rest_capacity_paused",
			"github_rest_recovery",
			"global_capacity_full",
			"provider_rate_window_backpressure",
			"reserved_for_higher_priority_project",
		},
		TerminalStates: []string{"Done", "Cancelled", "Canceled"},
	}

	tests := []struct {
		name      string
		configure func(*Config)
		decisions []Decision
		want      int
	}{
		{
			name:      "60 minute healthy run stays quiet",
			decisions: repeatedDecisions(now.Add(-time.Hour), 31, 2*time.Minute, "already_running", "In Progress"),
		},
		{
			name:      "dependency wait stays quiet",
			decisions: repeatedDecisions(now.Add(-time.Hour), 20, 3*time.Minute, "blocked_by_dependency", "Blocked"),
		},
		{
			name:      "global capacity wait stays quiet",
			decisions: repeatedDecisions(now.Add(-time.Hour), 20, 3*time.Minute, "global_capacity_full", "Todo"),
		},
		{
			name:      "provider rate window pacing stays quiet",
			decisions: repeatedDecisions(now.Add(-time.Hour), 20, 3*time.Minute, "provider_rate_window_backpressure", "Todo"),
		},
		{
			name:      "REST capacity pause stays quiet",
			decisions: repeatedDecisions(now.Add(-time.Hour), 20, 3*time.Minute, "github_rest_capacity_paused", "Todo"),
		},
		{
			name:      "REST recovery stays quiet",
			decisions: repeatedDecisions(now.Add(-time.Hour), 20, 3*time.Minute, "github_rest_recovery", "Todo"),
		},
		{
			name:      "priority reservation stays quiet",
			decisions: repeatedDecisions(now.Add(-time.Hour), 20, 3*time.Minute, "reserved_for_higher_priority_project", "Todo"),
		},
		{
			name: "successful selections stay quiet regardless of count",
			decisions: mutateDecisions(
				repeatedDecisions(now.Add(-4*time.Hour), 104, 2*time.Minute, "selected", "Todo"),
				func(decision *Decision) { decision.Result = "selected" },
			),
		},
		{
			name: "operator configured reason stays quiet",
			configure: func(cfg *Config) {
				cfg.RepeatedDecisionBenignReasons = append(cfg.RepeatedDecisionBenignReasons, "planned_maintenance")
			},
			decisions: repeatedDecisions(now.Add(-time.Hour), 20, 3*time.Minute, "planned_maintenance", "Todo"),
		},
		{
			name: "closed issue stays quiet",
			decisions: mutateDecisions(
				repeatedDecisions(now.Add(-time.Hour), 20, 3*time.Minute, "merge_slot_revoked", "Merging"),
				func(decision *Decision) { decision.Closed = true },
			),
		},
		{
			name: "merged issue stays quiet",
			decisions: mutateDecisions(
				repeatedDecisions(now.Add(-time.Hour), 20, 3*time.Minute, "merge_slot_revoked", "Merging"),
				func(decision *Decision) { decision.Merged = true },
			),
		},
		{
			name:      "cancelled issue stays quiet",
			decisions: repeatedDecisions(now.Add(-time.Hour), 20, 3*time.Minute, "merge_slot_revoked", "Cancelled"),
		},
		{
			name:      "historical issue absent from current items stays quiet",
			decisions: repeatedDecisions(now.Add(-time.Hour), 20, 3*time.Minute, "merge_slot_revoked", ""),
		},
		{
			name:      "genuine stall warns",
			decisions: repeatedDecisions(now.Add(-time.Hour), 20, 3*time.Minute, "merge_slot_revoked", "Merging"),
			want:      1,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := baseConfig
			cfg.RepeatedDecisionBenignReasons = append([]string(nil), baseConfig.RepeatedDecisionBenignReasons...)
			if tt.configure != nil {
				tt.configure(&cfg)
			}
			got := Evaluate(cfg, Input{ProjectID: "detent", Decisions: tt.decisions}, now)
			if len(got) != tt.want {
				t.Fatalf("Evaluate() warnings = %#v, want %d", got, tt.want)
			}
			if tt.want == 1 && (got[0].Kind != KindRepeatedDecision || got[0].Count != 20) {
				t.Fatalf("Evaluate()[0] = %#v, want repeated decision warning with count 20", got[0])
			}
		})
	}
}

func TestEvaluateRepeatedDecisionRendersAuthorizationDetail(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 17, 20, 0, 0, 0, time.UTC)
	const detail = "issue does not match authorization selector: missing required label `detent`"
	decisions := repeatedDecisions(now.Add(-time.Hour), 3, 20*time.Minute, "authorization_selector_declined", "Todo")
	for index := range decisions {
		decisions[index].Detail = detail
	}
	warnings := Evaluate(Config{
		Enabled:                true,
		RepeatedDecisionCount:  3,
		RepeatedDecisionWindow: 24 * time.Hour,
	}, Input{ProjectID: "corp", Decisions: decisions}, now)

	if len(warnings) != 1 || warnings[0].Reason != "authorization_selector_declined" || warnings[0].Detail != detail {
		t.Fatalf("warnings = %#v, want actionable authorization detail", warnings)
	}
}

func TestEvaluateProviderRateWindowPacing(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 7, 15, 0, 0, 0, time.UTC)
	cfg := Config{
		Enabled:                       true,
		NoCompletionThreshold:         12 * time.Hour,
		RepeatedDecisionCount:         20,
		RepeatedDecisionWindow:        24 * time.Hour,
		RepeatedDecisionBenignReasons: []string{"provider_rate_window_backpressure"},
	}

	tests := []struct {
		name        string
		completions []Completion
		wantKind    string
	}{
		{
			name:        "paced but progressing",
			completions: []Completion{{At: now.Add(-time.Hour)}},
		},
		{
			name:     "paced and stalled",
			wantKind: KindProjectLiveness,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			warnings := Evaluate(cfg, Input{
				ProjectID:    "detent",
				Dispatchable: []Item{{ID: "issue-1", EnteredAt: now.Add(-13 * time.Hour)}},
				Completions:  tt.completions,
				Decisions:    repeatedDecisions(now.Add(-time.Hour), 20, 3*time.Minute, "provider_rate_window_backpressure", "Todo"),
			}, now)
			if tt.wantKind == "" {
				if len(warnings) != 0 {
					t.Fatalf("Evaluate() warnings = %#v, want none", warnings)
				}
				return
			}
			if len(warnings) != 1 || warnings[0].Kind != tt.wantKind {
				t.Fatalf("Evaluate() warnings = %#v, want one %s warning", warnings, tt.wantKind)
			}
		})
	}
}

func repeatedDecisions(start time.Time, count int, interval time.Duration, reason string, state string) []Decision {
	decisions := make([]Decision, 0, count)
	for index := range count {
		decisions = append(decisions, Decision{
			IssueID:      "issue-1",
			Identifier:   "digitaldrywood/detent#1",
			CurrentState: state,
			Result:       "skipped",
			Reason:       reason,
			At:           start.Add(time.Duration(index) * interval),
		})
	}
	return decisions
}

func mutateDecisions(decisions []Decision, mutate func(*Decision)) []Decision {
	for index := range decisions {
		mutate(&decisions[index])
	}
	return decisions
}

func TestNewNotifierDeliversWebhook(t *testing.T) {
	t.Parallel()
	received := make(chan Notification, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Authorization"); got != "Bearer example" {
			t.Errorf("Authorization = %q, want Bearer example", got)
		}
		var payload Notification
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("decode payload: %v", err)
		}
		received <- payload
		writer.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	notifier, err := NewNotifier(DeliveryConfig{
		WebhookURL: server.URL,
		Headers:    map[string]string{"Authorization": "Bearer example"},
		Timeout:    time.Second,
	})
	if err != nil {
		t.Fatalf("NewNotifier() error = %v", err)
	}
	warning := Warning{
		ID:               "warning-1",
		Kind:             KindLaneAging,
		AgeSeconds:       3600,
		ThresholdSeconds: 1800,
	}
	if err := notifier.Notify(t.Context(), warning); err != nil {
		t.Fatalf("Notify() error = %v", err)
	}
	payload := <-received
	if payload.Schema != 1 || payload.Event != "detent.staleness.warning" || payload.Warning.ID != warning.ID {
		t.Fatalf("payload = %#v", payload)
	}
	if payload.Warning.AgeSeconds != 3600 || payload.Warning.ThresholdSeconds != 1800 {
		t.Fatalf("payload warning durations = %#v", payload.Warning)
	}
}

func TestNewNotifierRejectsMissingURL(t *testing.T) {
	t.Parallel()
	if _, err := NewNotifier(DeliveryConfig{}); err == nil {
		t.Fatal("NewNotifier() error = nil, want missing URL error")
	}
}
