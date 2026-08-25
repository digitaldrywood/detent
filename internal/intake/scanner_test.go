package intake

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

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

				cmd := exec.CommandContext(t.Context(), "git")
				cmd.Args = []string{"git", "-C", root, "init", "--quiet"}
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git init error = %v, output = %s", err, output)
				}
				cmd = exec.CommandContext(t.Context(), "git")
				cmd.Args = []string{"git", "-C", root, "add", "--", ".gitignore", "main.go"}
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git add error = %v, output = %s", err, output)
				}
			},
			wantPaths: []string{"main.go"},
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
