package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func retentionBackend(t *testing.T) *LocalGit {
	t.Helper()
	backend, err := NewLocalGit(LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: initSourceRepo(t), AutoBranch: true})
	if err != nil {
		t.Fatal(err)
	}
	backend.scanWorkspacePaths = func(context.Context, string) ([]int, error) { return nil, nil }
	return backend
}

func retentionFixture(t *testing.T, path string, at time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("retained content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

func TestLandingRetention(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		age    time.Duration
		remove bool
	}{
		{name: "stale", age: 2 * time.Hour, remove: true},
		{name: "boundary", age: time.Hour, remove: true},
		{name: "recent", age: time.Hour - time.Second},
		{name: "active", age: 2 * time.Hour},
		{name: "busy", age: 2 * time.Hour},
		{name: "locked", age: 2 * time.Hour},
		{name: "missing", age: 2 * time.Hour, remove: true},
		{name: "unregistered", age: 2 * time.Hour},
		{name: "staging", age: 2 * time.Hour, remove: true},
		{name: "legacy receipt", age: 2 * time.Hour, remove: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			backend := retentionBackend(t)
			head := strings.TrimSpace(runGit(t, backend.sourceRoot, "rev-parse", "HEAD"))
			issue := Issue{Identifier: "DD-LAND", Landing: &LandOptions{HeadSHA: head}}
			info, err := backend.infoForIssue(issue)
			if err != nil {
				t.Fatal(err)
			}
			path := info.Path
			if test.name == "staging" {
				path = filepath.Join(backend.root, "landing-"+info.Key)
			}
			if test.name == "unregistered" {
				retentionFixture(t, filepath.Join(path, "keep"), time.Now())
			} else {
				runGit(t, backend.sourceRoot, "worktree", "add", "--detach", path, head)
			}
			now := time.Now()
			at := now.Add(-test.age)
			stamp := path
			request := RetentionRequest{Now: now}
			switch test.name {
			case "active":
				request.Active = []Issue{issue}
			case "busy":
				backend.scanWorkspacePaths = func(context.Context, string) ([]int, error) { return []int{123}, nil }
			case "locked":
				runGit(t, backend.sourceRoot, "worktree", "lock", path)
			case "missing":
				dir := strings.TrimSpace(runGit(t, path, "rev-parse", "--absolute-git-dir"))
				stamp = filepath.Join(dir, "gitdir")
				if err := os.RemoveAll(path); err != nil {
					t.Fatal(err)
				}
			case "legacy receipt":
				dir := strings.TrimSpace(runGit(t, path, "rev-parse", "--absolute-git-dir"))
				if err := os.WriteFile(filepath.Join(dir, landingRecordFile), []byte(`{"head_sha":"`+head+`","result":{"MergeSHA":"`+head+`"}}`), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Chtimes(stamp, at, at); err != nil {
				t.Fatal(err)
			}
			totals, err := backend.SweepRetention(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if test.remove {
				want = 1
			}
			if totals.Landing.Count != want {
				t.Fatalf("landing removed = %+v, want %d", totals.Landing, want)
			}
			registered := strings.Contains(runGit(t, backend.sourceRoot, "worktree", "list", "--porcelain"), filepath.ToSlash(path))
			if test.name != "unregistered" && registered == test.remove {
				t.Fatalf("landing registration remains = %v, remove = %v", registered, test.remove)
			}
			_, statErr := os.Stat(path)
			if test.remove && !errors.Is(statErr, os.ErrNotExist) || !test.remove && statErr != nil {
				t.Fatalf("landing directory: %v", statErr)
			}
			if test.name == "legacy receipt" {
				runGit(t, backend.sourceRoot, "worktree", "add", "--detach", path, head)
				if kept, ok := keptLanding(t.Context(), path, head, "HEAD"); !ok || kept.MergeSHA != head {
					t.Fatalf("legacy receipt lost: %+v, %v", kept, ok)
				}
			}
		})
	}
}

func TestRetentionHookLogs(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name   string
		age    time.Duration
		remove bool
	}{
		{"recent", 13 * 24 * time.Hour, false}, {"boundary", 14 * 24 * time.Hour, true}, {"expired", 15 * 24 * time.Hour, true}, {"future", -time.Hour, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, err := os.OpenRoot(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := root.Close(); err != nil {
					t.Error(err)
				}
			}()
			path := ".detent/hook-logs/key/run.log"
			retentionFixture(t, filepath.Join(root.Name(), path), now.Add(-test.age))
			var total RemovalTotal
			if err := sweepHookLogs(root, now, &total); err != nil {
				t.Fatal(err)
			}
			_, err = root.Lstat(path)
			if errors.Is(err, fs.ErrNotExist) != test.remove {
				t.Fatalf("removed=%v error=%v", test.remove, err)
			}
			if test.remove && (total.Count != 1 || total.Bytes != 16) {
				t.Fatalf("total=%+v", total)
			}
		})
	}
}

func TestRetentionQuarantine(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name  string
		count int
		age   time.Duration
		want  int
	}{
		{"young", 3, time.Hour, 0}, {"count", 7, time.Hour, 2}, {"age", 3, 4 * 24 * time.Hour, 3}, {"boundary", 1, 3 * 24 * time.Hour, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := retentionBackend(t)
			for i := range test.count {
				name := "workspace-" + now.Add(-test.age-time.Duration(i)*time.Second).Format(quarantineTimestampFormat)
				retentionFixture(t, filepath.Join(backend.root, ".detent/quarantine", name, "file"), now)
			}
			totals, err := backend.SweepRetention(t.Context(), RetentionRequest{Now: now})
			if err != nil {
				t.Fatal(err)
			}
			if totals.Quarantine.Count != test.want || totals.Quarantine.Bytes != int64(test.want*16) {
				t.Fatalf("totals=%+v", totals)
			}
		})
	}
}

