package workspace

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"io"
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
	if err := os.Symlink(target, filepath.Join(quarantine, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := backend.SweepRetention(t.Context(), RetentionRequest{Now: now}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(target)
	if err != nil || info.Mode().Perm() != 0o500 {
		t.Fatalf("symlink target mode changed: info=%v err=%v", info, err)
	}
	if _, err := os.Stat(filepath.Join(target, "keep")); err != nil {
		t.Fatalf("symlink target content removed: %v", err)
	}
}

func TestRetentionQuarantineUnremovablePath(t *testing.T) {
	t.Parallel()
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
		t.Run(test.name, func(t *testing.T) {
			backend := retentionBackend(t)
			path := filepath.Join(backend.root, "workspace", workerScratchRelativePath, "attempt-test")
			retentionFixture(t, filepath.Join(path, "file"), now)
			if err := os.Chtimes(path, now.Add(-test.age), now.Add(-test.age)); err != nil {
				t.Fatal(err)
			}
			if test.live {
				backend.scanWorkspacePaths = func(context.Context, string) ([]int, error) { return []int{999999}, nil }
			}
			request := RetentionRequest{Now: now, ScratchState: func(context.Context, string) (bool, bool, error) {
				if test.fail {
					return false, false, errors.New("unavailable")
				}
				return test.registered, test.terminal, nil
			}}
			total, err := backend.SweepRetention(t.Context(), request)
			if (err != nil) != test.fail {
				t.Fatalf("err=%v", err)
			}
			_, err = os.Stat(path)
			if errors.Is(err, fs.ErrNotExist) != test.remove {
				t.Fatalf("remove=%v stat=%v total=%+v", test.remove, err, total)
			}
		})
	}
}

func TestRetentionCompletedWorkspace(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name                      string
		age                       time.Duration
		active, live, lookupError bool
		remove                    bool
	}{
		{name: "expired unpushed", age: 8 * 24 * time.Hour, remove: true}, {name: "boundary", age: 7 * 24 * time.Hour, remove: true},
		{name: "recent", age: 6 * 24 * time.Hour}, {name: "active", age: 8 * 24 * time.Hour, active: true},
		{name: "live", age: 8 * 24 * time.Hour, live: true}, {name: "lookup failure", age: 8 * 24 * time.Hour, lookupError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := retentionBackend(t)
			issue := Issue{ID: "2681", Identifier: "repo#2681"}
			info, err := backend.Create(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			if err := backend.recordCleanupOwnership(t.Context(), info, issue, true); err != nil {
				t.Fatal(err)
			}
			retentionFixture(t, filepath.Join(info.Path, "unique.txt"), now)
			runGit(t, info.Path, "add", "unique.txt")
			runGit(t, info.Path, "commit", "-m", "unpushed work")
			head := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))
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
				request.Active = []Issue{issue}
			}
			if test.live {
				backend.scanWorkspacePaths = func(context.Context, string) ([]int, error) { return []int{999999}, nil }
			}
			total, err := backend.SweepRetention(t.Context(), request)
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
			archives, err := filepath.Glob(filepath.Join(backend.root, ".detent/retained", info.Key+"-*"))
			if err != nil || len(archives) != 1 {
				t.Fatalf("archives=%v err=%v", archives, err)
			}
			restored := filepath.Join(t.TempDir(), "restored")
			runGit(t, backend.sourceRoot, "clone", filepath.Join(archives[0], "commits.bundle"), restored)
			if got := strings.TrimSpace(runGit(t, restored, "rev-parse", "HEAD")); got != head {
				t.Fatalf("restored head=%s want=%s", got, head)
			}
			runGit(t, restored, "apply", "--cached", filepath.Join(archives[0], "staged.diff"))
			runGit(t, restored, "apply", filepath.Join(archives[0], "working-tree.diff"))
			if got := runGit(t, restored, "show", ":unique.txt"); got != "staged version\n" {
				t.Fatalf("index = %q", got)
			}
			if got, err := os.ReadFile(filepath.Join(restored, "unique.txt")); err != nil || string(got) != "working version\n" {
				t.Fatalf("working file = %q, %v", got, err)
			}
			if _, err := os.Stat(filepath.Join(restored, "README.md")); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("deleted tracked file restored: %v", err)
			}
			file, err := os.Open(filepath.Join(archives[0], "working-tree.tar.gz"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := file.Close(); err != nil {
					t.Error(err)
				}
			}()
			compressed, err := gzip.NewReader(file)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := compressed.Close(); err != nil {
					t.Error(err)
				}
			}()
			reader := tar.NewReader(compressed)
			found := false
			for {
				header, err := reader.Next()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if header.Name == "dirty.txt" {
					data, err := io.ReadAll(reader)
					if err != nil {
						t.Fatal(err)
					}
					found = string(data) == "retained content"
				}
			}
			if !found {
				t.Fatal("untracked data missing from archive")
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

func TestRetentionArchiveFailureKeepsWorkspace(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"archive destination", "unmanaged git"} {
		t.Run(failure, func(t *testing.T) {
			backend := retentionBackend(t)
			issue := Issue{ID: "2681", Identifier: "repo#2681"}
			info, err := backend.Create(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			if err := backend.recordCleanupOwnership(t.Context(), info, issue, true); err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
			retentionFixture(t, filepath.Join(info.Path, "untracked.txt"), now)
			switch failure {
			case "archive destination":
				retentionFixture(t, filepath.Join(backend.root, ".detent/retained"), now)
			case "unmanaged git":
				if err := os.Remove(filepath.Join(info.Path, ".git")); err != nil {
					t.Fatal(err)
				}
			}
			_, err = backend.SweepRetention(t.Context(), RetentionRequest{Now: now, Completed: func(context.Context, []Issue) (map[string]time.Time, error) {
				return map[string]time.Time{issue.ID: now.Add(-8 * 24 * time.Hour)}, nil
			}})
			if err == nil {
				t.Fatal("expected archive failure")
			}
			if data, err := os.ReadFile(filepath.Join(info.Path, "untracked.txt")); err != nil || string(data) != "retained content" {
				t.Fatalf("original data=%q err=%v", data, err)
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

func TestRetentionRemovalFailureDeduplicatesArchives(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"unchanged", "modified", "partially removed"} {
		t.Run(change, func(t *testing.T) {
			backend := retentionBackend(t)
			issue := Issue{ID: "2681", Identifier: "repo#2681"}
			info, err := backend.Create(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			if err := backend.recordCleanupOwnership(t.Context(), info, issue, true); err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
			retentionFixture(t, filepath.Join(info.Path, "untracked.txt"), now)
			runGit(t, backend.sourceRoot, "worktree", "lock", info.Path)
			request := RetentionRequest{Now: now, Completed: func(context.Context, []Issue) (map[string]time.Time, error) {
				return map[string]time.Time{issue.ID: now.Add(-8 * 24 * time.Hour)}, nil
			}}
			for sweep := range 3 {
				if change == "partially removed" && sweep == 1 {
					if err := os.Remove(filepath.Join(info.Path, "untracked.txt")); err != nil {
						t.Fatal(err)
					}
				}
				if change == "modified" && sweep == 1 {
					if err := os.WriteFile(filepath.Join(info.Path, "untracked.txt"), []byte("changed content"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				totals, err := backend.SweepRetention(t.Context(), request)
				if err == nil {
					t.Fatal("expected locked worktree removal to fail")
				}
				if totals.Workspaces.Count != 0 {
					t.Fatalf("totals=%+v", totals)
				}
			}
			archives, err := filepath.Glob(filepath.Join(backend.root, ".detent/retained", info.Key+"-*"))
			want := 1
			if change != "unchanged" {
				want = 2
			}
			if err != nil || len(archives) != want {
				t.Fatalf("archives=%v want=%d err=%v", archives, want, err)
			}
			for _, archive := range archives {
				runGit(t, backend.sourceRoot, "bundle", "verify", filepath.Join(archive, "commits.bundle"))
			}
			runGit(t, backend.sourceRoot, "worktree", "unlock", info.Path)
			totals, err := backend.SweepRetention(t.Context(), request)
			if err != nil || totals.Workspaces.Count != 1 {
				t.Fatalf("totals=%+v err=%v", totals, err)
			}
			archives, err = filepath.Glob(filepath.Join(backend.root, ".detent/retained", info.Key+"-*"))
			if err != nil || len(archives) != want {
				t.Fatalf("archives after retry=%v want=%d err=%v", archives, want, err)
			}
		})
	}
}
