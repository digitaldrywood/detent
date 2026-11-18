//go:build darwin || linux

package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/toolcache"
	"github.com/digitaldrywood/detent/internal/workspace/hookproc"
	"github.com/digitaldrywood/detent/internal/workspace/workspacetest"
)

func TestDetachedHookHelper(t *testing.T) { workspacetest.RunDetachedHook(t) }

func TestHookDaemonWorkspaceTeardown(t *testing.T) {
	for _, mode := range []string{"remove", "failed removal hook", "failed creation quarantine", "fresh checkout quarantine"} {
		t.Run(mode, func(t *testing.T) {
			daemon := workspacetest.DetachedHook(t)
			workspacetest.UseDaemonCandidates(t, daemon)
			hook := daemon.Command
			if mode == "failed creation quarantine" {
				hook += "; echo evidence > evidence; exit 19"
			}
			source := initSourceRepo(t)
			backend, err := NewLocalGit(LocalGitOptions{Root: t.TempDir(), SourceRoot: source, AutoBranch: true, Hooks: Hooks{AfterCreate: hook}})
			if err != nil {
				t.Fatal(err)
			}
			ctx := t.Context()
			issue := Issue{Identifier: "EXAMPLE-1"}
			info, err := backend.Create(ctx, issue)
			switch mode {
			case "failed creation quarantine":
				var failure *HookError
				if !errors.As(err, &failure) || failure.ExitCode != 19 {
					t.Fatalf("create error = %v", err)
				}
				entries, _ := filepath.Glob(filepath.Join(backend.root, ".detent", "quarantine", "*", "evidence"))
				if len(entries) != 1 || readFile(t, entries[0]) != "evidence\n" {
					t.Fatalf("quarantine evidence %v after %v", entries, err)
				}
			default:
				if err != nil || !daemon.Alive(t) {
					t.Fatalf("service failed to start: %v", err)
				}
				if mode == "remove" || mode == "failed removal hook" {
					if mode == "failed removal hook" {
						backend.hooks.BeforeRemove = "exit 21"
					}
					issue.Terminal = true
					issue.LandedHeadSHA = strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))
					_, err = backend.CleanupIssue(ctx, issue)
				} else {
					backend.hooks.AfterCreate = ""
					issue.FreshCheckout = true
					_, err = backend.Create(ctx, issue)
				}
				if mode == "failed removal hook" {
					var failure *HookError
					if !errors.As(err, &failure) || failure.ExitCode != 21 {
						t.Fatalf("removal failure = %v", err)
					}
					if _, err := os.Stat(info.Path); err != nil {
						t.Fatalf("retained checkout: %v", err)
					}
				} else if err != nil {
					t.Fatalf("teardown %s: %v", info.Path, err)
				}
			}
			if daemon.Alive(t) {
				t.Fatal("detached service still holds its lock after teardown")
			}
		})
	}
}

func TestHookOwnershipSurvivesMissingWorkspace(t *testing.T) {
	first := workspacetest.DetachedHook(t)
	second := workspacetest.DetachedHook(t)
	workspacetest.UseDaemonCandidates(t, first, second)
	fs, err := NewFilesystem(FilesystemOptions{Root: t.TempDir(), Hooks: Hooks{AfterCreate: "TEST_HOOK_IGNORE_TERM=1 " + first.Command, BeforeRun: second.Command}})
	if err != nil {
		t.Fatal(err)
	}
	issue := Issue{Identifier: "EXAMPLE-1"}
	firstCtx, stopFirst := WithHookProcesses(t.Context())
	secondCtx, stopSecond := WithHookProcesses(t.Context())
	info, err := fs.Create(firstCtx, issue)
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.BeforeRun(secondCtx, info, issue); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(info.Path, filepath.Join(t.TempDir(), "retained")); err != nil {
		t.Fatal(err)
	}
	if err := stopFirst(); err != nil {
		t.Fatal(err)
	}
	if first.Alive(t) || !second.Alive(t) {
		t.Fatal("first teardown did not stop just its own daemon")
	}
	if err := stopSecond(); err != nil {
		t.Fatal(err)
	}
	if second.Alive(t) {
		t.Fatal("second daemon survived its own teardown")
	}
}

