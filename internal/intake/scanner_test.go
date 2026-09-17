package intake

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/testenv"
)

func TestMain(m *testing.M) {
	if err := testenv.ClearGitEnvironment(); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

func TestStaleTODOScanner(t *testing.T) {
	tests := []struct {
		name      string
		setup     func(*testing.T, string)
		wantPaths []string
		wantErr   string
	}{
		{
			name: "Git worktree scans only tracked regular files",
			setup: func(t *testing.T, root string) {
				writeScannerTestFile(t, root, ".gitignore", ".next/\n")
				writeScannerTestFile(t, root, "main.go", "package main\n\n// TODO: handle retries\n")
				writeScannerTestFile(t, root, ".next/server/chunk.js", "// TODO: compiled vendor chunk\n")
				writeScannerTestFile(t, root, "untracked.go", "// TODO: untracked source\n")
				writeScannerTestFile(t, root, "detent.yaml", "target_state: Todo\n- Todo\n# config and stale-TODO scan\n")
				writeScannerTestFile(t, root, "WORKFLOW.md", "For `Todo`:\nThe TODO markers are scanned weekly.\n")

				cmd := exec.CommandContext(t.Context(), "git")
				cmd.Args = []string{"git", "-C", root, "init", "--quiet"}
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git init error = %v, output = %s", err, output)
				}
				cmd = exec.CommandContext(t.Context(), "git")
				cmd.Args = []string{"git", "-C", root, "add", "--", ".gitignore", "main.go", "detent.yaml", "WORKFLOW.md"}
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git add error = %v, output = %s", err, output)
				}
				scannerGit(t, root, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "fixture")
				scannerGit(t, root, "remote", "add", "origin", root)

			},
			wantPaths: []string{"main.go"},
		},
		{
			name: "missing origin fails without checkout fallback",
			setup: func(t *testing.T, root string) {
				scannerGit(t, root, "init")
				writeScannerTestFile(t, root, "main.go", "// TODO: local only\n")
				scannerGit(t, root, "add", ".")
			},
			wantErr: "resolve stale TODO remote default branch",
		},

		{
			name: "non-Git root returns actionable error",
			setup: func(t *testing.T, root string) {
				t.Setenv("GIT_CEILING_DIRECTORIES", os.TempDir())
				writeScannerTestFile(t, root, "main.go", "// TODO: source without repository\n")
			},
			wantErr: "source root must be a Git worktree",
		},
		{
			name: "bare repository returns actionable error",
			setup: func(t *testing.T, root string) {
				cmd := exec.CommandContext(t.Context(), "git")
				cmd.Args = []string{"git", "-C", root, "init", "--bare", "--quiet"}
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git init --bare error = %v, output = %s", err, output)
				}
			},
			wantErr: "source root must be a Git worktree",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			tt.setup(t, root)

			scanner, err := DefaultScannerFactory().New("stale-todos", root)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			events, err := scanner.Scan(context.Background())
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Scan() error = %v, want error containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Scan() error = %v", err)
			}

			gotPaths := make([]string, 0, len(events))
			for _, event := range events {
				gotPaths = append(gotPaths, event.Fields["path"])
			}
			if strings.Join(gotPaths, ",") != strings.Join(tt.wantPaths, ",") {
				t.Fatalf("event paths = %v, want %v", gotPaths, tt.wantPaths)
			}
		})
	}
}

func writeScannerTestFile(t *testing.T, root string, path string, contents string) {
	t.Helper()

	fullPath := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o700); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", filepath.Dir(fullPath), err)
	}
	if err := os.WriteFile(fullPath, []byte(contents), 0o600); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", fullPath, err)
	}
}

func TestTODOMarkerShape(t *testing.T) {
	for _, tt := range []struct {
		line string
		want bool
	}{
		{"target_state: Todo", false}, {"- Todo", false}, {"For `Todo`:", false},
		{"# config and stale-TODO scan", false}, {"// todo: lowercase", false},
		{"// TODO: retry", true}, {"# FIXME retry", true}, {"// TODO(owner): retry", true},
		{"/* TODO retry */", true}, {"TODO: retry", true},
		{"* FIXME retry", true}, {"<!-- TODO retry -->", true}, {"-- TODO retry", true},
		{"value := 1 // TODO retry", true}, {"The TODO markers are scanned", false},
		{"NOTTODO: prose", false}, {"stale-TODO: prose", false},
	} {
		t.Run(tt.line, func(t *testing.T) {
			if got := (todoMarker(tt.line) != nil); got != tt.want {
				t.Fatalf("match = %t, want %t", got, tt.want)
			}
		})
	}
}

func scannerGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", root}, args...)...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func TestStaleTODOScannerRevision(t *testing.T) {
	for _, branch := range []string{"main", "trunk"} {
		t.Run(branch, func(t *testing.T) {
			remote, root := t.TempDir(), t.TempDir()
			scannerGit(t, remote, "init", "-b", branch)
			scannerGit(t, remote, "config", "user.email", "test@example.com")
			scannerGit(t, remote, "config", "user.name", "Test")
			writeScannerTestFile(t, remote, "removed.go", "// TODO: removed remotely\n")
			scannerGit(t, remote, "add", ".")
			scannerGit(t, remote, "commit", "-m", "old")
			scannerGit(t, root, "clone", remote, ".")
			scannerGit(t, root, "fetch", "origin")
			scannerGit(t, remote, "rm", "removed.go")
			writeScannerTestFile(t, remote, "new.go", "// TODO: current default branch\n")
			writeScannerTestFile(t, remote, "large.go", "// TODO: oversized\n"+strings.Repeat("x", maxScannedFileBytes))
			if err := os.Symlink("new.go", filepath.Join(remote, "link.go")); err != nil {
				t.Fatal(err)
			}
			scannerGit(t, remote, "add", ".")
			scannerGit(t, remote, "commit", "-m", "new")
			writeScannerTestFile(t, root, "removed.go", "// TODO: dirty checkout\n")
			beforeRefs := scannerGit(t, root, "show-ref")
			beforeIndex := scannerGit(t, root, "ls-files", "--stage")
			fetchHeadPath := filepath.Join(root, ".git", "FETCH_HEAD")
			beforeFetch, err := os.ReadFile(fetchHeadPath)
			if err != nil {
				t.Fatal(err)
			}
			beforeHead := scannerGit(t, root, "rev-parse", "HEAD")
			beforeStatus := scannerGit(t, root, "status", "--porcelain")
			events, err := (staleTODOScanner{root: root}).Scan(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if len(events) != 1 || events[0].Fields["path"] != "new.go" {
				t.Fatalf("events = %+v, want only new.go", events)
			}
			afterFetch, err := os.ReadFile(fetchHeadPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(afterFetch) != string(beforeFetch) {
				t.Fatal("FETCH_HEAD changed")
			}
			if scannerGit(t, root, "show-ref") != beforeRefs {
				t.Fatal("refs changed")
			}
			if scannerGit(t, root, "ls-files", "--stage") != beforeIndex {
				t.Fatal("index changed")
			}
			if got := scannerGit(t, root, "rev-parse", "HEAD"); got != beforeHead {
				t.Fatal("checkout HEAD changed")
			}
			if got := scannerGit(t, root, "status", "--porcelain"); got != beforeStatus {
				t.Fatal("checkout status changed")
			}
		})
	}
}
