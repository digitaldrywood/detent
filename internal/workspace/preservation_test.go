package workspace

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalGitCleanupChecksEveryWorkspace(t *testing.T) {
	t.Parallel()
	for _, record := range []string{"none", "ordinary", "preserved"} {
		for _, work := range []string{"clean", "tracked", "staged", "untracked", "unpushed", "detached", "missing", "broken", "different branch", "no remote"} {
			t.Run(record+"/"+work, func(t *testing.T) {
				t.Parallel()
				source := initSourceRepo(t)
				if work != "no remote" {
					runGit(t, source, "remote", "add", "origin", initBareRemote(t))
					runGit(t, source, "push", "-u", "origin", "main")
				}
				backend, err := NewLocalGit(LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true})
				if err != nil {
					t.Fatal(err)
				}
				issue := Issue{Identifier: "cleanup-safety"}
				info, err := backend.Create(t.Context(), issue)
				if err != nil {
					t.Fatal(err)
				}
				if record != "none" {
					if err := backend.recordCleanupOwnership(t.Context(), info, issue, true); err != nil {
						t.Fatal(err)
					}
				}
				if record == "preserved" {
					if _, err := backend.PreserveIssue(t.Context(), issue); err != nil {
						t.Fatal(err)
					}
				}
				if work == "detached" {
					runGit(t, info.Path, "checkout", "--detach")
				}
				if work != "clean" && work != "no remote" && work != "broken" {
					name := "README.md"
					if work == "untracked" {
						name = "implementation.go"
					}
					if err := os.WriteFile(filepath.Join(info.Path, name), []byte("unique work\n"), 0o600); err != nil {
						t.Fatal(err)
					}
					if work != "tracked" && work != "untracked" {
						runGit(t, info.Path, "add", name)
						if work != "staged" {
							runGit(t, info.Path, "commit", "-m", "unique work")
						}
					}
				}
				if work == "different branch" {
					runGit(t, info.Path, "checkout", "--detach", "origin/main")
				}
				head := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))
				status := runGit(t, info.Path, "status", "--porcelain")
				if work == "missing" {
					if err := os.Rename(info.Path, filepath.Join(t.TempDir(), "moved")); err != nil {
						t.Fatal(err)
					}
				}
				if work == "broken" {
					if err := os.Rename(filepath.Join(info.Path, ".git"), filepath.Join(info.Path, "saved-git")); err != nil {
						t.Fatal(err)
					}
				}
				result, err := backend.CleanupIssue(t.Context(), issue)
				if work == "clean" {
					if err != nil || result.Worktrees != 1 || result.Branches != 1 {
						t.Fatalf("cleanup = %+v, %v; want removed workspace and branch", result, err)
					}
					return
				}
				if !errors.Is(err, ErrWorkspacePreserved) || result.Worktrees != 0 || result.Branches != 0 {
					t.Fatalf("cleanup = %+v, %v; want retained work", result, err)
				}
				if !branchExists(t, source, info.Branch) {
					t.Fatal("cleanup deleted branch holding retained work")
				}
				if work != "missing" && work != "broken" {
					if got := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD")); got != head {
						t.Fatalf("head = %q, want %q", got, head)
					}
					if got := runGit(t, info.Path, "status", "--porcelain"); got != status {
						t.Fatalf("status = %q, want %q", got, status)
					}
				}
			})
		}
	}
}