func TestHookCleanupRetainsOwnerAfterInspectionFailure(t *testing.T) {
	daemon := workspacetest.DetachedHook(t)
	workspacetest.UseDaemonCandidates(t, daemon)
	ctx, cancel := context.WithCancel(t.Context())
	attempt, finish := WithHookProcesses(ctx)
	fs, err := NewFilesystem(FilesystemOptions{Root: t.TempDir(), Hooks: Hooks{AfterCreate: "echo setup > evidence; " + daemon.Command, BeforeRemove: "echo stopping > removed"}})
	if err != nil {
		t.Fatal(err)
	}
	issue := Issue{Identifier: "EXAMPLE-1"}
	scan := hookproc.ReapProcessScanner
	t.Cleanup(func() { hookproc.ReapProcessScanner = scan })
	hookproc.ReapProcessScanner = func(context.Context, string) ([]int, error) { return nil, nil }
	info, err := fs.Create(attempt, issue)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fs.CleanupIssue(t.Context(), issue); !errors.Is(err, ErrWorkspacePreserved) {
		t.Fatalf("cleanup while hooks are in use = %v", err)
	}
	if _, err := os.Stat(filepath.Join(info.Path, "removed")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("foreign attempt ran before_remove: %v", err)
	}
	cancel()
	unavailable := errors.New("process inventory unavailable")
	hookproc.ReapProcessScanner = func(ctx context.Context, path string) ([]int, error) {
		if _, ok := ctx.Deadline(); !ok || ctx.Err() != nil || path != info.Path {
			t.Fatalf("invalid cleanup context or path %q: %v", path, ctx.Err())
		}
		return nil, unavailable
	}
	if err := finish(); !errors.Is(err, unavailable) {
		t.Fatalf("attempt cleanup error = %v", err)
	}
	hookproc.ReapProcessScanner = func(context.Context, string) ([]int, error) { return nil, nil }
	if _, err := fs.CleanupIssue(t.Context(), issue); err == nil {
		t.Fatalf("removal discarded failed hook ownership: %v", err)
	}
	if got := readFile(t, filepath.Join(info.Path, "evidence")); got != "setup\n" {
		t.Fatalf("retained evidence = %q", got)
	}
	hookproc.ReapProcessScanner = scan
	if _, err := fs.CleanupIssue(t.Context(), issue); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(info.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace still present: %v", err)
	}
}

func TestHookDaemonHostDiscovery(t *testing.T) {
	for _, tc := range []struct{ name, hookPrefix string }{
		{"inherited descriptor", ""},
		{"closed descriptor", "exec 4>&-; "},
		{"cleared environment", "unset DETENT_HOOK_OWNER DETENT_WORKSPACE; "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attempt, finish := WithHookProcesses(t.Context())
			t.Cleanup(func() {
				if err := finish(); err != nil {
					t.Error(err)
				}
			})
			daemon := workspacetest.DetachedHook(t)
			fs, err := NewFilesystem(FilesystemOptions{Root: t.TempDir(), Hooks: Hooks{AfterCreate: tc.hookPrefix + daemon.Command}})
			if err != nil {
				t.Fatal(err)
			}
			prior := hookproc.ReapProcessScanner
			hookproc.ReapProcessScanner = hookproc.ProcessIDs
			t.Cleanup(func() { hookproc.ReapProcessScanner = prior })
			info, err := fs.Create(attempt, Issue{Identifier: "EXAMPLE-1"})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(info.Path, filepath.Join(t.TempDir(), "retired")); err != nil {
				t.Fatal(err)
			}
			daemon.Request(t)
			if err := finish(); err != nil {
				t.Fatal(err)
			}
			if daemon.Alive(t) {
				t.Fatal("native process inventory missed the detached hook")
			}
		})
	}
}

func TestManagedHookServiceLivesThroughWork(t *testing.T) {
	daemon := workspacetest.DetachedHook(t)
	workspacetest.UseDaemonCandidates(t, daemon)
	cacheRoot := t.TempDir()
	cache, err := toolcache.NewManaged(t.Context(), cacheRoot, filepath.Join(cacheRoot, "owner"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, finish := WithHookProcesses(cache.WithContext(t.Context()))
	backend, err := NewLocalGit(LocalGitOptions{Root: t.TempDir(), SourceRoot: initSourceRepo(t), AutoBranch: true, Hooks: Hooks{AfterCreate: daemon.Command}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Create(ctx, Issue{Identifier: "EXAMPLE-1"}); err != nil {
		t.Fatal(err)
	}
	scratch := daemon.Request(t)
	if _, err := os.Stat(scratch); err != nil {
		t.Fatalf("managed scratch during work: %v", err)
	}
	if err := finish(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(scratch); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("managed scratch after teardown: %v", err)
	}
	if daemon.Alive(t) {
		t.Fatal("managed hook service survived work")
	}
}
