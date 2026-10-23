package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/digitaldrywood/detent/internal/backendcapacity"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func TestHandleRunResultRoutesTerminalRetryByWorkProduct(t *testing.T) {
	t.Parallel()

	scope := backendcapacity.Scope{BackendID: "codex", BackendKind: "codex", Provider: "openai"}
	backendPrefix := strings.Repeat("backend diagnostic ", 30)
	firstBackendBody := backendPrefix + "first failure" + backendPrefix
	secondBackendBody := backendPrefix + "second failure" + backendPrefix
	capacityReset := time.Date(2026, 7, 18, 16, 0, 0, 0, time.UTC)
	tests := []struct {
		name            string
		limit           *int
		wantBlocked     bool
		completionErr   bool
		noStore         bool
		noAttemptID     bool
		issue           connector.Issue
		result          runpkg.RunResult
		runError        error
		wantState       string
		wantTransitions []string
	}{
		{name: "distinct backend bodies retry", issue: terminalRetryTestIssue("distinct-backend"), runError: failureBreakerBackendError{body: secondBackendBody}, wantState: "Todo", wantTransitions: []string{"Todo"}},
		{name: "identical backend bodies retry", issue: terminalRetryTestIssue("identical-backend"), runError: failureBreakerBackendError{body: firstBackendBody}, wantState: "Todo", wantTransitions: []string{"Todo"}},
		{name: "zero parks failed persistence", limit: new(0), issue: terminalRetryTestIssue("zero-store-error"), runError: errors.New("runner failed"), completionErr: true, wantState: "Todo", wantTransitions: []string{"Todo"}},
		{name: "zero parks missing store", limit: new(0), issue: terminalRetryTestIssue("zero-no-store"), runError: errors.New("runner failed"), noStore: true, wantState: "Todo", wantTransitions: []string{"Todo"}},
		{name: "zero parks missing attempt ID", limit: new(0), issue: terminalRetryTestIssue("zero-no-id"), runError: errors.New("runner failed"), noAttemptID: true, wantState: "Todo", wantTransitions: []string{"Todo"}},
		{name: "zero preserves overload without store", limit: new(0), issue: terminalRetryTestIssue("zero-overload-no-store"), runError: backendcapacity.NewError(scope, backendcapacity.Details{Type: backendcapacity.ErrorTypeTransientOverload, Kind: "serverOverloaded"}, errors.New("provider overloaded")), noStore: true, wantState: "In Progress"},
		{name: "zero preserves capacity without store", limit: new(0), issue: terminalRetryTestIssue("zero-capacity-no-store"), runError: backendcapacity.NewError(scope, backendcapacity.Details{Kind: "usageLimitExceeded", ResetAt: &capacityReset}, errors.New("provider usage limit reached")), noStore: true, wantState: "Todo", wantTransitions: []string{"Todo"}},
		{name: "zero preserves pushed work without store", limit: new(0), issue: terminalRetryTestIssue("zero-pushed-no-store"), result: runpkg.RunResult{PullRequestHeadPushed: true}, runError: errors.New("runner failed"), noStore: true, wantState: "In Progress"},
		{name: "default preserves failed persistence retry", issue: terminalRetryTestIssue("default-store-error"), runError: errors.New("runner failed"), completionErr: true, wantState: "Todo", wantTransitions: []string{"Todo"}},
		{name: "zero parks runner failure", limit: new(0), issue: terminalRetryTestIssue("zero-failure"), runError: errors.New("runner failed"), wantState: "Todo", wantTransitions: []string{"Todo"}},
		{name: "zero preserves transient overload", limit: new(0), issue: terminalRetryTestIssue("zero-overload"), runError: backendcapacity.NewError(scope, backendcapacity.Details{Type: backendcapacity.ErrorTypeTransientOverload, Kind: "serverOverloaded"}, errors.New("provider overloaded")), wantState: "In Progress"},
		{name: "zero preserves capacity wait", limit: new(0), issue: terminalRetryTestIssue("zero-capacity"), runError: backendcapacity.NewError(scope, backendcapacity.Details{Kind: "usageLimitExceeded", ResetAt: &capacityReset}, errors.New("provider usage limit reached")), wantState: "Todo", wantTransitions: []string{"Todo"}},
		{name: "zero preserves pushed work", limit: new(0), issue: terminalRetryTestIssue("zero-pushed"), result: runpkg.RunResult{PullRequestHeadPushed: true}, runError: errors.New("runner failed"), wantState: "In Progress"},
		{name: "zero preserves linked PR", limit: new(0), issue: terminalRetryTestIssueWithPullRequest("zero-pr"), runError: errors.New("runner failed"), wantState: "In Progress"},
		{
			name:            "no pushed work returns to todo",
			issue:           terminalRetryTestIssue("empty"),
			runError:        errors.New("runner failed"),
			wantState:       "Todo",
			wantTransitions: []string{"Todo"},
		},
		{
			name:      "transient overload retains current lane",
			issue:     terminalRetryTestIssue("overload"),
			runError:  backendcapacity.NewError(scope, backendcapacity.Details{Type: backendcapacity.ErrorTypeTransientOverload, Kind: "serverOverloaded"}, errors.New("provider overloaded")),
			wantState: "In Progress",
		},
		{
			name:      "started turn timeout retains current lane",
			issue:     terminalRetryTestIssue("startup-timeout"),
			runError:  backendcapacity.NewError(scope, backendcapacity.Details{Type: backendcapacity.ErrorTypeTransientOverload, Kind: backendcapacity.StartupTimeoutKind}, context.DeadlineExceeded),
			wantState: "In Progress",
		},
		{
			name:            "provider capacity returns to todo",
			issue:           terminalRetryTestIssue("capacity"),
			runError:        backendcapacity.NewError(scope, backendcapacity.Details{Kind: "usageLimitExceeded", ResetAt: &capacityReset}, errors.New("provider usage limit reached")),
			wantState:       "Todo",
			wantTransitions: []string{"Todo"},
		},
		{
			name:      "pushed branch keeps in progress",
			issue:     terminalRetryTestIssue("pushed"),
			result:    runpkg.RunResult{PullRequestHeadPushed: true},
			runError:  errors.New("runner failed"),
			wantState: "In Progress",
		},
		{
			name:      "linked pull request keeps in progress",
			issue:     terminalRetryTestIssueWithPullRequest("pull-request"),
			runError:  errors.New("runner failed"),
			wantState: "In Progress",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
			tracker := &terminalRetryConnector{issues: map[string]connector.Issue{tt.issue.ID: cloneIssue(tt.issue)}}
			attempts := &terminalRetryWorkAttemptStore{}
			if tt.completionErr {
				attempts.completionErr = errors.New("store unavailable")
			}
			cfg := normalizeConfig(Config{
				Recovery:              workflowconfig.Recovery{TerminalAttemptRetryLimit: tt.limit},
				ActiveStates:          []string{"Todo", "In Progress"},
				TerminalStates:        []string{"Done"},
				MaxRetryBackoff:       time.Minute,
				FailureRetryBaseDelay: time.Second,
			})
			o := &Orchestrator{cfg: cfg, connector: tracker, workAttempts: attempts}
			if tt.noStore {
				o.workAttempts = nil
			}
			state := newState(cfg)
			state.Running[tt.issue.ID] = Running{
				Issue:         cloneIssue(tt.issue),
				Attempt:       1,
				WorkAttemptID: 42,
				Mode:          runpkg.RunModeImplement,
				StartedAt:     now.Add(-time.Minute),
			}
			state.Claimed[tt.issue.ID] = Claimed{Issue: cloneIssue(tt.issue), ClaimedAt: now.Add(-time.Minute)}
			if tt.noAttemptID {
				running := state.Running[tt.issue.ID]
				running.WorkAttemptID = 0
				state.Running[tt.issue.ID] = running
			}

			tt.result.TurnStarted = true
			o.handleRunResult(t.Context(), &state, runpkg.Completion{
				IssueID:      tt.issue.ID,
				Result:       tt.result,
				Err:          tt.runError,
				CompletedAt:  now,
				RetryAttempt: 2,
				RetryDelay:   time.Minute,
			})

			retry, ok := state.Retry[tt.issue.ID]
			if !ok {
				t.Fatalf("Retry[%q] missing", tt.issue.ID)
			}
			if retry.Issue.State != tt.wantState {
				t.Fatalf("Retry[%q].Issue.State = %q, want %q", tt.issue.ID, retry.Issue.State, tt.wantState)
			}
			if _, claimed := state.Claimed[tt.issue.ID]; claimed {
				t.Fatalf("Claimed[%q] present after terminal completion", tt.issue.ID)
			}
			if got := tracker.transitionStates(); !slices.Equal(got, tt.wantTransitions) {
				t.Fatalf("state transitions = %v, want %v", got, tt.wantTransitions)
			}
			if tt.noStore || tt.noAttemptID {
				if len(attempts.completions) != 0 {
					t.Fatal("unexpected completion persistence")
				}
				return
			}
			if len(attempts.completions) != 1 {
				t.Fatalf("work attempt completions = %d, want 1", len(attempts.completions))
			}
			if got := terminalRetryMetadataPushed(attempts.completions[0].WorkerMetadataJSON); got != tt.result.PullRequestHeadPushed {
				t.Fatalf("persisted work_product_pushed = %v, want %v", got, tt.result.PullRequestHeadPushed)
			}
		})
	}
}

func TestTerminalAttemptRetryableFailureExcludesBackendCapacity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		terminal      store.WorkAttemptTerminalState
		errorClass    string
		wantRetryable bool
	}{
		{name: "service restart remains resumable", terminal: store.WorkAttemptTerminalAbandoned, errorClass: "service_restart", wantRetryable: true},
		{name: "transient provider failure remains resumable", terminal: store.WorkAttemptTerminalFailure, errorClass: backendcapacity.TransientOverloadErrorClass, wantRetryable: true},
		{name: "protocol interruption remains resumable", terminal: store.WorkAttemptTerminalFailure, errorClass: "backend_protocol_error", wantRetryable: true},
		{name: "ordinary failure", terminal: store.WorkAttemptTerminalFailure, errorClass: workAttemptErrorRunner, wantRetryable: true},
		{name: "provider capacity", terminal: store.WorkAttemptTerminalCapacity, errorClass: backendcapacity.ErrorClass},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := terminalAttemptRetryableFailure(telemetry.WorkAttempt{
				TerminalState: string(tt.terminal),
				ErrorClass:    tt.errorClass,
			})
			if got != tt.wantRetryable {
				t.Fatalf("terminalAttemptRetryableFailure() = %v, want %v", got, tt.wantRetryable)
			}
		})
	}
}

