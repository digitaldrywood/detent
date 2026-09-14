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

// TestLocalGitWorkspaceSessionLifecycle covers the create/close pair for the
// workspace session worktree of decisions 18.1 (`worktree: "fresh"`). The
// repository deliberately has no remote in one arm: the fifth dogfood run's
// orphan appeared exactly there, because the old preservation rule measured a
// session worktree against remote refs it could never reach.
func TestLocalGitWorkspaceSessionLifecycle(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		remote        bool
		work          string
		wantPreserved bool
	}{
		{name: "clean session without remote", wantPreserved: false},
		{name: "clean session with remote", remote: true, wantPreserved: false},
		{name: "session with uncommitted files", work: "dirty", wantPreserved: true},
		{name: "session branch with its own commit", work: "committed", wantPreserved: true},
		{name: "session committed past head_sha", work: "detached commit", wantPreserved: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			source := initSourceRepo(t)
			if tt.remote {
				publishCleanupSource(t, source)
			}
			// A commit the remote never saw, so the ordinary preservation
			// rule would refuse to remove anything branched from it.
			runGit(t, source, "commit", "--allow-empty", "-m", "local only")
			head := strings.TrimSpace(runGit(t, source, "rev-parse", "HEAD"))
			backend, err := NewLocalGit(LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true})
			if err != nil {
				t.Fatalf("NewLocalGit() error = %v", err)
			}

			issue := Issue{
				ProjectID: "detent", ID: "wi_0c1e9fe3", Identifier: "wi_0c1e9fe3-ws",
				WorkspaceSession: true, PullRequestHeadSHA: head,
			}
			info, err := backend.Create(t.Context(), issue)
			if err != nil {
				t.Fatalf("Create() error = %v", err)
			}
			if !strings.HasPrefix(info.Branch, workspaceSessionBranchPrefix) {
				t.Fatalf("session branch = %q, want the %q namespace", info.Branch, workspaceSessionBranchPrefix)
			}
			// What the runner does for a fresh session: detach onto the
			// commit the person asked to look at.
			runGit(t, info.Path, "checkout", "--detach", head)

			switch tt.work {
			case "dirty":
				if err := os.WriteFile(filepath.Join(info.Path, "notes.md"), []byte("typed in the terminal\n"), 0o600); err != nil {
					t.Fatalf("write session file: %v", err)
				}
			case "committed":
				runGit(t, info.Path, "checkout", info.Branch)
				runGit(t, info.Path, "commit", "--allow-empty", "-m", "typed in the terminal")
			case "detached commit":
				runGit(t, info.Path, "commit", "--allow-empty", "-m", "typed in the terminal")
			}

			result, err := backend.CleanupIssue(t.Context(), issue)
			if tt.wantPreserved {
				if !errors.Is(err, ErrWorkspacePreserved) {
					t.Fatalf("CleanupIssue() = %+v, %v; want preservation", result, err)
				}
				if _, statErr := os.Stat(info.Path); statErr != nil {
					t.Fatalf("preserved session worktree missing: %v", statErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("CleanupIssue() error = %v", err)
			}
			if result.Worktrees != 1 || result.Branches != 1 {
				t.Fatalf("CleanupIssue() = %+v, want one worktree and one branch removed", result)
			}
			if _, statErr := os.Stat(info.Path); !errors.Is(statErr, fs.ErrNotExist) {
				t.Fatalf("session worktree remains: %v", statErr)
			}
			if branchExists(t, source, info.Branch) {
				t.Fatalf("session branch %q remains", info.Branch)
			}
		})
	}
}

// TestLocalGitWorkspaceSessionBranchIsolation proves the session namespace is
// only reachable through Issue.WorkspaceSession, so an attempt's branch keeps
// the preservation rule it had.
func TestLocalGitWorkspaceSessionBranchIsolation(t *testing.T) {
	t.Parallel()

	source := initSourceRepo(t)
	backend, err := NewLocalGit(LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true})
	if err != nil {
		t.Fatalf("NewLocalGit() error = %v", err)
	}
	tests := []struct {
		name    string
		issue   Issue
		wantPre string
	}{
		{name: "attempt", issue: Issue{Identifier: "detent#2218"}, wantPre: autoBranchPrefix},
		{name: "attempt with explicit branch", issue: Issue{Identifier: "detent#2218", BranchName: "feature/x"}, wantPre: "feature/"},
		{name: "session", issue: Issue{Identifier: "detent#2218", WorkspaceSession: true}, wantPre: workspaceSessionBranchPrefix},
		{name: "session ignores ref", issue: Issue{Identifier: "detent#2218", BranchName: "main", WorkspaceSession: true}, wantPre: workspaceSessionBranchPrefix},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			branch := backend.branchName(tt.issue, issueKey(tt.issue))
			if !strings.HasPrefix(branch, tt.wantPre) {
				t.Fatalf("branchName() = %q, want prefix %q", branch, tt.wantPre)
			}
			if got := isWorkspaceSessionBranch(branch); got != tt.issue.WorkspaceSession {
				t.Fatalf("isWorkspaceSessionBranch(%q) = %t, want %t", branch, got, tt.issue.WorkspaceSession)
			}
		})
	}
}

