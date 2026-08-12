package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
)

func TestSyncCIAvailability(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 8, 18, 0, 0, 0, time.UTC)
	orch := Orchestrator{cfg: normalizeConfig(Config{Project: scheduler.ProjectCandidate{ID: "detent"}})}
	state := newState(orch.cfg)
	tests := []struct {
		name       string
		issues     []connector.Issue
		wantActive bool
		wantChecks int
		wantPRs    int
		wantOldest int64
	}{
		{
			name: "healthy CI",
			issues: []connector.Issue{
				ciAvailabilityIssue("healthy", 11, 0, nil),
			},
		},
		{
			name: "one slow check does not raise condition",
			issues: []connector.Issue{
				ciAvailabilityIssue("slow", 12, 1, []connector.PullRequestCheck{{Name: "Verify", Status: "queued", QueueSeconds: 1_800}}),
			},
		},
		{
			name: "inactive pull requests do not raise condition",
			issues: func() []connector.Issue {
				closed := ciAvailabilityIssue("closed", 15, 2, []connector.PullRequestCheck{{Name: "Verify", Status: "queued", QueueSeconds: 3_600}})
				closed.PullRequest.State = "CLOSED"
				merged := ciAvailabilityIssue("merged", 16, 3, []connector.PullRequestCheck{{Name: "Lint", Status: "queued", QueueSeconds: 2_700}})
				merged.PullRequest.State = "MERGED"
				return []connector.Issue{closed, merged}
			}(),
		},
		{
			name: "sustained unstarted checks across PRs raise condition",
			issues: []connector.Issue{
				ciAvailabilityIssue("first", 13, 2, []connector.PullRequestCheck{{Name: "Verify", Status: "queued", QueueSeconds: 3_600}}),
				ciAvailabilityIssue("second", 14, 3, []connector.PullRequestCheck{{Name: "Lint", Status: "queued", QueueSeconds: 2_700}}),
			},
			wantActive: true,
			wantChecks: 5,
			wantPRs:    2,
			wantOldest: 3_600,
		},
		{
			name: "recovery clears condition",
			issues: []connector.Issue{
				ciAvailabilityIssue("first", 13, 0, nil),
				ciAvailabilityIssue("second", 14, 0, nil),
			},
		},
	}

	for index, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			observedAt := now.Add(time.Duration(index) * time.Minute)
			orch.syncCIAvailability(&state, tt.issues, observedAt)
			if got := state.CIUnavailable != nil; got != tt.wantActive {
				t.Fatalf("CIUnavailable active = %v, want %v", got, tt.wantActive)
			}
			if !tt.wantActive {
				return
			}
			if got := state.CIUnavailable.UnstartedCheckCount; got != tt.wantChecks {
				t.Fatalf("UnstartedCheckCount = %d, want %d", got, tt.wantChecks)
			}
			if got := state.CIUnavailable.PullRequestCount; got != tt.wantPRs {
				t.Fatalf("PullRequestCount = %d, want %d", got, tt.wantPRs)
			}
			if got := state.CIUnavailable.OldestQueueSeconds; got != tt.wantOldest {
				t.Fatalf("OldestQueueSeconds = %d, want %d", got, tt.wantOldest)
			}
		})
	}
}

func TestCIUnavailableDispatchPolicy(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 8, 18, 0, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{
		Project:             scheduler.ProjectCandidate{ID: "detent"},
		MaxConcurrentAgents: 3,
		ActiveStates:        []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates:      []string{"Done", "Cancelled"},
	})
	tests := []struct {
		name          string
		condition     bool
		stateName     string
		withPR        bool
		wantAllowed   bool
		wantSkipCause string
	}{
		{name: "merge waits while CI unavailable", condition: true, stateName: "Merging", withPR: true, wantSkipCause: dispatchSkipCIUnavailable},
		{name: "Todo still dispatches while CI unavailable", condition: true, stateName: "Todo", wantAllowed: true},
		{name: "Rework still dispatches while CI unavailable", condition: true, stateName: "Rework", withPR: true, wantAllowed: true},
		{name: "merge resumes after CI recovery", stateName: "Merging", withPR: true, wantAllowed: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := newState(cfg)
			if tt.condition {
				state.CIUnavailable = &CICondition{DetectedAt: now}
			}
			issue := dispatchTestIssue("issue", tt.stateName)
			if tt.withPR {
				issue = dispatchTestIssueWithPullRequest("issue", tt.stateName, "open")
				issue.PullRequest.CIStatus = "pending"
			}
			decision := newDispatchPlanner(cfg).dispatchableIssueDecision(issue, &state, false, now, "")
			if decision.dispatchable != tt.wantAllowed || decision.reason != tt.wantSkipCause {
				t.Fatalf("dispatch decision = %#v, want allowed %v reason %q", decision, tt.wantAllowed, tt.wantSkipCause)
			}
		})
	}
}

