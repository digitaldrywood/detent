package gitfixture

import (
	"os"
	"path/filepath"
	"testing"
)

type command func(t *testing.T, dir string, args ...string) string

func SourceRepo(t *testing.T, git command) string {
	t.Helper()

	dir := t.TempDir()
	git(t, dir, "init", "-b", "main")
	git(t, dir, "config", "maintenance.autoDetach", "false")
	git(t, dir, "config", "core.autocrlf", "false")
	git(t, dir, "config", "user.name", "Test User")
	git(t, dir, "config", "user.email", "test@example.com")
	git(t, dir, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("source repo\n"), 0o600); err != nil {
		t.Fatalf("write README.md: %v", err)
	}
	git(t, dir, "add", "README.md")
	git(t, dir, "commit", "-m", "initial")
	return dir
}

func AddOrigin(t *testing.T, source string, git command) string {
	t.Helper()
	remote := filepath.Join(t.TempDir(), "origin.git")
	git(t, source, "init", "--bare", "-b", "main", remote)
	git(t, source, "remote", "add", "origin", remote)
	git(t, source, "push", "-u", "origin", "main")
	return remote
}

func ReadFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