func TestHandleRunResultPersistsPublishedPushCommandEvidence(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 28, 15, 0, 0, 0, time.UTC)
	issue := terminalRetryTestIssue("published-post-push-failure")
	tracker := &terminalRetryConnector{issues: map[string]connector.Issue{issue.ID: cloneIssue(issue)}}
	attempts := &terminalRetryWorkAttemptStore{}
	cfg := normalizeConfig(Config{
		ActiveStates:          []string{"Todo", "In Progress"},
		TerminalStates:        []string{"Done"},
		MaxRetryBackoff:       time.Minute,
		FailureRetryBaseDelay: time.Second,
	})
	orchestrator := &Orchestrator{cfg: cfg, connector: tracker, workAttempts: attempts}
	state := newState(cfg)
	state.Running[issue.ID] = Running{
		Issue:         cloneIssue(issue),
		Attempt:       1,
		WorkAttemptID: 2029,
		Mode:          runpkg.RunModeImplement,
		StartedAt:     now.Add(-time.Minute),
	}
	state.Claimed[issue.ID] = Claimed{Issue: cloneIssue(issue), ClaimedAt: now.Add(-time.Minute)}
	exitCode := 19
	command := "git push origin HEAD && exit 19"
	targetRef := &runpkg.DeliverableTargetRefEvidence{
		Remote:                     "origin",
		Ref:                        "refs/heads/detent/published",
		PostCommandLocalHeadSHA:    "new-head",
		PostCommandRemoteHeadSHA:   "new-head",
		PostCommandRemoteRefExists: true,
		InitialObserved:            true,
		PostCommandObserved:        true,
		AdvancedToLocalHead:        true,
	}
	runErr := &runpkg.DeliverableCommandError{
		OperationClass: "post_push",
		Operation:      "post-push command",
		Arguments:      "exit 19",
		ItemID:         "push-item",
		Command:        command,
		Status:         "failed",
		ExitCode:       &exitCode,
		Message:        "later assertion failed",
	}

	orchestrator.handleRunResult(t.Context(), &state, runpkg.Completion{
		IssueID: issue.ID,
		Request: runpkg.RunRequest{Mode: runpkg.RunModeImplement},
		Result: runpkg.RunResult{
			FinalState:            runpkg.FinalStateFailed,
			PullRequestHeadPushed: true,
			DeliverableCommands: []runpkg.DeliverableCommandEvidence{{
				ItemID:         "push-item",
				OperationClass: "post_push",
				Operation:      "post-push command",
				Command:        command,
				Status:         "failed",
				ExitCode:       &exitCode,
				Outcome:        "published_post_push_failed",
				TargetRef:      targetRef,
			}},
		},
		Err:          runErr,
		CompletedAt:  now,
		RetryAttempt: 2,
		RetryDelay:   time.Minute,
	})

	if len(attempts.completions) != 1 {
		t.Fatalf("work attempt completions = %d, want one", len(attempts.completions))
	}
	completion := attempts.completions[0]
	if completion.ErrorClass != workAttemptErrorPostPushCommand {
		t.Fatalf("ErrorClass = %q, want %q", completion.ErrorClass, workAttemptErrorPostPushCommand)
	}
	var metadata struct {
		WorkProductPushed   bool                                `json:"work_product_pushed"`
		DeliverableCommands []runpkg.DeliverableCommandEvidence `json:"deliverable_commands"`
	}
	if err := json.Unmarshal([]byte(completion.WorkerMetadataJSON), &metadata); err != nil {
		t.Fatalf("decode worker metadata: %v", err)
	}
	if !metadata.WorkProductPushed || len(metadata.DeliverableCommands) != 1 {
		t.Fatalf("worker metadata = %#v, want pushed command evidence", metadata)
	}
	persisted := metadata.DeliverableCommands[0]
	if persisted.Command != command || persisted.ExitCode == nil || *persisted.ExitCode != exitCode || persisted.TargetRef == nil || !persisted.TargetRef.AdvancedToLocalHead {
		t.Fatalf("persisted command evidence = %#v", persisted)
	}
	// Post-push command evidence belongs to the deliverable owner; it must
	// not become a project-wide outage that stops unrelated work.
	if len(state.FailureBreaker.Failures) != 0 || state.FailureBreaker.Active() {
		t.Fatalf("published command failure paused unrelated work: %#v", state.FailureBreaker)
	}
}

