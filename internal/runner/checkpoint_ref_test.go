package runner

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
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
	}{
		{name: "no prior attempt"},
		{name: "local-only checkpoint", attempts: []tracker.NativeAttempt{{NativeRunData: tracker.NativeRunData{AttemptID: "a"}, Checkpoint: &tracker.NativeCheckpoint{Storage: "local_only", WorktreeState: "dirty"}}}},
		{name: "pushed checkpoint", attempts: []tracker.NativeAttempt{{NativeRunData: tracker.NativeRunData{AttemptID: "a"}, Checkpoint: pushed()}}, want: true},
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
			got := nativeRestorableCheckpoint(tracker.NativeRecovery{Attempts: test.attempts}, test.current)
			if (got != nil) != test.want {
				t.Fatalf("restorable = %#v, want %t", got, test.want)
			}
			if got != nil && (got.Ref != tracker.CheckpointRefPrefix+"work" || got.CommitSHA != strings.Repeat("c", 40) || got.TreeSHA != strings.Repeat("e", 40)) {
				t.Fatalf("restorable = %#v", got)
			}
		})
	}
}

func TestCheckpointRefsPushAfterCompletedTurn(t *testing.T) {
	t.Parallel()
	backend := &wipExecutionWorkspace{retainedExecutionWorkspace: retainedExecutionWorkspace{fakeWorkspaceBackend: &fakeWorkspaceBackend{recoveryStates: []workspace.RecoveryState{{TrackedPaths: []string{"work.go"}}}}}}
	recorded := make(chan tracker.NativeCheckpoint, 1)
	execution := &testExecution{onCheckpoint: func(c tracker.NativeCheckpoint) { recorded <- c }}
	r := &Runner{workspace: backend, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	refs := newCheckpointRefs(backend, execution, workspace.Info{}, workspace.Issue{}, nil)
	req := RunRequest{Execution: execution, Issue: connector.Issue{ID: "work"}}
	r.startCheckpointRefs(t.Context(), refs, req, backend)
	defer refs.halt()
	refs.requestPush()
	select {
	case checkpoint := <-recorded:
		if !checkpoint.GitRef() || checkpoint.CommitSHA != strings.Repeat("c", 40) || checkpoint.WorktreeState != "dirty" {
			t.Fatalf("recorded checkpoint = %#v", checkpoint)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("completed turn did not push a checkpoint")
	}
}

func TestCheckpointRefCoversChangeSource(t *testing.T) {
	t.Parallel()
	pushed := &tracker.NativeCheckpoint{Resume: "fresh_checkout", WorktreeState: "dirty", ExternalEffect: "none", EffectState: "none"}
	remoteCheckpoint(pushed)
	withRef := tracker.NativeRecovery{Attempts: []tracker.NativeAttempt{{NativeRunData: tracker.NativeRunData{AttemptID: "a"}, Checkpoint: pushed}}}
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
