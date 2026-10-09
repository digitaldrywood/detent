package workspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
)

func TestLandingAdvancedBaseRetry(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}
	if runtime.GOOS == "windows" {
		t.Skip("git hook requires a POSIX shell")
	}
	t.Parallel()
	for _, mode := range []string{gate.LandingPerChange, gate.LandingRollingBarrier} {
		t.Run(mode, func(t *testing.T) {
			f := newLandingFixture(t)
			if err := os.WriteFile(filepath.Join(f.source, "advance.txt"), []byte("parallel landing\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			runGit(t, f.source, "add", "advance.txt")
			runGit(t, f.source, "commit", "-m", "parallel landing")
			next := strings.TrimSpace(runGit(t, f.source, "rev-parse", "HEAD"))
			runGit(t, f.source, "push", "origin", next+":refs/heads/advance")
			marker := filepath.Join(t.TempDir(), "advanced")
			hook := "#!/bin/sh\nif test ! -f " + shellQuote(marker) + "; then\n unset GIT_QUARANTINE_PATH GIT_OBJECT_DIRECTORY GIT_ALTERNATE_OBJECT_DIRECTORIES\n git update-ref refs/heads/main " + shellQuote(next) + " || exit 1\n touch " + shellQuote(marker) + "\nfi\n"
			if err := os.WriteFile(filepath.Join(f.remote, "hooks", "pre-receive"), []byte(hook), 0o700); err != nil {
				t.Fatal(err)
			}
			result, err := f.backend.LandChange(t.Context(), f.info, f.issue, LandOptions{HeadSHA: f.head, Method: "squash", Message: "land feature", LandingMode: mode, ValidationCommand: "true"})
			if mode == gate.LandingPerChange {
				var refusal *LandRefusal
				if !errors.As(err, &refusal) || refusal.Kind != LandRefusalBaseMoved {
					t.Fatalf("error=%v", err)
				}
				return
			}
			if err != nil || result.MergeSHA != f.remoteMain(t) || result.BaseBefore != next || !result.Rebased || result.Gate.Command != "" {
				t.Fatalf("result=%+v error=%v", result, err)
			}
			if got := strings.TrimSpace(runGit(t, f.remote, "show", "main:advance.txt")); got != "parallel landing" {
				t.Fatalf("lost parallel landing: %q", got)
			}
			if got := strings.TrimSpace(runGit(t, f.remote, "show", "main:feature.txt")); got != "feature\nmore" {
				t.Fatalf("lost reviewed feature: %q", got)
			}
		})
	}
}

func TestLandingBarrierRunsOutsideSourceLock(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}
	if runtime.GOOS == "windows" {
		t.Skip("fixture requires POSIX named pipes")
	}
	t.Parallel()
	for _, code := range []int{0, 7} {
		t.Run(string(rune('0'+code)), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			f := newLandingFixture(t)
			before := f.remoteMain(t)
			for _, base := range []string{"", "main"} {
				if head, err := f.backend.LandingBarrierHead(ctx, base); err != nil || head != before {
					t.Fatalf("observed head for %q = %q error=%v, want %s", base, head, err, before)
				}
			}
			started, resume := filepath.Join(t.TempDir(), "started"), filepath.Join(t.TempDir(), "resume")
			if output, err := exec.CommandContext(ctx, "mkfifo", started, resume).CombinedOutput(); err != nil {
				t.Fatalf("mkfifo: %s %v", output, err)
			}
			signal, err := os.OpenFile(started, os.O_RDWR, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			defer signal.Close()
			unblock, err := os.OpenFile(resume, os.O_RDWR, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			defer unblock.Close()
			resultCh := make(chan struct {
				result gate.CommandResult
				err    error
			}, 1)
			command := "printf x > " + shellQuote(started) + "; read line < " + shellQuote(resume) + "; exit " + string(rune('0'+code))
			go func() {
				result, err := f.backend.RunLandingBarrier(ctx, "barrier", "main", command)
				resultCh <- struct {
					result gate.CommandResult
					err    error
				}{result, err}
			}()
			ready := make(chan error, 1)
			go func() { var b [1]byte; _, err := signal.Read(b[:]); ready <- err }()
			select {
			case err := <-ready:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("barrier did not start")
			}
			landed, err := f.backend.LandChange(ctx, f.info, f.issue, LandOptions{HeadSHA: f.head, Method: "squash", LandingMode: gate.LandingRollingBarrier, ValidationCommand: "exit 19"})
			if err != nil || landed.MergeSHA == "" || landed.Gate.Command != "" {
				t.Fatalf("landing blocked by barrier: %+v %v", landed, err)
			}
			if _, err := unblock.WriteString("continue\n"); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-resultCh:
				if got.err != nil || got.result.HeadSHA != before || got.result.ExitCode != code || got.result.Command != command {
					t.Fatalf("barrier=%+v error=%v", got.result, got.err)
				}
			case <-ctx.Done():
				t.Fatal("barrier did not finish")
			}
		})
	}
}

func TestLandingBarrierRepairPublication(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}
	t.Parallel()
	for _, test := range []struct {
		name      string
		edit      bool
		commit    bool
		moveBase  bool
		published bool
		wantErr   bool
	}{
		{name: "no change publishes nothing"},
		{name: "committed repair fast-forwards the base", edit: true, commit: true, published: true},
		{name: "uncommitted repair is refused", edit: true, wantErr: true},
		{name: "moved base refuses the stale repair", edit: true, commit: true, moveBase: true, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			f := newLandingFixture(t)
			before := f.remoteMain(t)
			path, head, release, err := f.backend.PrepareLandingBarrierRepair(ctx, "barrier", "main")
			if err != nil || head != before {
				t.Fatalf("prepare head=%s want=%s error=%v", head, before, err)
			}
			defer func() {
				if err := release(); err != nil {
					t.Error(err)
				}
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Errorf("repair worktree left at %s: %v", path, err)
				}
			}()
			if test.edit {
				if err := os.WriteFile(filepath.Join(path, "README.md"), []byte("repaired\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if test.commit {
				runGit(t, path, "-c", "commit.gpgsign=false", "commit", "-am", "fix: repair barrier")
			}
			if test.moveBase {
				if err := os.WriteFile(filepath.Join(f.source, "moved.txt"), []byte("moved\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				runGit(t, f.source, "add", "moved.txt")
				runGit(t, f.source, "commit", "-m", "moved base")
				runGit(t, f.source, "push", "origin", "HEAD:main")
			}
			moved := f.remoteMain(t)
			published, err := f.backend.PublishLandingBarrierRepair(ctx, path, "main", head)
			if (err != nil) != test.wantErr || (published != "") != test.published {
				t.Fatalf("published=%q error=%v", published, err)
			}
			want := moved
			if test.published {
				want = published
			}
			if got := f.remoteMain(t); got != want {
				t.Fatalf("remote main=%s want=%s", got, want)
			}
		})
	}
}
