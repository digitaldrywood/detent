package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/forgeavailability"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestCompletionFenceDeferralOutcomes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		fenceErr error
		retired  bool
	}{
		{name: "503", fenceErr: completionDeferralAvailabilityError()},
		{name: "403", fenceErr: &github.StatusError{StatusCode: 403, Err: github.ErrAuthenticationFailed}},
		{name: "primary rate limit", fenceErr: &github.StatusError{StatusCode: 429, Err: github.ErrRateLimited}},
		{name: "secondary rate limit", fenceErr: &github.StatusError{StatusCode: 403, Err: github.ErrRateLimited, RateLimitKind: "secondary"}},
		{name: "GraphQL quota", fenceErr: &github.GraphQLErrorList{Err: github.ErrRateLimited, Errors: []github.GraphQLError{{Type: "RATE_LIMITED", Message: "API rate limit exceeded"}}}},
		{name: "reserve exhausted", fenceErr: fmt.Errorf("completion lane: %w", connector.ErrResourceExhausted)},
		{name: "timeout", fenceErr: context.DeadlineExceeded},
		{name: "canceled read", fenceErr: context.Canceled},
		{name: "transport", fenceErr: errors.New("connection reset by peer")},
		{name: "retired native fencing token", fenceErr: fmt.Errorf("%w: stale_fencing_token", runpkg.ErrExecutionAuthorityUnavailable), retired: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Date(2026, 8, 17, 19, 0, 0, 0, time.UTC)
			issue := completionDeferralIssue("issue-fence", "In Progress")
			tracker := &completionDeferralConnector{
				backendCapacityTestConnector: backendCapacityTestConnector{},
				issue:                        issue,
				firstErr:                     tt.fenceErr,
			}
			runtimeStore := openCompletionDeferralStore(t, filepath.Join(t.TempDir(), "detent.db"))
			cfg := completionDeferralConfig()
			orch := Orchestrator{cfg: cfg, connector: tracker, workAttempts: runtimeStore, now: func() time.Time { return now }}
			state := newState(cfg)
			attemptID := startCompletionDeferralAttempt(t, runtimeStore, issue, now)
			state.Running[issue.ID] = completionDeferralRunning(issue, attemptID, now)
			state.Claimed[issue.ID] = Claimed{Issue: cloneIssue(issue), ClaimedAt: now.Add(-time.Minute)}

			orch.handleQueuedRunResults(t.Context(), &state, completionDeferralEvent(issue, attemptID, now))
			if tt.retired {
				if len(state.deferredCompletions) != 0 || len(state.Retry) != 0 || len(state.Running) != 0 || len(state.Claimed) != 0 {
					t.Fatal("obsolete native completion retained live state")
				}
				receipt, err := runtimeStore.WorkAttempt(t.Context(), attemptID)
				if err != nil || receipt.TerminalState != store.WorkAttemptTerminalAbandoned {
					t.Fatalf("obsolete native completion receipt = %+v, %v", receipt, err)
				}
				orch.retryDeferredCompletions(t.Context(), &state, now.Add(time.Hour))
				if tracker.fetchCount() != 1 {
					t.Fatal("retired token was retried")
				}
				return
			}

			record, deferred := state.deferredCompletions[issue.ID]
			if !deferred {
				t.Fatal("fence read failure did not defer completion")
			}
			if got := tracker.fetchCount(); got != 1 {
				t.Fatalf("completion fence fetches = %d, want 1", got)
			}
			receipt, err := runtimeStore.WorkAttempt(t.Context(), attemptID)
			if err != nil {
				t.Fatal(err)
			}
			if receipt.Status != store.WorkAttemptStatusActive || receipt.Phase != deferredCompletionPhase || receipt.WaitReason != connector.TrackerUnavailableCondition || receipt.NextAction != deferredCompletionNextAction {
				t.Fatalf("deferred work attempt = %#v, want active tracker wait", receipt)
			}
			if strings.Contains(receipt.WorkerMetadataJSON, `"lane_revocation"`) || len(tracker.comments) != 0 {
				t.Fatalf("unreadable fence produced a revocation: %s", receipt.WorkerMetadataJSON)
			}
			if !strings.Contains(record.Availability.Message, tt.fenceErr.Error()) {
				t.Fatalf("fence error = %q, want original error %v", record.Availability.Message, tt.fenceErr)
			}
			if record.Result.Output != "validated completion" || record.Result.Tokens.TotalTokens != 37 {
				t.Fatalf("preserved result = %#v", record.Result)
			}
			snapshot := state.Snapshot(now)
			if len(snapshot.Queue) != 1 || snapshot.Queue[0].QueueState != telemetry.QueueStateWaitingOnTracker {
				t.Fatalf("snapshot queue = %#v, want waiting_on_tracker", snapshot.Queue)
			}
			if outcome := orch.dispatchIssueWithOutcome(t.Context(), &state, issue, 1, now, ""); outcome.dispatched || outcome.reason != dispatchSkipCompletionDeferred {
				t.Fatalf("dispatch while completion deferred = %#v, want suppressed", outcome)
			}
		})
	}
}

