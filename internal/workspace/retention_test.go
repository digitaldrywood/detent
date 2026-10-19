package workspace

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
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
	if testing.Short() {
		t.Skip("git subprocess integration")
	}
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
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

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
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

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
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

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
		t.Skipf("symlinks unavailable: %v", err)
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

func TestRetentionQuarantineUnremovablePath(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("chmod on Windows does not deny directory removal; this fixture requires POSIX permissions")
	}
	backend := retentionBackend(t)
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	parent := filepath.Join(backend.root, ".detent/quarantine")
	quarantine := filepath.Join(parent, "workspace-"+now.Add(-4*24*time.Hour).Format(quarantineTimestampFormat))
	retentionFixture(t, filepath.Join(quarantine, "file"), now)
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(parent, 0o700); err != nil {
			t.Error(err)
		}
	})
	for range 2 {
		totals, err := backend.SweepRetention(t.Context(), RetentionRequest{Now: now})
		if err == nil || !strings.Contains(err.Error(), filepath.Join(".detent/quarantine", filepath.Base(quarantine))) {
			t.Fatalf("retention error = %v, want quarantine path", err)
		}
		if totals.Quarantine.Count != 0 {
			t.Fatalf("totals=%+v", totals)
		}
	}
}

func TestRetentionAttempts(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

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
			name              string
			external, removed bool
		}{{name: "legacy"}, {name: "external", external: true}, {name: "external removed workspace", external: true, removed: true}} {
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
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

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
		{name: "expired landing preserves Code", age: 2 * time.Hour, landing: true, remove: true},
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
			if _, err := backend.CleanupIssue(t.Context(), issue); !errors.Is(err, ErrWorkspacePreserved) {
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
			if codeUnchanged != nil {
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
			if total.Ownership.Count != 1 || total.Ownership.Bytes == 0 {
				t.Fatalf("ownership removals = %+v", total.Ownership)
			}
			if _, err := os.Stat(filepath.Join(backend.root, ".detent/retained")); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("expired workspace left a local copy: %v", err)
			}
		})
	}
}

func TestRetentionOwnership(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

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
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

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
				t.Skipf("symlinks unavailable: %v", err)
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
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

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
