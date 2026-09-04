package workspace

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalGitPreservesRevokedWorkAcrossCleanupAndRestart(t *testing.T) {
	t.Parallel()

	for _, kind := range []string{"tracked", "staged", "untracked", "unpushed", "pushed"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			source := initSourceRepo(t)
			remote := initBareRemote(t)
			runGit(t, source, "remote", "add", "origin", remote)
			runGit(t, source, "push", "-u", "origin", "main")
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
			if kind == "staged" || kind == "unpushed" || kind == "pushed" {
				runGit(t, info.Path, "add", name)
			}
			if kind == "unpushed" || kind == "pushed" {
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
				if kind != "unpushed" {
					runGit(t, info.Path, "add", name)
					runGit(t, info.Path, "commit", "-m", "publish recovered implementation")
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
