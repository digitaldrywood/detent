package orchestrator

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestStrandedActiveIssueSnapshots(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 31, 16, 0, 0, 0, time.UTC)
	laneEnteredAt := now.Add(-30 * time.Minute)
	completedLongAgo := now.Add(-15 * time.Minute)
	completedRecently := now.Add(-5 * time.Minute)
	liveLease := now.Add(time.Minute)
	staleLease := now.Add(-12 * time.Minute)
	issue := telemetry.Issue{
		ID:                   "issue-1",
		Identifier:           "digitaldrywood/detent#1606",
		URL:                  "https://github.com/digitaldrywood/detent/issues/1606",
		Title:                "Surface stranded active work",
		State:                "In Progress",
		CurrentLaneEnteredAt: &laneEnteredAt,
	}
	baseState := State{
		MaxConcurrentAgents:     3,
		PoolAvailable:           1,
		StrandedActiveThreshold: 10 * time.Minute,
		Running:                 map[string]Running{},
		SchedulerDecisions: []telemetry.SchedulerDecision{
			{IssueID: issue.ID, Result: "skipped", Reason: "older refusal", DecisionAt: now.Add(-20 * time.Minute)},
			{Identifier: issue.Identifier, Result: "skipped", WaitReason: "priority reservation", DecisionAt: now.Add(-time.Minute)},
		},
	}

	tests := []struct {
		name         string
		state        State
		issue        telemetry.Issue
		wantCount    int
		wantDuration int64
		wantSince    time.Time
		wantReason   string
	}{
		{
			name:         "reports gap after latest completed attempt",
			state:        withStrandedActiveAttempts(baseState, telemetry.WorkAttempt{IssueID: issue.ID, Status: "completed", CompletedAt: &completedLongAgo}),
			issue:        issue,
			wantCount:    1,
			wantDuration: int64((15 * time.Minute) / time.Second),
			wantSince:    completedLongAgo,
			wantReason:   "priority reservation",
		},
		{
			name:      "suppresses normal between-session gap",
			state:     withStrandedActiveAttempts(baseState, telemetry.WorkAttempt{Identifier: issue.Identifier, Status: "completed", CompletedAt: &completedRecently}),
			issue:     issue,
			wantCount: 0,
		},
		{
			name: "suppresses issue with running worker",
			state: withStrandedActiveRunning(baseState, Running{Issue: connector.Issue{
				ID: issue.ID, Identifier: issue.Identifier, URL: issue.URL, State: issue.State,
			}}),
			issue: issue,
		},
		{
			name:      "suppresses issue with live persisted attempt",
			state:     withStrandedActiveAttempts(baseState, telemetry.WorkAttempt{IssueURL: issue.URL, Status: "active", LeaseExpiresAt: &liveLease}),
			issue:     issue,
			wantCount: 0,
		},
		{
			name: "reports from expired active attempt lease",
			state: withStrandedActiveAttempts(baseState, telemetry.WorkAttempt{
				IssueID: issue.ID, Status: "active", LeaseExpiresAt: &staleLease,
			}),
			issue:        issue,
			wantCount:    1,
			wantDuration: int64((12 * time.Minute) / time.Second),
			wantSince:    staleLease,
			wantReason:   "priority reservation",
		},
		{
			name:         "reports gap when pool has no capacity",
			state:        withStrandedActivePoolAvailable(baseState, 0),
			issue:        issue,
			wantCount:    1,
			wantDuration: int64((30 * time.Minute) / time.Second),
			wantSince:    laneEnteredAt,
			wantReason:   "priority reservation",
		},
		{
			name:         "reports gap when project state capacity is full",
			state:        withStrandedActiveStateLimit(baseState, 1, Running{Issue: connector.Issue{ID: "other", State: "In Progress"}}),
			issue:        issue,
			wantCount:    1,
			wantDuration: int64((30 * time.Minute) / time.Second),
			wantSince:    laneEnteredAt,
			wantReason:   "priority reservation",
		},
		{
			name:  "suppresses non-working state",
			state: baseState,
			issue: func() telemetry.Issue {
				other := issue
				other.State = "Todo"
				return other
			}(),
			wantCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := strandedActiveIssueSnapshots(tt.state, []telemetry.Issue{tt.issue}, now)
			if len(got) != tt.wantCount {
				t.Fatalf("strandedActiveIssueSnapshots() = %#v, want %d issue(s)", got, tt.wantCount)
			}
			if tt.wantCount == 0 {
				return
			}
			if got[0].DurationSeconds != tt.wantDuration || !got[0].Since.Equal(tt.wantSince) {
				t.Fatalf("diagnostic timing = %ds since %s, want %ds since %s", got[0].DurationSeconds, got[0].Since, tt.wantDuration, tt.wantSince)
			}
			if got[0].LastRefusalReason != tt.wantReason {
				t.Fatalf("LastRefusalReason = %q, want %q", got[0].LastRefusalReason, tt.wantReason)
			}
		})
	}
}