func TestHandleRunResultReconcilesDeliverableRecoveryExactHead(t *testing.T) {
	t.Parallel()

	const (
		branch  = "detent/acme_widgets_18"
		headSHA = "current-head"
	)
	tests := []struct {
		name                string
		cached              *connector.PullRequest
		lookup              *connector.PullRequest
		created             *connector.PullRequest
		createErr           error
		lookupErrors        []error
		lookupFoundAfter    int
		wantBlocked         bool
		wantReason          string
		wantLookupCalls     int
		wantCreateCalls     int
		wantPRNumber        int
		wantTransitions     []string
		wantRetry           bool
		wantActive          bool
		wantMergedReason    bool
		wantReasonCode      string
		wantForgeWait       bool
		wantDeferred        bool
		wantInstanceFailure bool
		withoutCreator      bool
		errorMessage        string
		commitsAhead        int
		remoteBranch        bool
		joinedErr           error
	}{
		{
			name: "open pull request on exact current head reconciles",
			lookup: &connector.PullRequest{
				Number: 18, BranchName: branch, State: "OPEN", HeadSHA: headSHA,
			},
			wantLookupCalls: 1,
			wantPRNumber:    18,
			commitsAhead:    1,
			remoteBranch:    true,
		},
		{
			name:                "recovery preserves joined workspace failure",
			lookup:              &connector.PullRequest{Number: 18, BranchName: branch, State: "OPEN", HeadSHA: headSHA},
			joinedErr:           runpkg.ErrWorkspacePreparation,
			wantInstanceFailure: true, commitsAhead: 1, remoteBranch: true,
		},
		{
			name:      "recovery preserves joined checkpoint failure",
			lookup:    &connector.PullRequest{Number: 18, BranchName: branch, State: "OPEN", HeadSHA: headSHA},
			joinedErr: errors.New("checkpoint persistence failed"),
			wantRetry: true, commitsAhead: 1, remoteBranch: true,
		},
		{
			name:      "recovery preserves joined lease failure",
			lookup:    &connector.PullRequest{Number: 18, BranchName: branch, State: "OPEN", HeadSHA: headSHA},
			joinedErr: errors.New("native execution lease lost"),
			wantRetry: true, commitsAhead: 1, remoteBranch: true,
		},
		{
			name:      "recovery preserves joined session failure",
			lookup:    &connector.PullRequest{Number: 18, BranchName: branch, State: "OPEN", HeadSHA: headSHA},
			joinedErr: errors.New("session persistence failed"),
			wantRetry: true, commitsAhead: 1, remoteBranch: true,
		},
		{
			name: "no pull request opens draft",
			created: &connector.PullRequest{
				Number: 19, BranchName: branch, State: "OPEN", HeadSHA: headSHA, Draft: true,
			},
			wantLookupCalls: 3,
			wantCreateCalls: 1,
			wantPRNumber:    19,
			wantTransitions: []string{},
			wantActive:      true,
			commitsAhead:    1,
			remoteBranch:    true,
		},
		{
			name:            "draft creation outage waits on forge",
			createErr:       errors.New("HTTP 503: unavailable"),
			wantLookupCalls: 3,
			wantCreateCalls: 1,
			wantForgeWait:   true,
			commitsAhead:    1,
			remoteBranch:    true,
		},
		{
			name:            "draft creation rate limit waits on forge",
			createErr:       connector.NewRetryableError("github rate limited"),
			wantLookupCalls: 3,
			wantCreateCalls: 1,
			wantDeferred:    true,
			commitsAhead:    1,
			remoteBranch:    true,
		},
		{
			name:            "draft creation credentials wait on forge",
			createErr:       errors.New("github authentication failed"),
			wantLookupCalls: 3,
			wantCreateCalls: 1,
			wantForgeWait:   true,
			commitsAhead:    1,
			remoteBranch:    true,
		},
		{
			name:            "unsupported draft creation does not retry",
			withoutCreator:  true,
			wantBlocked:     true,
			wantReason:      "draft pull request creation unavailable",
			wantLookupCalls: 3,
			commitsAhead:    1,
			remoteBranch:    true,
		},
		{
			name: "credential failure still reconciles existing pull request",
			lookup: &connector.PullRequest{
				Number: 18, BranchName: branch, State: "OPEN", HeadSHA: headSHA,
			},
			errorMessage:    "GitHub credentials are unavailable",
			wantLookupCalls: 1,
			wantPRNumber:    18,
			commitsAhead:    1,
			remoteBranch:    true,
		},
		{
			name:            "zero commits ahead parks without pull request lookup",
			wantBlocked:     true,
			wantReason:      "no_commits_to_deliver",
			wantReasonCode:  noCommitsToDeliverReason,
			wantLookupCalls: 0,
		},
		{
			name:            "deleted remote branch returns to Rework",
			wantLookupCalls: 0,
			wantTransitions: []string{autoPromoteReworkState},
			wantRetry:       true,
			commitsAhead:    1,
		},
		{
			name: "transient not found retries then reconciles",
			lookup: &connector.PullRequest{
				Number: 18, BranchName: branch, State: "OPEN", HeadSHA: headSHA,
			},
			lookupFoundAfter: 3,
			wantLookupCalls:  3,
			wantPRNumber:     18,
			commitsAhead:     1,
			remoteBranch:     true,
		},
		{
			name: "merged pull request routes to merged deliverable reconciliation",
			lookup: &connector.PullRequest{
				Number: 18, BranchName: branch, State: "MERGED", HeadSHA: headSHA,
				CIStatus: "success", CheckRunCount: 1,
			},
			wantLookupCalls:  1,
			wantPRNumber:     18,
			wantMergedReason: true,
			commitsAhead:     1,
			remoteBranch:     true,
		},
		{
			name: "closed unmerged pull request parks accurately",
			lookup: &connector.PullRequest{
				Number: 18, BranchName: branch, State: "CLOSED", HeadSHA: headSHA,
			},
			wantBlocked:     true,
			wantReason:      "closed without merge",
			wantLookupCalls: 1,
			commitsAhead:    1,
			remoteBranch:    true,
		},
		{
			name: "lookup unavailable retries then parks",
			lookupErrors: []error{
				errors.New("lookup unavailable 1"),
				errors.New("lookup unavailable 2"),
				errors.New("lookup unavailable 3"),
			},
			wantBlocked:     true,
			wantReason:      "PR lookup unavailable",
			wantLookupCalls: 3,
			commitsAhead:    1,
			remoteBranch:    true,
		},
		{
			name: "stale cached hydration is not trusted",
			cached: &connector.PullRequest{
				Number: 18, BranchName: branch, State: "OPEN", HeadSHA: headSHA,
			},
			wantLookupCalls: 3,
			wantCreateCalls: 1,
			wantRetry:       true,
			commitsAhead:    1,
			remoteBranch:    true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			now := time.Date(2026, 8, 12, 18, 0, 0, 0, time.UTC)
			issue := terminalRetryTestIssue(tt.name)
			issue.BranchName = branch
			issue.PRRepository = "acme/widgets"
			issue.PullRequest = tt.cached
			issue.Comments = []connector.IssueComment{{
				Body: "## Codex Workpad\n\n```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```",
			}}
			tracker := &terminalRetryConnector{
				issues:           map[string]connector.Issue{issue.ID: cloneIssue(issue)},
				lookup:           tt.lookup,
				created:          tt.created,
				createErr:        tt.createErr,
				lookupErrors:     append([]error(nil), tt.lookupErrors...),
				lookupFoundAfter: tt.lookupFoundAfter,
			}
			attempts := &terminalRetryWorkAttemptStore{}
			cfg := normalizeConfig(Config{ActiveStates: []string{"Todo", "In Progress", "Rework"}, TerminalStates: []string{"Done"}})
			var recoveryConnector connector.Connector = tracker
			if tt.withoutCreator {
				recoveryConnector = struct {
					connector.Connector
					connector.PullRequestHeadLookup
				}{Connector: tracker, PullRequestHeadLookup: tracker}
			}
			o := &Orchestrator{
				cfg: cfg, connector: recoveryConnector, workAttempts: attempts,
				recoveryInspector:       staticBlockedRecoveryInspector{snapshot: runpkg.BlockedRecoverySnapshot{HeadSHA: headSHA, WorkspacePresent: true, WorkspaceStatus: "present", Health: "ready"}},
				deliverableRecoveryWait: func(context.Context, time.Duration) bool { return true },
			}
			state := newState(cfg)
			state.Running[issue.ID] = Running{
				Issue: issue, Attempt: 1, WorkAttemptID: 42, Mode: runpkg.RunModeImplement,
				StartedAt: now.Add(-time.Minute), WorkProductPushed: true, WorkspacePath: "/work/" + branch,
				DiffStats: DiffStats{
					Status: "clean", HeadSHA: headSHA,
					DeliveryStateChecked: true, CommitsAhead: tt.commitsAhead, RemoteBranchExists: tt.remoteBranch,
				},
			}
			state.Claimed[issue.ID] = Claimed{Issue: issue, ClaimedAt: now.Add(-time.Minute)}
			errorMessage := tt.errorMessage
			if errorMessage == "" {
				errorMessage = "HTTP 503: unavailable"
			}
			commandErr := &runpkg.DeliverableCommandError{
				OperationClass: "pull_request", Operation: "codex_apps/github.create_pull_request", Arguments: `{"head":"` + branch + `"}`,
				Status: "failed", Message: errorMessage,
			}

			runErr := errors.Join(&runpkg.DeliverableRecoveryError{Branch: branch, Err: commandErr}, tt.joinedErr)

			o.handleRunResult(t.Context(), &state, runpkg.Completion{
				IssueID: issue.ID,
				Result: runpkg.RunResult{
					FinalState: runpkg.FinalStateNeedsHumanAttention, PullRequestHeadPushed: true,
					DiffStats: runpkg.DiffStats{
						Status: "clean", HeadSHA: headSHA,
						DeliveryStateChecked: true, CommitsAhead: tt.commitsAhead, RemoteBranchExists: tt.remoteBranch,
					},
				},
				Err:         runErr,
				CompletedAt: now,
			})

			if tracker.lookupCalls != tt.wantLookupCalls {
				t.Fatalf("lookup calls = %d, want %d", tracker.lookupCalls, tt.wantLookupCalls)
			}
			if tt.wantLookupCalls > 0 && (tracker.lookupRepository != "acme/widgets" || tracker.lookupBranch != branch || tracker.lookupHeadSHA != headSHA) {
				t.Fatalf(
					"lookup target = %q/%q@%q, want acme/widgets/%s@%s",
					tracker.lookupRepository,
					tracker.lookupBranch,
					tracker.lookupHeadSHA,
					branch,
					headSHA,
				)
			}
			if tracker.createCalls != tt.wantCreateCalls {
				t.Fatalf("create calls = %d, want %d", tracker.createCalls, tt.wantCreateCalls)
			}
			if tt.wantCreateCalls > 0 {
				if tracker.createRepository != "acme/widgets" || tracker.createBranch != branch || tracker.createTitle != issue.Title {
					t.Fatalf("create target = (%q, %q, %q), want (%q, %q, %q)", tracker.createRepository, tracker.createBranch, tracker.createTitle, "acme/widgets", branch, issue.Title)
				}
				if !strings.Contains(tracker.createBody, "Fixes digitaldrywood/detent#1432-") || !strings.Contains(tracker.createBody, "```detent-origin") {
					t.Fatalf("create body = %q, want issue reference and provenance stamp", tracker.createBody)
				}
			}
			blocked, ok := state.Blocked[issue.ID]
			if ok {
				t.Fatalf("machine failure entered Blocked: %#v", blocked)
			}
			if tt.wantBlocked {
				if _, ok := state.Retry[issue.ID]; !ok || len(state.Blocked) != 0 || slices.Contains(tracker.transitionStates(), blockedStatusState) {
					t.Fatal("delivery failure did not remain retryable")
				}
				return
			}
			if tt.wantTransitions != nil && !slices.Equal(tracker.transitionStates(), tt.wantTransitions) {
				t.Fatalf("state transitions = %v, want %v", tracker.transitionStates(), tt.wantTransitions)
			}
			if tt.wantForgeWait {
				retry, retrying := state.Retry[issue.ID]
				if !retrying || !retry.ForgeUnavailable || retry.Attempt != 1 || retry.ForgeRetry == nil {
					t.Fatalf("Retry[%q] = %#v, want same-attempt forge wait", issue.ID, retry)
				}
				if retry.ForgeRetry.Branch != branch || !retry.ForgeRetry.WorkProductPushed {
					t.Fatalf("ForgeRetry = %#v, want pushed branch %q preserved", retry.ForgeRetry, branch)
				}
				if len(attempts.completions) != 1 || attempts.completions[0].TerminalState != store.WorkAttemptTerminalCapacity || attempts.completions[0].ErrorClass != forgeUnavailableErrorClass {
					t.Fatalf("work attempt completions = %#v, want forge capacity wait", attempts.completions)
				}
				return
			}
			if tt.wantDeferred {
				if _, deferred := state.deferredCompletions[issue.ID]; !deferred {
					t.Fatalf("deferredCompletions[%q] missing after infrastructure failure", issue.ID)
				}
				if retry := state.Retry[issue.ID]; !retry.CompletionDeferred || retry.Attempt != 1 {
					t.Fatalf("Retry[%q] = %#v, want same-attempt durable completion deferral", issue.ID, retry)
				}
				if len(attempts.completions) != 0 {
					t.Fatalf("work attempt completions = %#v, want active deferred attempt", attempts.completions)
				}
				return
			}
			if tt.joinedErr != nil {
				if len(attempts.completions) != 1 || attempts.completions[0].TerminalState != store.WorkAttemptTerminalFailure || !strings.Contains(attempts.completions[0].ErrorMessage, tt.joinedErr.Error()) || attempts.completions[0].ErrorClass == permissionWaitReason {
					t.Fatalf("joined failure outcome = %#v", attempts.completions)
				}
			}
			if tt.wantInstanceFailure {
				completion := attempts.completions[0]
				if completion.ErrorClass != workAttemptErrorWorkspace || state.FailureBreaker.Active() || len(state.FailureBreaker.Failures) != 0 {
					t.Fatalf("workspace failure lost its instance owner: completion=%#v breaker=%#v", completion, state.FailureBreaker)
				}
				var metadata struct {
					WorkProductPushed bool `json:"work_product_pushed"`
				}
				if err := json.Unmarshal([]byte(completion.WorkerMetadataJSON), &metadata); err != nil || !metadata.WorkProductPushed {
					t.Fatalf("workspace failure lost pushed work evidence: metadata=%s error=%v", completion.WorkerMetadataJSON, err)
				}
				if _, retried := state.Retry[issue.ID]; !retried || len(state.PriorAttempts) != 0 || len(state.Claimed) != 0 || len(state.Completed) != 0 || len(tracker.transitionStates()) != 0 {
					t.Fatalf("unexpected state after completion: retry=%+v blocked=%+v breaker=%+v", state.Retry, state.Blocked, state.FailureBreaker)
				}
				return
			}
			if tt.wantRetry {
				if _, retrying := state.Retry[issue.ID]; !retrying {
					t.Fatalf("Retry[%q] missing after machine-recoverable delivery failure", issue.ID)
				}
				if len(attempts.completions) != 1 || attempts.completions[0].ErrorClass == deliverableRecoveryNeedsHumanReason {
					t.Fatalf("work attempt completions = %#v, want non-human recovery failure", attempts.completions)
				}
				if _, completed := state.Completed[issue.ID]; completed {
					t.Fatalf("Completed[%q] present during delivery recovery retry", issue.ID)
				}
				return
			}
			if tt.wantActive {
				if _, retrying := state.Retry[issue.ID]; !retrying {
					t.Fatalf("Retry[%q] missing after draft recovery continuation", issue.ID)
				}
			}
			if len(attempts.completions) != 1 || attempts.completions[0].TerminalState != store.WorkAttemptTerminalSuccess {
				t.Fatalf("work attempt completions = %#v, want successful reconciliation", attempts.completions)
			}
			if tt.wantMergedReason {
				record := implementProgressRecordFromCompletion(t, attempts.completions[0])
				if record.Reason != implementMergedCompletionReason {
					t.Fatalf("completion reason = %q, want %q", record.Reason, implementMergedCompletionReason)
				}
				if got := tracker.transitionStates(); !slices.Equal(got, []string{"Done"}) || len(state.Retry) != 0 || len(state.Claimed) != 0 || len(state.Completed) != 0 {
					t.Fatalf("merged recovery: transitions=%v retry=%d claimed=%d completed=%d, want Done without continuation", got, len(state.Retry), len(state.Claimed), len(state.Completed))
				}
				return
			}
			completed := state.Completed[issue.ID]
			if completed.Issue.PullRequest == nil || completed.Issue.PullRequest.Number != tt.wantPRNumber {
				t.Fatalf("Completed[%q].Issue.PullRequest = %#v, want reconciled PR %d", issue.ID, completed.Issue.PullRequest, tt.wantPRNumber)
			}
			if tt.wantActive && completed.FinalState != issue.State {
				t.Fatalf("Completed[%q].FinalState = %q, want active state %q", issue.ID, completed.FinalState, issue.State)
			}
		})
	}
}

