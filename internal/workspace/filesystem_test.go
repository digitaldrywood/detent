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
	if err := os.WriteFile(filepath.Join(info.Path, "artifacts", "manifest.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write artifact: %v", err)
	}

	stat, err := backend.DiffStat(context.Background(), info, issue)
	if err != nil {
		t.Fatalf("DiffStat() error = %v", err)
	}
	if stat.Files != 1 {
		t.Fatalf("DiffStat().Files = %d, want 1", stat.Files)
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
