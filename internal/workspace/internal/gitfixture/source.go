package gitfixture

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/workspace/gitexec"
)

var (
	sourceRepoSeedDir  string
	sourceRepoSeedOnce sync.Once
	sourceRepoSeedErr  error
)

func RunTests(m *testing.M) int {
	if err := testenv.ClearGitEnvironment(); err != nil {
		panic(err)
	}
	if err := os.Unsetenv("GOFLAGS"); err != nil {
		panic(err)
	}
	for key, value := range map[string]string{"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1"} {
		if err := os.Setenv(key, value); err != nil {
			panic(err)
		}
	}

	packageTemp, err := os.MkdirTemp("", "workspace-tests-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() {
		if err := os.RemoveAll(packageTemp); err != nil {
			fmt.Fprintf(os.Stderr, "remove package temp: %v\n", err)
		}
	}()
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		if err := os.Setenv(key, packageTemp); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
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
		{"config", "commit.gpgsign", "false"},
		{"add", "README.md"},
		{"commit", "-m", "initial"},
	} {
		cmd := gitexec.Command(ctx, dir, args...)
		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git %v: %w\n%s", args, err, output)
		}
	}
	return nil
}

// Git may finish background maintenance after seed construction returns. Lock
// files are coordination state, not repository contents; exclude them before
// CopyFS asks for their metadata or opens them. Ordinary copy errors stay fatal.
type seedCopyFS struct{ fs.FS }

func (f seedCopyFS) ReadDir(name string) ([]fs.DirEntry, error) {
	entries, err := fs.ReadDir(f.FS, name)
	if err != nil {
		return nil, err
	}
	if name != ".git" && !strings.HasPrefix(name, ".git/") {
		return entries, nil
	}
	filtered := make([]fs.DirEntry, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".lock") {
			continue
		}
		if name == ".git/hooks" && !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sample") {
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered, nil
}

func copySourceRepoSeed(dir string, source fs.FS) error {
	return os.CopyFS(dir, seedCopyFS{source})
}

func SourceRepo(t *testing.T) string {
	t.Helper()

	dir := testenv.TempDir(t)
	SourceRepoAt(t, dir)
	return dir
}

func SourceRepoAt(t *testing.T, dir string) {
	t.Helper()

	sourceRepoSeedOnce.Do(func() {
		sourceRepoSeedErr = buildSourceRepoSeed(t.Context(), sourceRepoSeedDir)
	})
	if sourceRepoSeedErr != nil {
		t.Fatalf("build source repo seed: %v", sourceRepoSeedErr)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir source repo: %v", err)
	}
	if err := copySourceRepoSeed(dir, os.DirFS(sourceRepoSeedDir)); err != nil {
		t.Fatalf("copy source repo seed: %v", err)
	}
}

func BareRemote(t *testing.T) string {
	t.Helper()

	root := testenv.TempDir(t)
	dir := filepath.Join(root, "origin.git")
	Git(t, root, "init", "--bare", "-b", "main", dir)
	return dir
}

func Git(t *testing.T, dir string, args ...string) string {
	t.Helper()

	cmd := gitexec.Command(t.Context(), dir, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, output)
	}
	return string(output)
}

func ReadFile(t *testing.T, path string) string {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

func WriteFile(t *testing.T, root, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, path)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, path), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