func TestRetentionQuarantineReadOnlyDirectory(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name     string
		readOnly string
	}{
		{name: "writable"},
		{name: "read-only nested directory", readOnly: "cache"},
		{name: "read-only quarantine directory", readOnly: "."},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := retentionBackend(t)
			quarantine := filepath.Join(backend.root, ".detent/quarantine", "workspace-"+now.Add(-4*24*time.Hour).Format(quarantineTimestampFormat))
			retentionFixture(t, filepath.Join(quarantine, "cache", "file"), now)
			if test.readOnly != "" {
				readOnly := filepath.Join(quarantine, test.readOnly)
				if err := os.Chmod(readOnly, 0o500); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := os.Chmod(readOnly, 0o700); err != nil && !errors.Is(err, fs.ErrNotExist) {
						t.Error(err)
					}
				})
			}
			totals, err := backend.SweepRetention(t.Context(), RetentionRequest{Now: now})
			if err != nil {
				t.Fatal(err)
			}
			if totals.Quarantine.Count != 1 || totals.Quarantine.Bytes != 16 {
				t.Fatalf("totals=%+v", totals)
			}
			if _, err := os.Stat(quarantine); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("quarantine remains: %v", err)
			}
		})
	}
}

func TestRetentionQuarantineDoesNotChmodSymlinkTarget(t *testing.T) {
	t.Parallel()
	backend := retentionBackend(t)
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	quarantine := filepath.Join(backend.root, ".detent/quarantine", "workspace-"+now.Add(-4*24*time.Hour).Format(quarantineTimestampFormat))
	retentionFixture(t, filepath.Join(quarantine, "file"), now)
	target := t.TempDir()
	retentionFixture(t, filepath.Join(target, "keep"), now)
	if err := os.Chmod(target, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(target, 0o700); err != nil {
			t.Error(err)
		}
	})
	before, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(quarantine, "link")); err != nil {
		t.Fatalf("create symlink fixture: %v", err)
	}
	if _, err := backend.SweepRetention(t.Context(), RetentionRequest{Now: now}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(target)
	if err != nil || info.Mode().Perm() != before.Mode().Perm() {
		t.Fatalf("symlink target mode changed: info=%v err=%v", info, err)
	}
	if _, err := os.Stat(filepath.Join(target, "keep")); err != nil {
		t.Fatalf("symlink target content removed: %v", err)
	}
}

func TestRetentionAttempts(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name                             string
		registered, terminal, live, fail bool
		age                              time.Duration
		remove                           bool
	}{
		{name: "orphan old", age: 2 * time.Hour, remove: true}, {name: "orphan boundary", age: time.Hour, remove: true},
		{name: "orphan recent", age: time.Minute}, {name: "registered active", registered: true, age: 24 * time.Hour},
		{name: "terminal", registered: true, terminal: true, remove: true}, {name: "live", terminal: true, live: true},
		{name: "lookup failed", fail: true, age: 24 * time.Hour},
	} {
		for _, location := range []struct {
			name                      string
			external, removed, shared bool
		}{{name: "legacy"}, {name: "external", external: true}, {name: "external removed workspace", external: true, removed: true}, {name: "shared external", external: true, shared: true}} {
			t.Run(test.name+"/"+location.name, func(t *testing.T) {
				backend := retentionBackend(t)
				workspacePath := filepath.Join(backend.root, "workspace")
				path := filepath.Join(workspacePath, workerScratchRelativePath, "attempt-test")
				if location.external {
					t.Cleanup(func() { _ = os.RemoveAll(workerScratchGroup(backend.root)) })
					if !location.removed {
						if err := os.MkdirAll(workspacePath, 0o700); err != nil {
							t.Fatal(err)
						}
					}
					if err := ensurePrivateDirectories(workerScratchBase(), WorkerScratchRoot(workspacePath)); err != nil {
						t.Fatal(err)
					}
					path = filepath.Join(WorkerScratchRoot(workspacePath), "attempt-test")
					if location.shared {
						group := workerScratchGroupIn(sharedWorkerScratchBase(), backend.root)
						t.Cleanup(func() { _ = os.RemoveAll(group) })
						if err := ensurePrivateDirectories(sharedWorkerScratchBase(), filepath.Join(group, "workspace")); err != nil {
							t.Fatal(err)
						}
						path = filepath.Join(group, "workspace", "attempt-test")
					}
				}
				retentionFixture(t, filepath.Join(path, "file"), now)
				if err := os.Chtimes(path, now.Add(-test.age), now.Add(-test.age)); err != nil {
					t.Fatal(err)
				}
				if test.live {
					backend.scanWorkspacePaths = func(context.Context, string) ([]int, error) { return []int{999999}, nil }
				}
				var queried []string
				request := RetentionRequest{Now: now, ScratchState: func(_ context.Context, scratch string) (bool, bool, error) {
					queried = append(queried, scratch)
					if test.fail {
						return false, false, errors.New("unavailable")
					}
					return test.registered, test.terminal, nil
				}}
				total, err := backend.SweepRetention(t.Context(), request)
				if (err != nil) != test.fail {
					t.Fatalf("err=%v", err)
				}
				if len(queried) != 1 || queried[0] != path {
					t.Fatalf("scratch state queried for %q, want %q", queried, path)
				}
				_, err = os.Stat(path)
				if errors.Is(err, fs.ErrNotExist) != test.remove {
					t.Fatalf("remove=%v stat=%v total=%+v", test.remove, err, total)
				}
				_, err = os.Stat(filepath.Dir(path))
				if wantParentRemoved := location.removed && test.remove; errors.Is(err, fs.ErrNotExist) != wantParentRemoved {
					t.Fatalf("scratch parent removed=%v, want %v", err, wantParentRemoved)
				}
			})
		}
	}
}

