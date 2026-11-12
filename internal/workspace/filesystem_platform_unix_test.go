//go:build !windows

package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestFilesystemCleanupRemediatesGeneratedCachePermissions(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "workspaces")
	backend, err := NewFilesystem(FilesystemOptions{Root: root})
	if err != nil {
		t.Fatalf("NewFilesystem() error = %v", err)
	}

	issue := Issue{Identifier: "DD-CACHE-PERM"}
	info, err := backend.Create(context.Background(), issue)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	t.Cleanup(func() {
		restoreWritableTree(t, info.Path)
	})

	cacheDir := filepath.Join(info.Path, "tmp", ".gomodcache-ignored", "modernc.org", "libc@v1.73.4")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatalf("mkdir cache dir: %v", err)
	}
	cacheFile := filepath.Join(cacheDir, "libc_amd64.go")
	if err := os.WriteFile(cacheFile, []byte("package libc\n"), 0o600); err != nil {
		t.Fatalf("write cache file: %v", err)
	}
	if err := os.Chmod(cacheFile, 0o444); err != nil {
		t.Fatalf("chmod cache file: %v", err)
	}
	if err := os.Chmod(cacheDir, 0o555); err != nil {
		t.Fatalf("chmod cache dir: %v", err)
	}

	result, err := backend.CleanupIssue(context.Background(), issue)
	if err != nil {
		t.Fatalf("CleanupIssue() error = %v", err)
	}
	if result.Worktrees != 1 {
		t.Fatalf("CleanupIssue().Worktrees = %d, want 1", result.Worktrees)
	}
	if _, err := os.Stat(info.Path); !os.IsNotExist(err) {
		t.Fatalf("workspace still exists or unexpected stat error: %v", err)
	}
}

func TestFilesystemCreateRejectsArtifactSymlinkEscape(t *testing.T) {
	t.Parallel()

	testRoot := t.TempDir()
	root := filepath.Join(testRoot, "workspaces")
	workspacePath := filepath.Join(root, "DD-SYM")
	outside := filepath.Join(testRoot, "outside")
	if err := os.MkdirAll(workspacePath, 0o700); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(workspacePath, "artifacts")); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	backend, err := NewFilesystem(FilesystemOptions{Root: root})
	if err != nil {
		t.Fatalf("NewFilesystem() error = %v", err)
	}

	_, err = backend.Create(context.Background(), Issue{Identifier: "DD-SYM"})
	if err == nil {
		t.Fatal("Create() error = nil, want symlink escape rejection")
	}
	if _, err := os.Lstat(filepath.Join(workspacePath, "artifacts")); err != nil {
		t.Fatalf("symlink stat error = %v", err)
	}
}
