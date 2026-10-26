package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestNativeRestorableCheckpoint(t *testing.T) {
	t.Parallel()
	pushed := func() *tracker.NativeCheckpoint {
		c := &tracker.NativeCheckpoint{Resume: "fresh_checkout", WorktreeState: "dirty", ExternalEffect: "none", EffectState: "none"}
		remoteCheckpoint(c)
		return c
	}
	for _, test := range []struct {
		name     string
		attempts []tracker.NativeAttempt
		current  *workspace.ChangeSource
		want     bool
		transfer string
	}{
		{name: "no prior attempt"},
		{name: "local-only checkpoint", attempts: []tracker.NativeAttempt{{NativeRunData: tracker.NativeRunData{AttemptID: "a"}, Checkpoint: &tracker.NativeCheckpoint{Storage: "local_only", WorktreeState: "dirty"}}}},
		{name: "pushed checkpoint", attempts: []tracker.NativeAttempt{{NativeRunData: tracker.NativeRunData{AttemptID: "a"}, Checkpoint: pushed()}}, want: true},
		{name: "explicit transfer supersedes a later dirty checkpoint", transfer: "version_selected", attempts: []tracker.NativeAttempt{{NativeRunData: tracker.NativeRunData{AttemptID: "v"}}, {NativeRunData: tracker.NativeRunData{AttemptID: "a"}, Checkpoint: pushed()}}, current: &workspace.ChangeSource{Version: tracker.ChangeVersion{ChangeVersionInput: tracker.ChangeVersionInput{AttemptID: "v"}}}},
		{name: "pushed after its Change version", attempts: []tracker.NativeAttempt{{NativeRunData: tracker.NativeRunData{AttemptID: "v"}}, {NativeRunData: tracker.NativeRunData{AttemptID: "a"}, Checkpoint: pushed()}}, current: &workspace.ChangeSource{Version: tracker.ChangeVersion{ChangeVersionInput: tracker.ChangeVersionInput{AttemptID: "v"}}}, want: true},
		{name: "newer Change version supersedes it", attempts: []tracker.NativeAttempt{{NativeRunData: tracker.NativeRunData{AttemptID: "a"}, Checkpoint: pushed()}, {NativeRunData: tracker.NativeRunData{AttemptID: "v"}}}, current: &workspace.ChangeSource{Version: tracker.ChangeVersion{ChangeVersionInput: tracker.ChangeVersionInput{AttemptID: "v"}}}},
		{name: "uncertain publication stays with reconciliation", attempts: []tracker.NativeAttempt{{NativeRunData: tracker.NativeRunData{AttemptID: "a"}, Checkpoint: func() *tracker.NativeCheckpoint {
			c := pushed()
			c.ExternalEffect, c.EffectState = "pr_create", "pending"
			return c
		}()}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := nativeRestorableCheckpoint(tracker.NativeRecovery{Attempts: test.attempts, SourceVersionID: test.transfer}, test.current)
			if (got != nil) != test.want {
				t.Fatalf("restorable = %#v, want %t", got, test.want)
			}
			if got != nil && (got.Ref != tracker.CheckpointRefPrefix+"work" || got.CommitSHA != strings.Repeat("c", 40) || got.TreeSHA != strings.Repeat("e", 40)) {
				t.Fatalf("restorable = %#v", got)
			}
		})
	}
}

type checkpointFinalizingWorkspace struct {
	*wipExecutionWorkspace
	onFinalize func() error
	unchanged  bool
}

func (w *checkpointFinalizingWorkspace) PushCheckpointRef(ctx context.Context, info workspace.Info, issue workspace.Issue, previous workspace.CheckpointRef, validate func(context.Context) error) (workspace.CheckpointRef, bool, error) {
	if w.unchanged {
		return previous, false, validate(ctx)
	}
	return w.wipExecutionWorkspace.PushCheckpointRef(ctx, info, issue, previous, validate)
}

func (w *checkpointFinalizingWorkspace) FinalizeNativeWork(ctx context.Context, _ workspace.Info, _ workspace.Issue, validate func(context.Context) error) (string, error) {
	if err := w.onFinalize(); err != nil {
		return "", err
	}
	return "", validate(ctx)
}