func TestLocalGitCleanupRecordedLanding(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		method        string
		terminal      bool
		wrongHead     bool
		verifiedPR    bool
		recorded      bool
		extraCommit   bool
		dirty         bool
		wantPreserved bool
	}{
		{name: "verified merged PR", method: "squash", terminal: true, verifiedPR: true},
		{name: "mismatched PR head", method: "squash", terminal: true, verifiedPR: true, wrongHead: true},
		{name: "verified PR later commit", method: "squash", terminal: true, verifiedPR: true, extraCommit: true},
		{name: "verified PR dirty", method: "squash", terminal: true, verifiedPR: true, dirty: true},
		{name: "active PR proof", method: "squash", verifiedPR: true, wantPreserved: true},
		{name: "squash landed", method: "squash", terminal: true, recorded: true},
		{name: "merge landed", method: "merge", terminal: true, recorded: true},
		{name: "fast-forward landed", method: "fast-forward", terminal: true, recorded: true},
		{name: "nonterminal squash", method: "squash", recorded: true, wantPreserved: true},
		{name: "terminal without landing", method: "squash", terminal: true},
		{name: "work after landing", method: "squash", terminal: true, recorded: true, extraCommit: true},
		{name: "uncommitted work after landing", method: "squash", terminal: true, recorded: true, dirty: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			source := initSourceRepo(t)
			publishCleanupSource(t, source)
			backend, err := NewLocalGit(LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true})
			if err != nil {
				t.Fatal(err)
			}
			issue := Issue{ProjectID: "native", ID: "wi_1", Identifier: "native#1", Terminal: tt.terminal}
			info, err := backend.Create(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(info.Path, "native.txt"), []byte("delivered work\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			runGit(t, info.Path, "add", "native.txt")
			runGit(t, info.Path, "commit", "-m", "native work")
			landedHead := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))
			runGit(t, info.Path, "push", "origin", info.Branch)
			switch tt.method {
			case "squash":
				runGit(t, source, "merge", "--squash", info.Branch)
				runGit(t, source, "commit", "-m", "squash native work")
			case "merge":
				runGit(t, source, "merge", "--no-ff", "-m", "merge native work", info.Branch)
			case "fast-forward":
				runGit(t, source, "merge", "--ff-only", info.Branch)
			}
			runGit(t, source, "push", "origin", "main")
			runGit(t, source, "push", "origin", "--delete", info.Branch)
			if tt.method == "squash" {
				count, err := retainedGitCommitCount(t.Context(), info.Path, "HEAD")
				if err != nil || count == 0 {
					t.Fatalf("original head should fail live branch proof: count %d, %v", count, err)
				}
			}
			if tt.verifiedPR {
				issue.CleanupDeliveredHeadSHA = landedHead
				if tt.wrongHead {
					issue.CleanupDeliveredHeadSHA = strings.TrimSpace(runGit(t, source, "rev-parse", "HEAD"))
				}
			}
			if tt.recorded {
				issue.LandedHeadSHA = landedHead
			}
			if tt.extraCommit {
				runGit(t, info.Path, "commit", "--allow-empty", "-m", "later work")
			}
			if tt.dirty {
				if err := os.WriteFile(filepath.Join(info.Path, "later.txt"), []byte("later work\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			result, err := backend.CleanupIssue(t.Context(), issue)
			if got := errors.Is(err, ErrWorkspacePreserved); got != tt.wantPreserved {
				t.Fatalf("CleanupIssue() = %+v, %v; want preserved %t", result, err, tt.wantPreserved)
			}
			if !tt.wantPreserved && err != nil {
				t.Fatalf("CleanupIssue() error = %v", err)
			}
			_, statErr := os.Stat(info.Path)
			if tt.wantPreserved != (statErr == nil) {
				t.Fatalf("workspace present = %t, want %t", statErr == nil, tt.wantPreserved)
			}
			if branchExists(t, source, info.Branch) != tt.wantPreserved {
				t.Fatalf("branch present = %t, want %t", branchExists(t, source, info.Branch), tt.wantPreserved)
			}
		})
	}
}