// TestLocalGitAttemptBranchStillPreservesUnpushedWork guards the rule the
// session exemption must not reach: an attempt worktree whose auto-branch
// carries commits the remote never saw is still retained.
func TestLocalGitAttemptBranchStillPreservesUnpushedWork(t *testing.T) {
	t.Parallel()

	source := initSourceRepo(t)
	publishCleanupSource(t, source)
	backend, err := NewLocalGit(LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true})
	if err != nil {
		t.Fatalf("NewLocalGit() error = %v", err)
	}
	issue := Issue{ProjectID: "detent", Identifier: "detent#2218"}
	info, err := backend.Create(t.Context(), issue)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	runGit(t, info.Path, "commit", "--allow-empty", "-m", "unpushed work")

	result, err := backend.CleanupIssue(t.Context(), issue)
	if !errors.Is(err, ErrWorkspacePreserved) {
		t.Fatalf("CleanupIssue() = %+v, %v; want preservation", result, err)
	}
	if !branchExists(t, source, info.Branch) {
		t.Fatalf("attempt branch %q was removed", info.Branch)
	}
	if _, statErr := os.Stat(info.Path); statErr != nil {
		t.Fatalf("attempt worktree missing: %v", statErr)
	}
}

// TestLocalGitReconcileWorkspaceSessionResidual is the fifth run's loop: the
// session's runner died, so the worktree is left registered with nobody to
// close it and the residual reconciler is the only thing that can. It used to
// fail on every pass with "workspace retained for recovery".
func TestLocalGitReconcileWorkspaceSessionResidual(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		work          string
		wantRemoved   int
		wantPreserved int
	}{
		{name: "clean session residual", wantRemoved: 1},
		{name: "session residual with uncommitted files", work: "dirty", wantPreserved: 1},
		{name: "session residual with its own commit", work: "committed", wantPreserved: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			source := initSourceRepo(t)
			// No remote at all, as in the dogfood preview repository.
			backend, err := NewLocalGit(LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true})
			if err != nil {
				t.Fatalf("NewLocalGit() error = %v", err)
			}
			issue := Issue{ProjectID: "dogfood", ID: "wi_0c1e9fe3", Identifier: "wi_0c1e9fe3-ws", WorkspaceSession: true}
			info, err := backend.Create(t.Context(), issue)
			if err != nil {
				t.Fatalf("Create() error = %v", err)
			}
			head := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))
			runGit(t, info.Path, "checkout", "--detach", head)
			switch tt.work {
			case "dirty":
				if err := os.WriteFile(filepath.Join(info.Path, "notes.md"), []byte("typed in the terminal\n"), 0o600); err != nil {
					t.Fatalf("write session file: %v", err)
				}
			case "committed":
				runGit(t, info.Path, "checkout", info.Branch)
				runGit(t, info.Path, "commit", "--allow-empty", "-m", "typed in the terminal")
			}
			backend.scanWorkspacePaths = func(context.Context, string) ([]int, error) { return nil, nil }

			result, err := backend.ReconcileResiduals(t.Context(), nil)
			if (err != nil) != (tt.wantPreserved > 0) {
				t.Fatalf("ReconcileResiduals() error = %v, want preserved=%d", err, tt.wantPreserved)
			}
			if result.Removed != tt.wantRemoved || result.PreservedSkipped != tt.wantPreserved || result.UnownedSkipped != 0 {
				t.Fatalf("ReconcileResiduals() = %+v, want removed=%d preserved=%d unowned=0", result, tt.wantRemoved, tt.wantPreserved)
			}
			_, statErr := os.Stat(info.Path)
			if tt.wantRemoved == 1 {
				if !errors.Is(statErr, fs.ErrNotExist) {
					t.Fatalf("session residual remains: %v", statErr)
				}
				if branchExists(t, source, info.Branch) {
					t.Fatalf("session branch %q remains", info.Branch)
				}
				return
			}
			if statErr != nil || !branchExists(t, source, info.Branch) {
				t.Fatalf("retained session residual missing: %v", statErr)
			}
		})
	}
}
