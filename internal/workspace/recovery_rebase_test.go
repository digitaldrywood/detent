package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecoveryStateDetectsPausedRebase(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	root := filepath.Join(dir, "workspaces")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit := func(cwd string, args ...string) {
		t.Helper()
		if _, err := runGitAt(t.Context(), cwd, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	runGit(source, "init", "-b", "develop")
	runGit(source, "config", "user.email", "test@example.com")
	runGit(source, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(source, "file.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(source, "add", "file.txt")
	runGit(source, "commit", "-m", "base")

	backend, err := NewLocalGit(LocalGitOptions{Root: root, SourceRoot: source, AutoBranch: true})
	if err != nil {
		t.Fatal(err)
	}
	issue := Issue{ID: "wi_rebase_test", Identifier: "owner/repository#1", BaseRef: "develop"}
	info, err := backend.Create(t.Context(), issue)
	if err != nil {
		t.Fatal(err)
	}

	state, err := backend.RecoveryState(t.Context(), info, issue)
	if err != nil {
		t.Fatal(err)
	}
	if state.RebaseInProgress {
		t.Fatal("RebaseInProgress = true before rebase, want false")
	}

	gitDir, err := runGitAt(t.Context(), info.Path, "rev-parse", "--git-dir")
	if err != nil {
		t.Fatal(err)
	}
	resolvedGitDir := strings.TrimSpace(gitDir)
	if !filepath.IsAbs(resolvedGitDir) {
		resolvedGitDir = filepath.Join(info.Path, resolvedGitDir)
	}
	if err := os.MkdirAll(filepath.Join(resolvedGitDir, "rebase-merge"), 0o755); err != nil {
		t.Fatal(err)
	}

	state, err = backend.RecoveryState(t.Context(), info, issue)
	if err != nil {
		t.Fatal(err)
	}
	if !state.RebaseInProgress {
		t.Fatal("RebaseInProgress = false during paused rebase with clean tree, want true")
	}
	if len(state.TrackedPaths) != 0 || len(state.UntrackedPaths) != 0 {
		t.Fatalf("expected clean tree paths, got tracked=%v untracked=%v", state.TrackedPaths, state.UntrackedPaths)
	}
}