func TestDeferredCompletionRestartAndRecovery(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name               string
		recoveredState     string
		retryCount         int
		wantDeferred       bool
		wantTerminal       store.WorkAttemptTerminalState
		wantAcceptedTokens int64
		forgeClass         string
		approvalDenied     bool
		intakeOff          bool
		native             bool
		mixed              bool
	}{
		{name: "intake off preserves deferred completion through restart", intakeOff: true, recoveredState: "In Progress", retryCount: 1, wantTerminal: store.WorkAttemptTerminalSuccess, wantAcceptedTokens: 37},
		{
			name:           "deferral survives restart",
			recoveredState: "In Progress",
			wantDeferred:   true,
		},
		{
			name:               "retry after recovery accepts intact lane",
			recoveredState:     "In Progress",
			retryCount:         1,
			wantTerminal:       store.WorkAttemptTerminalSuccess,
			wantAcceptedTokens: 37,
		},
		{
			name:               "retry after recovery honors observed lane",
			recoveredState:     "Todo",
			retryCount:         1,
			wantTerminal:       store.WorkAttemptTerminalSuccess,
			wantAcceptedTokens: 37,
		},
		{
			name:               "completion cannot be accepted twice",
			recoveredState:     "In Progress",
			retryCount:         2,
			wantTerminal:       store.WorkAttemptTerminalSuccess,
			wantAcceptedTokens: 37,
		},
		{name: "canonical server wrapper", recoveredState: "In Progress", retryCount: 1, wantTerminal: store.WorkAttemptTerminalCapacity, forgeClass: forgeavailability.ClassServer},
		{name: "native server wrapper", recoveredState: "In Progress", retryCount: 1, wantTerminal: store.WorkAttemptTerminalCapacity, forgeClass: forgeavailability.ClassServer, native: true},
		{name: "canonical approval wrapper", recoveredState: "In Progress", retryCount: 1, wantTerminal: store.WorkAttemptTerminalCapacity, forgeClass: forgeavailability.ClassTransport, approvalDenied: true},
		{name: "native approval wrapper", recoveredState: "In Progress", retryCount: 1, wantTerminal: store.WorkAttemptTerminalCapacity, forgeClass: forgeavailability.ClassTransport, approvalDenied: true, native: true},
		{name: "canonical mixed wrapper", recoveredState: "In Progress", retryCount: 1, wantTerminal: store.WorkAttemptTerminalCapacity, forgeClass: forgeavailability.ClassServer, mixed: true},
		{name: "native mixed wrapper", recoveredState: "In Progress", retryCount: 1, wantTerminal: store.WorkAttemptTerminalCapacity, forgeClass: forgeavailability.ClassServer, mixed: true, native: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Date(2026, 8, 17, 19, 30, 0, 0, time.UTC)
			dbPath := filepath.Join(t.TempDir(), "detent.db")
			issue := completionDeferralIssue("issue-restart", "In Progress")
			tracker := &completionDeferralConnector{
				backendCapacityTestConnector: backendCapacityTestConnector{},
				issue:                        issue,
				firstErr:                     completionDeferralAvailabilityError(),
			}
			cfg := completionDeferralConfig()
			cfg.ForgeHost = "github.com"
			var trackerOwner connector.Connector = tracker
			var nativeTracker *nativeWorkflowConnector
			if tt.native {
				nativeTracker = &nativeWorkflowConnector{autoPromoteTickConnector: &autoPromoteTickConnector{}, states: []connector.WorkflowState{
					{Name: "In Progress", Dispatchable: true, Transitions: []string{"Blocked", "Done"}},
					{Name: "Blocked"}, {Name: "Done", Terminal: true},
				}}
				trackerOwner = nativeTracker
			}
			initialStore := openCompletionDeferralStoreWithoutCleanup(t, dbPath)
			attemptID := startCompletionDeferralAttempt(t, initialStore, issue, now)
			initialOrch := Orchestrator{cfg: cfg, connector: trackerOwner, workAttempts: initialStore, now: func() time.Time { return now }}
			initialOrch.localIntakeDisabled.Store(tt.intakeOff)
			initialState := newState(cfg)
			initialState.Running[issue.ID] = completionDeferralRunning(issue, attemptID, now)
			initialState.Claimed[issue.ID] = Claimed{Issue: cloneIssue(issue), ClaimedAt: now.Add(-time.Minute)}
			event := completionDeferralEvent(issue, attemptID, now)
			if tt.forgeClass != "" {
				message := "HTTP 503: upstream unavailable"
				if tt.approvalDenied {
					message = "tool approval declined"
				}
				delivery := &runpkg.DeliverableRecoveryError{Branch: "detent/1869", Err: &runpkg.DeliverableCommandError{
					OperationClass: "pull_request", Operation: "gh pr create", Message: message, ApprovalDenied: tt.approvalDenied,
					Command: "command-only-unrecorded", Arguments: `{"head":"detent/1869"}`, Body: "opaque backend detail",
				}}
				event.Err = forgeavailability.NewError(forgeavailability.Scope{Host: "forge.example.test", Operation: "gh pr create"}, tt.forgeClass, delivery)
				if tt.mixed {
					event.Err = errors.Join(event.Err, runpkg.ErrWorkspacePreparation)
				}
				event.Result.FinalState = runpkg.FinalStateNeedsHumanAttention
				event.Result.FinalMessage = "May I merge?"
				event.Result.PullRequestHeadPushed = true
				event.Result.WorkspaceBranch = "detent/1869"
			}
			initialOrch.handleRunResult(t.Context(), &initialState, event)
			if err := initialStore.Close(); err != nil {
				t.Fatalf("Close(initial store) error = %v", err)
			}

			restartedStore := openCompletionDeferralStore(t, dbPath)
			recoveredIssue := cloneIssue(issue)
			recoveredIssue.State = tt.recoveredState
			tracker.setIssue(recoveredIssue)
			if nativeTracker != nil {
				nativeTracker.stateIssues = []connector.Issue{recoveredIssue}
			}
			restartAt := now.Add(2 * time.Minute)
			restartedOrch := Orchestrator{cfg: cfg, connector: trackerOwner, workAttempts: restartedStore, now: func() time.Time { return restartAt }}
			restartedOrch.localIntakeDisabled.Store(tt.intakeOff)
			restartedState := newState(cfg)
			restartedOrch.recoverDurableWorkAttempts(t.Context(), &restartedState, restartAt)

			if _, ok := restartedState.deferredCompletions[issue.ID]; !ok {
				t.Fatalf("deferred completion missing after restart: %#v", restartedState.deferredCompletions)
			}
			if retry, ok := restartedState.Retry[issue.ID]; !ok || !retry.CompletionDeferred {
				t.Fatalf("Retry[%q] = %#v, want recovered completion deferral", issue.ID, retry)
			}
			if tt.forgeClass != "" {
				decoded := restartedState.deferredCompletions[issue.ID].completion()
				availability, typed := forgeavailability.As(decoded.Err)
				if !typed || availability.Scope.Host != "forge.example.test" || availability.Scope.Operation != "gh pr create" || availability.Class != tt.forgeClass {
					t.Fatalf("restored worker availability = %+v, want original forge scope/class", availability)
				}
				if !tt.mixed {
					var command *runpkg.DeliverableCommandError
					if !errors.As(decoded.Err, &command) || command.ApprovalDenied != tt.approvalDenied || command.Arguments != "" || command.Command != "" || command.Body != "" {
						t.Fatalf("restored summarized command = %+v", command)
					}
				}
			}
			if _, ok := restartedState.Claimed[issue.ID]; !ok {
				t.Fatalf("Claimed[%q] missing after restart", issue.ID)
			}
			if outcome := restartedOrch.dispatchIssueWithOutcome(t.Context(), &restartedState, recoveredIssue, 1, restartAt, ""); outcome.dispatched || outcome.reason != dispatchSkipCompletionDeferred {
				t.Fatalf("dispatch after restart = %#v, want suppressed", outcome)
			}
			if tt.retryCount > 1 {
				fetchesBeforeDuplicate := tracker.fetchCount()
				restartedOrch.handleRunResult(t.Context(), &restartedState, completionDeferralEvent(issue, attemptID, restartAt))
				if tracker.fetchCount() != fetchesBeforeDuplicate {
					t.Fatalf("duplicate delivery retried completion fence")
				}
				receipt, err := restartedStore.WorkAttempt(t.Context(), attemptID)
				if err != nil {
					t.Fatalf("WorkAttempt() after duplicate delivery error = %v", err)
				}
				if receipt.Status != store.WorkAttemptStatusActive || receipt.Phase != deferredCompletionPhase {
					t.Fatalf("duplicate delivery changed deferred receipt = %#v", receipt)
				}
			}

			fetchesAfterFirstRetry := 0
			for retryIndex := range tt.retryCount {
				if ok := restartedOrch.retryDeferredCompletions(t.Context(), &restartedState, restartAt); !ok {
					t.Fatal("retryDeferredCompletions() = false, want completed fence decision")
				}
				if retryIndex == 0 {
					fetchesAfterFirstRetry = tracker.fetchCount()
				}
			}
			if tt.retryCount > 1 && tracker.fetchCount() != fetchesAfterFirstRetry {
				t.Fatalf("completion fence fetches after duplicate retry = %d, want unchanged %d", tracker.fetchCount(), fetchesAfterFirstRetry)
			}

			receipt, err := restartedStore.WorkAttempt(t.Context(), attemptID)
			if err != nil {
				t.Fatalf("WorkAttempt() error = %v", err)
			}
			if tt.wantDeferred {
				if receipt.Status != store.WorkAttemptStatusActive || receipt.Phase != deferredCompletionPhase {
					t.Fatalf("restarted receipt = %#v, want active completion deferral", receipt)
				}
				return
			}
			if receipt.TerminalState != tt.wantTerminal {
				t.Fatalf("terminal state = %q, want %q", receipt.TerminalState, tt.wantTerminal)
			}
			if tt.forgeClass != "" {
				wantClass := tt.forgeClass
				if tt.approvalDenied {
					wantClass = forgeavailability.ClassWorkerGitHubCredentialUnavailable
				}
				retry := restartedState.Retry[issue.ID]
				condition := restartedState.ForgeUnavailable["forge.example.test"]
				if receipt.ErrorClass != forgeUnavailableErrorClass || !allowanceInfrastructureAttempt(receipt) || !retry.ForgeUnavailable || retry.Attempt != 2 || retry.ForgeRetry == nil || retry.ForgeRetry.Branch != "detent/1869" || !retry.ForgeRetry.WorkProductPushed || condition.Operation != "gh pr create" || condition.ErrorClass != wantClass {
					t.Fatalf("receipt = %+v, retry = %+v, condition = %+v, want same-attempt instance %s wait", receipt, retry, condition, wantClass)
				}
				if _, blocked := restartedState.Blocked[issue.ID]; blocked {
					t.Fatal("instance outage became a human hold")
				}
				if nativeTracker != nil && (len(nativeTracker.updates) != 0 || len(nativeTracker.comments) != 0) || len(tracker.updates) != 0 || len(tracker.comments) != 0 {
					t.Fatal("instance outage mutated tracker state")
				}
			}
			if _, ok := restartedState.deferredCompletions[issue.ID]; ok {
				t.Fatalf("deferred completion remains after fence decision")
			}
			if restartedState.TokenTotals.TotalTokens != tt.wantAcceptedTokens {
				t.Fatalf("accepted tokens = %d, want %d", restartedState.TokenTotals.TotalTokens, tt.wantAcceptedTokens)
			}
		})
	}
}

