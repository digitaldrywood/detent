package workspace

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

var (
	sourceRepoSeedDir  string
	sourceRepoSeedOnce sync.Once
	sourceRepoSeedErr  error
)

func runWorkspaceTests(m *testing.M) int {
	var err error
	sourceRepoSeedDir, err = os.MkdirTemp("", "workspace-source-seed-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() {
		if err := os.RemoveAll(sourceRepoSeedDir); err != nil {
			fmt.Fprintf(os.Stderr, "remove source repo seed: %v\n", err)
		}
	}()
	return m.Run()
}

// Build the identical initial history once per test process. Each caller gets a
// full file copy, not a clone or hardlinks: refs, config, index, and objects must
// remain independent when tests mutate or remove their repositories. Build on
// first use so Git setup remains inside the package timer and caller's context.
func buildSourceRepoSeed(ctx context.Context, dir string) error {
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("source repo\n"), 0o600); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "core.autocrlf", "false"},
		{"config", "user.name", "Test User"},
		{"config", "user.email", "test@example.com"},
		{"add", "README.md"},
		{"commit", "-m", "initial"},
	} {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git %v: %w\n%s", args, err, output)
		}
	}
	return nil
}

func TestSourceRepoFixturesAreIndependent(t *testing.T) {
	for _, name := range []string{"existing directory", "nested directory"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			first := initSourceRepo(t)
			second := t.TempDir()
			if name == "nested directory" {
				second = filepath.Join(second, "nested", "source")
			}
			initSourceRepoAt(t, second)

			// Check every copied file, including Git objects, for hardlink aliasing.
			if err := filepath.WalkDir(first, func(path string, entry os.DirEntry, err error) error {
				if err != nil || entry.IsDir() {
					return err
				}
				rel, err := filepath.Rel(first, path)
				if err != nil {
					return err
				}
				a, err := os.Stat(path)
				if err != nil {
					return err
				}
				b, err := os.Stat(filepath.Join(second, rel))
				if err != nil {
					return err
				}
				if os.SameFile(a, b) {
					return fmt.Errorf("fixtures share file %s", rel)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}

			runGit(t, first, "config", "user.name", "Changed User")
			runGit(t, first, "checkout", "-b", "changed")
			if err := os.WriteFile(filepath.Join(first, "README.md"), []byte("changed\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			runGit(t, first, "add", "README.md")
			runGit(t, first, "commit", "-m", "changed")
			if err := os.RemoveAll(first); err != nil {
				t.Fatal(err)
			}

			// A later copy also proves the shared seed survived these mutations.
			for _, dir := range []string{second, initSourceRepo(t)} {
				for _, check := range []struct {
					args []string
					want string
				}{
					{[]string{"status", "--porcelain"}, ""},
					{[]string{"branch", "--format=%(refname:short)"}, "main\n"},
					{[]string{"config", "user.name"}, "Test User\n"},
					{[]string{"config", "core.autocrlf"}, "false\n"},
					{[]string{"log", "--format=%s"}, "initial\n"},
					{[]string{"show", "HEAD:README.md"}, "source repo\n"},
				} {
					if got := runGit(t, dir, check.args...); got != check.want {
						t.Errorf("git %v = %q, want %q", check.args, got, check.want)
					}
				}
				if got := readFile(t, filepath.Join(dir, "README.md")); got != "source repo\n" {
					t.Errorf("README.md = %q", got)
				}
			}
		})
	}
}
