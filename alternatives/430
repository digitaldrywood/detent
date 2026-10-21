package aidebug

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestPromptIncludesDiscriminatingEvidence(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 8, 27, 15, 4, 5, 0, time.UTC)
	tests := []struct {
		name   string
		mutate func(*Projection)
		want   []string
	}{
		{
			name: "blocked with cause",
			mutate: func(projection *Projection) {
				projection.Issue.Blocked = BlockedEvidence{CausePresent: true, Cause: "credentials_missing", CauseAuthor: "detent", Remedy: "configure the provider credential", RecoveryPredicate: []map[string]any{}}
			},
			want: []string{`"cause_present": true`, `"cause": "credentials_missing"`, `"cause_author": "detent"`},
		},
		{
			name: "blocked without cause",
			mutate: func(projection *Projection) {
				projection.Issue.RuntimeState = "blocked"
				projection.Issue.Blocked = BlockedEvidence{Cause: "No blocked cause is recorded.", CauseAuthor: "none", RecoveryPredicate: []map[string]any{}}
			},
			want: []string{`"detent_runtime_state": "blocked"`, `"cause_present": false`, "No blocked cause is recorded."},
		},
		{
			name: "dependency waiting",
			mutate: func(projection *Projection) {
				projection.Issue.Dependencies = []DependencyEvidence{{Depth: 1, Identifier: "digitaldrywood/detent#1999", CurrentState: "In Progress", TrackerState: "In Progress", Source: "github_native"}}
				projection.Issue.SchedulerDecisions = []SchedulerDecisionEvidence{{At: at, Result: "skipped", WaitReason: "blocked_by_open_issue"}}
			},
			want: []string{"digitaldrywood/detent#1999", `"current_state": "In Progress"`, `"wait_reason": "blocked_by_open_issue"`},
		},
		{
			name: "loop parked",
			mutate: func(projection *Projection) {
				projection.Issue.Park = ParkEvidence{Parked: true, BreakerKind: "dispatch_loop", Thresholds: map[string]int64{"dispatch_limit": 3}, ConsecutiveCounts: map[string]int64{"dispatches": 3}, ParkCount: 1, Causes: []map[string]any{{"cause": "no_progress_limit"}}}
				projection.Issue.Attempts = []AttemptEvidence{{StartedAt: at, AttemptNumber: 1, TerminalState: "success"}, {StartedAt: at.Add(time.Minute), AttemptNumber: 2, TerminalState: "no_progress"}, {StartedAt: at.Add(2 * time.Minute), AttemptNumber: 3, TerminalState: "success"}}
			},
			want: []string{`"parked": true`, `"breaker_kind": "dispatch_loop"`, `"alternating_success_no_progress_pattern": true`},
		},
		{
			name: "lifetime capped",
			mutate: func(projection *Projection) {
				projection.Project.Brakes.LifetimeSessionLimit = 120
				projection.Project.Brakes.LifetimeTokenLimit = 750_000_000
				projection.Issue.SchedulerDecisions = []SchedulerDecisionEvidence{{At: at, Result: "skipped", WaitReason: "lifetime_session_limit"}}
			},
			want: []string{`"lifetime_session_limit": 120`, `"lifetime_token_limit": 750000000`, `"wait_reason": "lifetime_session_limit"`},
		},
		{
			name: "not dispatching due to capacity",
			mutate: func(projection *Projection) {
				projection.Fleet.AgentPools = json.RawMessage(`[{"name":"default","used":4,"capacity":4}]`)
				projection.Fleet.RunningCount = 4
				projection.Issue.SchedulerDecisions = []SchedulerDecisionEvidence{{At: at, Result: "skipped", WaitReason: "global_capacity_full", CapacityJSON: `{"used":4,"capacity":4}`}}
				projection.Issue.SchedulerWaitCounts = map[string]int{"global_capacity_full": 7}
			},
			want: []string{`"global_slots_in_use": 4`, `"wait_reason": "global_capacity_full"`, `"global_capacity_full": 7`, `"capacity": 4`},
		},
		{
			name: "healthy card",
			mutate: func(projection *Projection) {
				projection.Issue.TrackerState = "In Progress"
				projection.Issue.RuntimeState = "running"
				projection.Issue.StateDisagreement = true
				projection.Issue.Blocked = BlockedEvidence{Cause: "No blocked cause is recorded.", CauseAuthor: "none", RecoveryPredicate: []map[string]any{}}
			},
			want: []string{`"tracker_state": "In Progress"`, `"detent_runtime_state": "running"`, `"tracker_runtime_disagreement": true`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			projection := testProjection(at)
			tt.mutate(&projection)
			FinalizeAggregates(projection.Issue)
			prompt, err := projection.Prompt()
			if err != nil {
				t.Fatalf("Prompt() error = %v", err)
			}
			for _, want := range tt.want {
				if !strings.Contains(prompt, want) {
					t.Fatalf("Prompt() missing %q:\n%s", want, prompt)
				}
			}
			if !strings.Contains(prompt, "2026-08-27T15:04:05Z") {
				t.Fatalf("Prompt() does not use UTC RFC3339 timestamp:\n%s", prompt)
			}
		})
	}
}

