package runner

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/runner/phaserecovery"
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
			got := phaserecovery.Checkpoint(tracker.NativeRecovery{Attempts: test.attempts, SourceVersionID: test.transfer}, test.current)
			if (got != nil) != test.want {
				t.Fatalf("restorable = %#v, want %t", got, test.want)
			}
			if got != nil && (got.Ref != tracker.CheckpointRefPrefix+"work" || got.CommitSHA != strings.Repeat("c", 40) || got.TreeSHA != strings.Repeat("e", 40)) {
				t.Fatalf("restorable = %#v", got)
			}
		})
	}
}

func TestIncorporationRefusalRestoresPublishedSource(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name           string
		gitRef         bool
		retained       bool
		dirty          bool
		uncertain      bool
		stale          bool
		unpublished    bool
		noFinalization bool
		unpushed       bool
		files          int
		wantFresh      bool
		wantPushed     bool
	}{
		{name: "clean integration git checkpoint yields to published source", gitRef: true, wantFresh: true},
		{name: "clean local integration checkpoint yields to published source", wantFresh: true},
		{name: "retained clean integration workspace yields to published source", gitRef: true, retained: true, wantFresh: true},
		{name: "retained clean local checkpoint without finalization restores current source", retained: true, noFinalization: true, wantFresh: true},
		{name: "retained unpushed checkpoint without finalization preserves source", retained: true, noFinalization: true, unpushed: true},
		{name: "dirty checkpoint preserves unpublished edits", gitRef: true, dirty: true, wantPushed: true},
		{name: "uncertain publication retains custody", gitRef: true, uncertain: true},
		{name: "different version cannot redirect recovery", gitRef: true, stale: true, wantPushed: true},
		{name: "empty clean publication mismatch restores current source", gitRef: true, unpublished: true, wantFresh: true},
		{name: "empty unpushed publication mismatch restores current source", gitRef: true, unpublished: true, unpushed: true, wantFresh: true},
		{name: "retained empty publication mismatch restores current source", gitRef: true, retained: true, unpublished: true, unpushed: true, wantFresh: true},
		{name: "unpublished source edits retain their checkpoint", gitRef: true, unpublished: true, unpushed: true, files: 1, wantPushed: true},
		{name: "dirty publication mismatch preserves source", gitRef: true, unpublished: true, dirty: true, wantPushed: true},
		{name: "uncertain publication mismatch retains custody", gitRef: true, unpublished: true, unpushed: true, uncertain: true},
		{name: "stale publication mismatch cannot redirect recovery", gitRef: true, unpublished: true, unpushed: true, stale: true, wantPushed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			checkpoint := &tracker.NativeCheckpoint{Storage: "local_only", Availability: "available", Resume: "fresh_checkout", WorktreeState: "clean", HeadSHA: "integration", ExternalEffect: "none", EffectState: "none"}
			if test.gitRef {
				remoteCheckpoint(checkpoint)
				checkpoint.HeadSHA = "integration"
			}
			if test.dirty {
				checkpoint.WorktreeState = "dirty"
			}
			if test.unpushed {
				checkpoint.WorktreeState = "unpushed"
			}
			if test.uncertain {
				checkpoint.ExternalEffect, checkpoint.EffectState = "git_push", "ambiguous"
			}
			ref := &tracker.NativeChangeReference{ChangeID: "change", VersionID: "version", HeadSHA: "published"}
			if test.stale {
				ref.VersionID = "previous"
			}
			recovery := tracker.NativeRecovery{Lease: tracker.NativeLease{MachineID: "machine"}, Attempts: []tracker.NativeAttempt{{
				NativeRunData: tracker.NativeRunData{MachineID: "machine", Finalization: &tracker.NativeFinalization{ChangeID: ref.ChangeID, VersionID: ref.VersionID, HeadSHA: ref.HeadSHA, SourceVersion: ref}},
				Checkpoint:    checkpoint,
			}}}
			if test.unpublished {
				f := recovery.Attempts[0].Finalization
				f.VersionID, f.Settled, f.Changed, f.Files = "", true, true, test.files
			}
			if test.noFinalization {
				recovery.Attempts[0].Finalization = nil
			}
			backend := existingWorkspaceBackend{err: workspace.ErrMissingWorkspace}
			if test.retained {
				backend.err = nil
			}
			issue := workspace.Issue{Source: &workspace.ChangeSource{Version: tracker.ChangeVersion{ID: "version", ChangeID: "change", ChangeVersionInput: tracker.ChangeVersionInput{HeadSHA: "published"}}}}
			restoreRunWorkspaceCheckpoint(RunRequest{Execution: &testExecution{recovery: recovery}}, backend, &issue, false)
			if issue.FreshCheckout != test.wantFresh || (issue.Checkpoint != nil) != test.wantPushed {
				t.Fatalf("recovery replayed integration checkpoint: fresh=%t checkpoint=%+v", issue.FreshCheckout, issue.Checkpoint)
			}
		})
	}
}

