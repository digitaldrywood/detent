package orchestrator

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/forgeavailability"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestWorkspacePreparationDrainsInstanceAndPreservesIssueFailureBreakers(t *testing.T) {
	t.Parallel()

	const retryLimit = 3
	tests := []struct {
		name string
		err  error
	}{
		{
			name: "dangling gitdir",
			err:  fmt.Errorf("%w: create workspace: dangling gitdir", runpkg.ErrWorkspacePreparation),
		},
		{
			name: "stale clean recovery",
			err:  fmt.Errorf("%w: create workspace: recover stale clean workspace", runpkg.ErrWorkspacePreparation),
		},
		{
			name: "after_create database timeout",
			err:  fmt.Errorf("%w: create workspace: after_create: workspace db: postgresql://127.0.0.1:5432: timeout: context deadline exceeded", runpkg.ErrWorkspacePreparation),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			issue := connector.Issue{ID: "issue-workspace", Identifier: "digitaldrywood/detent#1907", State: "In Progress"}
			tracker := &terminalRetryConnector{issues: map[string]connector.Issue{issue.ID: cloneIssue(issue)}}
			attempts := &terminalRetryWorkAttemptStore{}
			cfg := normalizeConfig(Config{
				ActiveStates:   []string{"Todo", "In Progress"},
				ObservedStates: []string{"Blocked"},
				TerminalStates: []string{"Done"},
				FailureBreaker: FailureBreakerConfig{
					SameClassLimit: repeatedFailureThreshold,
					Window:         time.Hour,
					Cooldown:       time.Hour,
				},
			})
			orch := &Orchestrator{cfg: cfg, connector: tracker, workAttempts: attempts}
			state := newState(cfg)
			state.InstantFailures[issue.ID] = InstantFailure{Issue: issue, Count: 2}
			state.RepeatedFailures[issue.ID] = RepeatedFailure{Issue: issue, Count: 2}
			base := time.Date(2026, 8, 18, 20, 0, 0, 0, time.UTC)

			for attempt := 1; attempt <= retryLimit; attempt++ {
				completedAt := base.Add(time.Duration(attempt) * time.Minute)
				running := Running{
					Issue:         issue,
					Mode:          runpkg.RunModePlan,
					Attempt:       attempt,
					WorkAttemptID: int64(attempt),
					StartedAt:     completedAt.Add(-time.Minute),
				}
				state.Running[issue.ID] = running
				orch.upsertWorkAttemptSnapshot(&state, telemetry.WorkAttempt{
					AttemptID: int64(attempt), IssueID: issue.ID, Identifier: issue.Identifier,
					Status: string(store.WorkAttemptStatusActive), StartedAt: running.StartedAt,
				})
				orch.handleRunResult(t.Context(), &state, runpkg.Completion{
					IssueID:      issue.ID,
					Request:      runpkg.RunRequest{Mode: runpkg.RunModePlan},
					Err:          tt.err,
					CompletedAt:  completedAt,
					RetryAttempt: attempt + 1,
					RetryDelay:   time.Second,
				})

				if state.InstantFailures[issue.ID].Count != 2 || state.RepeatedFailures[issue.ID].Count != 2 {
					t.Fatalf("attempt %d failure breakers = instant %#v repeated %#v, want preserved counts", attempt, state.InstantFailures[issue.ID], state.RepeatedFailures[issue.ID])
				}
				if len(state.Retry) != 0 || len(state.Blocked) != 0 || len(tracker.comments) != 0 {
					t.Fatal("workspace failure retained issue retry accounting")
				}
				if len(state.ForgeUnavailable) != 0 {
					t.Fatalf("workspace failure started forge condition or canary: %#v", state.ForgeUnavailable)
				}
			}
			if !state.FailureBreaker.Active() || projectFailureBreakerAllowsDispatch(&state, base.Add(3*time.Minute)) {
				t.Fatal("instance did not drain after three failures")
			}

			if failures := state.FailureBreaker.Failures[workAttemptErrorWorkspace]; len(failures) != retryLimit {
				t.Fatalf("FailureBreaker.Failures[%q] = %#v, want %d preserved failures", workAttemptErrorWorkspace, failures, retryLimit)
			}

		})
	}
}