func TestFinalizeAggregatesRequiresARealAlternatingPattern(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		states []string
		want   bool
	}{
		{name: "three alternating outcomes", states: []string{"success", "no_progress", "success"}, want: true},
		{name: "two relevant outcomes among unrelated failures", states: []string{"failure", "success", "no_progress"}},
		{name: "single relevant outcome among unrelated failures", states: []string{"failure", "success", "cancelled"}},
		{name: "repeated relevant outcome", states: []string{"success", "no_progress", "no_progress"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			attempts := make([]AttemptEvidence, 0, len(tt.states))
			for _, state := range tt.states {
				attempts = append(attempts, AttemptEvidence{TerminalState: state})
			}
			if got := alternatingSuccessNoProgress(attempts); got != tt.want {
				t.Fatalf("alternatingSuccessNoProgress(%v) = %t, want %t", tt.states, got, tt.want)
			}
		})
	}
}

func TestPromptScopesUseSameSectionStructure(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 8, 27, 15, 4, 5, 0, time.UTC)
	tests := []struct {
		name      string
		scope     Scope
		configure func(*Projection)
		want      []string
		dontWant  []string
	}{
		{name: "issue", scope: ScopeIssue, configure: func(projection *Projection) {}, want: []string{"## Issue evidence", "## Project evidence", "## Fleet evidence"}},
		{name: "project", scope: ScopeProject, configure: func(projection *Projection) { projection.Issue = nil }, want: []string{"## Project evidence", "## Fleet evidence"}, dontWant: []string{"## Issue evidence"}},
		{name: "fleet", scope: ScopeFleet, configure: func(projection *Projection) { projection.Issue = nil; projection.Project = nil }, want: []string{"## Fleet evidence"}, dontWant: []string{"## Issue evidence", "## Project evidence"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			projection := testProjection(at)
			projection.Scope = tt.scope
			tt.configure(&projection)
			prompt, err := projection.Prompt()
			if err != nil {
				t.Fatalf("Prompt() error = %v", err)
			}
			for _, want := range tt.want {
				if !strings.Contains(prompt, want) {
					t.Fatalf("Prompt() missing %q", want)
				}
			}
			for _, dontWant := range tt.dontWant {
				if strings.Contains(prompt, dontWant) {
					t.Fatalf("Prompt() contains %q", dontWant)
				}
			}
		})
	}
}

func testProjection(at time.Time) Projection {
	projection := NewProjection(ScopeIssue, at)
	projection.Detent = DetentEvidence{Version: "v0.88.0", Host: "worker.example", InstanceName: "dogfood", DefectDestinationRepository: "digitaldrywood/detent"}
	projection.Issue = &IssueEvidence{
		ID: "I_2006", Identifier: "digitaldrywood/detent#2006", Title: "AI Debug", URL: "https://github.com/digitaldrywood/detent/issues/2006",
		ProjectID: "detent", TrackerKind: "github", TrackerState: "In Progress", RuntimeState: "In Progress", CurrentLane: "In Progress",
		Blocked:      BlockedEvidence{Cause: "No blocked cause is recorded.", CauseAuthor: "none", RecoveryPredicate: []map[string]any{}},
		Park:         ParkEvidence{Thresholds: map[string]int64{}, ConsecutiveCounts: map[string]int64{}, Causes: []map[string]any{}},
		Dependencies: []DependencyEvidence{}, Attempts: []AttemptEvidence{}, Sessions: []SessionEvidence{}, SchedulerDecisions: []SchedulerDecisionEvidence{}, SchedulerWaitCounts: map[string]int{}, LaneTransitions: []LaneTransitionEvidence{}, HookAndCIErrors: []string{},
	}
	projection.Project = &ProjectEvidence{ID: "detent", Repository: "digitaldrywood/detent", ConfigDestinationRepo: "digitaldrywood/detent", Authorization: map[string]any{}, GateDefinition: json.RawMessage(`{}`), Dispatch: json.RawMessage(`{}`)}
	projection.Fleet = FleetEvidence{AgentPools: json.RawMessage(`[]`), ProviderRateState: json.RawMessage(`{}`), GitHubBudgets: json.RawMessage(`{}`), Dispatch: json.RawMessage(`{}`), CapacityConditions: json.RawMessage(`{}`)}
	return projection
}
