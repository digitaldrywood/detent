package orchestrator

import (
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/store"
)

type failureBreakerBackendError struct {
	body string
}

func (e failureBreakerBackendError) Error() string {
	return "backend failed"
}

func (e failureBreakerBackendError) BackendErrorBody() string {
	return e.body
}

func TestProjectAttemptFailureClass(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		err           error
		terminalState store.WorkAttemptTerminalState
		errorClass    string
		errorMessage  string
		want          string
	}{
		{
			name:          "token ceiling ignores changing counters",
			err:           errors.New("session token ceiling exceeded: total_tokens=16000001 ceiling_tokens=16000000"),
			terminalState: store.WorkAttemptTerminalFailure,
			want:          projectFailureClassSessionTokenCeiling,
		},
		{
			name:          "deliverable command has stable class",
			err:           errors.New("deliverable command failed (gh pr create): exit status 1"),
			terminalState: store.WorkAttemptTerminalFailure,
			want:          projectFailureClassDeliverableCommand,
		},
		{
			name:          "backend body is hashed",
			err:           failureBreakerBackendError{body: `{"code":"overloaded"}`},
			terminalState: store.WorkAttemptTerminalFailure,
			want:          projectFailureClassBackendError + ":" + projectFailureHash(`{"code":"overloaded"}`),
		},
		{
			name:          "durable error class is retained",
			terminalState: store.WorkAttemptTerminalNoProgress,
			errorClass:    "spend_since_progress_circuit_breaker",
			want:          "spend_since_progress_circuit_breaker",
		},
		{
			name:          "no progress has stable class",
			terminalState: store.WorkAttemptTerminalNoProgress,
			want:          projectFailureClassNoProgress,
		},
		{
			name:          "final state is hashed",
			terminalState: store.WorkAttemptTerminalFailure,
			errorMessage:  "failed",
			want:          projectFailureClassRunnerFinalState + ":" + projectFailureHash("failed"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := projectAttemptFailureClass(tt.err, tt.terminalState, tt.errorClass, tt.errorMessage); got != tt.want {
				t.Fatalf("projectAttemptFailureClass() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestProjectFailureBreakerMixedSuccessStreamDoesNotTrip(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, time.July, 12, 12, 0, 0, 0, time.UTC)
	orch := &Orchestrator{}
	state := State{FailureBreaker: newProjectFailureBreaker(FailureBreakerConfig{
		SameClassLimit: 3,
		Window:         time.Hour,
		Cooldown:       time.Minute,
	})}

	for index := range 6 {
		at := base.Add(time.Duration(index) * time.Minute)
		orch.recordProjectAttemptOutcome(&state, "issue", at, store.WorkAttemptTerminalFailure, errors.New("same error"), workAttemptErrorRunner, "same error")
		orch.recordProjectAttemptOutcome(&state, "issue", at.Add(time.Second), store.WorkAttemptTerminalSuccess, nil, "", "")
	}

	if state.FailureBreaker.Active() {
		t.Fatalf("FailureBreaker = %#v, want inactive", state.FailureBreaker)
	}
	if len(state.FailureBreaker.Failures) != 0 {
		t.Fatalf("Failures = %#v, want cleared by successful yield", state.FailureBreaker.Failures)
	}
}

func TestProjectFailureBreakerPrunesFailuresOutsideWindow(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, time.July, 12, 12, 0, 0, 0, time.UTC)
	orch := &Orchestrator{}
	state := State{FailureBreaker: newProjectFailureBreaker(FailureBreakerConfig{
		SameClassLimit: 2,
		Window:         time.Minute,
		Cooldown:       time.Minute,
	})}

	orch.recordProjectAttemptOutcome(&state, "issue-1", base, store.WorkAttemptTerminalFailure, errors.New("same error"), workAttemptErrorRunner, "same error")
	orch.recordProjectAttemptOutcome(&state, "issue-2", base.Add(2*time.Minute), store.WorkAttemptTerminalFailure, errors.New("same error"), workAttemptErrorRunner, "same error")

	if state.FailureBreaker.Active() {
		t.Fatalf("FailureBreaker = %#v, want failures outside window not to trip", state.FailureBreaker)
	}
	for _, failures := range state.FailureBreaker.Failures {
		if len(failures) != 1 {
			t.Fatalf("Failures = %#v, want one failure inside rolling window", state.FailureBreaker.Failures)
		}
	}
}

func TestProjectFailureBreakerCanaryStateMachine(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, time.July, 12, 12, 0, 0, 0, time.UTC)
	config := FailureBreakerConfig{SameClassLimit: 2, Window: time.Hour, Cooldown: time.Minute}
	orch := &Orchestrator{cfg: Config{FailureBreaker: config}, now: func() time.Time { return base }}
	state := State{FailureBreaker: newProjectFailureBreaker(config)}

	orch.recordProjectAttemptOutcome(&state, "issue-1", base, store.WorkAttemptTerminalFailure, errors.New("same error"), workAttemptErrorRunner, "same error")
	orch.recordProjectAttemptOutcome(&state, "issue-2", base.Add(time.Second), store.WorkAttemptTerminalFailure, errors.New("same error"), workAttemptErrorRunner, "same error")
	if !state.FailureBreaker.Active() {
		t.Fatal("FailureBreaker.Active() = false, want true")
	}
	if projectFailureBreakerAllowsDispatch(&state, base.Add(30*time.Second)) {
		t.Fatal("projectFailureBreakerAllowsDispatch() = true during cooldown")
	}

	canaryAt := state.FailureBreaker.ResumeAt
	if !projectFailureBreakerAllowsDispatch(&state, canaryAt) {
		t.Fatal("projectFailureBreakerAllowsDispatch() = false when canary is due")
	}
	canary, allowed := tryReserveProjectFailureBreakerCanary(&state, "canary-1", canaryAt)
	if !canary || !allowed {
		t.Fatalf("tryReserveProjectFailureBreakerCanary() = (%t, %t), want (true, true)", canary, allowed)
	}
	if projectFailureBreakerAllowsDispatch(&state, canaryAt) {
		t.Fatal("second dispatch allowed while canary is running")
	}
	orch.reloadProjectFailureBreaker(&state, config, canaryAt)
	if projectFailureBreakerAllowsDispatch(&state, canaryAt) {
		t.Fatal("workflow reload allowed a second dispatch while canary is running")
	}

	orch.recordProjectAttemptOutcome(&state, "canary-1", canaryAt.Add(time.Second), store.WorkAttemptTerminalFailure, errors.New("same error"), workAttemptErrorRunner, "same error")
	if got := state.FailureBreaker.ResumeAt; got != canaryAt.Add(time.Second+config.Cooldown) {
		t.Fatalf("ResumeAt = %s, want full cooldown through %s", got, canaryAt.Add(time.Second+config.Cooldown))
	}
	if state.FailureBreaker.CanaryIssueID != "" {
		t.Fatalf("CanaryIssueID = %q, want cleared after retrip", state.FailureBreaker.CanaryIssueID)
	}

	reloadAt := canaryAt.Add(10 * time.Second)
	orch.reloadProjectFailureBreaker(&state, config, reloadAt)
	if !projectFailureBreakerAllowsDispatch(&state, reloadAt) {
		t.Fatal("workflow reload did not make one canary eligible")
	}
	canary, allowed = tryReserveProjectFailureBreakerCanary(&state, "canary-2", reloadAt)
	if !canary || !allowed {
		t.Fatalf("tryReserveProjectFailureBreakerCanary() after reload = (%t, %t), want (true, true)", canary, allowed)
	}
	orch.recordProjectAttemptOutcome(&state, "canary-2", reloadAt.Add(time.Second), store.WorkAttemptTerminalFailure, errors.New("different error"), workAttemptErrorRunner, "different error")
	if state.FailureBreaker.Active() {
		t.Fatalf("FailureBreaker = %#v, want different class to close it", state.FailureBreaker)
	}

	successAt := reloadAt.Add(2 * time.Second)
	orch.recordProjectAttemptOutcome(&state, "issue-3", successAt, store.WorkAttemptTerminalFailure, errors.New("same error"), workAttemptErrorRunner, "same error")
	orch.recordProjectAttemptOutcome(&state, "issue-4", successAt.Add(time.Second), store.WorkAttemptTerminalFailure, errors.New("same error"), workAttemptErrorRunner, "same error")
	if !state.FailureBreaker.Active() {
		t.Fatal("FailureBreaker.Active() = false before success canary")
	}
	orch.recordProjectAttemptOutcome(&state, "in-flight-success", successAt.Add(2*time.Second), store.WorkAttemptTerminalSuccess, nil, "", "")
	if !state.FailureBreaker.Active() {
		t.Fatal("FailureBreaker.Active() = false after non-canary success")
	}
	canaryAt = state.FailureBreaker.ResumeAt
	canary, allowed = tryReserveProjectFailureBreakerCanary(&state, "canary-success", canaryAt)
	if !canary || !allowed {
		t.Fatalf("tryReserveProjectFailureBreakerCanary() for success = (%t, %t), want (true, true)", canary, allowed)
	}
	orch.recordProjectAttemptOutcome(&state, "canary-success", canaryAt, store.WorkAttemptTerminalSuccess, nil, "", "")
	if state.FailureBreaker.Active() {
		t.Fatalf("FailureBreaker = %#v, want success to close it", state.FailureBreaker)
	}
}