func TestClassifyWorkspaceForgeReadFailure(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, detail, class string
		operation           string
	}{
		{name: "SSH refusal", operation: "ls-remote", detail: "git@github.com: Permission denied (publickey).", class: forgeavailability.ClassTransport},
		{name: "connection reset", operation: "fetch", detail: "ssh://git@github.com/acme/repo: connection reset by peer", class: forgeavailability.ClassTransport},
		{name: "forge 503", operation: "fetch", detail: "https://github.com/acme/repo: HTTP 503 Service Unavailable", class: forgeavailability.ClassServer},
		{name: "non transient", operation: "fetch", detail: "git@github.com: invalid refspec"},
		{name: "other host", operation: "fetch", detail: "git@other.example: Permission denied (publickey)."},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			command := &workspace.CommandError{Command: "git", Args: []string{"-C", "/source", tt.operation, "origin"}, Output: tt.detail, Err: errors.New("exit status 128")}
			err := fmt.Errorf("%w: create workspace: %w", runpkg.ErrWorkspacePreparation, command)
			got := classifyWorkspaceForgeReadFailure(err, "github.com")
			availability, ok := forgeavailability.As(got)
			if tt.class == "" {
				if ok {
					t.Fatalf("classified non-forge error: %v", got)
				}
			} else if !ok || availability.Class != tt.class || availability.Scope.Host != "github.com" {
				t.Fatalf("classification = %#v, want %s on github.com", availability, tt.class)
			}
			if !errors.Is(got, runpkg.ErrWorkspacePreparation) || !errors.Is(got, command) {
				t.Fatalf("lost workspace command evidence: %v", got)
			}
		})
	}
}

func TestWorkspaceSSHRefusalDoesNotTripProjectBreaker(t *testing.T) {
	t.Parallel()
	issue := connector.Issue{ID: "ssh-refusal", State: "In Progress"}
	tracker := &terminalRetryConnector{issues: map[string]connector.Issue{issue.ID: issue}}
	cfg := normalizeConfig(Config{ActiveStates: []string{"In Progress"}, FailureBreaker: FailureBreakerConfig{SameClassLimit: 3, Window: time.Hour, Cooldown: time.Hour}})
	attempts := &terminalRetryWorkAttemptStore{}
	orch := &Orchestrator{cfg: cfg, connector: tracker, workAttempts: attempts}
	state := newState(cfg)
	base := time.Date(2026, 9, 24, 15, 35, 0, 0, time.UTC)
	for attempt := 1; attempt <= 3; attempt++ {
		at := base.Add(time.Duration(attempt) * time.Minute)
		state.Running[issue.ID] = Running{Issue: issue, Attempt: attempt, WorkAttemptID: int64(attempt), StartedAt: at.Add(-time.Second)}
		err := fmt.Errorf("%w: create workspace: after_create: git -C /source ls-remote --symref origin HEAD: git@github.com: Permission denied (publickey).", runpkg.ErrWorkspacePreparation)
		orch.handleRunResult(t.Context(), &state, runpkg.Completion{IssueID: issue.ID, Err: err, CompletedAt: at})
		if state.FailureBreaker.Active() || len(state.FailureBreaker.Failures[workAttemptErrorWorkspace]) != 0 {
			t.Fatalf("attempt %d counted SSH refusal toward project breaker: %#v", attempt, state.FailureBreaker)
		}
		if len(state.ForgeUnavailable) != 1 || !state.Retry[issue.ID].ForgeUnavailable || !state.Retry[issue.ID].DueAt.After(at) {
			t.Fatalf("attempt %d forge condition = %#v retry = %#v", attempt, state.ForgeUnavailable, state.Retry[issue.ID])
		}
		if got := attempts.completions[len(attempts.completions)-1].ErrorClass; got != forgeavailability.Condition {
			t.Fatalf("attempt %d error class = %q, want %q", attempt, got, forgeavailability.Condition)
		}
	}
}

func TestWorkspaceBreakerHonorsFailureCooldown(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 9, 24, 15, 35, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{FailureBreaker: FailureBreakerConfig{SameClassLimit: 3, Window: time.Hour, Cooldown: time.Hour}, BlockedRecovery: BlockedRecoveryConfig{BreakerCooldown: 24 * time.Hour}})
	orch := &Orchestrator{cfg: cfg}
	state := newState(cfg)
	state.FailureBreaker.PreTurn = true
	for attempt := 1; attempt <= 3; attempt++ {
		at := base.Add(time.Duration(attempt) * time.Minute)
		orch.recordProjectFailureBreakerEvidence(&state, ProjectFailure{IssueID: fmt.Sprintf("issue-%d", attempt)}, workAttemptErrorWorkspace, at)
	}
	if !state.FailureBreaker.ResumeAt.Equal(base.Add(3*time.Minute + time.Hour)) {
		t.Fatalf("resume_at = %s, want one hour after third failure", state.FailureBreaker.ResumeAt)
	}
}
