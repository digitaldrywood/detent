package workspacerunner_test

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/workspace"
	"github.com/digitaldrywood/detent/internal/workspacerunner"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

// TestGitWorktreePrepareRelease covers both halves of decisions 18.1: a fresh
// workspace session gets a worktree of its own that closing removes without
// leaving a branch behind, and a retained attempt worktree is left exactly
// where it is, because closing a workspace never deletes the attempt's
// artifacts.
func TestGitWorktreePrepareRelease(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		worktree    string
		ref         string
		wantRemoved bool
	}{
		// A session's ref names what to look at, so "main" must not become
		// the worktree's branch and collide with the source checkout.
		{name: "fresh session is removed", worktree: workspacesession.WorktreeFresh, ref: "main", wantRemoved: true},
		{name: "unspecified worktree is a session", worktree: "", ref: "main", wantRemoved: true},
		{name: "retained attempt is left alone", worktree: workspacesession.WorktreeRetained},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			source := initWorktreeSourceRepo(t)
			head := strings.TrimSpace(runWorktreeGit(t, source, "rev-parse", "HEAD"))
			backend, err := workspace.NewLocalGit(workspace.LocalGitOptions{
				Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true,
			})
			if err != nil {
				t.Fatalf("NewLocalGit() error = %v", err)
			}
			worktrees := &workspacerunner.GitWorktree{Backend: backend, ProjectID: "dogfood"}
			checkout := hubclient.WorkspaceCheckout{
				WorkItemID: "wi_0c1e9fe3", Worktree: tt.worktree, HeadSHA: head, Ref: tt.ref,
			}

			if tt.worktree == workspacesession.WorktreeRetained {
				// The attempt made this worktree; the workspace only attaches
				// to it, so it must already exist under the attempt's own
				// identifier.
				if _, err := backend.Create(t.Context(), workspace.Issue{ProjectID: "dogfood", ID: "wi_0c1e9fe3", Identifier: "wi_0c1e9fe3"}); err != nil {
					t.Fatalf("create attempt worktree: %v", err)
				}
			}

			path, err := worktrees.Prepare(t.Context(), checkout)
			if err != nil {
				t.Fatalf("Prepare() error = %v", err)
			}
			if _, statErr := os.Stat(path); statErr != nil {
				t.Fatalf("prepared worktree missing: %v", statErr)
			}
			if err := worktrees.Release(t.Context(), path, checkout); err != nil {
				t.Fatalf("Release() error = %v", err)
			}

			_, statErr := os.Stat(path)
			if tt.wantRemoved {
				if !errors.Is(statErr, fs.ErrNotExist) {
					t.Fatalf("session worktree remains at %s: %v", path, statErr)
				}
				if branches := worktreeBranches(t, source); branches != "" {
					t.Fatalf("session left branches behind: %s", branches)
				}
				return
			}
			if statErr != nil {
				t.Fatalf("retained worktree removed: %v", statErr)
			}
			if branches := worktreeBranches(t, source); branches == "" {
				t.Fatalf("retained attempt branch was removed")
			}
		})
	}
}

// TestGitWorktreeReleaseSkipsWorktreesItDidNotMake keeps Release honest about
// the one worktree it is allowed to remove: a path it never created is not
// its to clean up, whatever the checkout says.
func TestGitWorktreeReleaseSkipsWorktreesItDidNotMake(t *testing.T) {
	t.Parallel()

	source := initWorktreeSourceRepo(t)
	backend, err := workspace.NewLocalGit(workspace.LocalGitOptions{
		Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true,
	})
	if err != nil {
		t.Fatalf("NewLocalGit() error = %v", err)
	}
	issue := workspace.Issue{ProjectID: "dogfood", Identifier: "someone-else"}
	info, err := backend.Create(t.Context(), issue)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	worktrees := &workspacerunner.GitWorktree{Backend: backend, ProjectID: "dogfood"}

	checkout := hubclient.WorkspaceCheckout{WorkItemID: "wi_0c1e9fe3", Worktree: workspacesession.WorktreeFresh}
	if err := worktrees.Release(t.Context(), info.Path, checkout); err != nil {
		t.Fatalf("Release() error = %v", err)
	}
	if _, statErr := os.Stat(info.Path); statErr != nil {
		t.Fatalf("worktree the runner did not create was removed: %v", statErr)
	}
}

func initWorktreeSourceRepo(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	runWorktreeCommand(t, dir, "git", "init", "-b", "main", ".")
	runWorktreeGit(t, dir, "config", "user.name", "Test User")
	runWorktreeGit(t, dir, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("source repo\n"), 0o600); err != nil {
		t.Fatalf("write README.md: %v", err)
	}
	runWorktreeGit(t, dir, "add", "README.md")
	runWorktreeGit(t, dir, "commit", "-m", "initial")
	return dir
}

func runWorktreeGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return runWorktreeCommand(t, dir, "git", args...)
}

func runWorktreeCommand(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()

	command := exec.CommandContext(t.Context(), name, args...)
	command.Dir = dir
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v: %s", name, strings.Join(args, " "), err, output)
	}
	return string(output)
}

// worktreeBranches lists every branch but the repository's own, which is the
// only one neither a session nor an attempt worktree owns.
func worktreeBranches(t *testing.T, source string) string {
	t.Helper()

	output := runWorktreeGit(t, source, "for-each-ref", "--format=%(refname:short)", "refs/heads/")
	var kept []string
	for _, branch := range strings.Fields(output) {
		if branch != "main" {
			kept = append(kept, branch)
		}
	}
	return strings.Join(kept, " ")
}
