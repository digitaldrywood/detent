//go:build !windows

package workspace

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalGitCleanupRetriesAfterGitDeregistration(t *testing.T) {
	t.Parallel()
	enclosing := initSourceRepo(t)
	source := filepath.Join(enclosing, "source")
	initSourceRepoAt(t, source)
	publishCleanupSource(t, source)
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
	writeFileDiffFile(t, source, ".git/info/exclude", "node_modules/\n")
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
	t.Parallel()
	tests := []struct {
		name           string
		active         bool
		activeProcess  bool
		alias          bool
		registered     bool
		unverified     bool
		wantRemoved    int
		wantActive     int
		wantRegistered int
		wantExists     bool
	}{
		{name: "removes owned residual", wantRemoved: 1},
		{name: "retains unverified residual", unverified: true, wantExists: true},
		{name: "skips active issue", active: true, wantActive: 1, wantExists: true},
		{name: "skips native active alias", active: true, alias: true, wantActive: 1, wantExists: true},
		{name: "skips active process", activeProcess: true, wantActive: 1, wantExists: true},
		{name: "removes safe registered worktree", registered: true, wantRemoved: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			enclosing := initSourceRepo(t)
			if tt.alias {
				alias := filepath.Join(t.TempDir(), "alias")
				if err := os.Symlink(enclosing, alias); err != nil {
					t.Fatal(err)
				}
				enclosing = alias
			}
			source := filepath.Join(enclosing, "source")
			initSourceRepoAt(t, source)
			publishCleanupSource(t, source)
			root := filepath.Join(enclosing, "workspaces")
			backend, err := NewLocalGit(LocalGitOptions{Root: root, SourceRoot: source, AutoBranch: true})
			if err != nil {
				t.Fatalf("NewLocalGit() error = %v", err)
			}
			issue := Issue{ProjectID: "detent", ID: "2140", Identifier: "digitaldrywood/detent#2140"}
			if tt.alias {
				issue = Issue{ProjectID: "old-alias", ID: "wi_active", Identifier: "prj_example#1"}
			}
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
				if tt.alias {
					legacy := legacyIssueKey(issue)
					old := info.Path
					info.Key, info.Path, info.Branch = legacy, filepath.Join(root, legacy), autoBranchPrefix+strings.ToLower(legacy)
					if err := os.Rename(old, info.Path); err != nil {
						t.Fatal(err)
					}
					info.Path, err = filepath.EvalSymlinks(info.Path)
					if err != nil {
						t.Fatal(err)
					}
					relative := cleanupOwnershipRecordRelativePath(old)
					record, err := backend.readOwnershipRecord(relative)
					if err != nil {
						t.Fatal(err)
					}
					record.Key, record.Path, record.Branch = info.Key, info.Path, info.Branch
					if err := os.Remove(filepath.Join(root, relative)); err != nil {
						t.Fatal(err)
					}
					if err := backend.writeOwnershipRecord(record); err != nil {
						t.Fatal(err)
					}
				}
			}
			if tt.unverified {
				record, err := backend.readOwnershipRecord(cleanupOwnershipRecordRelativePath(info.Path))
				if err != nil {
					t.Fatal(err)
				}
				record.CleanupStarted = false
				if err := backend.writeOwnershipRecord(record); err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(func() { restoreWritableTree(t, info.Path) })
			backend.scanWorkspacePaths = func(context.Context, string) ([]int, error) {
				if tt.activeProcess {
					return []int{os.Getpid() + 1000}, nil
				}
				return nil, nil
			}
			var active []Issue
			if tt.active {
				activeIssue := issue
				if tt.alias {
					activeIssue.ProjectID = "new-alias"
				}
				active = []Issue{activeIssue}
			}

			result, err := backend.ReconcileResiduals(t.Context(), active)
			if tt.unverified {
				if err != nil || result.PreservedSkipped != 1 {
					t.Fatalf("reconcile = %+v, %v; want preservation", result, err)
				}
			} else if err != nil {
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