func TestLocalGitPreservesRevokedWorkAcrossCleanupAndRestart(t *testing.T) {
	t.Parallel()

	for _, kind := range []string{"tracked", "staged", "untracked", "unpushed", "pushed", "local only"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			source := initSourceRepo(t)
			remote := initBareRemote(t)
			if kind != "local only" {
				runGit(t, source, "remote", "add", "origin", remote)
				runGit(t, source, "push", "-u", "origin", "main")
			}
			opts := LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true}
			backend, err := NewLocalGit(opts)
			if err != nil {
				t.Fatal(err)
			}
			issue := Issue{ProjectID: "detent", ID: "2138", Identifier: "digitaldrywood/detent#2138"}
			info, err := backend.Create(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			name := "README.md"
			if kind == "untracked" {
				name = "implementation.go"
			}
			content := []byte("completed worker implementation\n")
			if err := os.WriteFile(filepath.Join(info.Path, name), content, 0o600); err != nil {
				t.Fatal(err)
			}
			if kind == "staged" || kind == "unpushed" || kind == "pushed" || kind == "local only" {
				runGit(t, info.Path, "add", name)
			}
			if kind == "unpushed" || kind == "pushed" || kind == "local only" {
				runGit(t, info.Path, "commit", "-m", "completed implementation")
			}
			if kind == "pushed" {
				runGit(t, info.Path, "push", "-u", "origin", info.Branch)
			}
			head := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))
			status := runGit(t, info.Path, "status", "--porcelain")
			preserved, err := backend.PreserveIssue(t.Context(), issue)
			if err != nil || !preserved.Preserved || preserved.Path != info.Path || preserved.HeadSHA != head {
				t.Fatalf("preservation = %#v, error = %v", preserved, err)
			}
			backend, err = NewLocalGit(opts)
			if err != nil {
				t.Fatal(err)
			}
			_, cleanupErr := backend.CleanupIssue(t.Context(), issue)
			if kind != "pushed" {
				if !errors.Is(cleanupErr, ErrWorkspacePreserved) {
					t.Fatalf("cleanup error = %v, want retained workspace", cleanupErr)
				}
				actual, err := os.ReadFile(filepath.Join(info.Path, name))
				if err != nil || string(actual) != string(content) {
					t.Fatalf("retained file = %q, error = %v", actual, err)
				}
				if got := runGit(t, info.Path, "status", "--porcelain"); got != status {
					t.Fatalf("retained index/status = %q, want %q", got, status)
				}
				if got := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD")); got != head {
					t.Fatalf("retained head = %q, want %q", got, head)
				}
				residual, err := backend.ReconcileResiduals(t.Context(), nil)
				if err != nil || residual.Removed != 0 || residual.PreservedSkipped != 1 {
					t.Fatalf("residual cleanup = %#v, error = %v", residual, err)
				}
				resumed, err := backend.Create(t.Context(), issue)
				if err != nil || resumed.Path != info.Path || resumed.Created {
					t.Fatalf("resumed workspace = %#v, error = %v", resumed, err)
				}
				if kind != "unpushed" && kind != "local only" {
					runGit(t, info.Path, "add", name)
					runGit(t, info.Path, "commit", "-m", "publish recovered implementation")
				}
				if kind == "local only" {
					runGit(t, source, "remote", "add", "origin", remote)
				}
				runGit(t, info.Path, "push", "-u", "origin", info.Branch)
				_, cleanupErr = backend.CleanupIssue(t.Context(), issue)
			}
			if cleanupErr != nil {
				t.Fatalf("cleanup after delivery: %v", cleanupErr)
			}
			if _, err := os.Stat(info.Path); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("delivered workspace remains: %v", err)
			}
		})
	}
}

func TestLocalGitPreservationVerifiesWorkEvidence(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"clean base", "dirty", "local commit", "pushed commit", "base branch pushed", "remote branch deleted"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			source := initSourceRepo(t)
			remote := initBareRemote(t)
			runGit(t, source, "remote", "add", "origin", remote)
			runGit(t, source, "push", "-u", "origin", "main")
			base := strings.TrimSpace(runGit(t, source, "rev-parse", "HEAD"))
			backend, err := NewLocalGit(LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true})
			if err != nil {
				t.Fatal(err)
			}
			issue := Issue{Identifier: "evidence", BaseRef: base}
			info, err := backend.Create(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			changed := kind == "dirty" || kind == "local commit" || kind == "pushed commit" || kind == "remote branch deleted"
			if changed {
				if err := os.WriteFile(filepath.Join(info.Path, "README.md"), []byte("worker implementation\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if kind != "dirty" {
					runGit(t, info.Path, "add", "README.md")
					runGit(t, info.Path, "commit", "-m", "implement change")
				}
			}
			if kind == "pushed commit" || kind == "base branch pushed" || kind == "remote branch deleted" {
				runGit(t, info.Path, "push", "origin", info.Branch)
			}
			if kind == "remote branch deleted" {
				runGit(t, remote, "update-ref", "-d", "refs/heads/"+info.Branch)
			}
			preserved, err := backend.PreserveIssue(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			if !preserved.LocalChangesVerified {
				t.Fatalf("unverified local evidence: %#v", preserved)
			}
			localWork := preserved.UnpushedCommits > 0 || len(preserved.TrackedPaths) > 0 || len(preserved.UntrackedPaths) > 0
			if localWork != (kind == "dirty" || kind == "local commit" || kind == "remote branch deleted") {
				t.Fatalf("local evidence = %#v", preserved)
			}
			delivery := preserved.Delivery
			delivered := delivery != nil && delivery.RemoteBranchExists && delivery.CommitsAhead > 0 && delivery.LocalHeadSHA == delivery.RemoteHeadSHA
			if delivered != (kind == "pushed commit") {
				t.Fatalf("delivery evidence = %#v", preserved)
			}
		})
	}
}

