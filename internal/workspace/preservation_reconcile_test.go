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
