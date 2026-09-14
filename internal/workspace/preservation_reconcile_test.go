package workspace

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalGitReconcileRechecksPreservation(t *testing.T) {
	t.Parallel()
	for _, record := range []string{"unrecorded", "owned", "preserved", "legacy preserved"} {
		for _, work := range []string{"published", "merged", "tracked", "staged", "untracked", "unpushed", "detached", "broken", "active issue", "active process"} {
			t.Run(record+"/"+work, func(t *testing.T) {
				t.Parallel()
				source := initSourceRepo(t)
				publishCleanupSource(t, source)
				opts := LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true}
				backend, err := NewLocalGit(opts)
				if err != nil {
					t.Fatal(err)
				}
				issue := Issue{ProjectID: "detent", Identifier: "digitaldrywood/detent#2612"}
				info, err := backend.Create(t.Context(), issue)
				if err != nil {
					t.Fatal(err)
				}
				name := "README.md"
				if work == "untracked" {
					name = "implementation.go"
				}
				if err := os.WriteFile(filepath.Join(info.Path, name), []byte("interrupted work\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if record == "owned" {
					if err := backend.recordCleanupOwnership(t.Context(), info, issue, true); err != nil {
						t.Fatal(err)
					}
				}
				if record == "preserved" || record == "legacy preserved" {
					if _, err := backend.PreserveIssue(t.Context(), issue); err != nil {
						t.Fatal(err)
					}
				}
				if record == "legacy preserved" {
					path := filepath.Join(opts.Root, cleanupOwnershipRecordRelativePath(info.Path))
					data, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					// Reproduce ownership records persisted by releases that latched preservation.
					data = []byte(strings.TrimSuffix(strings.TrimSpace(string(data)), "}") + ",\"preserve\":true}\n")
					if err := os.WriteFile(path, data, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if work != "tracked" && work != "untracked" {
					runGit(t, info.Path, "add", name)
					if work != "staged" {
						runGit(t, info.Path, "commit", "-m", "recover work")
						if work != "unpushed" && work != "detached" {
							runGit(t, info.Path, "push", "origin", info.Branch)
						}
					}
				}
				if work == "detached" {
					runGit(t, info.Path, "checkout", "--detach")
				}
				if work == "merged" {
					runGit(t, source, "merge", "--ff-only", info.Branch)
					runGit(t, source, "push", "origin", "main")
					runGit(t, source, "push", "origin", "--delete", info.Branch)
					runGit(t, source, "fetch", "--prune", "origin")
				}
				if work == "broken" {
					if err := os.Rename(filepath.Join(info.Path, ".git"), filepath.Join(info.Path, "saved-git")); err != nil {
						t.Fatal(err)
					}
				}
				backend, err = NewLocalGit(opts)
				if err != nil {
					t.Fatal(err)
				}
				backend.scanWorkspacePaths = func(context.Context, string) ([]int, error) {
					if work == "active process" {
						return []int{os.Getpid() + 1000}, nil
					}
					return nil, nil
				}
				var active []Issue
				if work == "active issue" {
					active = []Issue{issue}
				}
				result, err := backend.ReconcileResiduals(t.Context(), active)
				removed := work == "published" || work == "merged"
				retained := !removed && work != "active issue" && work != "active process"
				// An unrecorded detached worktree cannot establish automatic branch ownership.
				unowned := record == "unrecorded" && work == "detached"
				if err != nil {
					t.Fatalf("retention must not fail the scheduled sweep: %+v, %v", result, err)
				}
				if removed {
					if result.Removed != 1 || len(result.CompletedPaths) != 1 {
						t.Fatalf("safe workspace not reclaimed: %+v", result)
					}
					if _, err := os.Stat(info.Path); !errors.Is(err, fs.ErrNotExist) {
						t.Fatalf("workspace remains: %v", err)
					}
					if branchExists(t, source, info.Branch) {
						t.Fatal("branch remains")
					}
					if _, err := backend.readOwnershipRecord(cleanupOwnershipRecordRelativePath(info.Path)); !errors.Is(err, fs.ErrNotExist) {
						t.Fatalf("ownership remains: %v", err)
					}
				} else {
					if result.Removed != 0 {
						t.Fatalf("unsafe cleanup: %+v", result)
					}
					if got := readFile(t, filepath.Join(info.Path, name)); got != "interrupted work\n" {
						t.Fatalf("work changed: %q", got)
					}
					if retained && !unowned && (result.PreservedSkipped != 1 || len(result.Failures) != 1) {
						t.Fatalf("missing retention evidence: %+v", result)
					}
					if !retained && result.ActiveSkipped != 1 {
						t.Fatalf("missing active skip: %+v", result)
					}
				}
			})
		}
	}
}

func TestCleanupVerifiesLiveRemoteCommits(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"residual", "cleanup", "branch"} {
		for _, remoteState := range []string{"published", "deleted", "rewound", "merged", "unavailable"} {
			t.Run(action+"/"+remoteState, func(t *testing.T) {
				t.Parallel()
				source := initSourceRepo(t)
				remote := initBareRemote(t)
				runGit(t, source, "remote", "add", "origin", remote)
				runGit(t, source, "push", "-u", "origin", "main")
				backend, err := NewLocalGit(LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true})
				if err != nil {
					t.Fatal(err)
				}
				issue := Issue{Identifier: "live-remote-cleanup"}
				info, err := backend.Create(t.Context(), issue)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(info.Path, "README.md"), []byte("unique implementation\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				runGit(t, info.Path, "add", "README.md")
				runGit(t, info.Path, "commit", "-m", "implementation")
				runGit(t, info.Path, "push", "origin", info.Branch)
				head := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))
				switch remoteState {
				case "deleted":
					runGit(t, remote, "update-ref", "-d", "refs/heads/"+info.Branch)
				case "rewound":
					base := strings.TrimSpace(runGit(t, remote, "rev-parse", "main"))
					runGit(t, remote, "update-ref", "refs/heads/"+info.Branch, base)
				case "merged":
					runGit(t, source, "merge", "--ff-only", info.Branch)
					runGit(t, source, "push", "origin", "main")
					runGit(t, remote, "update-ref", "-d", "refs/heads/"+info.Branch)
				case "unavailable":
					if err := os.Rename(remote, remote+"-offline"); err != nil {
						t.Fatal(err)
					}
				}
				// Server-side changes deliberately leave the local tracking branch stale.
				if got := strings.TrimSpace(runGit(t, source, "rev-parse", "refs/remotes/origin/"+info.Branch)); got != head {
					t.Fatalf("tracking ref = %s, want %s", got, head)
				}
				if _, err := backend.PreserveIssue(t.Context(), issue); err != nil && remoteState != "unavailable" {
					t.Fatal(err)
				}
				backend.scanWorkspacePaths = func(context.Context, string) ([]int, error) { return nil, nil }
				safe := remoteState == "published" || remoteState == "merged"
				switch action {
				case "residual":
					result, err := backend.ReconcileResiduals(t.Context(), nil)
					if err != nil {
						t.Fatal(err)
					}
					if (result.Removed == 1) != safe {
						t.Fatalf("safe=%v, result=%+v", safe, result)
					}
				case "cleanup":
					result, err := backend.CleanupIssue(t.Context(), issue)
					if safe && err != nil {
						t.Fatal(err)
					}
					if !safe && !errors.Is(err, ErrWorkspacePreserved) {
						t.Fatalf("want preservation, got %+v, %v", result, err)
					}
				case "branch":
					runGit(t, info.Path, "checkout", "--detach", "origin/main")
					deleted, err := backend.deleteBranch(t.Context(), info.Branch)
					if deleted != safe || (safe && err != nil) {
						t.Fatalf("safe=%v, deleted=%v, err=%v", safe, deleted, err)
					}
				}
				if !safe {
					if !branchExists(t, source, info.Branch) {
						t.Fatal("deleted implementation branch")
					}
					if _, err := os.Stat(info.Path); err != nil {
						t.Fatalf("lost worktree: %v", err)
					}
				}
			})
		}
	}
}
