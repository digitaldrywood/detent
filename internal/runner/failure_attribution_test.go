package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/backendcapacity"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestCertainFailureBypassesDecision(t *testing.T) {
	for _, err := range []error{ErrNativeRecoveryRequired, ErrExecutionAuthorityUnavailable, fmt.Errorf("fencing: %w", ErrExecutionAuthorityUnavailable), context.Canceled, NewCancellationCause(context.Canceled, "runner.lease_expired"), ErrSessionMemoryCeilingExceeded, ErrTurnDurationExceeded} {
		t.Run(err.Error(), func(t *testing.T) {
			var runner *Runner
			if got := runner.attributeFailure(t.Context(), RunRequest{}, err); !errors.Is(got, err) {
				t.Fatalf("certain failure changed: %v", got)
			}
		})
	}
}

func TestFailureEvidenceBounds(t *testing.T) {
	for _, err := range []error{
		&workspace.HookError{Hook: "after_create", ExitCode: 2, Output: strings.Repeat("x", 9000) + "tail"},
		&workspace.CommandError{Command: "git", ExitCode: 128, Output: strings.Repeat("x", 9000) + "tail"},
	} {
		evidence := failureEvidence(err, strings.Repeat("s", 600))
		if len(evidence.OutputTail) > 8000 || len(evidence.Step) > 512 || !strings.HasSuffix(evidence.OutputTail, "tail") || evidence.ExitCode == nil || evidence.RecentOtherIssueSuccess != nil || evidence.SourceChangedConfiguration != nil {
			t.Fatalf("bad failure evidence: step=%d tail=%d exit=%v", len(evidence.Step), len(evidence.OutputTail), evidence.ExitCode)
		}
	}
}

func TestFailureDecisionControlsRetryAndPublication(t *testing.T) {
	for _, test := range []struct {
		name, outcome string
		failure       error
		attempt       int
		decisionCalls int
	}{
		{"changed hook", "instance", fmt.Errorf("%w: %w", ErrWorkspacePreparation, &workspace.HookError{Hook: "after_create", ExitCode: 2, Output: "setup failed"}), 2, 0},
		{"checkout disk full", "instance", fmt.Errorf("%w: %w", ErrWorkspacePreparation, &workspace.CommandError{Command: "git", ExitCode: 128, Output: "no space left on device"}), 2, 0},
		{"validation OOM", "instance", failedStep("validation", &workspace.ValidationError{Result: gate.CommandResult{ExitCode: 137}, Output: "OOM killed"}), 2, 1},
		{"deterministic failure", "issue", failedStep("validation", &workspace.CommandError{Command: "go", ExitCode: 1, Output: "assertion failed"}), 3, 1},
		{"unknown validation", "unknown", failedStep("validation", errors.New("command failed")), 3, 1},
		{"unknown hook", "instance", fmt.Errorf("%w: hook failed", ErrWorkspacePreparation), 2, 0},
		{"transport", "instance", fmt.Errorf("receive: %w", io.ErrUnexpectedEOF), 2, 0},
		{"startup", "instance", backendcapacity.NewError(backendcapacity.Scope{}, backendcapacity.Details{Type: backendcapacity.ErrorTypeTransientOverload, Kind: backendcapacity.StartupFailureKind}, errors.New("backend exited")), 2, 0},
		{"prior issue verdict for hook", "instance", RestoreFailureDecision(fmt.Errorf("%w: hook failed", ErrWorkspacePreparation), tracker.FailureAttribution{Outcome: "issue", Reason: "prior verdict"}), 2, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := &fakeCodexClient{failureOutcome: test.outcome}
			r, err := NewRunner(Dependencies{Workflow: config.Workflow{Config: config.Config{Agents: config.Agents{Routes: []config.AgentRoute{{Name: "default", Backend: "codex", Default: true}}}}}, Workspace: &attributionWorkspace{fakeWorkspaceBackend: &fakeWorkspaceBackend{}, diff: workspace.Diff{Patch: "diff --git a/detent.yaml b/detent.yaml\n+after_create: ./setup.sh"}}, AgentBackend: backend})
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			execution := &testExecution{recovery: tracker.NativeRecovery{Lease: tracker.NativeLease{MachineID: "machine_example"}, Issue: tracker.NativeIssue{NativeReference: tracker.NativeReference{WorkItemID: "wi_current"}}, InstanceSuccesses: []tracker.FailureStepSuccess{{Step: "validation", MachineID: "machine_example", WorkItemID: "wi_other", CompletedAt: now.Add(-time.Minute)}}}}
			attributed := r.attributeFailure(t.Context(), RunRequest{Mode: RunModeImplement, Execution: execution}, test.failure)
			decision, ok := FailureDecision(attributed)
			if !ok || decision.Outcome != test.outcome || backend.failureCalls != test.decisionCalls || test.decisionCalls > 0 && (!backend.failureRequest.ReadOnly || !backend.failureRequest.TextOnly) || !errors.Is(attributed, test.failure) {
				t.Fatalf("decision=%+v calls=%d error=%v", decision, backend.failureCalls, attributed)
			}
			if test.decisionCalls > 0 && !strings.Contains(backend.failureRequest.Prompt, "after_create: ./setup.sh") {
				t.Fatal("source diff missing from decision input")
			}
			if test.name == "validation OOM" && (decision.Evidence.RecentOtherIssueSuccess == nil || !*decision.Evidence.RecentOtherIssueSuccess || !strings.Contains(backend.failureRequest.Prompt, `"recent_other_issue_success":true`)) {
				t.Fatal("recent instance success missing from decision input")
			}
			attributed = encodeSSHError(attributed).err()
			supervisor, err := NewSupervisor(attributionTestRunner{failure: attributed}, SupervisorConfig{FailureRetryBaseDelay: time.Second, OverloadRetryDelay: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			completion := supervisor.Run(t.Context(), RunRequest{Attempt: 2})
			failure := nativeTerminalFailure(completion.Err, time.Now(), false)
			if !completion.Retryable || completion.RetryAttempt != test.attempt || failure == nil || failure.Decision == nil || failure.Decision.Outcome != test.outcome || failure.Attribution != test.outcome {
				t.Fatalf("retryable=%v attempt=%d want=%d failure=%+v", completion.Retryable, completion.RetryAttempt, test.attempt, failure)
			}
		})
	}
}

type attributionTestRunner struct{ failure error }

func (r attributionTestRunner) Run(context.Context, RunRequest) (RunResult, error) {
	return RunResult{}, r.failure
}

func TestFailureEvidenceCollectsBoundedSourceDiff(t *testing.T) {
	for _, test := range []struct {
		name string
		diff workspace.Diff
		want string
	}{
		{"changed hook", workspace.Diff{Patch: "diff --git a/detent.yaml b/detent.yaml\n+after_create: ./setup.sh"}, "diff --git a/detent.yaml b/detent.yaml\n+after_create: ./setup.sh"},
		{"truncated diff remains missing", workspace.Diff{Patch: "partial", Truncated: true}, ""},
		{"oversized backend result remains missing", workspace.Diff{Patch: strings.Repeat("x", 4001)}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := &attributionWorkspace{fakeWorkspaceBackend: &fakeWorkspaceBackend{}, diff: test.diff}
			r := &Runner{workspace: backend}
			evidence := r.collectFailureEvidence(t.Context(), RunRequest{}, errors.New("setup failed"))
			if backend.limit != 4000 || evidence.SourceDiff != test.want || evidence.SourceChangedConfiguration != nil {
				t.Fatalf("source evidence=%+v limit=%d", evidence, backend.limit)
			}
		})
	}
}

