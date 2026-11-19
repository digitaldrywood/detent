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

func TestReview1973FilesystemAttemptPaths(t *testing.T) {
	backend, err := NewFilesystem(FilesystemOptions{Root: t.TempDir(), OutputRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	issue := Issue{ID: "wi_example", Identifier: "prj_example#1",
		AttemptWorkspace: AttemptWorkspace{Organization: "org_example", Project: "prj_example", Attempt: "attempt_11111111111111111111111111111111"}}
	first, err := backend.Create(t.Context(), issue)
	if err != nil {
		t.Fatal(err)
	}
	issue.AttemptWorkspace.Attempt = "attempt_22222222222222222222222222222222"
	second, err := backend.Create(t.Context(), issue)
	if err != nil {
		t.Fatal(err)
	}
	if first.Path == second.Path || first.Key == second.Key {
		t.Fatalf("attempts share a workspace or output: %+v %+v", first, second)
	}
	for _, info := range []Info{first, second} {
		if _, err := os.Stat(filepath.Join(backend.outputRoot, info.Key)); err != nil {
			t.Fatal(err)
		}
	}
}