func TestTickDispatchesPlanApprovedIssueAfterLongRefresh(t *testing.T) {
	for _, tt := range []struct {
		name     string
		interval time.Duration
		pr       *connector.PullRequest
	}{
		{name: "eleven minute refresh", interval: 11 * time.Minute},
		{name: "twenty three minute refresh", interval: 23 * time.Minute},
		{name: "twenty three minute refresh with existing PR", interval: 23 * time.Minute, pr: &connector.PullRequest{Number: 3238, State: "OPEN", URL: "https://github.test/digitaldrywood/detent/pull/3238"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			approvedAt := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
			now := approvedAt.Add(tt.interval)
			issue := connector.NewIssue()
			issue.ID = "issue-3238"
			issue.Identifier = "digitaldrywood/detent#3238"
			issue.URL = "https://github.test/digitaldrywood/detent/issues/3238"
			issue.Title = "Dispatch approved plan"
			issue.State = gate.DefaultPlanStop
			issue.Labels = []string{"plan-approved"}
			issue.PullRequest = tt.pr
			cfg := normalizeConfig(Config{MaxConcurrentAgents: 1, ActiveStates: []string{"Todo", "In Progress", "Rework"}, TerminalStates: []string{"Done", "Cancelled"}, StrandedActiveThreshold: 10 * time.Minute, Plan: gate.PlanConfig{Enabled: true, Review: gate.PlanReviewHuman, Stop: gate.DefaultPlanStop}})
			tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
			orch := &Orchestrator{cfg: cfg, connector: tracker, supervisor: newTestSupervisor(t, FakeRunner{}, cfg), runResults: make(chan runpkg.Completion, 1)}
			state := newState(cfg)
			defer orch.releaseRunningSlots(&state)
			if transitioned := orch.reviewPlanIssues(t.Context(), &state, []connector.Issue{issue}, approvedAt); len(transitioned) != 1 {
				t.Fatalf("plan approval = %#v, want transition to In Progress", transitioned)
			}
			if len(tracker.updates) != 1 || tracker.updates[0].state != "In Progress" {
				t.Fatalf("plan approval updates = %#v, want In Progress", tracker.updates)
			}
			tracker.stateIssues[0].StageUpdatedAt = &approvedAt
			orch.tick(t.Context(), &state, now)
			if len(tracker.updates) != 1 {
				t.Fatalf("lane updates = %#v, want no stranded recovery transition", tracker.updates)
			}
			if _, ok := state.Running[issue.ID]; !ok {
				t.Fatalf("issue was not dispatched after %s; decisions = %#v", tt.interval, state.SchedulerDecisions)
			}
		})
	}
}

func withStrandedActiveAttempts(state State, attempts ...telemetry.WorkAttempt) State {
	state.WorkAttempts = attempts
	return state
}

func withStrandedActiveRunning(state State, running Running) State {
	state.Running = map[string]Running{running.Issue.ID: running}
	return state
}

func withStrandedActivePoolAvailable(state State, available int) State {
	state.PoolAvailable = available
	return state
}

func withStrandedActiveStateLimit(state State, limit int, running Running) State {
	state.MaxAgentsByState = map[string]int{strandedActiveState: limit}
	return withStrandedActiveRunning(state, running)
}