func TestDeliverableRecoveryCompletionDeferralSurvivesRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	t.Parallel()

	const (
		branch  = "detent/acme_widgets_19"
		headSHA = "deferred-head"
	)
	now := time.Date(2026, 9, 13, 16, 0, 0, 0, time.UTC)
	issue := terminalRetryTestIssue("deferred-recovery")
	issue.BranchName = branch
	issue.PRRepository = "acme/widgets"
	tracker := &terminalRetryConnector{
		issues:    map[string]connector.Issue{issue.ID: cloneIssue(issue)},
		createErr: connector.NewRetryableError("github rate limited"),
	}
	cfg := completionDeferralConfig()
	path := filepath.Join(t.TempDir(), "attempts.db")
	runtimeStore := openCompletionDeferralStoreWithoutCleanup(t, path)
	attemptID := startCompletionDeferralAttempt(t, runtimeStore, issue, now)
	orch := &Orchestrator{
		cfg: cfg, connector: tracker, workAttempts: runtimeStore, now: func() time.Time { return now },
		deliverableRecoveryWait: func(context.Context, time.Duration) bool { return true },
	}
	state := newState(cfg)
	running := completionDeferralRunning(issue, attemptID, now)
	running.WorkProductPushed = true
	running.DiffStats = DiffStats{Status: "clean", HeadSHA: headSHA, DeliveryStateChecked: true, CommitsAhead: 1, RemoteBranchExists: true}
	state.Running[issue.ID] = running
	state.Claimed[issue.ID] = Claimed{Issue: cloneIssue(issue), ClaimedAt: now.Add(-time.Minute)}
	event := completionDeferralEvent(issue, attemptID, now)
	event.Result.FinalState = runpkg.FinalStateNeedsHumanAttention
	event.Result.PullRequestHeadPushed = true
	event.Result.DiffStats = running.DiffStats
	event.Err = &runpkg.DeliverableRecoveryError{Branch: branch, Err: &runpkg.DeliverableCommandError{OperationClass: "pull_request", Operation: "gh pr create", Message: "gh pr create failed"}}
	for _, test := range []struct {
		name         string
		joinedErr    error
		legacy       bool
		legacyTyped  bool
		invalidClass bool
		invalidScope bool
		wantPure     bool
	}{
		{name: "pure typed receipt", wantPure: true},
		{name: "legacy v35 typed receipt", legacyTyped: true},
		{name: "unknown availability class", invalidClass: true},
		{name: "nonempty availability without class", invalidScope: true},
		{name: "workspace join", joinedErr: runpkg.ErrWorkspacePreparation},
		{name: "wrapped workspace join", joinedErr: fmt.Errorf("prepare checkout: %w", runpkg.ErrWorkspacePreparation)},
		{name: "checkpoint join", joinedErr: errors.New("checkpoint persistence failed")},
		{name: "lease join", joinedErr: errors.New("native execution lease lost")},
		{name: "session join", joinedErr: errors.New("session persistence failed")},
		{name: "legacy plain receipt", legacy: true},
		{name: "legacy mixed receipt", joinedErr: runpkg.ErrWorkspacePreparation, legacy: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := event
			input.Err = errors.Join(event.Err, test.joinedErr)
			record := newDeferredCompletion(input, running, tracker.createErr, now)
			if test.legacy {
				record.DeliverableRecovery = &deferredDeliverableRecovery{Branch: branch, Cause: "gh pr create failed"}
			}
			if test.legacy || test.legacyTyped {
				record.ForgeAvailability = nil
			}
			if test.invalidClass {
				record.ForgeAvailability.ErrorClass = "unknown"
			}
			if test.invalidScope {
				record.ForgeAvailability.Host = "forge.example.test"
			}
			data, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			var decoded deferredCompletion
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatal(err)
			}
			replayed := decoded.completion()
			if _, pure := runpkg.PullRequestDeliverableFailure(replayed.Err); pure != test.wantPure {
				t.Fatalf("replayed pure delivery = %v, want %v", pure, test.wantPure)
			}
			if test.wantPure && (decoded.ForgeAvailability == nil || *decoded.ForgeAvailability != (forgeWaitMetadata{})) {
				t.Fatal("new bare delivery receipt lost explicit availability absence")
			}
			if decoded.Error != input.Err.Error() {
				t.Fatalf("stored full error = %q, want %q", decoded.Error, input.Err.Error())
			}
			if !test.wantPure && replayed.Err.Error() != input.Err.Error() {
				t.Fatalf("replayed full error = %q, want %q", replayed.Err.Error(), input.Err.Error())
			}
			if got, want := errors.Is(replayed.Err, runpkg.ErrWorkspacePreparation), errors.Is(input.Err, runpkg.ErrWorkspacePreparation); got != want {
				t.Fatalf("replayed workspace preparation = %v, want %v", got, want)
			}
			if test.joinedErr != nil && !test.legacy && decoded.DeliverableRecovery != nil {
				t.Fatal("mixed error acquired a typed recovery receipt")
			}
		})
	}

	orch.handleRunResult(t.Context(), &state, event)
	if _, deferred := state.deferredCompletions[issue.ID]; !deferred {
		t.Fatal("draft creation rate limit did not defer completion")
	}
	if err := runtimeStore.Close(); err != nil {
		t.Fatal(err)
	}

	runtimeStore = openCompletionDeferralStoreWithoutCleanup(t, path)
	t.Cleanup(func() {
		if err := runtimeStore.Close(); err != nil {
			t.Error(err)
		}
	})
	tracker.createErr = nil
	tracker.created = &connector.PullRequest{Number: 19, BranchName: branch, State: "OPEN", HeadSHA: headSHA, Draft: true}
	restartedAt := now.Add(time.Second)
	restarted := &Orchestrator{
		cfg: cfg, connector: tracker, workAttempts: runtimeStore, now: func() time.Time { return restartedAt },
		deliverableRecoveryWait: func(context.Context, time.Duration) bool { return true },
	}
	recovered := newState(cfg)
	restarted.recoverDurableWorkAttempts(t.Context(), &recovered, restartedAt)
	record, deferred := recovered.deferredCompletions[issue.ID]
	if !deferred {
		t.Fatal("deliverable recovery completion deferral was not restored")
	}
	var restoredRecovery *runpkg.DeliverableRecoveryError
	if _, pure := runpkg.PullRequestDeliverableFailure(record.completion().Err); !pure {
		t.Fatal("restored completion lost pure typed delivery authority")
	}
	if !errors.As(record.completion().Err, &restoredRecovery) || restoredRecovery == nil || restoredRecovery.Branch != branch {
		t.Fatalf("restored completion error = %#v, want deliverable recovery for %q", record.completion().Err, branch)
	}
	if !restarted.retryDeferredCompletions(t.Context(), &recovered, now.Add(cfg.PollInterval)) {
		t.Fatal("restored deliverable recovery remained deferred after forge recovery")
	}
	if tracker.createCalls != 2 {
		t.Fatalf("draft creation calls = %d, want failed call before restart and successful retry", tracker.createCalls)
	}
	attempt, err := runtimeStore.WorkAttempt(t.Context(), attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.Status != store.WorkAttemptStatusTerminal || attempt.TerminalState != store.WorkAttemptTerminalSuccess {
		t.Fatalf("restored work attempt = %#v, want successful terminal reconciliation", attempt)
	}
}

func TestReconcileTerminalAttemptRetryStatesDemotesRecoveredEmptyAttempt(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 18, 13, 0, 0, 0, time.UTC)
	empty := terminalRetryTestIssue("service-restart-empty")
	pushed := terminalRetryTestIssue("service-restart-pushed")
	resumedRework := terminalRetryTestIssue("service-restart-rework")
	planned := terminalRetryTestIssue("operator-recovered-plan")
	successfulPlan := terminalRetryTestIssue("successful-plan")
	tracker := &terminalRetryConnector{issues: map[string]connector.Issue{
		empty.ID:          cloneIssue(empty),
		pushed.ID:         cloneIssue(pushed),
		resumedRework.ID:  cloneIssue(resumedRework),
		planned.ID:        cloneIssue(planned),
		successfulPlan.ID: cloneIssue(successfulPlan),
	}}
	cfg := normalizeConfig(Config{ActiveStates: []string{"Todo", "In Progress", "Rework"}, TerminalStates: []string{"Done"}, Recovery: workflowconfig.Recovery{TerminalAttemptRetryLimit: new(0)}})
	o := &Orchestrator{cfg: cfg, connector: tracker}
	state := newState(cfg)
	state.WorkAttempts = []telemetry.WorkAttempt{
		{
			AttemptID: 5, IssueID: resumedRework.ID, Identifier: resumedRework.Identifier,
			Status: string(store.WorkAttemptStatusTerminal), TerminalState: string(store.WorkAttemptTerminalAbandoned),
			ErrorClass: "service_restart", CompletedAt: timePointer(now.Add(-time.Minute)),
			WorkerMetadataJSON: `{"dispatch_source_state":"Rework"}`,
		},
		{
			AttemptID: 4, IssueID: planned.ID, Status: string(store.WorkAttemptStatusTerminal),
			TerminalState: string(store.WorkAttemptTerminalAbandoned), ErrorClass: "operator_abandoned",
			CompletedAt: timePointer(now.Add(-time.Minute)), WorkerMetadataJSON: `{"run_mode":"plan"}`,
		},
		{
			AttemptID: 3, IssueID: successfulPlan.ID, Status: string(store.WorkAttemptStatusTerminal),
			TerminalState: string(store.WorkAttemptTerminalSuccess), CompletedAt: timePointer(now.Add(-time.Minute)),
			WorkerMetadataJSON: `{"run_mode":"plan"}`,
		},
		{
			AttemptID:          2,
			IssueID:            pushed.ID,
			Identifier:         pushed.Identifier,
			Status:             string(store.WorkAttemptStatusTerminal),
			TerminalState:      string(store.WorkAttemptTerminalAbandoned),
			ErrorClass:         "service_restart",
			CompletedAt:        timePointer(now.Add(-time.Minute)),
			WorkerMetadataJSON: `{"dispatch_source_state":"Rework","work_product_pushed":true}`,
		},
		{
			AttemptID:     1,
			IssueID:       empty.ID,
			Identifier:    empty.Identifier,
			Status:        string(store.WorkAttemptStatusTerminal),
			TerminalState: string(store.WorkAttemptTerminalAbandoned),
			ErrorClass:    "service_restart",
			CompletedAt:   timePointer(now.Add(-2 * time.Minute)),
		},
	}

	transitions := o.reconcileTerminalAttemptRetryStates(t.Context(), &state, []connector.Issue{pushed, empty, planned, successfulPlan, resumedRework}, now)

	if len(transitions) != 2 || transitions[0].ID != empty.ID || transitions[0].State != "Todo" || transitions[1].ID != resumedRework.ID || transitions[1].State != "Rework" {
		t.Fatalf("transitions = %#v, want empty legacy attempt in Todo and recorded source restored to Rework", transitions)
	}
	if got := tracker.transitionStates(); !slices.Equal(got, []string{"Todo", "Rework"}) {
		t.Fatalf("state transitions = %v, want [Todo Rework]", got)
	}
	if len(state.Blocked) != 0 || tracker.issues[pushed.ID].State != "In Progress" {
		t.Fatalf("restart entered failure park or moved pushed work: blocked=%+v pushed=%s", state.Blocked, tracker.issues[pushed.ID].State)
	}
	o.cfg.Plan = gate.PlanConfig{Enabled: true, Review: gate.PlanReviewAutomated}
	for _, issue := range []connector.Issue{planned, successfulPlan} {
		if mode := o.dispatchMode(t.Context(), &state, tracker.issues[issue.ID]); mode != RunModeImplement {
			t.Fatalf("recovered plan dispatched mode %s, want implementation", mode)
		}
	}
}

