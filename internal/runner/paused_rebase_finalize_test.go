package runner

import (
	"context"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type pausedRebaseStepExecution struct {
	stubStepExecution
	stepCalls     int
	recordCalls   int
	previousStep  tracker.NativeStepReceipt
	validateCalls int
}

type stubStepExecution struct {
	Execution
}

func (e *pausedRebaseStepExecution) Recovery() tracker.NativeRecovery {
	var issue tracker.NativeIssue
	issue.ProjectID, issue.WorkItemID, issue.Revision = "prj_1", "wi_1", 1
	return tracker.NativeRecovery{
		Lease: tracker.NativeLease{ID: "lease_1", FencingToken: 1},
		Issue: issue,
	}
}

func (e *pausedRebaseStepExecution) Validate(context.Context) error {
	e.validateCalls++
	return nil
}

func (e *pausedRebaseStepExecution) Step(context.Context, tracker.NativeStepReceipt) (tracker.NativeStepReceipt, error) {
	e.stepCalls++
	return e.previousStep, nil
}

func (e *pausedRebaseStepExecution) RecordStep(context.Context, tracker.NativeStepReceipt) error {
	e.recordCalls++
	return nil
}

type pausedRebaseBackend struct {
	workspace.Backend
	state         workspace.RecoveryState
	finalizeCalls int
}

func (b *pausedRebaseBackend) RecoveryState(context.Context, workspace.Info, workspace.Issue) (workspace.RecoveryState, error) {
	return b.state, nil
}

func (b *pausedRebaseBackend) FinalizeNativeWork(context.Context, workspace.Info, workspace.Issue, func(context.Context) error) (string, error) {
	b.finalizeCalls++
	b.state.RebaseInProgress = false
	return "base_sha_new", nil
}

func TestFinalizeNativeCommitRunsDuringPausedRebase(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name              string
		rebaseInProgress  bool
		wantWorktreeState string
		wantFinalizeCalls int
	}{
		{name: "clean tree skips finalization when prior clean commit receipt exists", rebaseInProgress: false, wantWorktreeState: "clean", wantFinalizeCalls: 0},
		{name: "paused rebase with clean tree marks dirty and runs FinalizeNativeWork", rebaseInProgress: true, wantWorktreeState: "dirty", wantFinalizeCalls: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			state := workspace.RecoveryState{HeadSHA: "head_1", WorkspaceFingerprint: "fp_1", RebaseInProgress: tc.rebaseInProgress}
			if got := executionCheckpoint(&state).WorktreeState; got != tc.wantWorktreeState {
				t.Fatalf("WorktreeState = %q, want %q", got, tc.wantWorktreeState)
			}
			exec := &pausedRebaseStepExecution{
				previousStep: tracker.NativeStepReceipt{
					Step:             "commit",
					FinalizedBaseSHA: "base_sha_old",
					Source:           tracker.NativeCheckpoint{HeadSHA: "head_1", WorktreeState: "clean"},
				},
			}
			backend := &pausedRebaseBackend{state: state}
			r := &Runner{now: time.Now}
			_, err := r.finalizeNativeCommit(t.Context(), RunRequest{Execution: exec}, backend, workspace.Info{Path: "/tmp/ws"}, workspace.Issue{Identifier: "owner/repository#1"}, backend)
			if err != nil {
				t.Fatalf("finalizeNativeCommit() error = %v", err)
			}
			if backend.finalizeCalls != tc.wantFinalizeCalls {
				t.Fatalf("FinalizeNativeWork calls = %d, want %d", backend.finalizeCalls, tc.wantFinalizeCalls)
			}
		})
	}
}
