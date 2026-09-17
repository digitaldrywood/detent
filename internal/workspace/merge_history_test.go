package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalGitMergePreservesRemoteHistory(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name        string
		recreate    bool
		resolved    bool
		advanceBase bool
		ahead       bool
		stale       bool
		interrupted bool
		untracked   bool
	}{
		{name: "recreated worktree", recreate: true},
		{name: "unfinished conflicting rebase", interrupted: true},
		{name: "untracked file blocks restoration", stale: true, untracked: true},
		{name: "local head contains remote", ahead: true},
		{name: "resolved head contains remote", ahead: true, resolved: true},
		{name: "fresh local branch at base"},
		{name: "local branch behind remote", stale: true},
		{name: "resolved branch behind remote", resolved: true},
		{name: "rebase preserves published changes", advanceBase: true},
		{name: "resolved merge preserves published history", advanceBase: true, resolved: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			source := initSourceRepo(t)
			remote := initBareRemote(t)
			runGit(t, source, "remote", "add", "origin", remote)
			runGit(t, source, "push", "-u", "origin", "main")
			backend, err := NewBackend(KindLocalGit, LocalGitOptions{
				Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			issue := Issue{Identifier: "DD-HISTORY"}
			info, err := backend.Create(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			base := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))
			for _, name := range []string{"first", "second"} {
				if err := os.WriteFile(filepath.Join(info.Path, name), []byte(name), 0o600); err != nil {
					t.Fatal(err)
				}
				runGit(t, info.Path, "add", name)
				runGit(t, info.Path, "commit", "-m", name)
			}
			head := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))
			runGit(t, info.Path, "push", "origin", "HEAD:"+info.Branch)
			remoteHead := head
			if tt.ahead {
				runGit(t, info.Path, "commit", "--allow-empty", "-m", "local follow-up")
			}
			if tt.recreate {
				runGit(t, source, "worktree", "remove", info.Path)
				runGit(t, source, "branch", "-D", info.Branch)
				runGit(t, source, "update-ref", "-d", "refs/remotes/origin/"+info.Branch)
				info, err = backend.Create(t.Context(), issue)
				if err != nil {
					t.Fatal(err)
				}
				if got := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD")); got != head {
					t.Errorf("recreated head = %s, want remote %s", got, head)
				}
			} else if tt.advanceBase {
				if err := os.WriteFile(filepath.Join(source, "main-change"), []byte("main change"), 0o600); err != nil {
					t.Fatal(err)
				}
				runGit(t, source, "add", "main-change")
				runGit(t, source, "commit", "-m", "advance main")
				runGit(t, source, "push", "origin", "main")
				if tt.resolved {
					runGit(t, info.Path, "merge", "--no-edit", "origin/main")
				}
			} else if tt.interrupted {
				if err := os.WriteFile(filepath.Join(source, "first"), []byte("base conflict"), 0o600); err != nil {
					t.Fatal(err)
				}
				runGit(t, source, "add", "first")
				runGit(t, source, "commit", "-m", "conflict with published change")
				runGit(t, source, "push", "origin", "main")
				if _, err := runGitAt(t.Context(), info.Path, "rebase", "origin/main"); err == nil {
					t.Fatal("expected conflicting rebase")
				}
			} else if !tt.ahead {
				if tt.stale {
					runGit(t, info.Path, "reset", "--hard", "HEAD~1")
				} else {
					runGit(t, info.Path, "reset", "--hard", base)
				}
			}
			if tt.untracked {
				if err := os.WriteFile(filepath.Join(info.Path, "second"), []byte("untracked work"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			result, err := backend.(MergePreparer).PrepareMerge(t.Context(), info, issue, MergePrepareOptions{
				TargetBranch: "main", VerifyResolution: tt.resolved, ExpectedRemoteHead: head,
			})
			if err != nil {
				t.Fatal(err)
			}
			wantStatus := MergePrepareStatusConflict
			if tt.recreate || tt.ahead || tt.advanceBase {
				wantStatus = MergePrepareStatusClean
			}
			wantChanged := tt.ahead || tt.advanceBase
			if result.Status != wantStatus || result.HeadChanged != wantChanged {
				t.Errorf("PrepareMerge = %#v, want status %s, head changed %t", result, wantStatus, wantChanged)
			}
			if wantChanged {
				head = strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))
				if !tt.advanceBase || tt.resolved {
					runGit(t, info.Path, "merge-base", "--is-ancestor", remoteHead, head)
				}
			}
			for _, name := range []string{"first", "second"} {
				want := name
				if tt.untracked && name == "second" {
					want = "untracked work"
				}
				if got := readFile(t, filepath.Join(info.Path, name)); got != want {
					t.Errorf("preserved %s = %q", name, got)
				}
			}
			if tt.untracked && !strings.Contains(result.Message, "restore remote PR head") {
				t.Errorf("missing restoration diagnostic: %q", result.Message)
			}
			if tt.interrupted {
				if inProgress, err := rebaseInProgress(t.Context(), info.Path); err != nil || inProgress {
					t.Fatalf("rebase remains in progress: %t, %v", inProgress, err)
				}
			}
			if result.Status == MergePrepareStatusConflict && !tt.untracked {
				if got := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD")); got != remoteHead {
					t.Errorf("refused workspace head = %s, want remote %s", got, remoteHead)
				}
			}
			if got := strings.Fields(runGit(t, source, "ls-remote", "origin", "refs/heads/"+info.Branch))[0]; got != head {
				t.Errorf("remote head = %s, want preserved %s", got, head)
			}
		})
	}
}