func TestReconcileTerminalAttemptRetryStatesHandlesGitHubRESTCapacityCompatibility(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	resetAt := now.Add(time.Hour)
	retryAt := resetAt.Add(backendCapacityResetJitter)
	durableMetadata := `{"github_rest_wait":{"consumer":"shared_pool","credential_identity":"github-rest:worker","remaining":1009,"limit":5000,"reserve":1250,"reset_at":"` + resetAt.Format(time.RFC3339) + `","retry_at":"` + retryAt.Format(time.RFC3339) + `"}}`
	tests := []struct {
		name         string
		metadata     string
		errorClass   string
		foreignClaim bool
		ownClaim     bool
		unhydrated   bool
		limit        *int
		wantState    string
		wantDemotion bool
	}{
		{name: "legacy metadata-less attempt", wantState: "Todo", wantDemotion: true},
		{name: "legacy zero limit", limit: new(0), wantState: "Todo", wantDemotion: true},
		{name: "legacy recorded Rework source", metadata: `{"dispatch_source_state":"Rework"}`, limit: new(0), wantState: "Rework", wantDemotion: true},
		{name: "legacy pushed product", metadata: `{"dispatch_source_state":"Rework","work_product_pushed":true}`, limit: new(0), wantState: "In Progress"},
		{name: "legacy foreign claim", foreignClaim: true, wantState: "In Progress"},
		{name: "legacy unhydrated PR", unhydrated: true, wantState: "In Progress"},
		{name: "service restart foreign claim", errorClass: "service_restart", foreignClaim: true, wantState: "In Progress"},
		{name: "pre-turn foreign claim", errorClass: workAttemptErrorWorkspace, foreignClaim: true, wantState: "In Progress"},
		{name: "service restart own claim", errorClass: "service_restart", ownClaim: true, wantState: "Todo", wantDemotion: true},
		{name: "durable wait metadata", metadata: durableMetadata, wantState: "In Progress"},
		{name: "durable wait zero limit", metadata: durableMetadata, limit: new(0), wantState: "In Progress"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			issue := terminalRetryTestIssue(strings.ReplaceAll(tt.name, " ", "-"))
			if tt.foreignClaim {
				issue.Assignees = []string{"other-worker"}
				issue.Fields["Lease"] = formatClaimTime(now.Add(-30 * time.Second))
			}
			if tt.ownClaim {
				issue.Assignees = []string{"detent-worker"}
				issue.Fields["Lease"] = formatClaimTime(now.Add(-30 * time.Second))
			}
			if tt.unhydrated {
				issue.PullRequest = &connector.PullRequest{HydrationUnavailableReason: connector.PullRequestHydrationReasonRateLimited}
			}
			tracker := &terminalRetryConnector{issues: map[string]connector.Issue{issue.ID: cloneIssue(issue)}}
			cfg := normalizeConfig(Config{
				ActiveStates: []string{"Todo", "In Progress", "Rework"}, TerminalStates: []string{"Done"},
				Recovery: workflowconfig.Recovery{TerminalAttemptRetryLimit: tt.limit},
				Claiming: ClaimingConfig{Enabled: tt.foreignClaim || tt.ownClaim, AssigneeLogin: "detent-worker", LeaseField: "Lease", LeaseTTL: time.Minute},
			})
			o := &Orchestrator{cfg: cfg, connector: tracker}
			state := newState(cfg)
			errorClass := tt.errorClass
			if errorClass == "" {
				errorClass = githubRESTCapacityError
			}
			state.WorkAttempts = []telemetry.WorkAttempt{{
				AttemptID:          1,
				IssueID:            issue.ID,
				Identifier:         issue.Identifier,
				Status:             string(store.WorkAttemptStatusTerminal),
				TerminalState:      string(store.WorkAttemptTerminalCapacity),
				ErrorClass:         errorClass,
				CompletedAt:        timePointer(now.Add(-time.Minute)),
				WorkerMetadataJSON: tt.metadata,
			}}

			transitions := o.reconcileTerminalAttemptRetryStates(t.Context(), &state, []connector.Issue{issue}, now)

			if got := len(transitions) == 1; got != tt.wantDemotion {
				t.Fatalf("demoted = %v, want %v: %#v", got, tt.wantDemotion, transitions)
			}
			if tt.wantDemotion && transitions[0].State != tt.wantState {
				t.Fatalf("transition state = %q, want %q", transitions[0].State, tt.wantState)
			}
			if got := tracker.transitionStates(); tt.wantDemotion && !slices.Equal(got, []string{tt.wantState}) {
				t.Fatalf("state transitions = %v, want [%s]", got, tt.wantState)
			} else if !tt.wantDemotion && len(got) != 0 {
				t.Fatalf("state transitions = %v, want none", got)
			}
			if len(state.Blocked) != 0 || tracker.issues[issue.ID].State != tt.wantState {
				t.Fatalf("capacity restoration entered issue failure park or wrong lane: blocked=%+v lane=%s", state.Blocked, tracker.issues[issue.ID].State)
			}
		})
	}
}

func TestReconcileTerminalAttemptRetryStatesRespectsLiveForeignClaim(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 18, 13, 30, 0, 0, time.UTC)
	foreign := terminalRetryTestIssue("foreign-claim")
	foreign.Assignees = []string{"other-worker"}
	foreign.Fields["Lease"] = formatClaimTime(now.Add(-30 * time.Second))
	tracker := &terminalRetryConnector{issues: map[string]connector.Issue{foreign.ID: cloneIssue(foreign)}}
	cfg := normalizeConfig(Config{
		ActiveStates:   []string{"Todo", "In Progress"},
		TerminalStates: []string{"Done"},
		Claiming: ClaimingConfig{
			Enabled:       true,
			AssigneeLogin: "detent-worker",
			LeaseField:    "Lease",
			LeaseTTL:      time.Minute,
		},
	})
	o := &Orchestrator{cfg: cfg, connector: tracker}
	state := newState(cfg)
	state.WorkAttempts = []telemetry.WorkAttempt{{
		AttemptID:     1,
		IssueID:       foreign.ID,
		Identifier:    foreign.Identifier,
		Status:        string(store.WorkAttemptStatusTerminal),
		TerminalState: string(store.WorkAttemptTerminalFailure),
		ErrorClass:    "runner_error",
		CompletedAt:   timePointer(now.Add(-time.Minute)),
	}}

	transitions := o.reconcileTerminalAttemptRetryStates(t.Context(), &state, []connector.Issue{foreign}, now)

	if len(transitions) != 0 {
		t.Fatalf("transitions = %#v, want active foreign claim left In Progress", transitions)
	}
	if got := tracker.transitionStates(); len(got) != 0 {
		t.Fatalf("state transitions = %v, want none", got)
	}
}

func TestTerminalAttemptDemotionLetsUrgentTodoRankFirst(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 18, 14, 0, 0, 0, time.UTC)
	retrying := terminalRetryTestIssue("retrying")
	urgent := terminalRetryTestIssue("urgent")
	urgent.State = "Todo"
	urgentPriority := 1
	urgent.Priority = &urgentPriority
	tracker := &terminalRetryConnector{issues: map[string]connector.Issue{retrying.ID: cloneIssue(retrying), urgent.ID: cloneIssue(urgent)}}
	cfg := normalizeConfig(Config{
		ActiveStates:            []string{"Todo", "In Progress"},
		TerminalStates:          []string{"Done"},
		DispatchPriorityByState: []string{"In Progress", "Todo"},
	})
	o := &Orchestrator{cfg: cfg, connector: tracker}
	state := newState(cfg)
	state.WorkAttempts = []telemetry.WorkAttempt{{
		AttemptID:     1,
		IssueID:       retrying.ID,
		Identifier:    retrying.Identifier,
		Status:        string(store.WorkAttemptStatusTerminal),
		TerminalState: string(store.WorkAttemptTerminalFailure),
		ErrorClass:    "runner_error",
		CompletedAt:   timePointer(now.Add(-time.Minute)),
	}}

	transitions := o.reconcileTerminalAttemptRetryStates(t.Context(), &state, []connector.Issue{retrying, urgent}, now)
	issues := overlayIssueStateSnapshots([]connector.Issue{retrying, urgent}, transitions)
	sortIssuesForDispatch(issues, cfg.DispatchPriorityByState, cfg.DispatchPriorityByLabel, false)

	if len(issues) != 2 || issues[0].ID != urgent.ID {
		t.Fatalf("dispatch order = %v, want urgent Todo first", []string{issues[0].ID, issues[1].ID})
	}
}

func terminalRetryTestIssue(suffix string) connector.Issue {
	issue := connector.NewIssue()
	issue.ID = "issue-" + suffix
	issue.Identifier = "digitaldrywood/detent#1432-" + suffix
	issue.Title = "Terminal retry " + suffix
	issue.State = "In Progress"
	return issue
}

func terminalRetryTestIssueWithPullRequest(suffix string) connector.Issue {
	issue := terminalRetryTestIssue(suffix)
	prNumber := 1432
	issue.PRNumber = &prNumber
	issue.PullRequest = &connector.PullRequest{Number: prNumber, State: "OPEN"}
	return issue
}

type terminalRetryConnector struct {
	updateErrors     map[string]error
	issues           map[string]connector.Issue
	transitions      []string
	comments         []string
	lookup           *connector.PullRequest
	lookupErrors     []error
	lookupCalls      int
	lookupFoundAfter int
	lookupRepository string
	lookupBranch     string
	lookupHeadSHA    string
	created          *connector.PullRequest
	createErr        error
	createCalls      int
	createRepository string
	createBranch     string
	createTitle      string
	createBody       string
}

func (c *terminalRetryConnector) Name() string { return "terminal-retry" }

func (c *terminalRetryConnector) FetchCandidateIssues(context.Context) ([]connector.Issue, error) {
	return nil, nil
}

func (c *terminalRetryConnector) FetchIssuesByStates(context.Context, []string) ([]connector.Issue, error) {
	return nil, nil
}

func (c *terminalRetryConnector) FetchIssueStatesByIDs(_ context.Context, ids []string) ([]connector.Issue, error) {
	issues := make([]connector.Issue, 0, len(ids))
	for _, id := range ids {
		if issue, ok := c.issues[id]; ok {
			issues = append(issues, cloneIssue(issue))
		}
	}
	return issues, nil
}

func (c *terminalRetryConnector) FetchIssueComments(_ context.Context, issue connector.Issue) ([]connector.IssueComment, error) {
	return append([]connector.IssueComment(nil), c.issues[issue.ID].Comments...), nil
}

func (c *terminalRetryConnector) LookupPullRequestByHead(_ context.Context, repository string, branch string, headSHA string) (connector.PullRequest, bool, error) {
	c.lookupCalls++
	c.lookupRepository = repository
	c.lookupBranch = branch
	c.lookupHeadSHA = headSHA
	if c.lookupCalls <= len(c.lookupErrors) {
		return connector.PullRequest{}, false, c.lookupErrors[c.lookupCalls-1]
	}
	if c.lookup == nil {
		return connector.PullRequest{}, false, nil
	}
	if c.lookupFoundAfter > c.lookupCalls {
		return connector.PullRequest{}, false, nil
	}
	pullRequest := *c.lookup
	return pullRequest, true, nil
}

func (c *terminalRetryConnector) CreateDraftPullRequest(_ context.Context, repository string, branch string, title string, body string) (connector.PullRequest, error) {
	c.createCalls++
	c.createRepository = repository
	c.createBranch = branch
	c.createTitle = title
	c.createBody = body
	if c.createErr != nil {
		return connector.PullRequest{}, c.createErr
	}
	if c.created == nil {
		return connector.PullRequest{}, errors.New("draft pull request unavailable")
	}
	return *c.created, nil
}