func TestLocalGitPreservationInspectionFailureKeepsFiles(t *testing.T) {
	t.Parallel()
	backend, err := NewLocalGit(LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: initSourceRepo(t), AutoBranch: true})
	if err != nil {
		t.Fatal(err)
	}
	issue := Issue{Identifier: "detent#2138"}
	info, err := backend.Create(t.Context(), issue)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.PreserveIssue(t.Context(), issue); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(info.Path, ".git"), filepath.Join(info.Path, "saved-git-pointer")); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.CleanupIssue(t.Context(), issue); !errors.Is(err, ErrWorkspacePreserved) {
		t.Fatalf("cleanup error = %v, want preservation on failed inspection", err)
	}
	result, err := backend.ReconcileResiduals(t.Context(), nil)
	if err != nil || result.PreservedSkipped != 1 || result.Removed != 0 {
		t.Fatalf("residual cleanup = %#v, error = %v", result, err)
	}
	if _, err := os.Stat(filepath.Join(info.Path, "README.md")); err != nil {
		t.Fatalf("retained file missing: %v", err)
	}
}

func TestFilesystemPreservationSurvivesRestartAndResumption(t *testing.T) {
	t.Parallel()
	for _, artifact := range []bool{false, true} {
		name := "empty"
		if artifact {
			name = "artifact"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			opts := FilesystemOptions{Root: filepath.Join(t.TempDir(), "workspaces")}
			backend, err := NewFilesystem(opts)
			if err != nil {
				t.Fatal(err)
			}
			issue := Issue{ProjectID: "video", ID: "2138", Identifier: "video#2138"}
			info, err := backend.Create(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			artifactPath := filepath.Join(info.Path, "artifacts", "render.mp4")
			if artifact {
				if err := os.WriteFile(artifactPath, []byte("completed render"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before, err := backend.DiffStat(t.Context(), info, issue)
			if err != nil {
				t.Fatal(err)
			}
			preserver, ok := any(backend).(IssuePreserver)
			if !ok {
				t.Fatal("filesystem backend does not support preservation")
			}
			preserved, err := preserver.PreserveIssue(t.Context(), issue)
			if err != nil || !preserved.Preserved || preserved.Path != info.Path {
				t.Fatalf("preservation = %#v, error = %v", preserved, err)
			}
			backend, err = NewFilesystem(opts)
			if err != nil {
				t.Fatal(err)
			}
			for _, resumed := range []bool{false, true} {
				if resumed {
					resumedInfo, err := backend.Create(t.Context(), issue)
					if err != nil || resumedInfo.Created || resumedInfo.Path != info.Path {
						t.Fatalf("resumption = %#v, error = %v", resumedInfo, err)
					}
				}
				if _, err := backend.CleanupIssue(t.Context(), issue); !errors.Is(err, ErrWorkspacePreserved) {
					t.Fatalf("cleanup error = %v, want preservation", err)
				}
			}
			after, err := backend.DiffStat(t.Context(), info, issue)
			if err != nil || after != before {
				t.Fatalf("retained artifact evidence = %#v, error = %v, want %#v", after, err, before)
			}
			if artifact {
				content, err := os.ReadFile(artifactPath)
				if err != nil || string(content) != "completed render" {
					t.Fatalf("retained artifact = %q, error = %v", content, err)
				}
			}
		})
	}
}

func TestLocalGitTerminalCleanupRequiresOwnershipRecord(t *testing.T) {
	t.Parallel()
	t.Run("unowned dirty worktree is preserved", func(t *testing.T) {
		t.Parallel()
		source := initSourceRepo(t)
		backend, err := NewLocalGit(LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true})
		if err != nil {
			t.Fatal(err)
		}
		issue := Issue{ID: "wi-unowned-wt", Identifier: "owner/repo#701", Terminal: true}
		info, err := backend.infoForIssue(issue)
		if err != nil {
			t.Fatal(err)
		}
		runGit(t, source, "worktree", "add", "-b", info.Branch, info.Path, "HEAD")
		if err := os.WriteFile(filepath.Join(info.Path, "README.md"), []byte("manual edits\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := backend.CleanupIssue(t.Context(), issue); !errors.Is(err, ErrWorkspacePreserved) {
			t.Fatalf("CleanupIssue() error = %v, want ErrWorkspacePreserved", err)
		}
		if _, err := os.Stat(filepath.Join(info.Path, "README.md")); err != nil {
			t.Fatalf("unowned dirty worktree was deleted: %v", err)
		}
	})
	t.Run("unowned branch with unpushed commits is preserved when worktree and record are absent", func(t *testing.T) {
		t.Parallel()
		source := initSourceRepo(t)
		backend, err := NewLocalGit(LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true})
		if err != nil {
			t.Fatal(err)
		}
		other := Issue{ID: "wi-other", Identifier: "owner/repo#702"}
		otherInfo, err := backend.Create(t.Context(), other)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(otherInfo.Path, "README.md"), []byte("other issue commit\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		runGit(t, otherInfo.Path, "add", "README.md")
		runGit(t, otherInfo.Path, "commit", "-m", "other issue work")
		runGit(t, source, "worktree", "remove", "--force", otherInfo.Path)
		issue := Issue{ID: "wi-terminal", Identifier: "owner/repo#703", BranchName: otherInfo.Branch, Terminal: true}
		if _, err := backend.CleanupIssue(t.Context(), issue); !errors.Is(err, ErrWorkspacePreserved) {
			t.Fatalf("CleanupIssue() error = %v, want ErrWorkspacePreserved", err)
		}
		exists, err := backend.branchExists(t.Context(), otherInfo.Branch)
		if err != nil || !exists {
			t.Fatalf("unowned branch %s was deleted (exists=%v, err=%v)", otherInfo.Branch, exists, err)
		}
	})
	t.Run("replaced worktree at same path is preserved", func(t *testing.T) {
		t.Parallel()
		source := initSourceRepo(t)
		backend, err := NewLocalGit(LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true})
		if err != nil {
			t.Fatal(err)
		}
		issue := Issue{ID: "wi-replaced", Identifier: "owner/repo#704"}
		info, err := backend.Create(t.Context(), issue)
		if err != nil {
			t.Fatal(err)
		}
		runGit(t, source, "worktree", "remove", "--force", info.Path)
		runGit(t, source, "worktree", "add", "-b", "manual-replacement", info.Path, "HEAD")
		if err := os.WriteFile(filepath.Join(info.Path, "README.md"), []byte("replacement edits\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := backend.PreserveIssue(t.Context(), issue); err != nil {
			t.Fatal(err)
		}
		issue.Terminal = true
		if _, err := backend.CleanupIssue(t.Context(), issue); !errors.Is(err, ErrWorkspacePreserved) {
			t.Fatalf("CleanupIssue() error = %v, want ErrWorkspacePreserved", err)
		}
		if _, err := os.Stat(filepath.Join(info.Path, "README.md")); err != nil {
			t.Fatalf("replaced worktree was deleted: %v", err)
		}
	})
	t.Run("reused branch after worktree removal is preserved", func(t *testing.T) {
		t.Parallel()
		source := initSourceRepo(t)
		backend, err := NewLocalGit(LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true})
		if err != nil {
			t.Fatal(err)
		}
		issue := Issue{ID: "wi-reused-branch", Identifier: "owner/repo#705"}
		info, err := backend.Create(t.Context(), issue)
		if err != nil {
			t.Fatal(err)
		}
		runGit(t, source, "worktree", "remove", "--force", info.Path)
		altPath := filepath.Join(t.TempDir(), "reused-checkout")
		runGit(t, source, "worktree", "add", altPath, info.Branch)
		if err := os.WriteFile(filepath.Join(altPath, "README.md"), []byte("new work on reused branch\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		runGit(t, altPath, "add", "README.md")
		runGit(t, altPath, "commit", "-m", "new work on reused branch")
		runGit(t, source, "worktree", "remove", "--force", altPath)
		issue.Terminal = true
		if _, err := backend.CleanupIssue(t.Context(), issue); !errors.Is(err, ErrWorkspacePreserved) {
			t.Fatalf("CleanupIssue() error = %v, want ErrWorkspacePreserved", err)
		}
		exists, err := backend.branchExists(t.Context(), info.Branch)
		if err != nil || !exists {
			t.Fatalf("reused branch %s was deleted (exists=%v, err=%v)", info.Branch, exists, err)
		}
	})
	t.Run("pre-existing branch with unpushed commits attached by Create is preserved", func(t *testing.T) {
		t.Parallel()
		source := initSourceRepo(t)
		backend, err := NewLocalGit(LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true})
		if err != nil {
			t.Fatal(err)
		}
		issue := Issue{ID: "wi-preexisting", Identifier: "owner/repo#706"}
		planned, err := backend.infoForIssue(issue)
		if err != nil {
			t.Fatal(err)
		}
		seedPath := filepath.Join(t.TempDir(), "seed")
		runGit(t, source, "worktree", "add", "-b", planned.Branch, seedPath, "HEAD")
		if err := os.WriteFile(filepath.Join(seedPath, "README.md"), []byte("predates detent\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		runGit(t, seedPath, "add", "README.md")
		runGit(t, seedPath, "commit", "-m", "predates detent")
		runGit(t, source, "worktree", "remove", "--force", seedPath)
		info, err := backend.Create(t.Context(), issue)
		if err != nil {
			t.Fatal(err)
		}
		issue.Terminal = true
		if _, err := backend.CleanupIssue(t.Context(), issue); !errors.Is(err, ErrWorkspacePreserved) {
			t.Fatalf("CleanupIssue() error = %v, want ErrWorkspacePreserved", err)
		}
		exists, err := backend.branchExists(t.Context(), info.Branch)
		if err != nil || !exists {
			t.Fatalf("pre-existing branch %s was deleted (exists=%v, err=%v)", info.Branch, exists, err)
		}
	})
	t.Run("locked worktree removal failure preserves branch ref", func(t *testing.T) {
		t.Parallel()
		source := initSourceRepo(t)
		backend, err := NewLocalGit(LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true})
		if err != nil {
			t.Fatal(err)
		}
		issue := Issue{ID: "wi-locked", Identifier: "owner/repo#707"}
		info, err := backend.Create(t.Context(), issue)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(info.Path, "README.md"), []byte("unpushed work\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		runGit(t, info.Path, "add", "README.md")
		runGit(t, info.Path, "commit", "-m", "unpushed work")
		wantTip := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))
		runGit(t, source, "worktree", "lock", info.Path)
		issue.Terminal = true
		if _, err := backend.CleanupIssue(t.Context(), issue); err == nil {
			t.Fatal("CleanupIssue() succeeded on locked worktree, want error")
		}
		gotTip := strings.TrimSpace(runGit(t, source, "rev-parse", "--verify", "refs/heads/"+info.Branch))
		if gotTip != wantTip {
			t.Fatalf("branch %s tip = %q, want %q", info.Branch, gotTip, wantTip)
		}
		runGit(t, source, "worktree", "unlock", info.Path)
		if _, err := backend.CleanupIssue(t.Context(), issue); err != nil {
			t.Fatalf("CleanupIssue() after unlock error = %v", err)
		}
		if exists, err := backend.branchExists(t.Context(), info.Branch); err != nil || exists {
			t.Fatalf("branch %s still exists after unlock cleanup (exists=%v, err=%v)", info.Branch, exists, err)
		}
	})
	t.Run("renamed branch replaced by new branch of same name is preserved", func(t *testing.T) {
		t.Parallel()
		source := initSourceRepo(t)
		backend, err := NewLocalGit(LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true})
		if err != nil {
			t.Fatal(err)
		}
		issue := Issue{ID: "wi-renamed", Identifier: "owner/repo#708"}
		info, err := backend.Create(t.Context(), issue)
		if err != nil {
			t.Fatal(err)
		}
		runGit(t, info.Path, "branch", "-m", info.Branch, info.Branch+"-renamed")
		runGit(t, info.Path, "checkout", "-b", info.Branch)
		if err := os.WriteFile(filepath.Join(info.Path, "README.md"), []byte("replacement branch commit\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		runGit(t, info.Path, "add", "README.md")
		runGit(t, info.Path, "commit", "-m", "replacement branch commit")
		issue.Terminal = true
		if _, err := backend.CleanupIssue(t.Context(), issue); !errors.Is(err, ErrWorkspacePreserved) {
			t.Fatalf("CleanupIssue() error = %v, want ErrWorkspacePreserved", err)
		}
		exists, err := backend.branchExists(t.Context(), info.Branch)
		if err != nil || !exists {
			t.Fatalf("replacement branch %s was deleted (exists=%v, err=%v)", info.Branch, exists, err)
		}
	})
	t.Run("copied branch swapped into original name is preserved", func(t *testing.T) {
		t.Parallel()
		source := initSourceRepo(t)
		backend, err := NewLocalGit(LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true})
		if err != nil {
			t.Fatal(err)
		}
		issue := Issue{ID: "wi-copied", Identifier: "owner/repo#709"}
		info, err := backend.Create(t.Context(), issue)
		if err != nil {
			t.Fatal(err)
		}
		runGit(t, info.Path, "branch", "-c", info.Branch, info.Branch+"-copy")
		runGit(t, info.Path, "branch", "-m", info.Branch, info.Branch+"-orig")
		runGit(t, info.Path, "branch", "-m", info.Branch+"-copy", info.Branch)
		runGit(t, info.Path, "checkout", info.Branch)
		runGit(t, info.Path, "branch", "-d", info.Branch+"-orig")
		runGit(t, info.Path, "reflog", "expire", "--expire=now", "refs/heads/"+info.Branch)
		if err := os.WriteFile(filepath.Join(info.Path, "README.md"), []byte("copied branch commit\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		runGit(t, info.Path, "add", "README.md")
		runGit(t, info.Path, "commit", "-m", "copied branch commit")
		issue.Terminal = true
		if _, err := backend.CleanupIssue(t.Context(), issue); !errors.Is(err, ErrWorkspacePreserved) {
			t.Fatalf("CleanupIssue() error = %v, want ErrWorkspacePreserved", err)
		}
		exists, err := backend.branchExists(t.Context(), info.Branch)
		if err != nil || !exists {
			t.Fatalf("copied branch %s was deleted (exists=%v, err=%v)", info.Branch, exists, err)
		}
	})
	t.Run("interrupted cleanup retry finishes deleting owned unpushed branch", func(t *testing.T) {
		t.Parallel()
		source := initSourceRepo(t)
		backend, err := NewLocalGit(LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true})
		if err != nil {
			t.Fatal(err)
		}
		issue := Issue{ID: "wi-interrupted", Identifier: "owner/repo#710"}
		info, err := backend.Create(t.Context(), issue)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(info.Path, "README.md"), []byte("unpushed work\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		runGit(t, info.Path, "add", "README.md")
		runGit(t, info.Path, "commit", "-m", "unpushed work")
		issue.Terminal = true
		if _, _, err := backend.beginWorkspaceCleanup(t.Context(), info, issue, true); err != nil {
			t.Fatal(err)
		}
		runGit(t, source, "worktree", "remove", "--force", info.Path)
		offlinePath := filepath.Join(t.TempDir(), "offline-wt")
		runGit(t, source, "worktree", "add", "-b", "offline-branch", offlinePath, "HEAD")
		offlineAdmin := filepath.Join(source, ".git", "worktrees", "offline-wt")
		if err := os.RemoveAll(offlinePath); err != nil {
			t.Fatal(err)
		}
		if _, err := backend.CleanupIssue(t.Context(), issue); err != nil {
			t.Fatalf("CleanupIssue() retry after interrupted worktree removal error = %v", err)
		}
		if exists, err := backend.branchExists(t.Context(), info.Branch); err != nil || exists {
			t.Fatalf("owned branch %s still exists after interrupted cleanup retry (exists=%v, err=%v)", info.Branch, exists, err)
		}
		if _, err := os.Stat(offlineAdmin); err != nil {
			t.Fatalf("unrelated offline worktree registration was pruned: %v", err)
		}
	})
}
