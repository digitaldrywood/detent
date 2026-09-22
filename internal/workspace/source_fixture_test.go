package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
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
		filtered = append(filtered, entry)
	}
	return filtered, nil
}

// removingSeedFS removes a file after enumeration but before CopyFS opens it.
type removingSeedFS struct {
	fs.FS
	root    string
	target  string
	removed bool
}

func (f *removingSeedFS) ReadDir(name string) ([]fs.DirEntry, error) {
	entries, err := fs.ReadDir(f.FS, name)
	if err == nil && name == path.Dir(f.target) && !f.removed {
		err = os.Remove(filepath.Join(f.root, filepath.FromSlash(f.target)))
		f.removed = err == nil
	}
	return entries, err
}

func copySourceRepoSeed(dir string, source fs.FS) error {
	return os.CopyFS(dir, seedCopyFS{source})
}

func TestCopySourceRepoSeedTransientFiles(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name       string
		target     string
		remove     bool
		wantError  bool
		wantCopied bool
	}{
		{name: "disappearing maintenance lock", target: ".git/objects/maintenance.lock", remove: true},
		{name: "existing index lock", target: ".git/index.lock"},
		{name: "existing ref lock", target: ".git/refs/heads/main.lock"},
		{name: "tracked lock file", target: "dependencies.lock", wantCopied: true},
		{name: "missing git object", target: ".git/objects/ab/object", remove: true, wantError: true},
		{name: "missing tracked file", target: "README.md", remove: true, wantError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			source, destination := t.TempDir(), t.TempDir()
			target := filepath.Join(source, filepath.FromSlash(tt.target))
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(target, []byte("fixture"), 0o600); err != nil {
				t.Fatal(err)
			}
			sourceFS := os.DirFS(source)
			removing := &removingSeedFS{FS: sourceFS, root: source, target: tt.target}
			if tt.remove {
				sourceFS = removing
			}
			err := copySourceRepoSeed(destination, sourceFS)
			if tt.remove && !removing.removed {
				t.Fatal("file was not removed during directory enumeration")
			}
			if tt.wantError {
				if !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("copy error = %v, want missing file", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(destination, filepath.FromSlash(tt.target)))
			if tt.wantCopied {
				if err != nil || string(data) != "fixture" {
					t.Fatalf("copied file = %q, %v", data, err)
				}
			} else if !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("Git lock copied: read error = %v", err)
			}
		})
	}
}