func TestCheckpointRefsPushAfterCompletedTurn(t *testing.T) {
	t.Parallel()
	backend := &checkpointFinalizingWorkspace{wipExecutionWorkspace: &wipExecutionWorkspace{retainedExecutionWorkspace: retainedExecutionWorkspace{fakeWorkspaceBackend: &fakeWorkspaceBackend{recoveryStates: []workspace.RecoveryState{{TrackedPaths: []string{"work.go"}}}}}}}
	recorded := make(chan tracker.NativeCheckpoint, 1)
	execution := &testExecution{onCheckpoint: func(c tracker.NativeCheckpoint) { recorded <- c }}
	r := &Runner{workspace: backend, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), afterRunTimeout: time.Second}
	refs := newCheckpointRefs(backend, execution, workspace.Info{}, workspace.Issue{}, nil)
	req := RunRequest{Execution: execution, Issue: connector.Issue{ID: "work"}}
	r.startCheckpointRefs(t.Context(), refs, req, backend)
	stopped := false
	stop := refs.stop
	refs.stop = func() {
		stop()
		stopped = true
	}
	defer refs.halt()
	refs.requestPush()
	select {
	case checkpoint := <-recorded:
		if !checkpoint.GitRef() || checkpoint.CommitSHA != strings.Repeat("c", 40) || checkpoint.WorktreeState != "dirty" {
			t.Fatalf("recorded checkpoint = %#v", checkpoint)
		}
	case <-testenv.WaitContext(t, t.Context()).Done():
		t.Fatal("completed turn did not push a checkpoint")
	}
	finalized := false
	backend.onFinalize = func() error {
		if !stopped {
			return errors.New("host finalization overlapped the turn checkpoint publisher")
		}
		finalized = true
		return nil
	}
	req.checkpointRefs, req.finalizeNativeWork = refs, true
	if _, err := r.prepareExecutionCompletion(t.Context(), req, backend, workspace.Info{}, workspace.Issue{}, AgentResume{}, true); err != nil {
		t.Fatal(err)
	}
	if !finalized {
		t.Fatal("completed turn did not finalize its source")
	}
}

type checkpointCaptureWorkspace struct {
	*workspace.LocalGit
	afterCapture func()
}

func (w *checkpointCaptureWorkspace) RecoveryState(ctx context.Context, info workspace.Info, issue workspace.Issue) (workspace.RecoveryState, error) {
	state, err := w.LocalGit.RecoveryState(ctx, info, issue)
	if w.afterCapture != nil {
		change := w.afterCapture
		w.afterCapture = nil
		change()
	}
	return state, err
}

