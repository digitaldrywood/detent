package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
)

func TestRecoverDurableWorkAttemptsQueuesOwnedOrphanedSessionResume(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 10, 17, 0, 0, 0, time.UTC)
	issue := orphanRecoveryIssue()
	issue.Assignees = []string{"detent-bot"}
	issue.Fields["Detent Lease"] = formatClaimTime(now.Add(-time.Minute))
	tracker := hydratingDispatchConnector{issue: issue}
	attempts := &orphanRecoveryAttemptStore{
		orphans: []store.OrphanedAgentSession{orphanRecoverySession(now)},
	}
	orch, err := New(Config{
		Project:                scheduler.ProjectCandidate{ID: "detent"},
		ActiveStates:           []string{"Todo", "In Progress", "Merging"},
		ResumeOrphanedSessions: true,
		Claiming: ClaimingConfig{
			Enabled:       true,
			AssigneeLogin: "detent-bot",
			LeaseField:    "Detent Lease",
			LeaseTTL:      10 * time.Minute,
		},
	}, Dependencies{
		Connector:    tracker,
		WorkAttempts: attempts,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	state := newState(orch.cfg)

	orch.recoverDurableWorkAttempts(context.Background(), &state, now)

	if attempts.listCalls != 1 || len(attempts.marked) != 1 || attempts.marked[0] != 1155 {
		t.Fatalf("orphan reconciliation calls = list %d marked %#v", attempts.listCalls, attempts.marked)
	}
	retry, ok := state.Retry[issue.ID]
	if !ok {
		t.Fatalf("Retry[%q] missing", issue.ID)
	}
	if retry.RetryMode != runpkg.RetryModeResume || !retry.ResumeState.Orphaned {
		t.Fatalf("retry resume state = %#v", retry)
	}
	if retry.ResumeState.DetentSessionID != 1155 || retry.ResumeState.ProviderThreadID != "thread-1155" {
		t.Fatalf("retry provider state = %#v", retry.ResumeState)
	}
	if retry.Attempt != 3 || retry.WorkerHost != "local" {
		t.Fatalf("retry attempt/host = %d/%q, want 3/local", retry.Attempt, retry.WorkerHost)
	}
}

func TestRecoverDurableWorkAttemptsSkipsResumeWhenDisabled(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 10, 17, 10, 0, 0, time.UTC)
	issue := orphanRecoveryIssue()
	attempts := &orphanRecoveryAttemptStore{
		orphans: []store.OrphanedAgentSession{orphanRecoverySession(now)},
	}
	orch, err := New(Config{
		Project:                scheduler.ProjectCandidate{ID: "detent"},
		ActiveStates:           []string{"Todo", "In Progress", "Merging"},
		ResumeOrphanedSessions: false,
	}, Dependencies{
		Connector:    hydratingDispatchConnector{issue: issue},
		WorkAttempts: attempts,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	state := newState(orch.cfg)

	orch.recoverDurableWorkAttempts(context.Background(), &state, now)

	if attempts.listCalls != 0 || len(attempts.marked) != 0 {
		t.Fatalf("disabled orphan reconciliation calls = list %d marked %#v", attempts.listCalls, attempts.marked)
	}
	if len(state.Retry) != 0 {
		t.Fatalf("state.Retry = %#v, want fresh startup behavior", state.Retry)
	}
}

func TestRecoverDurableWorkAttemptsDoesNotResumeAnotherOwnersClaim(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 10, 17, 20, 0, 0, time.UTC)
	issue := orphanRecoveryIssue()
	issue.Assignees = []string{"another-instance"}
	issue.Fields["Detent Lease"] = formatClaimTime(now.Add(-time.Minute))
	attempts := &orphanRecoveryAttemptStore{
		orphans: []store.OrphanedAgentSession{orphanRecoverySession(now)},
	}
	orch, err := New(Config{
		Project:                scheduler.ProjectCandidate{ID: "detent"},
		ActiveStates:           []string{"Todo", "In Progress", "Merging"},
		ResumeOrphanedSessions: true,
		Claiming: ClaimingConfig{
			Enabled:       true,
			AssigneeLogin: "detent-bot",
			LeaseField:    "Detent Lease",
			LeaseTTL:      10 * time.Minute,
		},
	}, Dependencies{
		Connector:    hydratingDispatchConnector{issue: issue},
		WorkAttempts: attempts,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	state := newState(orch.cfg)

	orch.recoverDurableWorkAttempts(context.Background(), &state, now)

	if len(state.Retry) != 0 {
		t.Fatalf("state.Retry = %#v, want no cross-owner resume", state.Retry)
	}
	if len(attempts.marked) != 0 {
		t.Fatalf("marked sessions = %#v, want another owner's session untouched", attempts.marked)
	}
}

func TestRecoverDurableWorkAttemptsDoesNotResumeExpiredClaim(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 10, 17, 30, 0, 0, time.UTC)
	issue := orphanRecoveryIssue()
	issue.Assignees = []string{"detent-bot"}
	issue.Fields["Detent Lease"] = formatClaimTime(now.Add(-11 * time.Minute))
	attempts := &orphanRecoveryAttemptStore{
		orphans: []store.OrphanedAgentSession{orphanRecoverySession(now)},
	}
	orch, err := New(Config{
		Project:                scheduler.ProjectCandidate{ID: "detent"},
		ActiveStates:           []string{"Todo", "In Progress", "Merging"},
		ResumeOrphanedSessions: true,
		Claiming: ClaimingConfig{
			Enabled:       true,
			AssigneeLogin: "detent-bot",
			LeaseField:    "Detent Lease",
			LeaseTTL:      10 * time.Minute,
		},
	}, Dependencies{
		Connector:    hydratingDispatchConnector{issue: issue},
		WorkAttempts: attempts,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	state := newState(orch.cfg)

	orch.recoverDurableWorkAttempts(context.Background(), &state, now)

	if len(state.Retry) != 0 || len(attempts.marked) != 0 {
		t.Fatalf("expired claim recovery = retry %#v marked %#v, want fresh behavior", state.Retry, attempts.marked)
	}
}

func TestRecoverDurableWorkAttemptsDoesNotResumeOutsideActiveLane(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 16, 18, 35, 0, 0, time.UTC)
	issue := orphanRecoveryIssue()
	issue.State = "Blocked"
	attempts := &orphanRecoveryAttemptStore{
		orphans: []store.OrphanedAgentSession{orphanRecoverySession(now)},
	}
	orch, err := New(Config{
		Project:                scheduler.ProjectCandidate{ID: "detent"},
		ActiveStates:           []string{"Todo", "In Progress", "Merging"},
		ObservedStates:         []string{"Blocked"},
		ResumeOrphanedSessions: true,
	}, Dependencies{
		Connector:    hydratingDispatchConnector{issue: issue},
		WorkAttempts: attempts,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	state := newState(orch.cfg)

	orch.recoverDurableWorkAttempts(context.Background(), &state, now)

	if len(state.Retry) != 0 {
		t.Fatalf("state.Retry = %#v, want no inactive-lane resume", state.Retry)
	}
	if len(attempts.marked) != 0 {
		t.Fatalf("marked sessions = %#v, want inactive session left terminated", attempts.marked)
	}
}

func orphanRecoverySession(now time.Time) store.OrphanedAgentSession {
	return store.OrphanedAgentSession{
		ResumeState: store.AgentResumeState{
			DetentSessionID:   1155,
			ProviderThreadID:  "thread-1155",
			ProviderSessionID: "thread-1155-turn-1",
			RequestedModel:    "gpt-5.6-codex",
			AgentBackendID:    "codex",
			AgentBackendKind:  "codex",
			AgentRole:         "code",
			Orphaned:          true,
		},
		WorkAttemptID: 1155,
		ProjectID:     "detent",
		IssueID:       "issue-1155",
		Identifier:    "digitaldrywood/detent#1155",
		IssueURL:      "https://github.com/digitaldrywood/detent/issues/1155",
		WorkerType:    "agent",
		WorkerHost:    "local",
		Lane:          "In Progress",
		AttemptNumber: 2,
		StartedAt:     now.Add(-time.Minute),
	}
}

func orphanRecoveryIssue() connector.Issue {
	issue := connector.NewIssue()
	issue.ID = "issue-1155"
	issue.Identifier = "digitaldrywood/detent#1155"
	issue.Title = "Resume orphaned sessions"
	issue.State = "In Progress"
	issue.URL = "https://github.com/digitaldrywood/detent/issues/1155"
	return issue
}

type orphanRecoveryAttemptStore struct {
	recordingWorkAttemptStore
	orphans   []store.OrphanedAgentSession
	marked    []int64
	listCalls int
}

func (s *orphanRecoveryAttemptStore) ListOrphanedAgentSessions(context.Context, string) ([]store.OrphanedAgentSession, error) {
	s.listCalls++
	return append([]store.OrphanedAgentSession(nil), s.orphans...), nil
}

func (s *orphanRecoveryAttemptStore) MarkAgentSessionOrphaned(_ context.Context, sessionID int64, _ time.Time) error {
	s.marked = append(s.marked, sessionID)
	return nil
}
