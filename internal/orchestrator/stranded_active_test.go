package orchestrator

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
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
			name:      "suppresses gap when pool has no capacity",
			state:     withStrandedActivePoolAvailable(baseState, 0),
			issue:     issue,
			wantCount: 0,
		},
		{
			name:      "suppresses gap when project state capacity is full",
			state:     withStrandedActiveStateLimit(baseState, 1, Running{Issue: connector.Issue{ID: "other", State: "In Progress"}}),
			issue:     issue,
			wantCount: 0,
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
