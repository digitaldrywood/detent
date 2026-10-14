package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/backendcapacity"
	"github.com/digitaldrywood/detent/internal/codex"
	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestPreTurnFailuresRetryWithoutPausingProject(t *testing.T) {
	t.Parallel()
	startup := backendcapacity.NewError(backendcapacity.Scope{}, backendcapacity.Details{Type: backendcapacity.ErrorTypeTransientOverload, Kind: backendcapacity.StartupTimeoutKind}, context.DeadlineExceeded)
	protocol := errors.New("codex turn/start: JSON-RPC -32600 invalid request")
	workspace := fmt.Errorf("%w: after_create exited 1", runpkg.ErrWorkspacePreparation)
	wrappedStartup := errors.Join(runpkg.NewCancellationCause(startup, "orchestrator.parent_context"), startup)
	wrappedMergeStartup := errors.Join(runpkg.NewCancellationCause(runpkg.ErrMergeWorkerStartupTimeout, "orchestrator.parent_context"), context.Canceled)
	inputRefusal := &codex.ResponseError{Request: "turn/start", Code: -32602, Message: "Input exceeds the maximum length of 1048576"}
	details, ok := codex.ClassifyCapacityError(inputRefusal, nil, time.Now())
	if !ok || details.Kind != backendcapacity.StartupFailureKind {
		t.Fatalf("input refusal classification = %+v, classified = %v", details, ok)
	}
	inputFailure := backendcapacity.NewError(backendcapacity.Scope{}, details, inputRefusal)
	for _, tt := range []struct {
		name     string
		failures []error
	}{
		{"protocol", []error{protocol, protocol, protocol}},
		{"workspace", []error{workspace, workspace, workspace}},
		{"startup", []error{startup, startup, startup}},
		{"turn input refusal", []error{inputFailure, inputFailure, inputFailure}},
		{"mixed", []error{protocol, startup, workspace}},
		{"supervisor startup deadline", []error{wrappedStartup, wrappedStartup, wrappedStartup}},
		{"supervisor merge startup timer", []error{wrappedMergeStartup, wrappedMergeStartup, wrappedMergeStartup}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
			cfg := normalizeConfig(Config{ActiveStates: []string{"Todo", "In Progress", "Rework"}, BlockedRecovery: BlockedRecoveryConfig{BreakerCooldown: time.Minute}})
			tracker := &terminalRetryConnector{issues: map[string]connector.Issue{}}
			o := &Orchestrator{cfg: cfg, connector: tracker, workAttempts: &terminalRetryWorkAttemptStore{}}
			state := newState(cfg)
			for index, failure := range tt.failures {
				issue := terminalRetryTestIssue(strconv.Itoa(index))
				tracker.issues[issue.ID] = issue
				state.Running[issue.ID] = Running{Issue: issue, Attempt: 1, WorkAttemptID: int64(index + 1), DispatchSourceState: "Todo", DispatchTargetState: "In Progress", StartedAt: now.Add(-time.Second)}
				o.handleRunResult(t.Context(), &state, runpkg.Completion{IssueID: issue.ID, Err: failure, CompletedAt: now})
				retry, retried := state.Retry[issue.ID]
				if tracker.issues[issue.ID].State != "Todo" || len(state.Blocked) != 0 || len(tracker.comments) != 0 || !retried || retry.Attempt != 2 || !retry.DueAt.After(now) {
					t.Fatalf("pre-turn failure %d did not retry in its lane: state=%s blocked=%v comments=%v retry=%#v", index+1, tracker.issues[issue.ID].State, state.Blocked, tracker.comments, state.Retry)
				}
				if !projectFailureBreakerAllowsDispatch(&state, now) || state.FailureBreaker.Active() {
					t.Fatalf("instance failure %d paused project dispatch: breaker=%#v", index+1, state.FailureBreaker)
				}
			}
			if rows := projectFailureBreakerSnapshots(state); len(rows) != 0 {
				t.Fatalf("instance failures produced project breaker evidence: %#v", rows)
			}
		})
	}
}

func TestPreTurnAttemptTaxonomy(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		class   string
		metrics string
		want    bool
	}{
		{"runner before turn", workAttemptErrorRunner, `{"turns":0}`, true},
		{"runner after turn", workAttemptErrorRunner, `{"turns":1}`, false},
		{"runner used tokens", workAttemptErrorRunner, `{"turns":0,"total_tokens":5}`, false},
		{"unknown historical turns", workAttemptErrorRunner, `{}`, false},
		{"missing historical turns", workAttemptErrorRunner, "", false},
		{"malformed metrics", workAttemptErrorRunner, `{`, false},
		{"workspace hook", workAttemptErrorWorkspace, "", true},
		{"startup timeout", backendcapacity.StartupTimeoutErrorClass, "", true},
		{"startup exit", backendcapacity.StartupFailureErrorClass, "", true},
		{"provider capacity", backendcapacity.ErrorClass, `{"turns":0}`, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := preTurnAttempt(telemetry.WorkAttempt{ErrorClass: tt.class, MetricsJSON: tt.metrics}); got != tt.want {
				t.Fatalf("pre-turn = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPreTurnRestoresDispatchLane(t *testing.T) {
	t.Parallel()
	for _, lane := range []string{"Todo", "Rework", "Merging"} {
		t.Run(lane, func(t *testing.T) {
			t.Parallel()
			issue := terminalRetryTestIssue("lane")
			tracker := &terminalRetryConnector{issues: map[string]connector.Issue{issue.ID: issue}}
			cfg := normalizeConfig(Config{ActiveStates: []string{"Todo", "Rework", "Merging", "In Progress"}})
			o := &Orchestrator{cfg: cfg, connector: tracker}
			state := newState(cfg)
			running := Running{Issue: issue, DispatchSourceState: lane, DispatchTargetState: "In Progress"}
			o.restorePreTurnIssue(t.Context(), &state, running, time.Now())
			if tracker.issues[issue.ID].State != lane || len(tracker.comments) != 0 {
				t.Fatalf("issue = %#v, comments = %v", tracker.issues[issue.ID], tracker.comments)
			}
		})
	}
}
