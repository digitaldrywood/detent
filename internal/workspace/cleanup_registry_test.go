package workspace

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLocalGitCleanupRemovesHookArtifacts(t *testing.T) {
	t.Parallel()

	source := initSourceRepo(t)
	root := filepath.Join(t.TempDir(), "workspaces")
	hookCommand := "mkdir -p node_modules/pkg uploads .local/state && touch node_modules/pkg/index.js uploads/generated.bin .local/state/cache"
	if runtime.GOOS == "windows" {
		hookCommand = "mkdir node_modules\\pkg uploads .local\\state && type nul > node_modules\\pkg\\index.js && type nul > uploads\\generated.bin && type nul > .local\\state\\cache"
	}
	backend, err := NewLocalGit(LocalGitOptions{
		Root:       root,
		SourceRoot: source,
		AutoBranch: true,
		Hooks:      Hooks{AfterCreate: hookCommand},
	})
	if err != nil {
		t.Fatalf("NewLocalGit() error = %v", err)
	}
	issue := Issue{ProjectID: "detent", ID: "2140", Identifier: "digitaldrywood/detent#2140"}
	info, err := backend.Create(t.Context(), issue)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	artifacts := []struct {
		name string
		path string
	}{
		{name: "node modules", path: "node_modules/pkg/index.js"},
		{name: "generated upload", path: "uploads/generated.bin"},
		{name: "local state", path: ".local/state/cache"},
	}
	for _, artifact := range artifacts {
		t.Run(artifact.name, func(t *testing.T) {
			if _, err := os.Stat(filepath.Join(info.Path, filepath.FromSlash(artifact.path))); err != nil {
				t.Fatalf("hook artifact stat error = %v", err)
			}
		})
	}

	if _, err := backend.CleanupIssue(t.Context(), issue); err != nil {
		t.Fatalf("CleanupIssue() error = %v", err)
	}
	if _, err := os.Stat(info.Path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("workspace exists after cleanup, stat error = %v", err)
	}
}

func TestLocalGitCleanupRetriesAfterGitDeregistration(t *testing.T) {
	skipWindows(t)

	enclosing := initSourceRepo(t)
	source := filepath.Join(enclosing, "source")
	initSourceRepoAt(t, source)
	root := filepath.Join(enclosing, "workspaces")
	backend, err := NewLocalGit(LocalGitOptions{Root: root, SourceRoot: source, AutoBranch: true})
	if err != nil {
		t.Fatalf("NewLocalGit() error = %v", err)
	}
	issue := Issue{ProjectID: "detent", ID: "2140", Identifier: "digitaldrywood/detent#2140"}
	info, err := backend.Create(t.Context(), issue)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	locked := filepath.Join(info.Path, "node_modules", "locked")
	if err := os.MkdirAll(locked, 0o700); err != nil {
		t.Fatalf("create locked artifact directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(locked, "generated.js"), []byte("generated"), 0o400); err != nil {
		t.Fatalf("write locked artifact: %v", err)
	}
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatalf("lock artifact directory: %v", err)
	}
	t.Cleanup(func() { restoreWritableTree(t, info.Path) })

	removeCalls := 0
	backend.removeOwnedPath = func(root string, path string) error {
		removeCalls++
		if removeCalls == 1 {
			return fs.ErrPermission
		}
		return removeWorkspacePath(root, path)
	}
	_, err = backend.CleanupIssue(t.Context(), issue)
	if err == nil {
		t.Fatal("first CleanupIssue() error = nil, want final removal failure")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("first CleanupIssue() error = %v, want permission denial", err)
	}
	registered, err := backend.sourceWorktreeRegistered(t.Context(), info.Path)
	if err != nil {
		t.Fatalf("sourceWorktreeRegistered() error = %v", err)
	}
	if registered {
		t.Fatal("workspace remains registered after partial Git removal")
	}
	if _, err := os.Stat(info.Path); err != nil {
		t.Fatalf("residual workspace stat error = %v", err)
	}
	if recorded, err := backend.cleanupOwnershipRecorded(t.Context(), info.Path); err != nil || !recorded {
		t.Fatalf("cleanup ownership recorded = %t, error = %v", recorded, err)
	}

	if _, err := backend.CleanupIssue(t.Context(), issue); err != nil {
		t.Fatalf("second CleanupIssue() error = %v", err)
	}
	if _, err := os.Stat(info.Path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("workspace exists after retry, stat error = %v", err)
	}
	if recorded, err := backend.cleanupOwnershipRecorded(t.Context(), info.Path); err != nil || recorded {
		t.Fatalf("cleanup ownership recorded after retry = %t, error = %v", recorded, err)
	}
}

