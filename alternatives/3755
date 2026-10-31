package workspacerunner_test

import (
	"context"
	"errors"
	"fmt"
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

const attemptIdentifier = "prj_1#7"

func newGitWorktree(t *testing.T) (*workspacerunner.GitWorktree, *workspace.LocalGit, string, string) {
	t.Helper()
	source := initWorktreeSourceRepo(t)
	root := filepath.Join(t.TempDir(), "workspaces")
	backend, err := workspace.NewLocalGit(workspace.LocalGitOptions{Root: root, SourceRoot: source, AutoBranch: true})
	if err != nil {
		t.Fatalf("NewLocalGit() error = %v", err)
	}
	worktrees := &workspacerunner.GitWorktree{
		Backend: backend, ProjectID: "dogfood",
		Resolve: func(_ context.Context, workItemID string) (string, error) {
			if workItemID != "wi_0c1e9fe3" {
				return "", fmt.Errorf("unknown work item %s", workItemID)
			}
			return attemptIdentifier, nil
		},
	}
	return worktrees, backend, source, root
}

// worktreeDirs lists the worktrees under root, leaving out the backend's own
// bookkeeping directories.
func worktreeDirs(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var dirs []string
	for _, entry := range entries {
		if entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") {
			dirs = append(dirs, entry.Name())
		}
	}
	return dirs
}

func TestGitWorktreePrepareRelease(t *testing.T) {
	if testing.Short() {
		t.Skip("process lifecycle integration")
	}

	t.Parallel()

	tests := []struct {
		name        string
		worktree    string
		ref         string
		wantRemoved bool
	}{
		{name: "fresh session is removed", worktree: workspacesession.WorktreeFresh, ref: "main", wantRemoved: true},
		{name: "unspecified worktree is a session", worktree: "", ref: "main", wantRemoved: true},
		{name: "retained attempt is left alone", worktree: workspacesession.WorktreeRetained, ref: "someone-elses-branch"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			worktrees, backend, source, _ := newGitWorktree(t)
			head := strings.TrimSpace(runWorktreeGit(t, source, "rev-parse", "HEAD"))
			checkout := hubclient.WorkspaceCheckout{WorkItemID: "wi_0c1e9fe3", Worktree: tt.worktree, HeadSHA: head, Ref: tt.ref}

			var attempt workspace.Info
			if tt.worktree == workspacesession.WorktreeRetained {
				var err error
				attempt, err = backend.Create(t.Context(), workspace.Issue{ProjectID: "dogfood", ID: "wi_0c1e9fe3", Identifier: attemptIdentifier})
				if err != nil {
					t.Fatalf("create attempt worktree: %v", err)
				}
			}

			path, err := worktrees.Prepare(t.Context(), "ws_1", checkout)
			if err != nil {
				t.Fatalf("Prepare() error = %v", err)
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
			if path != attempt.Path {
				t.Fatalf("retained workspace served %s, want the attempt's %s", path, attempt.Path)
			}
			if statErr != nil {
				t.Fatalf("retained worktree removed: %v", statErr)
			}
			if branch := strings.TrimSpace(runWorktreeGit(t, path, "rev-parse", "--abbrev-ref", "HEAD")); branch != attempt.Branch {
				t.Fatalf("retained worktree is on %q, want the attempt's %q", branch, attempt.Branch)
			}
		})
	}
}

func TestGitWorktreeRetainedNeedsTheAttemptsWorktree(t *testing.T) {
	if testing.Short() {
		t.Skip("process lifecycle integration")
	}

	t.Parallel()

	tests := []struct {
		name       string
		workItemID string
		unresolved bool
	}{
		{name: "the attempt worktree is gone", workItemID: "wi_0c1e9fe3"},
		{name: "the work item cannot be resolved", workItemID: "wi_unknown"},
		{name: "no resolver is configured", workItemID: "wi_0c1e9fe3", unresolved: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			worktrees, _, _, root := newGitWorktree(t)
			if tt.unresolved {
				worktrees.Resolve = nil
			}
			_, err := worktrees.Prepare(t.Context(), "ws_1", hubclient.WorkspaceCheckout{
				WorkItemID: tt.workItemID, Worktree: workspacesession.WorktreeRetained,
			})
			if err == nil {
				t.Fatal("Prepare() error = nil, want a refusal")
			}
			if dirs := worktreeDirs(t, root); len(dirs) != 0 {
				t.Fatalf("a refused retained workspace created worktrees %v", dirs)
			}
		})
	}
}