func (c *terminalRetryConnector) HydratePullRequest(_ context.Context, issue connector.Issue) (connector.Issue, error) {
	if c.lookup == nil {
		return issue, nil
	}
	pullRequest := *c.lookup
	issue.PullRequest = &pullRequest
	issue.PRRepository = "acme/widgets"
	number := pullRequest.Number
	issue.PRNumber = &number
	return issue, nil
}

func (c *terminalRetryConnector) CreateComment(_ context.Context, _ string, body string) error {
	c.comments = append(c.comments, body)
	return nil
}

func (c *terminalRetryConnector) UpdateIssueState(_ context.Context, issueID string, state string) error {
	if err := c.updateErrors[state]; err != nil {
		return err
	}
	issue := c.issues[issueID]
	issue.State = state
	c.issues[issueID] = issue
	c.transitions = append(c.transitions, state)
	return nil
}

func (c *terminalRetryConnector) SetAssignee(context.Context, string, string) error { return nil }

func (c *terminalRetryConnector) SetField(context.Context, string, string, string) error { return nil }

func (c *terminalRetryConnector) transitionStates() []string {
	return append([]string(nil), c.transitions...)
}

type terminalRetryWorkAttemptStore struct {
	completionErr  error
	completions    []store.WorkAttemptCompletion
	history        []store.WorkAttempt
	historyErr     error
	historyQueries []store.WorkAttemptHistoryQuery
}

func (s *terminalRetryWorkAttemptStore) StartWorkAttempt(context.Context, store.WorkAttemptStart) (int64, error) {
	return 0, nil
}

func (s *terminalRetryWorkAttemptStore) WorkAttempt(context.Context, int64) (store.WorkAttempt, error) {
	return store.WorkAttempt{}, store.ErrNotFound
}

func (s *terminalRetryWorkAttemptStore) RecordWorkAttemptHeartbeat(context.Context, store.WorkAttemptHeartbeat) error {
	return nil
}

func (s *terminalRetryWorkAttemptStore) CompleteWorkAttempt(_ context.Context, completion store.WorkAttemptCompletion) error {
	s.completions = append(s.completions, completion)
	return s.completionErr
}

func (s *terminalRetryWorkAttemptStore) TimeoutExpiredWorkAttempts(context.Context, store.WorkAttemptTimeout) ([]store.WorkAttempt, error) {
	return nil, nil
}

func (s *terminalRetryWorkAttemptStore) ListActiveWorkAttempts(context.Context, store.WorkAttemptQuery) ([]store.WorkAttempt, error) {
	return nil, nil
}

func (s *terminalRetryWorkAttemptStore) ListRecentTerminalWorkAttempts(_ context.Context, query store.WorkAttemptHistoryQuery) ([]store.WorkAttempt, error) {
	s.historyQueries = append(s.historyQueries, query)
	return append([]store.WorkAttempt(nil), s.history...), s.historyErr
}

func (s *terminalRetryWorkAttemptStore) RecordSchedulerDecision(context.Context, store.SchedulerDecision) (int64, error) {
	return 0, nil
}

func (s *terminalRetryWorkAttemptStore) ListRecentSchedulerDecisions(context.Context, store.SchedulerDecisionQuery) ([]store.SchedulerDecision, error) {
	return nil, nil
}

func terminalRetryMetadataPushed(raw string) bool {
	return workAttemptMetadataHasPushedProduct(raw)
}

func TestStartupDoesNotReclaimLiveWorkAttempts(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	t.Parallel()
	for _, count := range []int{1, 2} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			ctx := t.Context()
			runtimeStore := openWorkAttemptRecoveryStore(t, ctx)
			cfg := normalizeConfig(Config{Project: scheduler.ProjectCandidate{ID: "detent"}})
			now := time.Now().UTC()
			for index := range count {
				_, err := runtimeStore.StartWorkAttempt(ctx, store.WorkAttemptStart{
					ProjectID: "detent", IssueID: strconv.Itoa(index), WorkerType: "implement",
					StartedAt: now, LeaseExpiresAt: now.Add(time.Hour),
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			o := &Orchestrator{cfg: cfg, workAttempts: runtimeStore}
			state := newState(cfg)
			o.recoverDurableWorkAttempts(ctx, &state, now.Add(time.Second))
			attempts, err := runtimeStore.ListActiveWorkAttempts(ctx, store.WorkAttemptQuery{ProjectID: "detent"})
			if err != nil || len(attempts) != count {
				t.Fatalf("active attempts = %d, error = %v", len(attempts), err)
			}
			for _, attempt := range attempts {
				if attempt.ErrorClass != "" {
					t.Fatalf("attempt classified during restart: %#v", attempt)
				}
			}
		})
	}
}

func TestRetainedWorkAttemptsExpireOnTick(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	t.Parallel()
	for _, tt := range []struct {
		name         string
		running      bool
		successor    bool
		phase        string
		wantTerminal bool
	}{
		{name: "orphan expires", wantTerminal: true},
		{name: "owned attempt survives expired heartbeat", running: true},
		{name: "deferred completion expires", phase: "completion_deferred", wantTerminal: true},
		{name: "expired completion preserves successor", phase: "completion_deferred", successor: true, wantTerminal: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			runtimeStore := openWorkAttemptRecoveryStore(t, ctx)
			cfg := normalizeConfig(Config{Project: scheduler.ProjectCandidate{ID: "detent"}})
			now := time.Now().UTC()
			id, err := runtimeStore.StartWorkAttempt(ctx, store.WorkAttemptStart{
				ProjectID: "detent", IssueID: "retained", WorkerType: "implement",
				StartedAt: now, LeaseExpiresAt: now.Add(time.Minute),
			})
			if err != nil {
				t.Fatal(err)
			}
			tracker := &rateLimitConnector{hasRateLimit: true, rateLimit: connector.GraphQLRateLimit{Remaining: 0, ResetAt: now.Add(time.Hour)}}
			o := newRateLimitTestOrchestrator(cfg, tracker)
			o.workAttempts = runtimeStore
			state := newState(cfg)
			o.recoverDurableWorkAttempts(ctx, &state, now)
			active, err := runtimeStore.ListActiveWorkAttempts(ctx, store.WorkAttemptQuery{ProjectID: "detent"})
			if err != nil || len(active) != 1 {
				t.Fatalf("startup active = %v, err = %v", active, err)
			}
			if tt.phase != "" {
				err := runtimeStore.RecordWorkAttemptHeartbeat(ctx, store.WorkAttemptHeartbeat{AttemptID: id, HeartbeatAt: now, LeaseExpiresAt: now.Add(time.Minute), Phase: tt.phase})
				if err != nil {
					t.Fatal(err)
				}
			}
			if tt.phase != "" {
				issue := connector.Issue{ID: "retained"}
				state.deferredCompletions[issue.ID] = deferredCompletion{Running: Running{Issue: issue, WorkAttemptID: id}}
				state.Claimed[issue.ID] = Claimed{Issue: issue, LeaseExpiresAt: now.Add(time.Minute)}
				state.Retry[issue.ID] = Retry{Issue: issue, CompletionDeferred: true, DueAt: now.Add(time.Hour)}
			}
			if tt.running {
				state.Running["retained"] = Running{Issue: connector.Issue{ID: "retained"}, WorkAttemptID: id}
			}
			o.tick(ctx, &state, now.Add(30*time.Second))
			active, err = runtimeStore.ListActiveWorkAttempts(ctx, store.WorkAttemptQuery{ProjectID: "detent"})
			if err != nil || len(active) != 1 {
				t.Fatalf("before expiry active = %v, err = %v", active, err)
			}
			if tt.successor {
				state.Running["retained"] = Running{Issue: connector.Issue{ID: "retained"}, WorkAttemptID: id + 1}
				state.Claimed["retained"] = Claimed{Issue: connector.Issue{ID: "retained"}, LeaseExpiresAt: now.Add(time.Hour)}
				state.Retry["retained"] = Retry{RecoveryAttemptID: id + 1, DueAt: now.Add(time.Hour)}
			}
			o.tick(ctx, &state, now.Add(2*time.Minute))
			active, err = runtimeStore.ListActiveWorkAttempts(ctx, store.WorkAttemptQuery{ProjectID: "detent"})
			if err != nil {
				t.Fatal(err)
			}
			if got := len(active) == 0; got != tt.wantTerminal {
				t.Fatalf("terminal = %t, want %t", got, tt.wantTerminal)
			}
			recent, err := runtimeStore.ListRecentTerminalWorkAttempts(ctx, store.WorkAttemptHistoryQuery{ProjectID: "detent", Limit: 10})
			if err != nil {
				t.Fatal(err)
			}
			if tt.phase != "" && len(state.deferredCompletions) != 0 {
				t.Fatal("expired completion retained ownership")
			}
			if tt.phase != "" {
				if tt.successor {
					if state.Running["retained"].WorkAttemptID != id+1 || state.Retry["retained"].RecoveryAttemptID != id+1 || !state.Claimed["retained"].LeaseExpiresAt.Equal(now.Add(time.Hour)) {
						t.Fatal("expired completion changed successor ownership")
					}
				} else if len(state.Claimed) != 0 || len(state.Retry) != 0 {
					t.Fatal("expired completion retained claim or retry")
				}
			}
			if tt.wantTerminal && (len(recent) != 1 || recent[0].ErrorClass != "lease_expired") {
				t.Fatalf("terminal attempts = %#v", recent)
			}
		})
	}
}

func TestTerminalRetryTransitionFailures(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name       string
		limit      *int
		failState  string
		wantState  string
		wantParked bool
	}{
		{name: "zero holds locally when tracker park fails", limit: new(0), failState: "Blocked", wantState: "Todo", wantParked: false},
		{name: "default preserves retry when tracker park fails", failState: "Blocked", wantState: "Todo"},
		{name: "retry transition failure retains original lane", failState: "Todo", wantState: "In Progress"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
			issue := terminalRetryTestIssue(tt.name)
			tracker := &terminalRetryConnector{issues: map[string]connector.Issue{issue.ID: issue}, updateErrors: map[string]error{tt.failState: errors.New("tracker unavailable")}}
			cfg := normalizeConfig(Config{Recovery: workflowconfig.Recovery{TerminalAttemptRetryLimit: tt.limit}, ActiveStates: []string{"Todo", "In Progress"}})
			o := &Orchestrator{cfg: cfg, connector: tracker, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
			state := newState(cfg)
			count := 3
			if tt.failState == "Todo" {
				count = 1
			}
			for i := 1; i <= count; i++ {
				state.WorkAttempts = append(state.WorkAttempts, telemetry.WorkAttempt{AttemptID: int64(i), IssueID: issue.ID, Status: string(store.WorkAttemptStatusTerminal), TerminalState: string(store.WorkAttemptTerminalFailure), ErrorClass: workAttemptErrorRunner})
			}
			updated, _, parked := o.demoteTerminalAttemptRetry(t.Context(), &state, issue, false, terminalAttemptRetryLimitCause, true, RunModeImplement, DiffStats{}, now)
			if updated.State != tt.wantState || parked != tt.wantParked {
				t.Fatalf("state/parked = %s/%t, want %s/%t", updated.State, parked, tt.wantState, tt.wantParked)
			}
			if len(state.Blocked) != 0 {
				t.Fatal("retry transition created a park")
			}
		})
	}
}

