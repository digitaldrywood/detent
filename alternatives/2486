package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func TestRestartResumeDispatchCompletionIdentity(t *testing.T) {
	t.Parallel()

	for _, lane := range []string{"In Progress", "Rework"} {
		t.Run(lane, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 9, 23, 17, 0, 0, 0, time.UTC)
			issue := orphanRecoveryIssue()
			issue.State = lane
			session := orphanRecoverySession(now)
			attempts := &orphanRecoveryAttemptStore{metadata: runningWorkAttemptMetadataJSON(Running{Policy: orchestratorTestPolicy()}, nil),
				orphans: []store.OrphanedAgentSession{session}}
			attempts.nextID = 2200
			runner := newWorkerHostRunner()
			orch, err := New(Config{
				Project:                scheduler.ProjectCandidate{ID: "detent"},
				ActiveStates:           []string{"In Progress", "Rework"},
				ResumeOrphanedSessions: true,
				WorkerHosts:            []string{"local"},
			}, Dependencies{
				Connector:    hydratingDispatchConnector{issue: issue},
				Runner:       runner,
				WorkAttempts: attempts,
			})
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			state := newState(orch.cfg)
			orch.recoverDurableWorkAttempts(t.Context(), &state, now)
			if !orch.dispatchIssue(t.Context(), &state, issue, state.Retry[issue.ID].Attempt, now, "local") {
				t.Fatal("restart resume dispatch failed")
			}
			request := receiveWorkerHostRunRequest(t, runner.started)
			t.Cleanup(state.Running[issue.ID].cancel)
			if request.RetryMode != runpkg.RetryModeResume || request.ResumeState.DetentSessionID != session.ResumeState.DetentSessionID {
				t.Fatalf("dispatched retry = %q, host = %q, session = %d; want orphaned provider resume", request.RetryMode, request.WorkerHost, request.ResumeState.DetentSessionID)
			}
			if request.WorkAttemptID != 2200 || request.Generation == 0 {
				t.Fatalf("completion identity = %d/%d, want new attempt 2200 and positive generation", request.WorkAttemptID, request.Generation)
			}
			prompt, err := runpkg.BuildPrompt(config.Workflow{Prompt: "Implement the issue"}, request.Issue, runpkg.PromptOptions{
				WorkAttemptID: request.WorkAttemptID,
				Generation:    request.Generation,
			})
			if err != nil {
				t.Fatalf("BuildPrompt() error = %v", err)
			}
			fence := strings.LastIndex(prompt, "```detent-status\n")
			if fence < 0 {
				t.Fatal("prompt missing completion handoff")
			}
			block := strings.SplitN(prompt[fence+len("```detent-status\n"):], "```", 2)[0]
			signal, err := workpad.ParseStatusBlock(block, "digitaldrywood/detent")
			if err != nil {
				t.Fatalf("ParseStatusBlock() error = %v", err)
			}
			if !workpad.CurrentAttemptCompletion(signal, request.WorkAttemptID, request.Generation) {
				t.Fatalf("resumed completion rejected: fields %#v", signal.Fields)
			}
		})
	}
}

func TestRecoverDurableWorkAttemptsQueuesOwnedOrphanedSessionResume(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 10, 17, 0, 0, 0, time.UTC)
	issue := orphanRecoveryIssue()
	issue.Assignees = []string{"detent-bot"}
	issue.Fields["Detent Lease"] = formatClaimTime(now.Add(-time.Minute))
	tracker := hydratingDispatchConnector{issue: issue}
	attempts := &orphanRecoveryAttemptStore{
		metadata: runningWorkAttemptMetadataJSON(Running{Policy: orchestratorTestPolicy()}, nil),
		orphans:  []store.OrphanedAgentSession{orphanRecoverySession(now)},
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
		metadata: runningWorkAttemptMetadataJSON(Running{Policy: orchestratorTestPolicy()}, nil),
		orphans:  []store.OrphanedAgentSession{orphanRecoverySession(now)},
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
		metadata: runningWorkAttemptMetadataJSON(Running{Policy: orchestratorTestPolicy()}, nil),
		orphans:  []store.OrphanedAgentSession{orphanRecoverySession(now)},
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
		metadata: runningWorkAttemptMetadataJSON(Running{Policy: orchestratorTestPolicy()}, nil),
		orphans:  []store.OrphanedAgentSession{orphanRecoverySession(now)},
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
		metadata: runningWorkAttemptMetadataJSON(Running{Policy: orchestratorTestPolicy()}, nil),
		orphans:  []store.OrphanedAgentSession{orphanRecoverySession(now)},
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
	metadata  string
	readErr   error
	markErr   error
	readCalls int
}

func (s *orphanRecoveryAttemptStore) WorkAttempt(context.Context, int64) (store.WorkAttempt, error) {
	s.readCalls++
	return store.WorkAttempt{WorkerMetadataJSON: s.metadata}, s.readErr
}

