package orchestrator

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func e2eInitSourceRepo(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	e2eRunCommand(t, dir, "git", "init", "-b", "main")
	e2eRunGit(t, dir, "config", "user.name", "Test User")
	e2eRunGit(t, dir, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("source repo\n"), 0o600); err != nil {
		t.Fatalf("write README.md: %v", err)
	}
	e2eRunGit(t, dir, "add", "README.md")
	e2eRunGit(t, dir, "commit", "-m", "initial")

	return dir
}

func e2eRunGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return e2eRunCommand(t, dir, "git", args...)
}

func e2eRunCommand(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), name, args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s failed: %v\n%s", name, strings.Join(args, " "), err, output)
	}
	return string(output)
}
