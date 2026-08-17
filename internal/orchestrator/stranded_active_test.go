package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
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

func TestRecoverStrandedActiveIssues(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 17, 15, 0, 0, 0, time.UTC)
	enteredAt := now.Add(-30 * time.Minute)
	baseIssue := connector.Issue{
		ID:             "issue-1860",
		Identifier:     "digitaldrywood/detent#1860",
		URL:            "https://github.com/digitaldrywood/detent/issues/1860",
		Title:          "Recover stranded active cards",
		State:          "In Progress",
		StageUpdatedAt: &enteredAt,
	}

	tests := []struct {
		name       string
		mutate     func(*connector.Issue, *State)
		workspace  runpkg.BlockedRecoverySnapshot
		wantTarget string
	}{
		{
			name:       "stranded with no artifacts recovers to Todo",
			workspace:  runpkg.BlockedRecoverySnapshot{WorkspaceStatus: "missing"},
			wantTarget: "Todo",
		},
		{
			name: "stranded with open pull request routes to Rework",
			mutate: func(issue *connector.Issue, _ *State) {
				issue.PullRequest = &connector.PullRequest{Number: 1861, State: "OPEN"}
			},
			workspace:  runpkg.BlockedRecoverySnapshot{WorkspaceStatus: "missing"},
			wantTarget: autoPromoteReworkState,
		},
		{
			name: "stranded with unpushed work routes to Rework",
			workspace: runpkg.BlockedRecoverySnapshot{
				WorkspaceStatus:  "present",
				WorkspacePresent: true,
				HeadSHA:          "work-head",
				BaseFingerprint:  "base-head",
				UnpushedCommits:  1,
			},
			wantTarget: autoPromoteReworkState,
		},
		{
			name: "stranded with dirty worktree routes to Rework",
			workspace: runpkg.BlockedRecoverySnapshot{
				WorkspaceStatus:  "present",
				WorkspacePresent: true,
				HeadSHA:          "base-head",
				BaseFingerprint:  "base-head",
				WorkspaceFiles:   2,
			},
			wantTarget: autoPromoteReworkState,
		},
		{
			name: "stranded with pushed workspace commit routes to Rework",
			workspace: runpkg.BlockedRecoverySnapshot{
				WorkspaceStatus:  "present",
				WorkspacePresent: true,
				HeadSHA:          "pushed-head",
				BaseFingerprint:  "base-head",
			},
			wantTarget: autoPromoteReworkState,
		},
		{
			name: "stranded with clean base workspace recovers to Todo",
			workspace: runpkg.BlockedRecoverySnapshot{
				WorkspaceStatus:  "present",
				WorkspacePresent: true,
				HeadSHA:          "base-head",
				BaseFingerprint:  "base-head",
			},
			wantTarget: "Todo",
		},
		{
			name: "live worker is never disturbed",
			mutate: func(issue *connector.Issue, state *State) {
				state.Running[issue.ID] = Running{Issue: *issue}
			},
			workspace: runpkg.BlockedRecoverySnapshot{WorkspaceStatus: "missing"},
		},
		{
			name:      "unavailable workspace evidence holds active lane",
			workspace: runpkg.BlockedRecoverySnapshot{WorkspaceStatus: "unavailable"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			issue := cloneIssue(baseIssue)
			state := State{
				StrandedActiveThreshold: 10 * time.Minute,
				Running:                 map[string]Running{},
				BoardIssues:             []connector.Issue{issue},
			}
			if tt.mutate != nil {
				tt.mutate(&issue, &state)
				state.BoardIssues[0] = cloneIssue(issue)
			}
			tracker := &strandedActiveRecoveryConnector{}
			orchestrator := &Orchestrator{
				cfg:               Config{ActiveStates: []string{"Todo", "In Progress", autoPromoteReworkState}},
				connector:         tracker,
				recoveryInspector: strandedActiveRecoveryInspector{snapshot: tt.workspace},
			}

			transitioned := orchestrator.recoverStrandedActiveIssues(t.Context(), &state, []connector.Issue{issue}, now)
			if tt.wantTarget == "" {
				if len(transitioned) != 0 || len(tracker.updates) != 0 {
					t.Fatalf("transitioned = %#v, updates = %#v, want no transition", transitioned, tracker.updates)
				}
				return
			}
			if _, ok := transitioned[issue.ID]; !ok {
				t.Fatalf("transitioned = %#v, want %s", transitioned, issue.ID)
			}
			if len(tracker.updates) != 1 || tracker.updates[0].issueID != issue.ID || tracker.updates[0].state != tt.wantTarget {
				t.Fatalf("updates = %#v, want %s -> %s", tracker.updates, issue.ID, tt.wantTarget)
			}
		})
	}
}

type strandedActiveRecoveryUpdate struct {
	issueID string
	state   string
}

type strandedActiveRecoveryConnector struct {
	connector.Connector
	updates []strandedActiveRecoveryUpdate
}

func (c *strandedActiveRecoveryConnector) UpdateIssueState(_ context.Context, issueID string, state string) error {
	c.updates = append(c.updates, strandedActiveRecoveryUpdate{issueID: issueID, state: state})
	return nil
}

type strandedActiveRecoveryInspector struct {
	snapshot runpkg.BlockedRecoverySnapshot
}

func (i strandedActiveRecoveryInspector) BlockedRecoverySnapshot(context.Context, runpkg.RunRequest) runpkg.BlockedRecoverySnapshot {
	return i.snapshot
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