type completionDeferralConnector struct {
	backendCapacityTestConnector
	mu       sync.Mutex
	issue    connector.Issue
	firstErr error
	fetches  int
}

func (c *completionDeferralConnector) FetchIssueStatesByIDs(context.Context, []string) ([]connector.Issue, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fetches++
	if c.fetches == 1 && c.firstErr != nil {
		return nil, c.firstErr
	}
	return []connector.Issue{cloneIssue(c.issue)}, nil
}

func (c *completionDeferralConnector) fetchCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fetches
}

func (c *completionDeferralConnector) setIssue(issue connector.Issue) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.issue = cloneIssue(issue)
}

func completionDeferralAvailabilityError() error {
	return connector.NewTrackerAvailabilityError(connector.TrackerAvailabilityScope{
		Connector:          "github",
		Endpoint:           "https://api.github.test/graphql",
		Operation:          "issue_lookup",
		CredentialIdentity: "github-rest:test",
	}, connector.TrackerAvailabilityClassServer, errors.New("upstream returned status 503"))
}

func completionDeferralConfig() Config {
	return normalizeConfig(Config{
		Project:                scheduler.ProjectCandidate{ID: "detent"},
		PollInterval:           time.Minute,
		ContinuationRetryDelay: time.Minute,
		ActiveStates:           []string{"In Progress"},
		TerminalStates:         []string{"Done"},
		MaxConcurrentAgents:    1,
		FailureRetryBaseDelay:  time.Minute,
		MaxRetryBackoff:        time.Minute,
		OverloadRetryDelay:     time.Minute,
	})
}