func TestBlockedRecoveryRecomputesDispatchWithoutDerivedHolds(t *testing.T) {
	now := time.Now().UTC()
	for _, tt := range []struct {
		name       string
		signal     *workpad.Signal
		operator   bool
		wantRetry  bool
		dependency bool
		condition  string
	}{
		{name: "terminal failure", wantRetry: true},
		{name: "expired metadata", wantRetry: true, condition: "expired"},
		{name: "changed revision", wantRetry: true, condition: "revision"},
		{name: "foreign lease", condition: "lease"},
		{name: "unavailable source", wantRetry: true, condition: "source"},
		{name: "capacity", wantRetry: true, condition: "capacity"},
		{name: "spend", wantRetry: true, condition: "spend"},
		{name: "unresolved native dependency", dependency: true, wantRetry: true},
		{name: "untyped machine blocker", signal: &workpad.Signal{Status: workpad.StatusBlocked}, wantRetry: true},
		{name: "typed human action", signal: &workpad.Signal{Source: workpad.SourceStructured, Status: workpad.StatusBlocked, HumanAction: "May the requested scope include publishing?"}},
		{name: "operator stop", operator: true},
		{name: "expired human judgment", condition: "human-expired", signal: &workpad.Signal{Source: workpad.SourceStructured, Status: workpad.StatusBlocked, HumanAction: "Approve requested scope"}},
		{name: "changed human revision", condition: "human-revision", signal: &workpad.Signal{Source: workpad.SourceStructured, Status: workpad.StatusBlocked, HumanAction: "Approve requested scope"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var decisions []dispatchableDecision
			for _, wipe := range []bool{false, true} {
				issue := connector.Issue{ID: "item", Identifier: "example-org/example-project#1", Title: "Example work", AssignedToWorker: true, State: "Blocked", WorkpadSignal: tt.signal}
				if tt.dependency {
					issue.BlockedBy = []connector.BlockedRef{{ID: "prerequisite", Identifier: "example-org/example-project#2", State: "Todo", Source: connector.BlockedRefSourceNative}}
				}
				tracker := &terminalRetryConnector{issues: map[string]connector.Issue{issue.ID: issue}}
				cfg := normalizeConfig(Config{ActiveStates: []string{"Todo", "Rework"}, TerminalStates: []string{"Done"}, FailureRetryBaseDelay: time.Second, MaxRetryBackoff: time.Minute})
				db := openWorkAttemptRecoveryStore(t, t.Context())
				o := &Orchestrator{cfg: cfg, connector: tracker, workflowMetrics: db}
				state := newState(cfg)
				hold := workflowLaneBlockedRecoveryMetadata{Owner: blockedRecoveryOwnerOrchestrator, Cause: terminalAttemptRetryLimitCause, SourceRevision: holdSourceRevision(issue), ExpiresAt: now.Add(time.Hour)}
				if tt.condition == "expired" || tt.condition == "human-expired" {
					hold.ExpiresAt = now.Add(-time.Second)
				}
				if tt.condition == "revision" || tt.condition == "human-revision" {
					issue.Description = "Updated source intent"
				}
				metadata := workflowLaneMetadata{}
				if !wipe {
					metadata.BlockedRecovery = &hold
				}
				reason := "machine_failure"
				if tt.operator {
					reason = string(store.WorkAttemptTerminalOperatorStopped)
				}
				if tt.signal != nil && tt.signal.HumanAction != "" {
					metadata.ReasonDetail = "```detent-status\nschema: 1\nstatus: blocked\nblockers: []\nhuman_action: " + tt.signal.HumanAction + "\n```"
				}
				data, err := json.Marshal(metadata)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.RecordWorkflowPhaseEvent(t.Context(), store.WorkflowPhaseEvent{ProjectID: o.workflowMetricsProjectID(), IssueID: issue.ID, Identifier: issue.Identifier, PhaseType: store.WorkflowPhaseTypeLane, PhaseName: "Blocked", Status: "entered", Reason: reason, StartedAt: now.Add(-time.Minute), FinishedAt: now.Add(-time.Minute), MetadataJSON: string(data)}); err != nil {
					t.Fatal(err)
				}
				_, foundHold := o.currentBlockedRecoveryPark(t.Context(), &state, issue)
				if foundHold != (!wipe && tt.condition != "expired" && tt.condition != "revision" && tt.condition != "human-expired" && tt.condition != "human-revision") {
					t.Fatalf("persisted derived hold accepted=%v wipe=%v", foundHold, wipe)
				}
				if strings.HasPrefix(tt.condition, "human-") {
					issue.WorkpadSignal = nil
				}
				if tt.condition == "lease" {
					o.cfg.Claiming = ClaimingConfig{Enabled: true, AssigneeLogin: "worker", LeaseField: "Lease", LeaseTTL: time.Hour}
					issue.Assignees = []string{"other-worker"}
					issue.Fields = map[string]string{"Lease": formatClaimTime(now)}
				}
				changed := o.recoverBlockedIssues(t.Context(), &state, []connector.Issue{issue}, now)
				if (len(changed) > 0) != tt.wantRetry {
					t.Fatalf("wipe=%v transitioned=%v, want retry=%v", wipe, changed, tt.wantRetry)
				}
				retry, found := state.Retry[issue.ID]
				if found != tt.wantRetry {
					t.Fatalf("wipe=%v retry=%v, want %v", wipe, found, tt.wantRetry)
				}
				if found {
					if !retry.DueAt.After(now) || retry.DueAt.After(now.Add(cfg.MaxRetryBackoff)) {
						t.Fatalf("unbounded retry: %v", retry.DueAt)
					}
					issue = retry.Issue
					delete(state.Retry, issue.ID)
				}
				switch tt.condition {
				case "source":
					issue.PullRequest = &connector.PullRequest{Number: 1, State: "open", HydrationUnavailableReason: "source unavailable"}
				case "capacity":
					o.cfg.MaxConcurrentAgents = 1
					state.MaxConcurrentAgents = 1
					state.Running["other"] = Running{Issue: connector.Issue{ID: "other"}}
				case "spend":
					o.cfg.BillingMode = "metered"
					state.BudgetRefusals[issue.ID] = BudgetRefusal{Issue: issue, Code: "per_issue_max_usd"}
				}
				decision := o.dispatchPlanner().dispatchableIssueDecisionForModelRequirement(issue, &state, false, now.Add(cfg.MaxRetryBackoff), "", false)
				decisions = append(decisions, decision)
				if tt.wantRetry && !tt.dependency && (tt.condition == "" || tt.condition == "expired" || tt.condition == "revision") && decision.reason != "" {
					t.Fatalf("retry not dispatchable: %#v", decision)
				}
				if (!tt.wantRetry || tt.dependency || tt.condition == "lease" || tt.condition == "source" || tt.condition == "capacity" || tt.condition == "spend") && decision.reason == "" {
					t.Fatal("human intent became dispatchable")
				}
			}
			if !reflect.DeepEqual(decisions[0], decisions[1]) {
				t.Fatalf("wiping derived holds changed dispatch: %#v", decisions)
			}
		})
	}
}

func TestStoredHoldRequiresProvenance(t *testing.T) {
	now := time.Now().UTC()
	issue := terminalRetryTestIssue("provenance")
	for _, tt := range []struct {
		name          string
		mutate        func(*workflowLaneBlockedRecoveryMetadata)
		valid         bool
		invalidSource bool
	}{
		{name: "current", valid: true},
		{name: "unencodable source", invalidSource: true},
		{name: "missing owner", mutate: func(m *workflowLaneBlockedRecoveryMetadata) { m.Owner = "" }},
		{name: "missing revision", mutate: func(m *workflowLaneBlockedRecoveryMetadata) { m.SourceRevision = "" }},
		{name: "wrong revision", mutate: func(m *workflowLaneBlockedRecoveryMetadata) { m.SourceRevision = "other" }},
		{name: "expired", mutate: func(m *workflowLaneBlockedRecoveryMetadata) { m.ExpiresAt = now.Add(-time.Second) }},
		{name: "missing expiry", mutate: func(m *workflowLaneBlockedRecoveryMetadata) { m.ExpiresAt = time.Time{} }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			issue := cloneIssue(issue)
			if tt.invalidSource {
				updatedAt := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
				issue.UpdatedAt = &updatedAt
			}
			hold := workflowLaneBlockedRecoveryMetadata{Owner: "orchestrator", SourceRevision: holdSourceRevision(issue), ExpiresAt: now.Add(time.Hour)}
			if tt.mutate != nil {
				tt.mutate(&hold)
			}
			tracker := &terminalRetryConnector{issues: map[string]connector.Issue{issue.ID: issue}}
			cfg := normalizeConfig(Config{})
			o := &Orchestrator{cfg: cfg, connector: tracker}
			state := newState(cfg)
			err := o.updateIssueStateByIDStrictWithMetadata(t.Context(), &state, issue.ID, issue, "Todo", now, "operator_move", workflowLaneMetadata{BlockedRecovery: &hold})
			if (err == nil) != tt.valid {
				t.Fatalf("lane write = %v, valid=%v", err, tt.valid)
			}
			if !tt.valid && len(tracker.transitions) != 0 {
				t.Fatal("invalid provenance wrote a lane")
			}
		})
	}
}