func TestLocalGitReconcileResiduals(t *testing.T) {
	skipWindows(t)

	tests := []struct {
		name           string
		active         bool
		activeProcess  bool
		registered     bool
		wantRemoved    int
		wantActive     int
		wantRegistered int
		wantExists     bool
	}{
		{name: "removes owned residual", wantRemoved: 1},
		{name: "skips active issue", active: true, wantActive: 1, wantExists: true},
		{name: "skips active process", activeProcess: true, wantActive: 1, wantExists: true},
		{name: "skips registered worktree", registered: true, wantRegistered: 1, wantExists: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			enclosing := initSourceRepo(t)
			source := filepath.Join(enclosing, "source")
			initSourceRepoAt(t, source)
			root := filepath.Join(enclosing, "workspaces")
			backend, err := NewLocalGit(LocalGitOptions{Root: root, SourceRoot: source, AutoBranch: true})
			if err != nil {
				t.Fatalf("NewLocalGit() error = %v", err)
			}
			issue := Issue{ProjectID: "detent", ID: "2140", Identifier: "digitaldrywood/detent#2140"}
			var info Info
			if tt.registered {
				info, err = backend.Create(t.Context(), issue)
				if err != nil {
					t.Fatalf("Create() error = %v", err)
				}
				if err := backend.recordCleanupOwnership(t.Context(), info, issue, true); err != nil {
					t.Fatalf("recordCleanupOwnership() error = %v", err)
				}
			} else {
				info = strandCleanupWorkspace(t, backend, source, issue)
			}
			t.Cleanup(func() { restoreWritableTree(t, info.Path) })
			if tt.activeProcess {
				backend.scanWorkspacePaths = func(context.Context, string) ([]int, error) {
					return []int{os.Getpid() + 1000}, nil
				}
			}
			var active []Issue
			if tt.active {
				active = []Issue{issue}
			}

			result, err := backend.ReconcileResiduals(t.Context(), active)
			if err != nil {
				t.Fatalf("ReconcileResiduals() error = %v", err)
			}
			if result.Removed != tt.wantRemoved || result.ActiveSkipped != tt.wantActive || result.RegisteredSkipped != tt.wantRegistered {
				t.Fatalf("ReconcileResiduals() = %+v, want removed=%d active=%d registered=%d", result, tt.wantRemoved, tt.wantActive, tt.wantRegistered)
			}
			if got := len(result.CompletedPaths); got != tt.wantRemoved {
				t.Fatalf("ReconcileResiduals() completed paths = %d, want %d", got, tt.wantRemoved)
			}
			_, statErr := os.Stat(info.Path)
			if got := statErr == nil; got != tt.wantExists {
				t.Fatalf("residual workspace exists = %t, want %t, stat error = %v", got, tt.wantExists, statErr)
			}
		})
	}
}

func TestLocalGitReconcileResidualsRejectsInsufficientOwnership(t *testing.T) {
	t.Parallel()

	source := initSourceRepo(t)
	root := filepath.Join(t.TempDir(), "workspaces")
	backend, err := NewLocalGit(LocalGitOptions{Root: root, SourceRoot: source, AutoBranch: true})
	if err != nil {
		t.Fatalf("NewLocalGit() error = %v", err)
	}
	foreign := filepath.Join(root, "detent-digitaldrywood_detent_9999-000000000000")
	if err := os.MkdirAll(filepath.Join(foreign, "node_modules"), 0o700); err != nil {
		t.Fatalf("create foreign directory: %v", err)
	}
	sourceCommonDir, err := gitCommonDir(t.Context(), source)
	if err != nil {
		t.Fatalf("gitCommonDir() error = %v", err)
	}
	record := cleanupOwnershipRecord{
		Schema:          cleanupOwnershipSchema,
		Path:            foreign,
		Key:             filepath.Base(foreign),
		SourceCommonDir: sourceCommonDir + "-other",
	}
	if err := backend.writeOwnershipRecord(record); err != nil {
		t.Fatalf("writeOwnershipRecord() error = %v", err)
	}

	result, err := backend.ReconcileResiduals(t.Context(), nil)
	if err != nil {
		t.Fatalf("ReconcileResiduals() error = %v", err)
	}
	if result.UnownedSkipped != 1 || result.Removed != 0 {
		t.Fatalf("ReconcileResiduals() = %+v, want one unowned skip", result)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatalf("foreign directory was changed, stat error = %v", err)
	}
}

func strandCleanupWorkspace(t *testing.T, backend *LocalGit, source string, issue Issue) Info {
	t.Helper()

	info, err := backend.Create(t.Context(), issue)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := backend.recordCleanupOwnership(t.Context(), info, issue, true); err != nil {
		t.Fatalf("recordCleanupOwnership() error = %v", err)
	}
	locked := filepath.Join(info.Path, "node_modules", "locked")
	if err := os.MkdirAll(locked, 0o700); err != nil {
		t.Fatalf("create locked artifact directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(locked, "generated.js"), []byte("generated"), 0o400); err != nil {
		t.Fatalf("write locked artifact: %v", err)
	}
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatalf("lock artifact directory: %v", err)
	}
	cmd := exec.CommandContext(t.Context(), "git", "-C", source, "worktree", "remove", "--force", info.Path)
	if output, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("git worktree remove succeeded, want partial removal failure: %s", output)
	}
	if err := os.Chmod(locked, 0o700); err != nil {
		t.Fatalf("restore locked artifact directory: %v", err)
	}
	registered, err := backend.sourceWorktreeRegistered(t.Context(), info.Path)
	if err != nil {
		t.Fatalf("sourceWorktreeRegistered() error = %v", err)
	}
	if registered {
		t.Fatal("workspace remains registered after partial removal")
	}
	return info
}