func completionDeferralIssue(id string, state string) connector.Issue {
	issue := connector.NewIssue()
	issue.ID = id
	issue.Identifier = "digitaldrywood/detent#1869"
	issue.Title = "Defer completion fence"
	issue.URL = "https://github.com/digitaldrywood/detent/issues/1869"
	issue.State = state
	return issue
}

func completionDeferralRunning(issue connector.Issue, attemptID int64, now time.Time) Running {
	return Running{
		Issue:         cloneIssue(issue),
		Attempt:       2,
		WorkAttemptID: attemptID,
		Generation:    7,
		Mode:          runpkg.RunModeImplement,
		StartedAt:     now.Add(-time.Minute),
		WorkerHost:    "worker-a",
	}
}

func completionDeferralEvent(issue connector.Issue, attemptID int64, now time.Time) runpkg.Completion {
	return runpkg.Completion{
		IssueID: issue.ID,
		Request: runpkg.RunRequest{
			ProjectID:     "detent",
			Issue:         cloneIssue(issue),
			Attempt:       2,
			WorkAttemptID: attemptID,
			Generation:    7,
			Mode:          runpkg.RunModeImplement,
			StartedAt:     now.Add(-time.Minute),
			WorkerHost:    "worker-a",
		},
		Result: runpkg.RunResult{
			FinalState: runpkg.FinalStateCompleted,
			Output:     "validated completion",
			Tokens:     runpkg.TokenTotals{InputTokens: 25, OutputTokens: 12, TotalTokens: 37},
			DiffStats:  runpkg.DiffStats{FilesChanged: 2, AddedLines: 8, RemovedLines: 3, Status: "clean"},
		},
		CompletedAt: now,
	}
}