func TestParkCIUnavailableWaiters(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 8, 18, 0, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{
		Project:             scheduler.ProjectCandidate{ID: "detent"},
		MaxConcurrentAgents: 2,
		ActiveStates:        []string{"Todo", "In Progress", "Rework", "Merging"},
	})
	orch := Orchestrator{cfg: cfg}
	state := newState(cfg)
	waitingCtx, waitingStop := context.WithCancelCause(context.Background())
	workingCtx, workingStop := context.WithCancelCause(context.Background())
	waiting := ciAvailabilityIssue("waiting", 21, 1, []connector.PullRequestCheck{{Name: "Verify", Status: "queued", QueueSeconds: 3_600}})
	waiting.State = "In Progress"
	working := ciAvailabilityIssue("working", 22, 1, []connector.PullRequestCheck{{Name: "Lint", Status: "queued", QueueSeconds: 3_600}})
	working.State = "In Progress"
	state.Running[waiting.ID] = Running{Issue: waiting, LastMessage: "waiting for CI checks", stop: waitingStop}
	state.Running[working.ID] = Running{Issue: working, LastMessage: "editing implementation", stop: workingStop}

	orch.parkCIUnavailableWaiters(&state, []connector.Issue{waiting, working}, now)

	if cause := context.Cause(waitingCtx); !errors.Is(cause, runpkg.ErrCIUnavailable) {
		t.Fatalf("waiting context cause = %v, want ErrCIUnavailable", cause)
	}
	if cause := context.Cause(workingCtx); cause != nil {
		t.Fatalf("working context cause = %v, want nil", cause)
	}
	if !state.Running[waiting.ID].CIStopRequested {
		t.Fatal("waiting run was not marked for CI-unavailable parking")
	}
}

func TestCIUnavailableCompletionResumesAfterRecovery(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 8, 18, 0, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{
		Project:             scheduler.ProjectCandidate{ID: "detent"},
		MaxConcurrentAgents: 1,
		ActiveStates:        []string{"Todo", "In Progress", "Rework", "Merging"},
	})
	orch := Orchestrator{cfg: cfg}
	state := newState(cfg)
	state.CIUnavailable = &CICondition{DetectedAt: now.Add(-time.Hour)}
	issue := ciAvailabilityIssue("parked", 23, 1, []connector.PullRequestCheck{{Name: "Verify", Status: "queued", QueueSeconds: 3_600}})
	issue.State = "In Progress"
	running := Running{Issue: issue, Attempt: 2, WorkerHost: "worker-a"}

	if !orch.handleCIUnavailableCompletion(t.Context(), &state, runpkg.Completion{
		IssueID:     issue.ID,
		CompletedAt: now,
		Err:         runpkg.ErrCIUnavailable,
	}, running) {
		t.Fatal("handleCIUnavailableCompletion() = false, want true")
	}
	retry, ok := state.Retry[issue.ID]
	if !ok || retry.Attempt != running.Attempt || !retry.DueAt.Equal(now) || !retry.CIUnavailable {
		t.Fatalf("Retry[%q] = %#v, want parked same-attempt retry", issue.ID, retry)
	}
	_, allowed, reason := newDispatchPlanner(cfg).retryAction(&state, issue, retry, now)
	if allowed || reason != dispatchSkipCIUnavailable {
		t.Fatalf("retry during outage = allowed %v reason %q, want paused", allowed, reason)
	}

	orch.syncCIAvailability(&state, []connector.Issue{ciAvailabilityIssue("healthy", 24, 0, nil)}, now.Add(time.Minute))
	if state.CIUnavailable != nil {
		t.Fatalf("CIUnavailable = %#v, want recovery", state.CIUnavailable)
	}
	_, allowed, reason = newDispatchPlanner(cfg).retryAction(&state, issue, retry, now.Add(time.Minute))
	if !allowed || reason != "" {
		t.Fatalf("retry after recovery = allowed %v reason %q, want allowed", allowed, reason)
	}
}

func ciAvailabilityIssue(id string, number int, count int, checks []connector.PullRequestCheck) connector.Issue {
	return connector.Issue{
		ID:               id,
		Identifier:       "digitaldrywood/detent#" + id,
		Title:            id,
		State:            "Merging",
		AssignedToWorker: true,
		PRRepository:     "digitaldrywood/detent",
		PullRequest: &connector.PullRequest{
			Number:              number,
			URL:                 "https://github.com/digitaldrywood/detent/pull/" + id,
			State:               "OPEN",
			CIStatus:            "pending",
			UnstartedCheckCount: count,
			UnstartedChecks:     checks,
		},
	}
}