func TestMachineFailuresUseRetryOwner(t *testing.T) {

	for _, tt := range []struct {
		name        string
		readErr     error
		writeErr    error
		unreachable bool
	}{
		{name: "workflow read failure", readErr: errors.New("workflow unavailable")},
		{name: "lane write failure", writeErr: errors.New("lane unavailable")},
		{name: "unreachable retry lane", unreachable: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
			issue := connector.Issue{ID: "retry-error", State: "Blocked"}
			states := []connector.WorkflowState{{Name: "Blocked", Transitions: []string{"Rework"}}, {Name: "Rework", Dispatchable: true}}
			if tt.unreachable {
				states[0].Transitions = nil
			}
			tracker := &nativeWorkflowConnector{autoPromoteTickConnector: &autoPromoteTickConnector{updateErr: tt.writeErr}, states: states, statesErr: tt.readErr}
			cfg := normalizeConfig(Config{ActiveStates: []string{"Todo", "Rework"}})
			o := &Orchestrator{cfg: cfg, connector: tracker}
			state := newState(cfg)
			err := o.retryMachineFailure(t.Context(), &state, issue, 2, now, "validation failed", "")
			if err == nil || tt.readErr != nil && !errors.Is(err, tt.readErr) || tt.writeErr != nil && !errors.Is(err, tt.writeErr) {
				t.Fatalf("retry error = %v", err)
			}
			if len(state.Retry) != 0 {
				t.Fatal("failed routing scheduled an executable retry")
			}
			event := runpkg.Completion{IssueID: issue.ID, CompletedAt: now, Result: runpkg.RunResult{FinalState: FinalStateCompleted}}
			running := Running{Issue: issue, Attempt: 1, WorkAttemptID: 42}
			o.retryMachineCompletion(t.Context(), &state, event, running, 2, "validation failed")
			deferred, ok := state.deferredCompletions[issue.ID]
			if !ok || deferred.Running.WorkAttemptID != 42 || deferred.Result.FinalState != FinalStateCompleted || !state.Retry[issue.ID].CompletionDeferred {
				t.Fatal("retry routing error lost the original completion")
			}
		})
	}
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name  string
		apply func(*Orchestrator, *State, Running)
	}{
		{"instant failure", func(o *Orchestrator, s *State, r Running) {
			o.retryMachineCompletion(t.Context(), s, runpkg.Completion{CompletedAt: now}, r, 50, "backend failed")
		}},
		{"repeated failure", func(o *Orchestrator, s *State, r Running) {
			o.retryMachineCompletion(t.Context(), s, runpkg.Completion{CompletedAt: now}, r, 50, "validation failed")
		}},
		{"merge current head CI wait", func(o *Orchestrator, s *State, r Running) {
			o.waitForMergeWorkerCurrentHeadCI(t.Context(), s, runpkg.Completion{IssueID: r.Issue.ID, CompletedAt: now}, r, r.Issue)
		}},
		{"merge hydration wait", func(o *Orchestrator, s *State, r Running) {
			event := runpkg.Completion{IssueID: r.Issue.ID, CompletedAt: now}
			o.waitForMergeWorkerPullRequestHydration(t.Context(), s, event, r, r.Issue)
		}},
		{"merge base refresh", func(o *Orchestrator, s *State, r Running) {
			event := runpkg.Completion{IssueID: r.Issue.ID, CompletedAt: now}
			o.refreshMergeWorkerBase(t.Context(), s, event, r, r.Issue, "merge_api_rejected_out_of_date_base")
		}},
		{"programmatic merge failure", func(o *Orchestrator, s *State, r Running) {
			event := runpkg.Completion{IssueID: r.Issue.ID, CompletedAt: now}
			o.failProgrammaticMergeWorkerResult(t.Context(), s, event, r, "merge_failed", errors.New("merge failed"))
		}},
		{"incomplete merge", func(o *Orchestrator, s *State, r Running) {
			event := runpkg.Completion{IssueID: r.Issue.ID, CompletedAt: now}
			o.handleIncompleteMergeWorkerResult(t.Context(), s, event, r)
		}},
		{"merge startup fallback", func(o *Orchestrator, s *State, r Running) {
			event := runpkg.Completion{IssueID: r.Issue.ID, CompletedAt: now}
			event.Err = runpkg.ErrMergeWorkerStartupTimeout
			o.handleMergeWorkerStartupTimeout(t.Context(), s, event, r)
		}},
		{"ordinary error", func(o *Orchestrator, s *State, r Running) {
			event := runpkg.Completion{IssueID: r.Issue.ID, CompletedAt: now}
			s.Running[r.Issue.ID] = r
			event.Err = errors.New("runner failed")
			event.Result.TurnStarted = true
			o.handleRunResult(t.Context(), s, event)
		}},
		{"merge exhaustion", func(o *Orchestrator, s *State, r Running) {
			o.blockExhaustedMergeWorker(t.Context(), s, r, runpkg.Completion{IssueID: r.Issue.ID, CompletedAt: now}, mergeWorkerRetryExhaustedReason, 50, errors.New("merge failed"))
		}},
		{"merge duration", func(o *Orchestrator, s *State, r Running) {
			o.parkMergeWorkerDurationExceeded(t.Context(), s, r, runpkg.Completion{IssueID: r.Issue.ID, CompletedAt: now}, time.Hour, "progress")
		}},
		{"artifact convergence", func(o *Orchestrator, s *State, r Running) {
			o.parkArtifactGateConvergence(t.Context(), s, runpkg.Completion{IssueID: r.Issue.ID, CompletedAt: now}, r)
		}},
		{"required check", func(o *Orchestrator, s *State, r Running) {
			o.blockPersistentlyMissingRequiredChecks(t.Context(), s, runpkg.Completion{IssueID: r.Issue.ID, CompletedAt: now}, r, r.Issue, nil)
		}},
		{"no progress", func(o *Orchestrator, s *State, r Running) {
			o.blockImplementProgress(t.Context(), s, r, implementCompletionProgressDecision{Issue: r.Issue}, runpkg.Completion{IssueID: r.Issue.ID, CompletedAt: now})
		}},
		{"spend progress", func(o *Orchestrator, s *State, r Running) {
			o.retryMachineCompletion(t.Context(), s, runpkg.Completion{CompletedAt: now}, r, 50, "no progress")
		}},
		{"lifetime", func(o *Orchestrator, s *State, r Running) {
			o.parkLifetimeLimit(t.Context(), s, r.Issue, lifetimeLimitDecision{}, now)
		}},
		{"configuration", func(o *Orchestrator, s *State, r Running) {
			o.blockHumanOwnedWorkerFailure(t.Context(), s, runpkg.Completion{CompletedAt: now, Err: errors.New("invalid configuration")}, r, "issue_configuration", "invalid configuration", "generated remedy", "failure")
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			issue := terminalRetryTestIssue("retry")
			issue.PullRequest = &connector.PullRequest{Number: 1, HeadSHA: "head"}
			tracker := &terminalRetryConnector{issues: map[string]connector.Issue{issue.ID: issue}}
			cfg := normalizeConfig(Config{ActiveStates: []string{"Todo", "In Progress", "Rework"}, MaxRetryBackoff: time.Minute, FailureRetryBaseDelay: time.Second})
			metrics, err := store.Open(t.Context(), store.Config{Backend: store.BackendSQLite, Path: filepath.Join(t.TempDir(), "retry.db")})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = metrics.Close() })
			scheduling := &hubSchedulingSource{}
			o := &Orchestrator{cfg: cfg, connector: tracker, workflowMetrics: metrics, scheduling: scheduling}
			state := newState(cfg)
			state.mergeReservations = map[string]mergeReservation{}
			for _, attempt := range []int{1, 2, 49} {
				state.Claimed[issue.ID] = Claimed{Issue: issue}
				beforeReleases := scheduling.releases
				tt.apply(o, &state, Running{Issue: issue, Attempt: attempt})
				retry, ok := state.Retry[issue.ID]
				if !ok || len(state.Blocked) != 0 || slices.Contains(tracker.transitions, "Blocked") {
					t.Fatalf("machine failure did not retry: retry=%v blocked=%v transitions=%v", ok, state.Blocked, tracker.transitions)
				}
				if !retry.DueAt.After(now) || retry.DueAt.After(now.Add(cfg.MaxRetryBackoff)) {
					t.Fatalf("retry outside capped backoff: %v", retry.DueAt)
				}
				o.releaseClaim(&state, issue.ID)
				if len(state.Retry) != 0 {
					t.Fatal("completion retained retry state")
				}
				if scheduling.releases != beforeReleases+1 {
					t.Fatalf("releases = %d, want %d", scheduling.releases, beforeReleases+1)
				}
				if strings.HasPrefix(tt.name, "merge ") && tt.name != "merge exhaustion" && tt.name != "merge duration" || tt.name == "programmatic merge failure" || tt.name == "incomplete merge" || tt.name == "ordinary error" {
					state.Retry[issue.ID] = retry
					upper := time.Second << attempt
					if attempt > 5 {
						upper = time.Minute
					}
					assertCompletionMachineRetry(t, o, state, issue.ID, now, attempt+1, upper)
				}
			}
			if tt.name == "merge current head CI wait" || tt.name == "merge hydration wait" || tt.name == "merge base refresh" {
				attempt := 9
				for cycle, want := range []int{1, 1, 2, 3, 1} {
					running := Running{Issue: issue, Attempt: attempt}
					running.DispatchLoopStart.Captured = true
					running.DispatchLoopStart.Fingerprint.PRNumber = 1
					running.DispatchLoopStart.Fingerprint.PRHeadSHA = "head"
					running.DispatchLoopStart.Fingerprint.WorkspaceTree = "before"
					running.DiffStats = DiffStats{TreeSHA: "before"}
					if cycle == 0 || cycle == 1 || cycle == 4 {
						running.DiffStats.TreeSHA = "after"
					}
					state.Claimed[issue.ID] = Claimed{Issue: issue}
					beforeReleases := scheduling.releases
					tt.apply(o, &state, running)
					retry := state.Retry[issue.ID]
					o.releaseClaim(&state, issue.ID)
					if scheduling.releases != beforeReleases+1 {
						t.Fatalf("cycle %d releases = %d, want %d", cycle, scheduling.releases, beforeReleases+1)
					}
					state.Retry[issue.ID] = retry
					assertCompletionMachineRetry(t, o, state, issue.ID, now, want, time.Second<<(want-1))
					if tt.name == "merge current head CI wait" {
						cleared, handled, _ := o.pollMergeWorkerCurrentHeadCI(t.Context(), &state, issue, retry, now)
						if handled || cleared.Attempt != want || cleared.Wait.Kind != "" {
							t.Fatalf("cleared wait = %#v, handled=%v", cleared, handled)
						}
					}
					attempt = want
				}
			}

		})
	}
}

func TestMachineRetrySurvivesStoreRestart(t *testing.T) {
	ctx := t.Context()
	dbConfig := store.Config{Backend: store.BackendSQLite, Path: filepath.Join(t.TempDir(), "retry.db")}
	db, err := store.Open(ctx, dbConfig)
	if err != nil {
		t.Fatal(err)
	}
	issue := terminalRetryTestIssue("durable-retry")
	cfg := normalizeConfig(Config{ActiveStates: []string{"Todo", "In Progress", "Rework"}, FailureRetryBaseDelay: time.Second, MaxRetryBackoff: time.Minute, OutputTruncationMaxBytes: 100})
	tracker := &terminalRetryConnector{issues: map[string]connector.Issue{issue.ID: issue}}
	o := &Orchestrator{cfg: cfg, connector: tracker, workflowMetrics: db, workAttempts: db}
	state := newState(cfg)
	now := time.Now().UTC()
	for _, attempt := range []int{2, 50} {
		if err := o.retryMachineFailure(ctx, &state, issue, attempt, now, strings.Repeat("界🙂", 100), ""); err != nil {
			t.Fatal(err)
		}
		want := state.Retry[issue.ID]
		if len(want.Error) > cfg.OutputTruncationMaxBytes || !utf8.ValidString(want.Error) || want.Error == "" {
			t.Fatalf("invalid bounded error: %q", want.Error)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		db, err = store.Open(ctx, dbConfig)
		if err != nil {
			t.Fatal(err)
		}
		o = &Orchestrator{cfg: cfg, connector: tracker, workflowMetrics: db, workAttempts: db}
		state = newState(cfg)
		o.restoreWorkAttemptRetryIntents(ctx, &state, []connector.Issue{issue}, now)
		got, ok := state.Retry[issue.ID]
		if !ok || got.Attempt != attempt || !got.DueAt.Equal(want.DueAt) || got.Error != want.Error {
			t.Fatalf("restart lost retry: got=%+v want=%+v", got, want)
		}
		if !got.DueAt.After(now) || got.DueAt.After(now.Add(cfg.MaxRetryBackoff)) {
			t.Fatal("retry escaped cap")
		}
		now = got.DueAt
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}