func (s *orphanRecoveryAttemptStore) ListOrphanedAgentSessions(context.Context, string) ([]store.OrphanedAgentSession, error) {
	s.listCalls++
	return append([]store.OrphanedAgentSession(nil), s.orphans...), nil
}

func (s *orphanRecoveryAttemptStore) MarkAgentSessionOrphaned(_ context.Context, sessionID int64, _ time.Time) error {
	if s.markErr != nil {
		return s.markErr
	}
	s.marked = append(s.marked, sessionID)
	return nil
}

func TestOrphanRecoveryRetriesReadFailure(t *testing.T) {
	for _, mode := range []string{"owned", "foreign owner", "expired lease", "inactive", "missing", "repeated failure", "attempt read failure", "policy missing", "mark failure"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
			issue := orphanRecoveryIssue()
			issue.Identifier = "example-org/example-project#1155"
			issue.URL = "https://github.com/example-org/example-project/issues/1155"
			issue.Assignees = []string{"detent-bot"}
			issue.Fields["Detent Lease"] = formatClaimTime(now)
			session := orphanRecoverySession(now)
			session.Identifier, session.IssueURL = issue.Identifier, issue.URL
			tracker := &orphanReadConnector{hydratingDispatchConnector: hydratingDispatchConnector{issue: issue}, fail: true}
			attempts := &orphanRecoveryAttemptStore{metadata: runningWorkAttemptMetadataJSON(Running{Policy: orchestratorTestPolicy()}, nil), orphans: []store.OrphanedAgentSession{session}}
			orch, err := New(Config{Project: scheduler.ProjectCandidate{ID: "example"}, ActiveStates: []string{"In Progress"}, ResumeOrphanedSessions: true,
				FailureRetryBaseDelay: time.Minute, MaxRetryBackoff: time.Minute,
				Claiming: ClaimingConfig{Enabled: true, AssigneeLogin: "detent-bot", LeaseField: "Detent Lease", LeaseTTL: 10 * time.Minute},
			}, Dependencies{Connector: tracker, WorkAttempts: attempts})
			if err != nil {
				t.Fatal(err)
			}
			state := newState(orch.cfg)
			orch.recoverOrphanedAgentSessions(t.Context(), &state, attempts.orphans, now)
			retry, ok := state.Retry[issue.ID]
			if !ok || !retry.TrackerUnavailable || retry.ResumeState.DetentSessionID != session.ResumeState.DetentSessionID || retry.RecoveryAttemptID != session.WorkAttemptID || !retry.DueAt.After(now) || len(state.Blocked) != 0 || len(attempts.marked) != 0 {
				t.Fatalf("failed read lost pending recovery: %+v marked=%v", retry, attempts.marked)
			}
			if got := orch.prepareDispatchCandidates(t.Context(), &state, []connector.Issue{issue}, now); len(got) != 0 || tracker.calls != 1 {
				t.Fatalf("retried before backoff: candidates=%d calls=%d", len(got), tracker.calls)
			}
			releaseTrackerUnavailableRetries(&state, now)
			if !state.Retry[issue.ID].TrackerUnavailable {
				t.Fatal("unrelated tracker recovery bypassed orphan reconciliation")
			}
			tracker.fail = mode == "repeated failure"
			switch mode {
			case "attempt read failure":
				attempts.readErr = errors.New("attempt read failed")
			case "policy missing":
				attempts.metadata = ""
			case "mark failure":
				attempts.markErr = errors.New("mark failed")
			case "foreign owner":
				tracker.issue.Assignees = []string{"another-runner"}
			case "expired lease":
				tracker.issue.Fields["Detent Lease"] = formatClaimTime(now.Add(-time.Hour))
			case "inactive":
				tracker.issue.State = "Blocked"
			case "missing":
				tracker.missing = true
			}
			got := orch.prepareDispatchCandidates(t.Context(), &state, nil, retry.DueAt)
			next, queued := state.Retry[issue.ID]
			if tracker.calls != 2 || len(state.Blocked) != 0 {
				t.Fatalf("calls=%d blocked=%v", tracker.calls, state.Blocked)
			}
			switch mode {
			case "owned":
				if !queued || next.TrackerUnavailable || len(got) != 1 || len(attempts.marked) != 1 || next.ResumeState.ProviderThreadID != session.ResumeState.ProviderThreadID || next.Policy.ID != retry.Policy.ID {
					t.Fatalf("resume not recovered: %+v candidates=%d marked=%v", next, len(got), attempts.marked)
				}
			case "repeated failure", "attempt read failure", "policy missing", "mark failure":
				if !queued || !next.TrackerUnavailable || next.Attempt <= retry.Attempt || !next.DueAt.After(retry.DueAt) || len(got) != 0 || len(attempts.marked) != 0 {
					t.Fatalf("failure did not retain backoff: %+v", next)
				}
				delay := next.DueAt.Sub(retry.DueAt)
				if delay < 52500*time.Millisecond || delay > time.Minute {
					t.Fatalf("retry delay %s outside capped jitter bounds", delay)
				}
				if next.ResumeState != retry.ResumeState || next.RecoveryAttemptID != retry.RecoveryAttemptID || next.WorkerHost != retry.WorkerHost || next.Policy.ID != retry.Policy.ID {
					t.Fatal("reconciliation failure changed custody or identity")
				}
			default:
				if queued || len(got) != 0 || len(attempts.marked) != 0 {
					t.Fatalf("fencing bypass: queued=%t candidates=%d marked=%v", queued, len(got), attempts.marked)
				}
			}
		})
	}
}