func TestInterruptedFenceKeepsCleanCheckpointWorktree(t *testing.T) {
	t.Parallel()
	scope := workspace.AttemptWorkspace{Organization: "org_example", Project: "prj_example", Attempt: "attempt_11111111111111111111111111111111"}
	for _, test := range []struct {
		name      string
		fence     tracker.FencingToken
		workspace error
		wantFresh bool
	}{
		{name: "same fence", fence: 1},
		{name: "different fence", fence: 2, wantFresh: true},
		{name: "missing worktree", fence: 1, workspace: workspace.ErrMissingWorkspace, wantFresh: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			checkpoint := &tracker.NativeCheckpoint{Storage: "local_only", Availability: "available", Resume: "fresh_checkout", WorktreeState: "clean", HeadSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ExternalEffect: "none", EffectState: "none"}
			recovery := tracker.NativeRecovery{
				Lease: tracker.NativeLease{ID: "lease_example", MachineID: "machine", FencingToken: test.fence},
				Attempts: []tracker.NativeAttempt{{
					NativeRunData: tracker.NativeRunData{AttemptID: scope.Attempt, LeaseID: "lease_example", MachineID: "machine", FencingToken: 1, Outcome: "interrupted"},
					Checkpoint:    checkpoint,
				}},
			}
			issue := workspace.Issue{AttemptWorkspace: scope}
			restoreRunWorkspaceCheckpoint(RunRequest{Execution: &testExecution{recovery: recovery}}, existingWorkspaceBackend{err: test.workspace}, &issue, false)
			if issue.FreshCheckout != test.wantFresh {
				t.Fatalf("fresh checkout = %t, want %t", issue.FreshCheckout, test.wantFresh)
			}
			if test.wantFresh && issue.BaseRef != checkpoint.HeadSHA {
				t.Fatalf("replacement base = %q, want %q", issue.BaseRef, checkpoint.HeadSHA)
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
			if got := checkpointRefCoversChangeSource(test.err, phaserecovery.Checkpoint(test.recovery, nil), test.landing, test.fresh); got != test.want {
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
	scope := workspace.AttemptWorkspace{Organization: "org_example", Project: "prj_example", Attempt: "attempt_11111111111111111111111111111111"}
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
			recovery := tracker.NativeRecovery{SourceAttemptID: scope.Attempt, Attempts: []tracker.NativeAttempt{{NativeRunData: tracker.NativeRunData{AttemptID: scope.Attempt, MachineID: tracker.MachineID(test.previous)}}}}
			recovery.Lease.MachineID = tracker.MachineID(test.lease)
			if sameMachineWorkspaceRetained(test.backend, recovery, workspace.Issue{ID: "issue"}) {
				t.Fatal("runner locality authorized an unscoped checkout")
			}
			if got := sameMachineWorkspaceRetained(test.backend, recovery, workspace.Issue{ID: "issue", AttemptWorkspace: scope}); got != test.want {
				t.Fatalf("sameMachineWorkspaceRetained() = %t, want %t", got, test.want)
			}
		})
	}
}