func TestCheckpointRefsCaptureUnchangedSnapshot(t *testing.T) {
	isolateAttemptDiffGitConfig(t)
	for _, test := range []struct {
		name               string
		staged             bool
		changeAfterCapture bool
	}{
		{name: "worktree to index"},
		{name: "index to worktree", staged: true},
		{name: "different content after capture", changeAfterCapture: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := initRunnerSourceRepo(t)
			remote := filepath.Join(t.TempDir(), "origin.git")
			runRunnerGit(t, source, "init", "--bare", remote)
			runRunnerGit(t, source, "remote", "add", "origin", remote)
			runRunnerGit(t, source, "push", "-u", "origin", "main")
			local, err := workspace.NewLocalGit(workspace.LocalGitOptions{Root: filepath.Dir(source), SourceRoot: source})
			if err != nil {
				t.Fatal(err)
			}
			backend := &checkpointCaptureWorkspace{LocalGit: local}
			info := workspace.Info{Path: source, Branch: "main"}
			issue := workspace.Issue{ID: "work", Identifier: "work", BaseRef: "origin/main"}
			write := func(content string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(source, "README.md"), []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			write("edited\n")
			if test.staged {
				runRunnerGit(t, source, "add", "README.md")
			}
			execution := &retryStepExecution{finalizingTestExecution: &finalizingTestExecution{}}
			r := &Runner{workspace: backend, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), now: time.Now}
			refs := newCheckpointRefs(backend, execution, info, issue, nil)
			req := RunRequest{Execution: execution, checkpointRefs: refs}
			if !r.pushCheckpointRef(t.Context(), refs, req, "turn_completed") {
				t.Fatal("turn did not publish its checkpoint")
			}
			published, captured := refs.last, *refs.captured
			if test.staged {
				runRunnerGit(t, source, "restore", "--staged", "README.md")
			} else {
				runRunnerGit(t, source, "add", "README.md")
			}
			state, err := backend.RecoveryState(t.Context(), info, issue)
			if err != nil {
				t.Fatal(err)
			}
			if state.WorkspaceFingerprint == captured.WorkspaceDigest {
				t.Fatal("staging did not change recovery fingerprint")
			}
			if r.pushCheckpointRef(t.Context(), refs, req, "turn_completed") {
				t.Fatal("equivalent tree published another checkpoint")
			}
			if refs.last != published || refs.captured.WorkspaceDigest != state.WorkspaceFingerprint {
				t.Fatal("no-op did not retain publication and refresh captured state")
			}
			if test.changeAfterCapture {
				backend.afterCapture = func() { write("different content\n") }
			}
			err = r.recordAgentStep(t.Context(), req, backend, info, issue, "complete")
			if test.changeAfterCapture {
				if !errors.Is(err, ErrNativeRecoveryRequired) || len(execution.steps) != 0 {
					t.Fatalf("changed source: steps=%+v err=%v", execution.steps, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(execution.steps) != 1 || execution.steps[0].Source.WorkspaceDigest != state.WorkspaceFingerprint || execution.steps[0].Source.CommitSHA != published.CommitSHA || execution.steps[0].Source.TreeSHA != published.TreeSHA {
				t.Fatalf("completed turn lost current fingerprint or published snapshot: %+v", execution.steps)
			}
		})
	}
}

func TestCheckpointRefsRetainAcknowledgedSource(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		failure   string
		selection string
		wantSaved bool
	}{
		{name: "selected checkpoint capture failure", failure: "capture", selection: "checkpoint", wantSaved: true},
		{name: "selected checkpoint acknowledgement failure", failure: "acknowledgement", selection: "checkpoint", wantSaved: true},
		{name: "newer Change capture failure", failure: "capture", selection: "change"},
		{name: "fresh checkout capture failure", failure: "capture", selection: "fresh"},
		{name: "newer retained workspace capture failure", failure: "capture", selection: "retained"},
		{name: "newer Change acknowledgement failure", failure: "acknowledgement", selection: "change"},
		{name: "different selected checkpoint capture failure", failure: "capture", selection: "different checkpoint"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			previous := tracker.NativeCheckpoint{HeadSHA: strings.Repeat("b", 40), WorkspaceDigest: "acknowledged", WorktreeState: "dirty", ExternalEffect: "none", EffectState: "none"}
			remoteCheckpoint(&previous)
			backend := &wipExecutionWorkspace{retainedExecutionWorkspace: retainedExecutionWorkspace{fakeWorkspaceBackend: &fakeWorkspaceBackend{recoveryStates: []workspace.RecoveryState{{HeadSHA: strings.Repeat("a", 40), WorkspaceFingerprint: "new edits", TrackedPaths: []string{"work.go"}}}}}}
			execution := &testExecution{recovery: tracker.NativeRecovery{SourceAttemptID: "prior", Attempts: []tracker.NativeAttempt{{NativeRunData: tracker.NativeRunData{AttemptID: "prior"}, Checkpoint: &previous}}}}
			issue := workspace.Issue{NativeRework: true}
			switch test.selection {
			case "checkpoint", "different checkpoint":
				issue.Checkpoint = &workspace.CheckpointRef{Ref: previous.Ref, CommitSHA: previous.CommitSHA, BaseSHA: previous.BaseSHA, TreeSHA: previous.TreeSHA}
				if test.selection == "different checkpoint" {
					issue.Checkpoint.CommitSHA = strings.Repeat("d", 40)
				}
			case "change":
				issue.Source = &workspace.ChangeSource{Version: tracker.ChangeVersion{ChangeVersionInput: tracker.ChangeVersionInput{HeadSHA: strings.Repeat("a", 40), AttemptID: "newer"}}}
			case "fresh":
				issue.FreshCheckout = true
			}
			refs := newCheckpointRefs(backend, execution, workspace.Info{}, issue, nil)
			initial := executionCheckpoint(&workspace.RecoveryState{HeadSHA: strings.Repeat("a", 40), WorkspaceFingerprint: "new edits", TrackedPaths: []string{"work.go"}})
			refs.apply(&initial)
			if issue.Checkpoint == nil && initial.GitRef() {
				t.Fatalf("unselected checkpoint attached to current source: %#v", initial)
			}
			r := &Runner{workspace: backend, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
			errUnavailable := errors.New("unavailable")
			if test.failure == "capture" {
				backend.publishErr = errUnavailable
			} else {
				execution.checkpointErr = errUnavailable
			}
			r.pushCheckpointRef(t.Context(), refs, RunRequest{}, "stage_finished")
			candidate := executionCheckpoint(&workspace.RecoveryState{HeadSHA: strings.Repeat("a", 40), WorkspaceFingerprint: "new edits", TrackedPaths: []string{"work.go"}})
			refs.apply(&candidate)
			if test.failure == "acknowledgement" {
				if err := recordCheckpointRef(context.Background(), refs, execution, candidate); !errors.Is(err, errUnavailable) {
					t.Fatalf("acknowledgement = %v", err)
				}
			}
			retained := executionCheckpoint(&workspace.RecoveryState{HeadSHA: strings.Repeat("a", 40), WorkspaceFingerprint: "new edits", TrackedPaths: []string{"work.go"}})
			refs.apply(&retained)
			if test.wantSaved {
				if retained.CommitSHA != previous.CommitSHA || retained.HeadSHA != previous.HeadSHA || retained.WorkspaceDigest != previous.WorkspaceDigest {
					t.Fatalf("invented saved-work receipt: %#v", retained)
				}
				if refs.saved == nil || refs.saved.CommitSHA != previous.CommitSHA || refs.saved.WorkspaceDigest != previous.WorkspaceDigest {
					t.Fatalf("acknowledged source changed: %#v", refs.saved)
				}
			} else {
				if refs.saved != nil || retained.WorkspaceDigest != "new edits" || retained.HeadSHA != strings.Repeat("a", 40) {
					t.Fatalf("unselected source retained: saved=%#v checkpoint=%#v", refs.saved, retained)
				}
				if test.failure == "capture" && issue.Checkpoint == nil && retained.GitRef() {
					t.Fatalf("failed capture published unselected source: %#v", retained)
				}
				if issue.Checkpoint != nil && retained.CommitSHA != issue.Checkpoint.CommitSHA {
					t.Fatalf("failed capture replaced selected checkpoint: %#v", retained)
				}
			}
		})
	}
}

func TestCheckpointRefCoversChangeSource(t *testing.T) {
	t.Parallel()
	pushed := &tracker.NativeCheckpoint{Resume: "fresh_checkout", WorktreeState: "dirty", ExternalEffect: "none", EffectState: "none"}
	remoteCheckpoint(pushed)
	withRef := tracker.NativeRecovery{Attempts: []tracker.NativeAttempt{{NativeRunData: tracker.NativeRunData{AttemptID: "a"}, Checkpoint: pushed}}}
	transferred := withRef
	transferred.SourceVersionID = "version_selected"
	localOnly := tracker.NativeRecovery{Attempts: []tracker.NativeAttempt{{NativeRunData: tracker.NativeRunData{AttemptID: "a"}, Checkpoint: &tracker.NativeCheckpoint{Storage: "local_only", WorktreeState: "dirty"}}}}
	missing := fmt.Errorf("%w: missing Hub bundle", ErrNativeRecoveryRequired)
	for _, test := range []struct {
		name     string
		err      error
		recovery tracker.NativeRecovery
		landing  bool
		fresh    bool
		want     bool
	}{
		{name: "missing source with pushed checkpoint", err: missing, recovery: withRef, want: true},
		{name: "transfer cannot substitute checkpoint for missing source", err: missing, recovery: transferred},
		{name: "missing source with local checkpoint", err: missing, recovery: localOnly},
		{name: "landing needs the Change source", err: missing, recovery: withRef, landing: true},
		{name: "fresh checkout ignores checkpoints", err: missing, recovery: withRef, fresh: true},
		{name: "other failures still stop the run", err: errors.New("hub unavailable"), recovery: withRef},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := checkpointRefCoversChangeSource(test.err, test.recovery, test.landing, test.fresh); got != test.want {
				t.Fatalf("covers = %t, want %t", got, test.want)
			}
		})
	}
}