type orphanReadConnector struct {
	hydratingDispatchConnector
	fail, missing bool
	calls         int
	issues        map[string]connector.Issue
}

func (c *orphanReadConnector) FetchIssueStatesByIDs(ctx context.Context, ids []string) ([]connector.Issue, error) {
	c.calls++
	if c.fail {
		return nil, errors.New("transient read failure")
	}
	if c.missing {
		return nil, nil
	}
	if c.issues != nil {
		issues := make([]connector.Issue, 0, len(ids))
		for _, id := range ids {
			if issue, ok := c.issues[id]; ok {
				issues = append(issues, issue)
			}
		}
		return issues, nil
	}
	return c.hydratingDispatchConnector.FetchIssueStatesByIDs(ctx, ids)
}

func TestOrphanRecoveryBoundedRetrySelection(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	tracker := &orphanReadConnector{fail: true}
	attempts := &orphanRecoveryAttemptStore{metadata: runningWorkAttemptMetadataJSON(Running{Policy: orchestratorTestPolicy()}, nil)}
	orch, err := New(Config{Project: scheduler.ProjectCandidate{ID: "example"}, ActiveStates: []string{"In Progress"}, ResumeOrphanedSessions: true},
		Dependencies{Connector: tracker, WorkAttempts: attempts})
	if err != nil {
		t.Fatal(err)
	}
	state := newState(orch.cfg)
	sessions := make([]store.OrphanedAgentSession, 10000)
	for i := range sessions {
		session := orphanRecoverySession(now)
		session.IssueID = fmt.Sprintf("issue-%d", i)
		session.Identifier = fmt.Sprintf("example-org/example-project#%d", i)
		session.IssueURL = ""
		session.WorkAttemptID = int64(i + 1)
		session.ResumeState.DetentSessionID = int64(i + 1)
		sessions[i] = session
	}
	orch.recoverOrphanedAgentSessions(t.Context(), &state, sessions, now)
	for id, retry := range state.Retry {
		retry.DueAt = now
		state.Retry[id] = retry
	}
	attempts.readCalls = 0
	attempts.readErr = errors.New("attempt unavailable")
	orch.reconcileOrphanedSessionRetries(t.Context(), &state, nil, now)
	if attempts.readCalls != dispatchCandidateLookahead {
		t.Fatalf("read %d attempts, want batch %d", attempts.readCalls, dispatchCandidateLookahead)
	}
	for _, session := range sessions[:dispatchCandidateLookahead] {
		retry := state.Retry[session.IssueID]
		if retry.Attempt != session.AttemptNumber+2 || !retry.DueAt.After(now) || retry.RecoveryAttemptID != session.WorkAttemptID {
			t.Fatalf("failed retry did not back off with custody: %+v", retry)
		}
	}
	attempts.readErr = nil
	tracker.fail = false
	tracker.issues = make(map[string]connector.Issue)
	for _, session := range sessions {
		issue := connector.NewIssue()
		issue.ID, issue.Identifier, issue.State = session.IssueID, session.Identifier, session.Lane
		issue.Title = "Resume pending session"
		tracker.issues[issue.ID] = issue
	}
	got := orch.reconcileOrphanedSessionRetries(t.Context(), &state, nil, now)
	if len(got) != dispatchCandidateLookahead || attempts.readCalls != 2*dispatchCandidateLookahead {
		t.Fatalf("later batch did not reconcile: candidates=%d reads=%d", len(got), attempts.readCalls)
	}
	for _, session := range sessions[dispatchCandidateLookahead : 2*dispatchCandidateLookahead] {
		if state.Retry[session.IssueID].TrackerUnavailable {
			t.Fatalf("later retry %s starved", session.IssueID)
		}
	}
	for id, retry := range state.Retry {
		retry.DueAt = now.Add(time.Hour)
		state.Retry[id] = retry
	}
	ready := sessions[3*dispatchCandidateLookahead]
	retry := state.Retry[ready.IssueID]
	retry.DueAt = now
	state.Retry[ready.IssueID] = retry
	reads := attempts.readCalls
	orch.reconcileOrphanedSessionRetries(t.Context(), &state, nil, now)
	if attempts.readCalls != reads {
		t.Fatal("selection scanned beyond a batch of not-yet-due entries")
	}
	got = orch.reconcileOrphanedSessionRetries(t.Context(), &state, nil, now)
	if len(got) != 1 || got[0].ID != ready.IssueID || attempts.readCalls != reads+1 {
		t.Fatal("selection did not resume after not-yet-due entries")
	}
}