func TestGitWorktreeFreshSessionsDoNotShareACheckout(t *testing.T) {
	if testing.Short() {
		t.Skip("process lifecycle integration")
	}

	t.Parallel()
	worktrees, _, _, _ := newGitWorktree(t)
	checkout := hubclient.WorkspaceCheckout{WorkItemID: "wi_0c1e9fe3", Worktree: workspacesession.WorktreeFresh}

	first, err := worktrees.Prepare(t.Context(), "ws_1", checkout)
	if err != nil {
		t.Fatal(err)
	}
	second, err := worktrees.Prepare(t.Context(), "ws_2", checkout)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("two sessions share the worktree %s", first)
	}
	if err := worktrees.Release(t.Context(), first, checkout); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(second); err != nil {
		t.Fatalf("closing one session removed the other's worktree: %v", err)
	}
}

func TestGitWorktreeReleaseSkipsWorktreesItDidNotMake(t *testing.T) {
	if testing.Short() {
		t.Skip("process lifecycle integration")
	}

	t.Parallel()
	worktrees, backend, _, _ := newGitWorktree(t)
	info, err := backend.Create(t.Context(), workspace.Issue{ProjectID: "dogfood", Identifier: "someone-else"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	checkout := hubclient.WorkspaceCheckout{WorkItemID: "wi_0c1e9fe3", Worktree: workspacesession.WorktreeFresh}
	if err := worktrees.Release(t.Context(), info.Path, checkout); err != nil {
		t.Fatalf("Release() error = %v", err)
	}
	if _, statErr := os.Stat(info.Path); statErr != nil {
		t.Fatalf("worktree the runner did not create was removed: %v", statErr)
	}
}

func TestGitWorktreePrepareRefusesAHeadThatIsNotACommitID(t *testing.T) {
	if testing.Short() {
		t.Skip("process lifecycle integration")
	}

	t.Parallel()

	tests := []struct {
		name string
		head string
	}{
		{name: "an option is refused", head: "--orphan"},
		{name: "a ref name is refused", head: "main"},
		{name: "a short hex id is refused", head: "abc12"},
		{name: "an unknown commit fails without leaving a worktree", head: strings.Repeat("0", 40)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			worktrees, _, source, root := newGitWorktree(t)
			_, err := worktrees.Prepare(t.Context(), "ws_1", hubclient.WorkspaceCheckout{
				WorkItemID: "wi_0c1e9fe3", Worktree: workspacesession.WorktreeFresh, HeadSHA: tt.head,
			})
			if err == nil {
				t.Fatal("Prepare() error = nil, want a refusal")
			}
			if dirs := worktreeDirs(t, root); len(dirs) != 0 {
				t.Fatalf("a refused checkout left worktrees behind: %v", dirs)
			}
			if branches := worktreeBranches(t, source); branches != "" {
				t.Fatalf("a refused checkout left branches behind: %s", branches)
			}
		})
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

// TestGitWorktreeFreshSessionChecksOutWhatWasAskedFor opens fresh sessions on a
// ref and on a head_sha, including ones that exist only on the remote, and
// refuses refs git would misread.
func TestGitWorktreeFreshSessionChecksOutWhatWasAskedFor(t *testing.T) {
	if testing.Short() {
		t.Skip("process lifecycle integration")
	}

	t.Parallel()

	tests := []struct {
		name    string
		ref     string
		head    string
		want    string
		wantErr bool
	}{
		{name: "a local branch", ref: "feature", want: "feature"},
		{name: "a branch only on the remote", ref: "remote-only", want: "remote-only"},
		{name: "a head only on the remote", head: "remote-only", want: "remote-only"},
		{name: "an option-shaped ref", ref: "--upload-pack=touch", wantErr: true},
		{name: "an invalid ref name", ref: "bad..ref", wantErr: true},
		{name: "a ref nobody has", ref: "missing", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			worktrees, _, source, root := newGitWorktree(t)
			upstream := filepath.Join(t.TempDir(), "upstream.git")
			runWorktreeCommand(t, source, "git", "clone", "--quiet", "--bare", source, upstream)
			runWorktreeGit(t, source, "remote", "add", "origin", upstream)
			runWorktreeGit(t, source, "fetch", "--quiet", "origin")
			runWorktreeGit(t, source, "switch", "--quiet", "-c", "feature")
			runWorktreeGit(t, source, "commit", "--quiet", "--allow-empty", "-m", "feature work")
			runWorktreeGit(t, source, "switch", "--quiet", "main")
			clone := filepath.Join(t.TempDir(), "clone")
			runWorktreeCommand(t, source, "git", "clone", "--quiet", upstream, clone)
			runWorktreeGit(t, clone, "config", "user.name", "Test User")
			runWorktreeGit(t, clone, "config", "user.email", "test@example.com")
			runWorktreeGit(t, clone, "switch", "--quiet", "-c", "remote-only")
			runWorktreeGit(t, clone, "commit", "--quiet", "--allow-empty", "-m", "remote work")
			runWorktreeGit(t, clone, "push", "--quiet", "origin", "remote-only")
			commits := map[string]string{
				"feature":     strings.TrimSpace(runWorktreeGit(t, source, "rev-parse", "feature")),
				"remote-only": strings.TrimSpace(runWorktreeGit(t, clone, "rev-parse", "HEAD")),
			}

			checkout := hubclient.WorkspaceCheckout{WorkItemID: "wi_0c1e9fe3", Worktree: workspacesession.WorktreeFresh, Ref: tt.ref}
			if tt.head != "" {
				checkout.HeadSHA = commits[tt.head]
			}
			path, err := worktrees.Prepare(t.Context(), "ws_1", checkout)
			if tt.wantErr {
				if err == nil {
					t.Fatal("Prepare() error = nil, want a refusal")
				}
				if dirs := worktreeDirs(t, root); len(dirs) != 0 {
					t.Fatalf("a refused session left worktrees behind: %v", dirs)
				}
				return
			}
			if err != nil {
				t.Fatalf("Prepare() error = %v", err)
			}
			if got := strings.TrimSpace(runWorktreeGit(t, path, "rev-parse", "HEAD")); got != commits[tt.want] {
				t.Fatalf("worktree HEAD = %s, want %s (%s)", got, commits[tt.want], tt.want)
			}
			if err := worktrees.Release(t.Context(), path, checkout); err != nil {
				t.Fatalf("Release() error = %v", err)
			}
			if _, statErr := os.Stat(path); !errors.Is(statErr, fs.ErrNotExist) {
				t.Fatalf("session worktree remains after release: %v", statErr)
			}
		})
	}
}

// TestGitWorktreeHoldsItsWorktreeAgainstResidualSweeps runs the orchestrator's
// residual sweep, from a backend instance of its own, while a session serves a
// fresh worktree and again after the session released it.
func TestGitWorktreeHoldsItsWorktreeAgainstResidualSweeps(t *testing.T) {
	if testing.Short() {
		t.Skip("process lifecycle integration")
	}

	t.Parallel()
	worktrees, _, source, root := newGitWorktree(t)
	sweeper, err := workspace.NewLocalGit(workspace.LocalGitOptions{Root: root, SourceRoot: source, AutoBranch: true})
	if err != nil {
		t.Fatal(err)
	}
	head := strings.TrimSpace(runWorktreeGit(t, source, "rev-parse", "HEAD"))
	checkout := hubclient.WorkspaceCheckout{WorkItemID: "wi_0c1e9fe3", Worktree: workspacesession.WorktreeFresh, HeadSHA: head}
	path, err := worktrees.Prepare(t.Context(), "ws_sweep", checkout)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name        string
		release     bool
		wantPresent bool
	}{
		{name: "a sweep while the session is open leaves it", wantPresent: true},
		{name: "a sweep after the session released it finds nothing left", release: true},
	}
	for _, tt := range tests {
		if tt.release {
			if err := worktrees.Release(t.Context(), path, checkout); err != nil {
				t.Fatalf("%s: Release() error = %v", tt.name, err)
			}
		}
		if _, err := sweeper.ReconcileResiduals(t.Context(), nil); err != nil {
			t.Fatalf("%s: ReconcileResiduals() error = %v", tt.name, err)
		}
		if _, statErr := os.Stat(path); (statErr == nil) != tt.wantPresent {
			t.Fatalf("%s: worktree present = %t, want %t", tt.name, statErr == nil, tt.wantPresent)
		}
	}
}