type existingWorkspaceBackend struct {
	workspace.Backend
	err error
}

func (b existingWorkspaceBackend) Existing(workspace.Issue) (workspace.Info, error) {
	return workspace.Info{}, b.err
}

func TestSameMachineWorkspaceRetained(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		previous string
		lease    string
		backend  workspace.Backend
		want     bool
	}{
		{name: "same runner keeps its retained workspace", previous: "machine", lease: "machine", backend: existingWorkspaceBackend{}, want: true},
		{name: "same runner without the workspace restores the ref", previous: "machine", lease: "machine", backend: existingWorkspaceBackend{err: workspace.ErrMissingWorkspace}},
		{name: "another runner restores the ref", previous: "machine", lease: "other", backend: existingWorkspaceBackend{}},
		{name: "unknown previous runner restores the ref", lease: "machine", backend: existingWorkspaceBackend{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			recovery := tracker.NativeRecovery{SourceAttemptID: "attempt", Attempts: []tracker.NativeAttempt{{NativeRunData: tracker.NativeRunData{AttemptID: "attempt", MachineID: tracker.MachineID(test.previous)}}}}
			recovery.Lease.MachineID = tracker.MachineID(test.lease)
			if got := sameMachineWorkspaceRetained(test.backend, recovery, workspace.Issue{ID: "issue"}); got != test.want {
				t.Fatalf("sameMachineWorkspaceRetained() = %t, want %t", got, test.want)
			}
		})
	}
}