func TestRetentionCompletedWorkspace(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name                      string
		age                       time.Duration
		active, live, lookupError bool
		landing                   bool
		alias                     bool
		remove                    bool
	}{
		{name: "expired unpushed", age: 2 * time.Hour, remove: true}, {name: "boundary", age: time.Hour, remove: true},
		{name: "recent", age: 30 * time.Minute}, {name: "active", age: 2 * time.Hour, active: true},
		{name: "live", age: 2 * time.Hour, live: true}, {name: "lookup failure", age: 2 * time.Hour, lookupError: true},
		{name: "expired completion removes Code and landing", age: 2 * time.Hour, landing: true, remove: true},
		{name: "active issue protects landing", age: 2 * time.Hour, landing: true, active: true},
		{name: "active native alias protects work", age: 2 * time.Hour, active: true, alias: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := retentionBackend(t)
			issue := Issue{ID: "2681", Identifier: "repo#2681"}
			if test.alias {
				issue = Issue{ID: "wi_retention", ProjectID: "old-alias", Identifier: "prj_example#1"}
				key := legacyIssueKey(issue)
				runGit(t, backend.sourceRoot, "worktree", "add", "-b", autoBranchPrefix+strings.ToLower(key), filepath.Join(backend.root, key), "HEAD")
			}
			info, err := backend.Create(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			codePath := info.Path
			var codeUnchanged func()
			if test.landing {
				remote := initBareRemote(t)
				runGit(t, backend.sourceRoot, "config", "url.file://"+remote+".insteadOf", "https://github.com/example/repo.git")
				runGit(t, backend.sourceRoot, "remote", "add", "origin", "https://github.com/example/repo.git")
				runGit(t, backend.sourceRoot, "push", "-u", "origin", "main")
				codeUnchanged = preserveLandingOwner(t, info.Path)
				head := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))
				issue.Landing = &LandOptions{Repository: RepositoryURL(t.Context(), backend.sourceRoot), HeadSHA: head}
				info, err = backend.Create(t.Context(), issue)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := backend.recordCleanupOwnership(t.Context(), info, issue, true); err != nil {
				t.Fatal(err)
			}
			retentionFixture(t, filepath.Join(info.Path, "unique.txt"), now)
			runGit(t, info.Path, "add", "unique.txt")
			runGit(t, info.Path, "commit", "-m", "unpushed work")
			if err := os.WriteFile(filepath.Join(info.Path, "unique.txt"), []byte("staged version\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			runGit(t, info.Path, "add", "unique.txt")
			if err := os.WriteFile(filepath.Join(info.Path, "unique.txt"), []byte("working version\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(info.Path, "README.md")); err != nil {
				t.Fatal(err)
			}

			retentionFixture(t, filepath.Join(info.Path, "dirty.txt"), now)
			if _, err := backend.CleanupIssue(t.Context(), issue); test.alias && err != nil || !test.alias && !errors.Is(err, ErrWorkspacePreserved) {
				t.Fatalf("baseline cleanup=%v", err)
			}
			request := RetentionRequest{Now: now, Completed: func(context.Context, []Issue) (map[string]time.Time, error) {
				if test.lookupError {
					return nil, errors.New("tracker unavailable")
				}
				return map[string]time.Time{issue.ID: now.Add(-test.age)}, nil
			}}
			if test.active {
				activeIssue := issue
				activeIssue.Landing = nil
				if test.alias {
					activeIssue.ProjectID = "new-alias"
				}
				request.Active = []Issue{activeIssue}
			}
			if test.live {
				backend.scanWorkspacePaths = func(context.Context, string) ([]int, error) { return []int{999999}, nil }
			}
			total, err := backend.SweepRetention(t.Context(), request)
			if codeUnchanged != nil && !test.remove {
				codeUnchanged()
			}
			if (err != nil) != test.lookupError {
				t.Fatalf("sweep error=%v", err)
			}
			_, err = os.Stat(info.Path)
			if errors.Is(err, fs.ErrNotExist) != test.remove {
				t.Fatalf("remove=%v stat=%v total=%+v", test.remove, err, total)
			}
			if !test.remove {
				return
			}
			wantRemoved := 1
			if test.landing {
				wantRemoved = 2
				if _, err := os.Stat(codePath); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("completed Code workspace remains: %v", err)
				}
			}
			if total.Workspaces.Count != wantRemoved || total.Workspaces.Bytes == 0 {
				t.Fatalf("workspace removals = %+v, want %d", total.Workspaces, wantRemoved)
			}
			if total.Ownership.Count != wantRemoved || total.Ownership.Bytes == 0 {
				t.Fatalf("ownership removals = %+v", total.Ownership)
			}
			if _, err := os.Stat(filepath.Join(backend.root, ".detent/retained")); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("expired workspace left a local copy: %v", err)
			}
		})
	}
}

