package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/instancelock"
)

// Exercise the real Make graph with controlled tools, so this regression does
// not run the full suite recursively. Both builds must start before either is
// released, even while a legacy caller holds the common-directory gate lock.
func TestMakeCheckFastOverlapsWorktrees(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

	if runtime.GOOS == "windows" {
		t.Skip("Makefile requires POSIX shell commands")
	}
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), validationIntegrationTimeout)
	defer cancel()
	env := make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(key, "GIT_") && key != "MAKEFLAGS" && key != "MFLAGS" && key != "MAKEOVERRIDES" {
			env = append(env, entry)
		}
	}
	env = append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	// Force the logical/physical path difference seen on macOS runners.
	physicalRoot := t.TempDir()
	root := filepath.Join(physicalRoot, "linked")
	if err := os.Symlink(physicalRoot, root); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", source}, args...)...)
		cmd.Env = env
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
	}
	git("init", "-b", "fixture")
	git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.com", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "fixture")
	lock, err := instancelock.Acquire(filepath.Join(source, ".git", "detent-validation.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := lock.Close(); err != nil {
			t.Error(err)
		}
	}()
	makefile, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	ready := make(chan error, 2)
	results := make(chan error, 2)
	var releases []io.WriteCloser
	defer func() {
		for _, release := range releases {
			// Wait closes these pipes on successful completion; close also
			// releases blocked helpers when setup or the handshake fails.
			if err := release.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
				t.Error(err)
			}
		}
	}()
	for _, name := range []string{"first", "second"} {
		dir := filepath.Join(root, name)
		git("worktree", "add", "--detach", dir, "HEAD")
		for file, content := range map[string]string{
			"Makefile": string(makefile), ".golangci-version": "test\n", "go.mod": "module fixture\n",
			"go": `#!/bin/sh
set -eu
case "$*" in
  'build ./...') exec "$DETENT_MAKE_TEST_BINARY" -test.run='^TestMakeCheckFastBuildHelper$' ;;
  'run ./tools/invariantcheck'|'run ./tools/migrationcheck'|'run ./internal/config/cmd/configdoc -root .') exit 0 ;;
  'run github.com/sqlc-dev/sqlc/cmd/sqlc@'*' diff -f sqlc/sqlc.yaml') exit 0 ;;
  *) echo "unexpected gate command: $*" >&2; exit 99 ;;
esac
`,
		} {
			if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		cmd := exec.CommandContext(ctx, "make", "-o", "check-app", "-o", "lint", "-o", "vet", "-o", "test-fast", "check-fast", "VERSION=test", "COMMIT=test", "DATE=test")
		cmd.Dir = dir
		cmd.Env = append(append([]string{}, env...), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "DETENT_MAKE_TEST_BINARY="+binary, "DETENT_MAKE_BUILD_HELPER=1")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		stdin, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, stdin)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		go func() {
			scanner := bufio.NewScanner(stdout)
			reported := false
			for scanner.Scan() {
				if scanner.Text() == "build ready" {
					ready <- nil
					reported = true
				}
			}
			waitErr := cmd.Wait()
			if !reported {
				ready <- fmt.Errorf("%s never reached build: %s", name, stderr.String())
			}
			if err := scanner.Err(); err != nil {
				results <- err
				return
			}
			results <- waitErr
		}()
	}
	for range 2 {
		select {
		case err := <-ready:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal("worktrees did not overlap:", ctx.Err())
		}
	}
	t.Logf("both worktrees reached build with the legacy lock held in %s", time.Since(started))
	for _, release := range releases {
		if _, err := io.WriteString(release, "release\n"); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		select {
		case err := <-results:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal("worktrees did not complete:", ctx.Err())
		}
	}
	for _, name := range []string{"first", "second"} {
		dir := filepath.Join(root, name)
		artifact, err := os.ReadFile(filepath.Join(dir, "tmp", "build-evidence"))
		if err != nil {
			t.Fatal(err)
		}
		want, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		// macOS may report /private/var for a worktree created under /var.
		got, err := os.Stat(string(artifact))
		if err != nil || !os.SameFile(got, want) {
			t.Fatalf("%s artifact = %q, %v; want worktree-local evidence", name, artifact, err)
		}
	}
}