func startCompletionDeferralAttempt(t *testing.T, runtimeStore store.Store, issue connector.Issue, now time.Time) int64 {
	t.Helper()
	attemptID, err := runtimeStore.StartWorkAttempt(t.Context(), store.WorkAttemptStart{
		ProjectID:      "detent",
		IssueID:        issue.ID,
		Identifier:     issue.Identifier,
		IssueURL:       issue.URL,
		WorkerType:     "implement",
		WorkerHost:     "worker-a",
		Lane:           issue.State,
		AttemptNumber:  2,
		StartedAt:      now.Add(-time.Minute),
		LeaseExpiresAt: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("StartWorkAttempt() error = %v", err)
	}
	return attemptID
}

func openCompletionDeferralStore(t *testing.T, path string) store.Store {
	t.Helper()
	runtimeStore := openCompletionDeferralStoreWithoutCleanup(t, path)
	t.Cleanup(func() {
		if err := runtimeStore.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})
	return runtimeStore
}

func openCompletionDeferralStoreWithoutCleanup(t *testing.T, path string) store.Store {
	t.Helper()
	runtimeStore, err := store.Open(t.Context(), store.Config{Backend: store.BackendSQLite, Path: path})
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	return runtimeStore
}

func TestCompletionFenceUnchangedOrUnknownLaneDefers(t *testing.T) {
	t.Parallel()
	for _, lane := range []string{""} {
		t.Run(lane, func(t *testing.T) {
			now := time.Now().UTC()
			issue := completionDeferralIssue("unchanged", lane)
			cfg := completionDeferralConfig()
			tracker := &completionDeferralConnector{issue: issue}
			attempts := &recordingWorkAttemptStore{}
			orch := Orchestrator{cfg: cfg, connector: tracker, workAttempts: attempts, now: func() time.Time { return now }}
			state := newState(cfg)
			state.Running[issue.ID] = completionDeferralRunning(issue, 2297, now)
			orch.handleRunResult(t.Context(), &state, completionDeferralEvent(issue, 2297, now))
			if len(state.deferredCompletions) != 1 || len(attempts.completions) != 0 || len(tracker.comments) != 0 {
				t.Fatal("unchanged or unknown lane finalized as revoked")
			}
		})
	}
}

func TestCompletionFenceRateLimitDeadlineSurvivesRestart(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		after time.Duration
		reset time.Duration
		want  time.Duration
	}{
		{name: "reset", reset: time.Hour, want: time.Hour},
		{name: "secondary backoff", after: 10 * time.Minute, want: 10 * time.Minute},
		{name: "later reset", after: time.Minute, reset: time.Hour, want: time.Hour},
	} {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Now().UTC()
			issue := completionDeferralIssue("quota", "In Progress")
			cfg := completionDeferralConfig()
			tracker := &completionDeferralConnector{issue: issue, firstErr: &github.StatusError{Err: github.ErrRateLimited, StatusCode: 429, RetryAfter: tt.after, ResetAt: now.Add(tt.reset)}}
			backend := openCompletionDeferralStore(t, filepath.Join(t.TempDir(), "detent.db"))
			id := startCompletionDeferralAttempt(t, backend, issue, now)
			orch := Orchestrator{cfg: cfg, connector: tracker, workAttempts: backend, now: func() time.Time { return now }}
			state := newState(cfg)
			state.Running[issue.ID] = completionDeferralRunning(issue, id, now)
			orch.handleRunResult(t.Context(), &state, completionDeferralEvent(issue, id, now))
			recovered := newState(cfg)
			orch.recoverDurableWorkAttempts(t.Context(), &recovered, now.Add(time.Second))
			if got := recovered.Retry[issue.ID].DueAt; !got.Equal(now.Add(tt.want)) {
				t.Fatalf("retry = %s, want %s", got, now.Add(tt.want))
			}
			orch.retryDeferredCompletions(t.Context(), &recovered, now.Add(tt.want-time.Second))
			if tracker.fetchCount() != 1 {
				t.Fatal("fence retried before quota recovered")
			}
		})
	}
}

func TestCompletionFenceMissingLaneDefers(t *testing.T) {
	t.Parallel()
	for _, returnedID := range []string{"", "missing-lane"} {
		t.Run(returnedID, func(t *testing.T) {
			now := time.Now().UTC()
			issue := completionDeferralIssue("missing-lane", "In Progress")
			cfg := completionDeferralConfig()
			tracker := &completionDeferralConnector{issue: connector.Issue{ID: returnedID}}
			attempts := &recordingWorkAttemptStore{}
			orch := Orchestrator{cfg: cfg, connector: tracker, workAttempts: attempts, now: func() time.Time { return now }}
			state := newState(cfg)
			state.Running[issue.ID] = completionDeferralRunning(issue, 2297, now)
			orch.handleRunResult(t.Context(), &state, completionDeferralEvent(issue, 2297, now))
			if len(state.deferredCompletions) != 1 || len(attempts.completions) != 0 {
				t.Fatal("missing tracker lane did not defer completion")
			}
		})
	}
}
