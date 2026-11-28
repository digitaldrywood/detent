package gitexec

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	commandshell "github.com/digitaldrywood/detent/internal/shell"
)

func TestIsolatedReplayWritePathsAndMergeTreeStayLocalInScratch(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Example", "GIT_AUTHOR_EMAIL=example@example.com",
			"GIT_COMMITTER_NAME=Example", "GIT_COMMITTER_EMAIL=example@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "a.txt")
	run("commit", "-m", "base")
	baseSHA := run("rev-parse", "HEAD")

	if err := os.WriteFile(filepath.Join(repo, "b.txt"), []byte("change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "b.txt")
	run("commit", "-m", "add b")
	headSHA := run("rev-parse", "HEAD")

	startedNonLocal := 0
	ctx := commandshell.WithCommandStarter(t.Context(), func(ctx context.Context, cmd *exec.Cmd) (func() error, error) {
		if !IsLocalGitCommand(cmd) {
			startedNonLocal++
			return nil, errors.New("unexpected fenced starter call for isolated scratch command: " + strings.Join(cmd.Args, " "))
		}
		ApplyLocalIsolation(cmd)
		return func() error { return runOwnedGitCommand(ctx, cmd) }, nil
	})

	paths, err := IsolatedReplayWritePaths(ctx, repo, baseSHA, []string{headSHA})
	if err != nil {
		t.Fatalf("IsolatedReplayWritePaths() error = %v", err)
	}
	if !slices.Equal(paths, []string{"b.txt"}) {
		t.Fatalf("IsolatedReplayWritePaths() = %v, want [b.txt]", paths)
	}
	if err := IsolatedMergeTree(ctx, repo, baseSHA, baseSHA, headSHA, func(_ func(...string) (string, error), merged string, mergeErr error) error {
		if mergeErr != nil || strings.TrimSpace(merged) == "" {
			return errors.New("expected clean isolated merge-tree")
		}
		return nil
	}); err != nil {
		t.Fatalf("IsolatedMergeTree() error = %v", err)
	}
	if startedNonLocal != 0 {
		t.Fatalf("startedNonLocal = %d, want 0", startedNonLocal)
	}

	diffIndexFenced := 0
	indexCtx := commandshell.WithCommandStarter(t.Context(), func(ctx context.Context, cmd *exec.Cmd) (func() error, error) {
		if slices.Contains(cmd.Args, "diff-index") {
			diffIndexFenced++
			return func() error { return runOwnedGitCommand(ctx, cmd) }, nil
		}
		if !IsLocalGitCommand(cmd) {
			return nil, errors.New("unexpected fenced starter call: " + strings.Join(cmd.Args, " "))
		}
		ApplyLocalIsolation(cmd)
		return func() error { return runOwnedGitCommand(ctx, cmd) }, nil
	})
	if err := IsolatedMergeTreeIndex(indexCtx, repo, baseSHA, baseSHA, headSHA, func(_ func(...string) (string, error), diffIndex func(string) (string, error), merged string, mergeErr error) error {
		if mergeErr != nil {
			return mergeErr
		}
		tree, _, _ := strings.Cut(merged, "\x00")
		_, err := diffIndex(strings.TrimSpace(tree))
		return err
	}); err != nil {
		t.Fatalf("IsolatedMergeTreeIndex() error = %v", err)
	}
	if diffIndexFenced != 1 {
		t.Fatalf("diffIndexFenced = %d, want 1", diffIndexFenced)
	}
}