func TestMakeCheckFastBuildHelper(t *testing.T) {
	if os.Getenv("DETENT_MAKE_BUILD_HELPER") != "1" {
		return
	}
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println("build ready")
	if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll("tmp", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("tmp/build-evidence", []byte(dir), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestMakeCheckPreflightWithoutSharedLock(t *testing.T) {
	if testing.Short() {
		t.Skip("process lifecycle integration")
	}

	if runtime.GOOS == "windows" {
		t.Skip("Makefile requires POSIX shell commands")
	}
	if _, err := exec.LookPath("make"); err != nil {
		t.Fatal(err)
	}
	makefile, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		broken    string
		invalid   bool
		wantTrace string
		wantError bool
		staleSQL  bool
	}{
		{"stale SQL", "", false, "invariants\nmigrations\nconfigdoc\nsqlc\n", true, true},
		{"broken config generator", "configdoc", false, "invariants\nmigrations\nconfigdoc\n", true, false},
		{"generation succeeds", "", false, "invariants\nmigrations\nconfigdoc\nsqlc\n", false, false},
		{"invariant failure", "", true, "invariants\n", true, false},
	}
	for _, target := range []string{"check", "check-fast"} {
		for _, tt := range tests {
			t.Run(target+"/"+tt.name, func(t *testing.T) {
				dir := t.TempDir()
				write := func(name, content string, mode os.FileMode) {
					t.Helper()
					if err := os.WriteFile(filepath.Join(dir, name), []byte(content), mode); err != nil {
						t.Fatal(err)
					}
				}
				write("Makefile", string(makefile), 0o600)
				write(".golangci-version", "test", 0o600)
				write("go.mod", "module fixture\n", 0o600)
				if tt.broken != "" {
					write("broken-"+tt.broken, "", 0o600)
				}
				if tt.staleSQL {
					write("stale-sql", "", 0o600)
				}
				if tt.invalid {
					write("invalid-invariants", "", 0o600)
				}
				write("git", "#!/bin/sh\npwd\n", 0o700)
				write("go", `#!/bin/sh
set -eu
case "$*" in
  'run ./tools/invariantcheck')
    echo invariants >> trace
    test ! -f invalid-invariants
    ;;
  'run ./tools/migrationcheck')
    echo migrations >> trace
    ;;
  'run github.com/sqlc-dev/sqlc/cmd/sqlc@'*' diff -f sqlc/sqlc.yaml')
    echo sqlc >> trace
    test ! -f stale-sql
    ;;
  'run ./internal/config/cmd/configdoc -root .')
    echo configdoc >> trace
    test ! -f broken-configdoc
    ;;
  'build ./...')
    echo build >> trace
    ;;
  *) exit 99 ;;
esac
`, 0o700)
				t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
				t.Setenv("MAKEFLAGS", "")
				t.Setenv("MFLAGS", "")
				t.Setenv("MAKEOVERRIDES", "")
				ctx, cancel := context.WithTimeout(t.Context(), validationIntegrationTimeout)
				defer cancel()
				cmd := exec.CommandContext(ctx, "make", "-o", "check-app", "-o", "build", "-o", "lint", "-o", "vet", "-o", "nilaway-audit", "-o", "test-race", "-o", "test-cover", "-o", "test-fast", target, "VERSION=test", "COMMIT=test", "DATE=test", "GOLANGCI_LINT_VERSION=test")
				cmd.Dir = dir
				output, err := cmd.CombinedOutput()
				if (err != nil) != tt.wantError {
					t.Fatalf("make gate error = %v, want error %v\n%s", err, tt.wantError, output)
				}
				trace, err := os.ReadFile(filepath.Join(dir, "trace"))
				if err != nil {
					t.Fatal(err)
				}
				wantTrace := tt.wantTrace
				if target == "check-fast" && !tt.wantError {
					wantTrace += "build\n"
				}
				if string(trace) != wantTrace {
					t.Errorf("trace = %q, want %q\n%s", trace, wantTrace, output)
				}
				if got := strings.Contains(string(output), "checks passed"); got == tt.wantError {
					t.Errorf("success message = %v, want %v\n%s", got, !tt.wantError, output)
				}
			})
		}
	}
}