type attributionWorkspace struct {
	*fakeWorkspaceBackend
	diff  workspace.Diff
	limit int
}

func (w *attributionWorkspace) Existing(workspace.Issue) (workspace.Info, error) {
	return workspace.Info{}, nil
}

func (w *attributionWorkspace) Diff(_ context.Context, _ workspace.Info, _ workspace.Issue, limit int) (workspace.Diff, error) {
	w.limit = limit
	return w.diff, nil
}

func TestFailureEvidenceRejectsUnrelatedInstanceSuccess(t *testing.T) {
	now := time.Now().UTC()
	for _, test := range []struct {
		name    string
		success tracker.FailureStepSuccess
	}{
		{"other machine", tracker.FailureStepSuccess{MachineID: "machine_other", WorkItemID: "wi_other", Step: "validation", CompletedAt: now}},
		{"same issue", tracker.FailureStepSuccess{MachineID: "machine_example", WorkItemID: "wi_current", Step: "validation", CompletedAt: now}},
		{"other step", tracker.FailureStepSuccess{MachineID: "machine_example", WorkItemID: "wi_other", Step: "commit", CompletedAt: now}},
		{"old", tracker.FailureStepSuccess{MachineID: "machine_example", WorkItemID: "wi_other", Step: "validation", CompletedAt: now.Add(-time.Hour)}},
		{"future", tracker.FailureStepSuccess{MachineID: "machine_example", WorkItemID: "wi_other", Step: "validation", CompletedAt: now.Add(time.Hour)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := &Runner{now: func() time.Time { return now }, workspace: &fakeWorkspaceBackend{}}
			execution := &testExecution{recovery: tracker.NativeRecovery{Lease: tracker.NativeLease{MachineID: "machine_example"}, Issue: tracker.NativeIssue{NativeReference: tracker.NativeReference{WorkItemID: "wi_current"}}, InstanceSuccesses: []tracker.FailureStepSuccess{test.success}}}
			evidence := r.collectFailureEvidence(t.Context(), RunRequest{Execution: execution}, failedStep("validation", errors.New("failed")))
			if evidence.RecentOtherIssueSuccess != nil {
				t.Fatalf("unrelated success accepted: %+v", evidence)
			}
		})
	}
}
