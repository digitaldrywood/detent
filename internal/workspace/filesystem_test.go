package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestFilesystemWorkspaceCreatesArtifactWorkspaceAndOutputRoot(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "workspaces")
	outputRoot := filepath.Join(t.TempDir(), "outputs")
	backend, err := NewFilesystem(FilesystemOptions{
		Root:       root,
		OutputRoot: outputRoot,
	})
	if err != nil {
		t.Fatalf("NewFilesystem() error = %v", err)
	}

	issue := Issue{
		ProjectID:  "video",
		ID:         "ad-1",
		Identifier: "store/ad-1",
	}
	info, err := backend.Create(context.Background(), issue)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if info.Path == "" || info.Key == "" || info.Branch != "" || !info.Created {
		t.Fatalf("Info = %#v", info)
	}
	if _, err := os.Stat(filepath.Join(info.Path, "artifacts")); err != nil {
		t.Fatalf("artifact directory missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outputRoot, info.Key)); err != nil {
		t.Fatalf("output directory missing: %v", err)
	}
	initialEvidence, err := backend.ArtifactEvidence(context.Background(), info, issue)
	if err != nil {
		t.Fatalf("initial ArtifactEvidence() error = %v", err)
	}
	if !initialEvidence.Available || initialEvidence.Files != 0 || initialEvidence.Fingerprint == "" {
		t.Fatalf("initial ArtifactEvidence() = %#v, want available empty output fingerprint", initialEvidence)
	}
	if err := os.WriteFile(filepath.Join(info.Path, "artifacts", "manifest.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	workspaceOnlyEvidence, err := backend.ArtifactEvidence(context.Background(), info, issue)
	if err != nil {
		t.Fatalf("workspace-only ArtifactEvidence() error = %v", err)
	}
	if workspaceOnlyEvidence != initialEvidence {
		t.Fatalf("workspace-only ArtifactEvidence() = %#v, want unchanged from %#v", workspaceOnlyEvidence, initialEvidence)
	}
	if err := os.WriteFile(filepath.Join(outputRoot, info.Key, "final.mp4"), []byte("rendered-video"), 0o600); err != nil {
		t.Fatalf("write output artifact: %v", err)
	}
	finalEvidence, err := backend.ArtifactEvidence(context.Background(), info, issue)
	if err != nil {
		t.Fatalf("final ArtifactEvidence() error = %v", err)
	}
	if !finalEvidence.Available || finalEvidence.Files != 1 || finalEvidence.Fingerprint == initialEvidence.Fingerprint {
		t.Fatalf("final ArtifactEvidence() = %#v, want one changed output file from %#v", finalEvidence, initialEvidence)
	}

	stat, err := backend.DiffStat(context.Background(), info, issue)
	if err != nil {
		t.Fatalf("DiffStat() error = %v", err)
	}
	if stat.Files != 1 {
		t.Fatalf("DiffStat().Files = %d, want 1", stat.Files)
	}
	if stat.Fingerprint == "" {
		t.Fatal("DiffStat().Fingerprint is empty")
	}
	if err := os.WriteFile(filepath.Join(info.Path, "artifacts", "manifest.json"), []byte("[]\n"), 0o600); err != nil {
		t.Fatalf("update artifact: %v", err)
	}
	updated, err := backend.DiffStat(context.Background(), info, issue)
	if err != nil {
		t.Fatalf("updated DiffStat() error = %v", err)
	}
	if updated.Files != stat.Files || updated.Fingerprint == stat.Fingerprint {
		t.Fatalf("updated DiffStat() = %+v, want unchanged count and changed fingerprint from %+v", updated, stat)
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

func TestFilesystemCleanupRemediatesGeneratedCachePermissions(t *testing.T) {
	t.Parallel()
	skipWindows(t)

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
	skipWindows(t)

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
