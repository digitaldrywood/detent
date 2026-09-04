package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
)

func TestWorkerGitHubTokenResolutionCompletionPreservesAttemptBudget(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	issue := implementProgressIssueWithoutPR()
	issue.ID = "issue-worker-github-token"
	issue.Identifier = "digitaldrywood/detent#2055"
	cfg := normalizeConfig(Config{
		Project:             scheduler.ProjectCandidate{ID: "detent"},
		MaxConcurrentAgents: 1,
		ActiveStates:        []string{"Todo", "In Progress"},
		TerminalStates:      []string{"Done", "Cancelled"},
		OverloadRetryDelay:  45 * time.Second,
	})
	attempts := &implementProgressAttemptStore{}
	var logs bytes.Buffer
	orch := &Orchestrator{
		cfg:          cfg,
		connector:    &implementProgressConnector{},
		workAttempts: attempts,
		logger:       slog.New(slog.NewTextHandler(&logs, nil)),
	}
	state := newState(cfg)
	state.Running[issue.ID] = Running{
		Issue: issue, Attempt: 4, WorkAttemptID: 2055, Mode: runpkg.RunModeImplement,
		StartedAt: now.Add(-time.Minute), DiffStats: DiffStats{Status: "clean"},
	}
	state.Claimed[issue.ID] = Claimed{Issue: issue, ClaimedAt: now.Add(-time.Minute)}
	state.InstantFailures[issue.ID] = InstantFailure{Issue: issue, Count: instantFailureThreshold - 1}
	state.RepeatedFailures[issue.ID] = RepeatedFailure{Issue: issue, Count: repeatedFailureThreshold - 1}
	state.FailureBreaker.Failures["existing"] = []ProjectFailure{{IssueID: "other", At: now.Add(-time.Minute)}}
	instantFailures := map[string]InstantFailure{issue.ID: state.InstantFailures[issue.ID]}
	repeatedFailures := map[string]RepeatedFailure{issue.ID: state.RepeatedFailures[issue.ID]}
	failureBreaker := cloneProjectFailureBreaker(state.FailureBreaker)

	resolutionErr := &runpkg.WorkerGitHubTokenResolutionError{
		Attempts: 3,
		Timeout:  15 * time.Second,
		Err:      context.DeadlineExceeded,
	}
	supervisor, err := runpkg.NewSupervisor(staticWorkerGitHubTokenResolutionBackend{err: resolutionErr}, runpkg.SupervisorConfig{
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewSupervisor() error = %v", err)
	}
	completion := supervisor.Run(t.Context(), runpkg.RunRequest{Issue: issue, Attempt: 4, Mode: runpkg.RunModeImplement})
	if completion.Retryable || completion.RetryAttempt != 0 || completion.RetryDelay != 0 {
		t.Fatalf("runner completion = %#v, want infrastructure-owned retry", completion)
	}
	orch.handleRunResult(t.Context(), &state, completion)

	if len(attempts.completions) != 1 {
		t.Fatalf("completions = %#v, want one durable infrastructure wait", attempts.completions)
	}
	completed := attempts.completions[0]
	if completed.TerminalState != store.WorkAttemptTerminalCapacity || completed.ErrorClass != workerGitHubTokenResolutionErrorClass {
		t.Fatalf("completion = %#v, want token-resolution capacity classification", completed)
	}
	var persisted struct {
		Wait workerGitHubTokenResolutionWaitMetadata `json:"worker_github_token_resolution_wait"`
	}
	if err := json.Unmarshal([]byte(completed.WorkerMetadataJSON), &persisted); err != nil {
		t.Fatalf("decode worker metadata: %v", err)
	}
	if persisted.Wait.Attempts != 3 || persisted.Wait.TimeoutMS != 15000 || !persisted.Wait.NextRetryAt.Equal(now.Add(45*time.Second)) {
		t.Fatalf("resolution wait = %#v, want attempts, timeout, and retry deadline", persisted.Wait)
	}
	retry, ok := state.Retry[issue.ID]
	if !ok || retry.Attempt != 4 || !retry.DueAt.Equal(now.Add(45*time.Second)) || retry.Wait.Kind != workerGitHubTokenResolutionWaitKind {
		t.Fatalf("retry = %#v, want same-attempt token-resolution wait", retry)
	}
	if !reflect.DeepEqual(state.InstantFailures, instantFailures) || !reflect.DeepEqual(state.RepeatedFailures, repeatedFailures) || !reflect.DeepEqual(state.FailureBreaker, failureBreaker) {
		t.Fatalf("failure budgets changed: instant=%#v repeated=%#v project=%#v", state.InstantFailures, state.RepeatedFailures, state.FailureBreaker)
	}
	if _, blocked := state.Blocked[issue.ID]; blocked {
		t.Fatalf("issue %q was blocked by machine-recoverable token resolution", issue.ID)
	}
	if strings.Contains(completed.WorkerMetadataJSON, "completion_progress") {
		t.Fatalf("worker metadata = %s, infrastructure failure reached progress processing", completed.WorkerMetadataJSON)
	}
	if !strings.Contains(logs.String(), workerGitHubTokenResolutionErrorClass) || !strings.Contains(logs.String(), "host credential store may be slow") {
		t.Fatalf("logs = %q, want distinct token-resolution infrastructure event", logs.String())
	}
}

func TestRecoverDurableWorkerGitHubTokenResolutionWait(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 3, 12, 10, 0, 0, time.UTC)
	nextRetryAt := now.Add(30 * time.Second)
	issue := connector.Issue{
		ID: "issue-worker-github-token", Identifier: "digitaldrywood/detent#2055", State: "In Progress",
	}
	wait := workerGitHubTokenResolutionWaitMetadata{
		Attempts: 3, TimeoutMS: 15000, DetectedAt: now.Add(-time.Minute), NextRetryAt: nextRetryAt,
	}
	attempts := &recordingWorkAttemptStore{recent: []store.WorkAttempt{{
		ID: 2055, ProjectID: "detent", IssueID: issue.ID, Identifier: issue.Identifier, Lane: issue.State,
		AttemptNumber: 4, WorkerHost: "worker-a", Status: store.WorkAttemptStatusTerminal, CompletedAt: now.Add(-time.Minute),
		TerminalState: store.WorkAttemptTerminalCapacity, ErrorClass: workerGitHubTokenResolutionErrorClass,
		ErrorMessage: "credential store timed out", WorkerMetadataJSON: marshalWorkAttemptJSON(map[string]any{"worker_github_token_resolution_wait": wait}),
	}}}
	cfg := normalizeConfig(Config{
		Project: scheduler.ProjectCandidate{ID: "detent"}, ActiveStates: []string{"Todo", "In Progress"},
		TerminalStates: []string{"Done", "Cancelled"}, MaxConcurrentAgents: 1,
	})
	orch := &Orchestrator{
		cfg: cfg, connector: &rateLimitConnector{issuesByID: []connector.Issue{issue}}, workAttempts: attempts,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)), now: func() time.Time { return now },
	}
	state := newState(cfg)

	orch.recoverDurableWorkAttempts(t.Context(), &state, now)

	retry, ok := state.Retry[issue.ID]
	if !ok || retry.Attempt != 4 || !retry.DueAt.Equal(nextRetryAt) || retry.WorkerHost != "worker-a" || retry.Wait.Kind != workerGitHubTokenResolutionWaitKind || !retry.Wait.StartedAt.Equal(wait.DetectedAt) {
		t.Fatalf("retry = %#v, want restored same-attempt token-resolution wait", retry)
	}
	if terminalAttemptRetryableFailure(telemetryWorkAttempt(attempts.recent[0], now)) {
		t.Fatal("durable token-resolution wait treated as a generic terminal retry")
	}
}