func TestRetentionOwnership(t *testing.T) {
	t.Parallel()
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "exists", true: "missing"}[missing], func(t *testing.T) {
			backend := retentionBackend(t)
			issue := Issue{ID: "2681", Identifier: "repo#2681"}
			info, err := backend.Create(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			if err := backend.recordCleanupOwnership(t.Context(), info, issue, true); err != nil {
				t.Fatal(err)
			}
			if missing {
				if err := os.RemoveAll(info.Path); err != nil {
					t.Fatal(err)
				}
			}
			totals, err := backend.SweepRetention(t.Context(), RetentionRequest{Now: time.Now()})
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if missing {
				want = 1
			}
			if totals.Ownership.Count != want {
				t.Fatalf("totals=%+v", totals)
			}
		})
	}
}

func TestRetentionRejectsRedirectedArtifacts(t *testing.T) {
	t.Parallel()
	for _, artifact := range []string{".detent/hook-logs", ".detent/quarantine", "workspace/.detent/worker-tmp"} {
		t.Run(artifact, func(t *testing.T) {
			backend := retentionBackend(t)
			now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
			target := filepath.Join(backend.root, "user-files")
			retentionFixture(t, filepath.Join(target, "important.log"), now.Add(-30*24*time.Hour))
			link := filepath.Join(backend.root, artifact)
			if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, link); err != nil {
				t.Fatalf("create symlink fixture: %v", err)
			}
			_, err := backend.SweepRetention(t.Context(), RetentionRequest{Now: now, ScratchState: func(context.Context, string) (bool, bool, error) { return false, false, nil }})
			if err == nil {
				t.Fatal("expected symlink rejection")
			}
			if _, err := os.Stat(filepath.Join(target, "important.log")); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRetentionCompletedResidue(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name     string
		foreign  bool
		checkout bool
		age      time.Duration
		remove   bool
		wantErr  bool
	}{
		{name: "expired residue", age: 8 * 24 * time.Hour, remove: true},
		{name: "recent residue", age: 30 * time.Minute},
		{name: "foreign git workspace", foreign: true, age: 8 * 24 * time.Hour, wantErr: true},
		{name: "checkout files remain", checkout: true, age: 8 * 24 * time.Hour, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			backend := retentionBackend(t)
			issue := Issue{ID: "2459", Identifier: "repo#2459"}
			info, err := backend.Create(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			if err := backend.recordCleanupOwnership(t.Context(), info, issue, true); err != nil {
				t.Fatal(err)
			}
			runGit(t, backend.sourceRoot, "worktree", "remove", "--force", info.Path)
			retentionFixture(t, filepath.Join(info.Path, ".detent", "notes.md"), now)
			if test.foreign {
				runGit(t, info.Path, "init")
			}
			if test.checkout {
				retentionFixture(t, filepath.Join(info.Path, "untracked.txt"), now)
			}
			request := RetentionRequest{Now: now, Completed: func(context.Context, []Issue) (map[string]time.Time, error) {
				return map[string]time.Time{issue.ID: now.Add(-test.age)}, nil
			}}
			total, err := backend.SweepRetention(t.Context(), request)
			if (err != nil) != test.wantErr {
				t.Fatalf("sweep error=%v", err)
			}
			_, err = os.Stat(info.Path)
			if errors.Is(err, fs.ErrNotExist) != test.remove {
				t.Fatalf("remove=%v stat=%v total=%+v", test.remove, err, total)
			}
			if !test.remove {
				return
			}
			if total.Workspaces.Count != 1 || total.Workspaces.Bytes != 16 || total.Ownership.Count != 1 {
				t.Fatalf("totals=%+v", total)
			}
		})
	}
}

func TestIndependentScopeForRetention(t *testing.T) {
	backend := retentionBackend(t)
	now := time.Now().UTC()
	issue := Issue{ID: "wi_example", Identifier: "prj_example#1", AttemptWorkspace: AttemptWorkspace{Organization: "org_first", Project: "prj_example", Attempt: "attempt_11111111111111111111111111111111"}}
	for _, org := range []string{"org_first", "org_second"} {
		issue.AttemptWorkspace.Organization = org
		info, err := backend.Create(t.Context(), issue)
		if err != nil {
			t.Fatal(err)
		}
		if err := backend.recordCleanupOwnership(t.Context(), info, issue, true); err != nil {
			t.Fatal(err)
		}
	}
	second, err := backend.infoForIssue(issue)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(second.Path, "unpublished.txt"), []byte("second organization"), 0600); err != nil {
		t.Fatal(err)
	}
	request := RetentionRequest{Now: now, Completed: func(ctx context.Context, issues []Issue) (map[string]time.Time, error) {
		return map[string]time.Time{"wi_example": now.Add(-2 * time.Hour)}, nil
	}}
	if _, err := backend.SweepRetention(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(second.Path, "unpublished.txt")); err != nil {
		t.Errorf("issue-only completion removed the other organization's unpublished file: %v", err)
	}
}

func attachBareOrigin(t *testing.T, sourceRoot string) {
	t.Helper()
	bare := filepath.Join(t.TempDir(), "origin.git")
	branch := strings.TrimSpace(runGit(t, sourceRoot, "rev-parse", "--abbrev-ref", "HEAD"))
	runGit(t, filepath.Dir(bare), "init", "--bare", "-b", branch, bare)
	runGit(t, sourceRoot, "remote", "add", "origin", bare)
	runGit(t, sourceRoot, "push", "-u", "origin", branch)
	runGit(t, sourceRoot, "remote", "set-head", "origin", "-a")
}

func TestAttemptWorkspaceRetention(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 27, 15, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name           string
		age            time.Duration
		activeIssue    bool
		activeForeign  bool
		activePath     bool
		liveCheckout   bool
		liveExec       bool
		heldCheckout   bool
		heldScratch    bool
		dirtyCheckout  bool
		unpushedCommit bool
		completed      bool
		scanErrExec    bool
		otherOrg       bool
		unscoped       bool
		malformedExec  bool
		wantRemoved    bool
		wantExecRemove bool
		wantErr        bool
	}{
		{name: "expired attempt removed with execution dir", age: 2 * time.Hour, wantRemoved: true, wantExecRemove: true},
		{name: "boundary age removed", age: time.Hour, wantRemoved: true, wantExecRemove: true},
		{name: "recent attempt preserved", age: time.Hour - time.Second},
		{name: "dirty open attempt preserved", age: 2 * time.Hour, dirtyCheckout: true},
		{name: "unpushed open attempt preserved", age: 2 * time.Hour, unpushedCommit: true},
		{name: "dirty terminal attempt preserved", age: 2 * time.Hour, dirtyCheckout: true, completed: true},
		{name: "active attempt workspace preserved", age: 2 * time.Hour, activeIssue: true},
		{name: "active foreign issue preserved", age: 2 * time.Hour, activeForeign: true},
		{name: "active running path preserved", age: 2 * time.Hour, activePath: true},
		{name: "live process in checkout preserves both", age: 2 * time.Hour, liveCheckout: true},
		{name: "live process in execution dir preserves both", age: 2 * time.Hour, liveExec: true},
		{name: "held checkout protects execution dir", age: 2 * time.Hour, heldCheckout: true},
		{name: "acquired scratch protects checkout and execution dir", age: 2 * time.Hour, heldScratch: true},
		{name: "scan error in execution dir preserves both", age: 2 * time.Hour, scanErrExec: true, wantErr: true},
		{name: "other organization preserved", age: 2 * time.Hour, otherOrg: true},
		{name: "unscoped request preserved", age: 2 * time.Hour, unscoped: true},
		{name: "malformed execution dir preserved", age: 2 * time.Hour, malformedExec: true, wantRemoved: true, wantExecRemove: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			backend := retentionBackend(t)
			attachBareOrigin(t, backend.sourceRoot)
			org := "org_first"
			if test.otherOrg {
				org = "org_second"
			}
			attempt := AttemptWorkspace{Organization: org, Project: "prj_example", Attempt: "attempt_11111111111111111111111111111111"}
			issue := Issue{ID: "wi_example", Identifier: "prj_example#1", AttemptWorkspace: attempt}
			info, err := backend.Create(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			if err := backend.recordCleanupOwnership(t.Context(), info, issue, true); err != nil {
				t.Fatal(err)
			}
			execDir := filepath.Join(backend.root, attempt.Key()+"-execution")
			if test.malformedExec {
				execDir = filepath.Join(backend.root, strings.TrimSuffix(attempt.Key(), "11111111111111111111111111111111")+"not-hex-execution")
			}
			retentionFixture(t, filepath.Join(execDir, "worker-1", "scratch.txt"), now.Add(-test.age))
			if test.dirtyCheckout {
				if err := os.WriteFile(filepath.Join(info.Path, "unpublished.txt"), []byte("wip"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if test.unpushedCommit {
				if err := os.WriteFile(filepath.Join(info.Path, "local.txt"), []byte("local commit"), 0o600); err != nil {
					t.Fatal(err)
				}
				runGit(t, info.Path, "add", "local.txt")
				runGit(t, info.Path, "commit", "-m", "local unpushed commit")
			}
			if err := os.Chtimes(info.Path, now.Add(-test.age), now.Add(-test.age)); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(execDir, now.Add(-test.age), now.Add(-test.age)); err != nil {
				t.Fatal(err)
			}
			backend.scanWorkspacePaths = func(_ context.Context, path string) ([]int, error) {
				if test.liveCheckout && path == info.Path {
					return []int{4242}, nil
				}
				if test.liveExec && path == execDir {
					return []int{4243}, nil
				}
				if test.scanErrExec && path == execDir {
					return nil, errors.New("scan failed")
				}
				return nil, nil
			}
			request := RetentionRequest{Now: now}
			if test.completed {
				request.Completed = func(context.Context, []Issue) (map[string]time.Time, error) {
					return map[string]time.Time{issue.ID: now.Add(-test.age)}, nil
				}
			}
			if !test.unscoped {
				request.Organization = "org_first"
				request.Project = "prj_example"
			}
			if test.activeIssue {
				request.Active = []Issue{issue}
			}
			if test.activeForeign {
				request.Active = []Issue{{ProjectID: "prj_example", ID: "wi_example", Identifier: "prj_example#1", CleanupInUse: true}}
			}
			if test.activePath {
				request.ActivePaths = []string{info.Path}
			}
			if test.heldCheckout {
				release, err := backend.Use(t.Context(), issue)
				if err != nil {
					t.Fatal(err)
				}
				defer release()
			}
			if test.heldScratch {
				_, release, err := attempt.AcquireScratch(t.Context(), backend.root)
				if err != nil {
					t.Fatal(err)
				}
				defer release()
				if err := os.Chtimes(execDir, now.Add(-test.age), now.Add(-test.age)); err != nil {
					t.Fatal(err)
				}
			}
			totals, err := backend.SweepRetention(t.Context(), request)
			if (err != nil) != test.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, test.wantErr)
			}
			_, wsErr := os.Stat(info.Path)
			if errors.Is(wsErr, fs.ErrNotExist) != test.wantRemoved {
				t.Fatalf("workspace removed=%v err=%v totals=%+v", !errors.Is(wsErr, fs.ErrNotExist), wsErr, totals)
			}
			_, execErr := os.Stat(execDir)
			if errors.Is(execErr, fs.ErrNotExist) != test.wantExecRemove {
				t.Fatalf("execution dir removed=%v err=%v totals=%+v", !errors.Is(execErr, fs.ErrNotExist), execErr, totals)
			}
			if test.wantRemoved && test.wantExecRemove && (totals.Workspaces.Count != 1 || totals.Ownership.Count != 1 || totals.Attempts.Count != 1) {
				t.Fatalf("unexpected totals: %+v", totals)
			}
		})
	}
	t.Run("overlapping scratch leases exclude cleanup until last releases", func(t *testing.T) {
		t.Parallel()
		backend := retentionBackend(t)
		attempt := AttemptWorkspace{Organization: "org_first", Project: "prj_example", Attempt: "attempt_22222222222222222222222222222222"}
		execDir := filepath.Join(backend.root, attempt.Key()+"-execution")
		_, release1, err := attempt.AcquireScratch(t.Context(), backend.root)
		if err != nil {
			t.Fatal(err)
		}
		_, release2, err := attempt.AcquireScratch(t.Context(), backend.root)
		if err != nil {
			release1()
			t.Fatal(err)
		}
		if err := os.Chtimes(execDir, now.Add(-2*time.Hour), now.Add(-2*time.Hour)); err != nil {
			release2()
			release1()
			t.Fatal(err)
		}
		req := RetentionRequest{Now: now, Organization: "org_first", Project: "prj_example"}
		if _, err := backend.SweepRetention(t.Context(), req); err != nil {
			release2()
			release1()
			t.Fatal(err)
		}
		if _, err := os.Stat(execDir); err != nil {
			release2()
			release1()
			t.Fatalf("execDir removed while both leases held: %v", err)
		}
		release1()
		if _, err := backend.SweepRetention(t.Context(), req); err != nil {
			release2()
			t.Fatal(err)
		}
		if _, err := os.Stat(execDir); err != nil {
			release2()
			t.Fatalf("execDir removed while second lease still held: %v", err)
		}
		release2()
		totals, err := backend.SweepRetention(t.Context(), req)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(execDir); !errors.Is(err, fs.ErrNotExist) || totals.Attempts.Count != 1 {
			t.Fatalf("execDir not removed after last lease released: err=%v totals=%+v", err, totals)
		}
	})
	t.Run("busy candidate scans are bounded per sweep without starving later dirs", func(t *testing.T) {
		t.Parallel()
		backend := retentionBackend(t)
		var scans int
		idleAttempt := AttemptWorkspace{Organization: "org_first", Project: "prj_example", Attempt: fmt.Sprintf("attempt_%032x", maxAttemptRetentionScans+1)}
		idleExecDir := filepath.Join(backend.root, idleAttempt.Key()+"-execution")
		backend.scanWorkspacePaths = func(_ context.Context, path string) ([]int, error) {
			scans++
			if path == idleExecDir {
				return nil, nil
			}
			return []int{4242}, nil
		}
		for i := range maxAttemptRetentionScans + 1 {
			attempt := AttemptWorkspace{Organization: "org_first", Project: "prj_example", Attempt: fmt.Sprintf("attempt_%032x", i+1)}
			execDir := filepath.Join(backend.root, attempt.Key()+"-execution")
			retentionFixture(t, filepath.Join(execDir, "worker-1", "scratch.txt"), now.Add(-2*time.Hour))
			if err := os.Chtimes(execDir, now.Add(-2*time.Hour), now.Add(-2*time.Hour)); err != nil {
				t.Fatal(err)
			}
		}
		req := RetentionRequest{Now: now, Organization: "org_first", Project: "prj_example"}
		if _, err := backend.SweepRetention(t.Context(), req); err != nil {
			t.Fatal(err)
		}
		if scans != maxAttemptRetentionScans {
			t.Fatalf("first sweep scans = %d, want %d", scans, maxAttemptRetentionScans)
		}
		if _, err := os.Stat(idleExecDir); err != nil {
			t.Fatalf("idle dir unexpectedly removed on first sweep: %v", err)
		}
		scans = 0
		totals, err := backend.SweepRetention(t.Context(), req)
		if err != nil {
			t.Fatal(err)
		}
		if scans > maxAttemptRetentionScans {
			t.Fatalf("second sweep scans = %d, exceeds %d", scans, maxAttemptRetentionScans)
		}
		if _, err := os.Stat(idleExecDir); !errors.Is(err, fs.ErrNotExist) || totals.Attempts.Count != 1 {
			t.Fatalf("idle dir starved on second sweep: err=%v totals=%+v", err, totals)
		}
	})
	t.Run("busy checkouts do not starve execution-only dirs within scan budget", func(t *testing.T) {
		t.Parallel()
		backend := retentionBackend(t)
		var scans int
		idleAttempt := AttemptWorkspace{Organization: "org_first", Project: "prj_example", Attempt: "attempt_ffffffffffffffffffffffffffffffff"}
		idleExecDir := filepath.Join(backend.root, idleAttempt.Key()+"-execution")
		backend.scanWorkspacePaths = func(_ context.Context, path string) ([]int, error) {
			scans++
			if path == idleExecDir {
				return nil, nil
			}
			return []int{4242}, nil
		}
		for i := range maxAttemptRetentionScans {
			attempt := AttemptWorkspace{Organization: "org_first", Project: "prj_example", Attempt: fmt.Sprintf("attempt_%032x", i+1)}
			issue := Issue{ID: fmt.Sprintf("wi_%d", i), Identifier: fmt.Sprintf("prj_example#%d", i+1), AttemptWorkspace: attempt}
			info, err := backend.Create(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			if err := backend.recordCleanupOwnership(t.Context(), info, issue, true); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(info.Path, now.Add(-2*time.Hour), now.Add(-2*time.Hour)); err != nil {
				t.Fatal(err)
			}
		}
		retentionFixture(t, filepath.Join(idleExecDir, "worker-1", "scratch.txt"), now.Add(-2*time.Hour))
		if err := os.Chtimes(idleExecDir, now.Add(-2*time.Hour), now.Add(-2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		req := RetentionRequest{Now: now, Organization: "org_first", Project: "prj_example"}
		totals, err := backend.SweepRetention(t.Context(), req)
		if err != nil {
			t.Fatal(err)
		}
		if scans > maxAttemptRetentionScans {
			t.Fatalf("scans = %d, exceeds %d", scans, maxAttemptRetentionScans)
		}
		if _, err := os.Stat(idleExecDir); !errors.Is(err, fs.ErrNotExist) || totals.Attempts.Count != 1 {
			t.Fatalf("execution-only dir starved by busy checkouts: err=%v totals=%+v", err, totals)
		}
	})
	t.Run("busy execution-only dirs do not starve later idle checkout", func(t *testing.T) {
		t.Parallel()
		backend := retentionBackend(t)
		attachBareOrigin(t, backend.sourceRoot)
		var scans int
		idleAttempt := AttemptWorkspace{Organization: "org_first", Project: "prj_example", Attempt: fmt.Sprintf("attempt_%032x", maxAttemptRetentionScans/2)}
		idleIssue := Issue{ID: "wi_idle", Identifier: "prj_example#999", AttemptWorkspace: idleAttempt}
		idleInfo, err := backend.Create(t.Context(), idleIssue)
		if err != nil {
			t.Fatal(err)
		}
		if err := backend.recordCleanupOwnership(t.Context(), idleInfo, idleIssue, true); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(idleInfo.Path, now.Add(-2*time.Hour), now.Add(-2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		idlePairedExec := filepath.Join(backend.root, idleAttempt.Key()+"-execution")
		retentionFixture(t, filepath.Join(idlePairedExec, "worker-1", "scratch.txt"), now.Add(-2*time.Hour))
		if err := os.Chtimes(idlePairedExec, now.Add(-2*time.Hour), now.Add(-2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		for i := range maxAttemptRetentionScans/2 - 1 {
			attempt := AttemptWorkspace{Organization: "org_first", Project: "prj_example", Attempt: fmt.Sprintf("attempt_%032x", i+1)}
			issue := Issue{ID: fmt.Sprintf("wi_%d", i), Identifier: fmt.Sprintf("prj_example#%d", i+1), AttemptWorkspace: attempt}
			info, err := backend.Create(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			if err := backend.recordCleanupOwnership(t.Context(), info, issue, true); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(info.Path, now.Add(-2*time.Hour), now.Add(-2*time.Hour)); err != nil {
				t.Fatal(err)
			}
		}
		for i := range maxAttemptRetentionScans/2 + 1 {
			attempt := AttemptWorkspace{Organization: "org_first", Project: "prj_example", Attempt: fmt.Sprintf("attempt_%032x", 1000+i)}
			execDir := filepath.Join(backend.root, attempt.Key()+"-execution")
			retentionFixture(t, filepath.Join(execDir, "worker-1", "scratch.txt"), now.Add(-2*time.Hour))
			if err := os.Chtimes(execDir, now.Add(-2*time.Hour), now.Add(-2*time.Hour)); err != nil {
				t.Fatal(err)
			}
		}
		backend.scanWorkspacePaths = func(_ context.Context, path string) ([]int, error) {
			scans++
			if path == idleInfo.Path || path == idlePairedExec {
				return nil, nil
			}
			return []int{4242}, nil
		}
		req := RetentionRequest{Now: now, Organization: "org_first", Project: "prj_example"}
		if _, err := backend.SweepRetention(t.Context(), req); err != nil {
			t.Fatal(err)
		}
		if scans > maxAttemptRetentionScans {
			t.Fatalf("first sweep scans = %d, exceeds %d", scans, maxAttemptRetentionScans)
		}
		scans = 0
		totals, err := backend.SweepRetention(t.Context(), req)
		if err != nil {
			t.Fatal(err)
		}
		if scans > maxAttemptRetentionScans {
			t.Fatalf("second sweep scans = %d, exceeds %d", scans, maxAttemptRetentionScans)
		}
		if _, err := os.Stat(idleInfo.Path); !errors.Is(err, fs.ErrNotExist) || totals.Workspaces.Count != 1 {
			t.Fatalf("idle checkout starved by busy execution dirs: err=%v totals=%+v", err, totals)
		}
	})
	t.Run("deferred dirty checkout preserves sibling execution dir and bounds completion lookup", func(t *testing.T) {
		t.Parallel()
		backend := retentionBackend(t)
		var completedBatchSize int
		for i := range maxAttemptRetentionScans/2 + 2 {
			attempt := AttemptWorkspace{Organization: "org_first", Project: "prj_example", Attempt: fmt.Sprintf("attempt_%032x", i+1)}
			issue := Issue{ID: fmt.Sprintf("wi_%d", i), Identifier: fmt.Sprintf("prj_example#%d", i+1), AttemptWorkspace: attempt}
			info, err := backend.Create(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			if err := backend.recordCleanupOwnership(t.Context(), info, issue, true); err != nil {
				t.Fatal(err)
			}
			if i >= maxAttemptRetentionScans/2 {
				if err := os.WriteFile(filepath.Join(info.Path, "unpublished.txt"), []byte("wip"), 0o600); err != nil {
					t.Fatal(err)
				}
				execDir := filepath.Join(backend.root, attempt.Key()+"-execution")
				retentionFixture(t, filepath.Join(execDir, "worker-1", "scratch.txt"), now.Add(-2*time.Hour))
				if err := os.Chtimes(execDir, now.Add(-2*time.Hour), now.Add(-2*time.Hour)); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Chtimes(info.Path, now.Add(-2*time.Hour), now.Add(-2*time.Hour)); err != nil {
				t.Fatal(err)
			}
		}
		deferredAttempt := AttemptWorkspace{Organization: "org_first", Project: "prj_example", Attempt: fmt.Sprintf("attempt_%032x", maxAttemptRetentionScans/2+1)}
		deferredExecDir := filepath.Join(backend.root, deferredAttempt.Key()+"-execution")
		backend.scanWorkspacePaths = func(_ context.Context, path string) ([]int, error) {
			if strings.Contains(path, deferredAttempt.Key()) {
				return nil, nil
			}
			return []int{4242}, nil
		}
		req := RetentionRequest{
			Now:          now,
			Organization: "org_first",
			Project:      "prj_example",
			Completed: func(_ context.Context, issues []Issue) (map[string]time.Time, error) {
				completedBatchSize = len(issues)
				return map[string]time.Time{}, nil
			},
		}
		if _, err := backend.SweepRetention(t.Context(), req); err != nil {
			t.Fatal(err)
		}
		if completedBatchSize != 0 {
			t.Fatalf("attempt retention unexpectedly called Completed with %d issues", completedBatchSize)
		}
		if _, err := os.Stat(deferredExecDir); err != nil {
			t.Fatalf("deferred dirty checkout lost sibling execution dir: %v", err)
		}
	})
}
func TestIndependentFilesystemKeyOmitted(t *testing.T) {
	backend, err := NewFilesystem(FilesystemOptions{Root: t.TempDir(), OutputRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	issue := Issue{ID: "wi_example", Identifier: "prj_example#1"}
	legacy, err := backend.Create(t.Context(), issue)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backend.outputRoot, legacy.Key, "legacy-only"), []byte("legacy"), 0600); err != nil {
		t.Fatal(err)
	}
	issue.AttemptWorkspace = AttemptWorkspace{Organization: "org_example", Project: "prj_example", Attempt: "attempt_11111111111111111111111111111111"}
	current, err := backend.Create(t.Context(), issue)
	if err != nil {
		t.Fatal(err)
	}
	for _, info := range []Info{{}, {Path: current.Path}, current} {
		evidence, err := backend.ArtifactEvidence(t.Context(), info, issue)
		if err != nil {
			t.Fatal(err)
		}
		if evidence.Files != 0 {
			t.Errorf("current attempt reports %d legacy files with info key %q", evidence.Files, info.Key)
		}
	}
	issue.AttemptWorkspace = AttemptWorkspace{}
	for _, info := range []Info{{Key: current.Key}, {Path: current.Path}, current} {
		if _, err := backend.ArtifactEvidence(t.Context(), info, issue); !errors.Is(err, ErrMissingWorkspace) {
			t.Fatalf("unscoped request accepted a private checkout: %v", err)
		}
	}
	if _, err := backend.CleanupIssue(t.Context(), Issue{Identifier: current.Key}); !errors.Is(err, ErrMissingWorkspace) {
		t.Fatalf("raw private key authorized cleanup: %v", err)
	}
	if _, err := os.Stat(current.Path); err != nil {
		t.Fatal(err)
	}

}
