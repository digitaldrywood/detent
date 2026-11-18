package gitrepo

import (
	"context"
	"crypto/sha1"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type Fixtures struct {
	bare   func(*testing.T) string
	Source func(*testing.T) string
}

func NewFixtures(root func() string) Fixtures {
	return Fixtures{
		bare:   repositorySeed(root, "bare", buildBareRepository),
		Source: repositorySeed(root, "source", buildSourceRepository),
	}
}

// Windows Git appends metadata beneath worktrees; testing's long subtest
// directory names can exhaust its path budget before Git creates that metadata.
func TempDir(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "windows" {
		return t.TempDir()
	}
	dir, err := os.MkdirTemp("", "native-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	return dir
}

func repositorySeed(root func() string, name string, build func(context.Context, string) error) func(*testing.T) string {
	seed := sync.OnceValues(func() (string, error) {
		dir, err := os.MkdirTemp(root(), name+"-seed-")
		if err != nil {
			return "", err
		}
		return dir, build(context.Background(), dir)
	})
	return func(t *testing.T) string {
		t.Helper()
		source, err := seed()
		if err != nil {
			t.Fatalf("build %s repository fixture: %v", name, err)
		}
		dir := TempDir(t)
		if err := os.CopyFS(dir, os.DirFS(source)); err != nil {
			t.Fatalf("copy %s repository fixture: %v", name, err)
		}
		return dir
	}
}

func seedGit(ctx context.Context, dir, input string, args ...string) error {
	commandArgs := append([]string{"-c", "maintenance.auto=false", "-c", "gc.auto=0"}, args...)
	command := exec.CommandContext(ctx, "git", commandArgs...)
	command.Dir = dir
	command.Env = append(os.Environ(), "GIT_CEILING_DIRECTORIES="+dir)
	command.Stdin = strings.NewReader(input)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, output)
	}
	if len(args) > 0 && args[0] == "init" {
		for _, setting := range [][2]string{{"maintenance.auto", "false"}, {"gc.auto", "0"}} {
			if err := seedGit(ctx, dir, "", "config", "--local", setting[0], setting[1]); err != nil {
				return err
			}
		}
	}
	return nil
}

func buildBareRepository(ctx context.Context, dir string) error {
	if err := seedGit(ctx, dir, "", "init", "--bare", "--object-format=sha1", "--template="); err != nil {
		return err
	}
	if err := seedGit(ctx, dir, "", "hash-object", "-w", "-t", "tree", "--stdin"); err != nil {
		return err
	}
	for _, label := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		if err := seedGit(ctx, dir, fixtureCommit(label), "hash-object", "-w", "-t", "commit", "--stdin"); err != nil {
			return err
		}
	}
	return nil
}

func buildSourceRepository(ctx context.Context, dir string) error {
	for _, args := range [][]string{
		{"init", "-b", "main"}, {"config", "core.autocrlf", "false"},
		{"config", "commit.gpgsign", "false"}, {"config", "core.hooksPath", os.DevNull},
		{"config", "user.name", "Test User"}, {"config", "user.email", "test@example.com"},
	} {
		if err := seedGit(ctx, dir, "", args...); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("source repo\n"), 0o600); err != nil {
		return err
	}
	if err := seedGit(ctx, dir, "", "add", "README.md"); err != nil {
		return err
	}
	return seedGit(ctx, dir, "", "commit", "-m", "initial")
}

func fixtureCommit(label string) string {
	return "tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\nauthor Test User <test@example.com> 1 +0000\ncommitter Test User <test@example.com> 1 +0000\n\n" + label + "\n"
}

func Head(label string) string {
	commit := fixtureCommit(label)
	return fmt.Sprintf("%x", sha1.Sum([]byte(fmt.Sprintf("commit %d\x00%s", len(commit), commit))))
}

func (repos Fixtures) Capture(ctx context.Context, t *testing.T, base, head string, repositories ...string) tracker.ChangeSourceCapture {
	t.Helper()
	var directory string
	if len(repositories) > 0 {
		directory = repositories[0]
	}
	for _, label := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		if head == Head(label) {
			directory = ""
			break
		}
	}
	if directory == "" {
		directory = repos.bare(t)
	}
	command := exec.CommandContext(ctx, "git", "-C", directory, "pack-objects", "--stdout", "--revs")
	command.Stdin = strings.NewReader(head + "\n")
	pack, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	bundle := append([]byte("# v2 git bundle\n"+head+" HEAD\n\n"), pack...)
	return tracker.ChangeSourceCapture{Source: tracker.ChangeSource{Format: "git-bundle", BaseSHA: base, HeadSHA: head, BundleSHA256: tracker.ChangeSourceDigest(bundle), DiffSHA256: strings.Repeat("d", 64), Bytes: int64(len(bundle))}, Bundle: bundle}
}

func ReplacementSource(t *testing.T, source, base string) tracker.ChangeSourceCapture {
	t.Helper()
	if err := os.WriteFile(filepath.Join(source, "replacement.txt"), []byte("replacement source\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = source
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v, %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("add", "replacement.txt")
	git("commit", "-m", "replacement source")
	capture, err := workspace.CaptureChangeSource(t.Context(), source, base, git("rev-parse", "HEAD"))
	if err != nil {
		t.Fatal(err)
	}
	return capture
}

func GitOutput(t *testing.T, path string, args ...string) string {
	t.Helper()
	output, err := exec.CommandContext(t.Context(), "git", append([]string{"-C", path}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return string(output)
}
func ImplementingState(t *testing.T, workspace string) []string {
	t.Helper()
	untracked, err := os.ReadFile(filepath.Join(workspace, "untracked.txt"))
	if err != nil {
		t.Fatal(err)
	}
	return []string{
		GitOutput(t, workspace, "rev-parse", "HEAD"),
		GitOutput(t, workspace, "status", "--porcelain"),
		GitOutput(t, workspace, "diff", "--binary"),
		GitOutput(t, workspace, "diff", "--cached", "--binary"),
		string(untracked),
	}
}