func TestWorkerGitHubTokenResolutionWaitMetadataValidation(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 3, 12, 20, 0, 0, time.UTC)
	valid := workerGitHubTokenResolutionWaitMetadata{
		Attempts: 3, TimeoutMS: 15000, DetectedAt: now, NextRetryAt: now.Add(time.Minute),
	}
	attempt := func(wait workerGitHubTokenResolutionWaitMetadata) store.WorkAttempt {
		return store.WorkAttempt{
			TerminalState: store.WorkAttemptTerminalCapacity,
			ErrorClass:    workerGitHubTokenResolutionErrorClass,
			WorkerMetadataJSON: marshalWorkAttemptJSON(map[string]any{
				"worker_github_token_resolution_wait": wait,
			}),
		}
	}
	tests := []struct {
		name    string
		attempt store.WorkAttempt
		want    bool
	}{
		{name: "valid", attempt: attempt(valid), want: true},
		{name: "malformed", attempt: store.WorkAttempt{TerminalState: store.WorkAttemptTerminalCapacity, ErrorClass: workerGitHubTokenResolutionErrorClass, WorkerMetadataJSON: `{`}},
		{name: "ordinary failure", attempt: store.WorkAttempt{TerminalState: store.WorkAttemptTerminalFailure, ErrorClass: workerGitHubTokenResolutionErrorClass, WorkerMetadataJSON: attempt(valid).WorkerMetadataJSON}},
		{name: "missing attempts", attempt: attempt(workerGitHubTokenResolutionWaitMetadata{TimeoutMS: valid.TimeoutMS, DetectedAt: valid.DetectedAt, NextRetryAt: valid.NextRetryAt})},
		{name: "missing timeout", attempt: attempt(workerGitHubTokenResolutionWaitMetadata{Attempts: valid.Attempts, DetectedAt: valid.DetectedAt, NextRetryAt: valid.NextRetryAt})},
		{name: "missing retry deadline", attempt: attempt(workerGitHubTokenResolutionWaitMetadata{Attempts: valid.Attempts, TimeoutMS: valid.TimeoutMS, DetectedAt: valid.DetectedAt})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, got := workerGitHubTokenResolutionWaitMetadataFromAttempt(tt.attempt)
			if got != tt.want {
				t.Fatalf("workerGitHubTokenResolutionWaitMetadataFromAttempt() valid = %v, want %v", got, tt.want)
			}
		})
	}
}

type staticWorkerGitHubTokenResolutionBackend struct {
	err error
}

func (b staticWorkerGitHubTokenResolutionBackend) Run(context.Context, runpkg.RunRequest) (runpkg.RunResult, error) {
	return runpkg.RunResult{}, b.err
}
