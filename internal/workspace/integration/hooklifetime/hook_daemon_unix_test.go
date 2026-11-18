//go:build darwin || linux

package hooklifetime_test

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/toolcache"
	"github.com/digitaldrywood/detent/internal/workspace"
	"github.com/digitaldrywood/detent/internal/workspace/hookproc"
	"github.com/digitaldrywood/detent/internal/workspace/internal/gitfixture"
)

func TestMain(m *testing.M) { os.Exit(gitfixture.RunTests(m)) }

func TestDetachedHookHelper(t *testing.T) { runDetachedHook(t) }

func TestHookDaemonWorkspaceTeardown(t *testing.T) {
	for _, mode := range []string{"remove", "failed removal hook", "failed creation quarantine", "fresh checkout quarantine"} {
		t.Run(mode, func(t *testing.T) {
			daemon := detachedHook(t)
			useDaemonCandidates(t, daemon)
			hook := daemon.Command
			if mode == "failed creation quarantine" {
				hook += "; echo evidence > evidence; exit 19"
			}
			opts := workspace.LocalGitOptions{Root: t.TempDir(), SourceRoot: gitfixture.SourceRepo(t), AutoBranch: true, Hooks: workspace.Hooks{AfterCreate: hook}}
			if mode == "failed removal hook" {
				opts.Hooks.BeforeRemove = "exit 21"
			}
			backend, err := workspace.NewLocalGit(opts)
			if err != nil {
				t.Fatal(err)
			}
			ctx := t.Context()
			issue := workspace.Issue{Identifier: "EXAMPLE-1"}
			info, err := backend.Create(ctx, issue)
			switch mode {
			case "failed creation quarantine":
				var failure *workspace.HookError
				if !errors.As(err, &failure) || failure.ExitCode != 19 {
					t.Fatalf("create error = %v", err)
				}
				entries, _ := filepath.Glob(filepath.Join(opts.Root, ".detent", "quarantine", "*", "evidence"))
				if len(entries) != 1 || gitfixture.ReadFile(t, entries[0]) != "evidence\n" {
					t.Fatalf("quarantine evidence %v after %v", entries, err)
				}
			default:
				if err != nil || !daemon.Alive(t) {
					t.Fatalf("service failed to start: %v", err)
				}
				if mode == "remove" || mode == "failed removal hook" {
					issue.Terminal = true
					issue.LandedHeadSHA = strings.TrimSpace(gitfixture.Git(t, info.Path, "rev-parse", "HEAD"))
					_, err = backend.CleanupIssue(ctx, issue)
				} else {
					opts.Hooks.AfterCreate = ""
					var next workspace.Backend
					next, err = workspace.ReconfigureBackend(backend, workspace.KindLocalGit, opts)
					if err != nil {
						t.Fatal(err)
					}
					issue.FreshCheckout = true
					_, err = next.Create(ctx, issue)
				}
				if mode == "failed removal hook" {
					var failure *workspace.HookError
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

func TestCheckoutRemovalPreservesForeignProcesses(t *testing.T) {
	for _, kind := range []string{"filesystem", "local git"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()

			service := detachedHook(t)
			hooks := workspace.Hooks{AfterCreate: service.Command}
			var backend interface {
				workspace.IssueCleaner
				Create(context.Context, workspace.Issue) (workspace.Info, error)
			}
			var err error
			if kind == "filesystem" {
				backend, err = workspace.NewFilesystem(workspace.FilesystemOptions{Root: root, Hooks: hooks})
			} else {
				backend, err = workspace.NewLocalGit(workspace.LocalGitOptions{Root: root, SourceRoot: gitfixture.SourceRepo(t), AutoBranch: true, Hooks: hooks})
			}
			if err != nil {
				t.Fatal(err)
			}
			ctx, finish := workspace.WithHookProcesses(t.Context())
			t.Cleanup(func() {
				if err := finish(); err != nil {
					t.Error(err)
				}
			})
			issue := workspace.Issue{Identifier: "EXAMPLE-1"}
			info, err := backend.Create(ctx, issue)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "local git" {
				issue.Terminal = true
				issue.LandedHeadSHA = strings.TrimSpace(gitfixture.Git(t, info.Path, "rev-parse", "HEAD"))
			}
			other := detachedHook(t)
			scratch, err := workspace.PrepareWorkerScratch(t.Context(), info.Path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := workspace.CleanupWorkerScratch(info.Path, scratch); err != nil {
					t.Error(err)
				}
			})
			otherCommand := "unset DETENT_HOOK_OWNER DETENT_WORKSPACE; TEST_HOOK_CWD=" + shellQuote(info.Path) + " DETENT_WORKER_SCRATCH=" + shellQuote(scratch) + " " + other.Command
			otherBackend, err := workspace.NewFilesystem(workspace.FilesystemOptions{Root: root, Hooks: workspace.Hooks{AfterCreate: otherCommand}})
			if err != nil {
				t.Fatal(err)
			}
			otherCtx, stopOther := workspace.WithHookProcesses(t.Context())
			t.Cleanup(func() {
				if err := stopOther(); err != nil {
					t.Error(err)
				}
			})
			if _, err := otherBackend.Create(otherCtx, workspace.Issue{Identifier: "EXAMPLE-2"}); err != nil {
				t.Fatal(err)
			}
			shell := exec.Command("sh", "-c", `read -r reply && test "$reply" = alive`)
			shell.Dir = info.Path
			shell.Env = []string{"PATH=/usr/bin:/bin"}
			input, err := shell.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := shell.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				input.Close()
				if shell.ProcessState == nil {
					_ = shell.Wait()
				}
			})
			result, err := backend.CleanupIssue(ctx, issue)
			if err != nil {
				t.Fatal(err)
			}
			if result.Worktrees != 1 || service.Alive(t) {
				t.Fatalf("retirement = %+v, service alive = %t", result, service.Alive(t))
			}
			if _, err := os.Stat(info.Path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("workspace after removal: %v", err)
			}
			other.Request(t)
			_, writeErr := io.WriteString(input, "alive\n")
			if err := errors.Join(writeErr, shell.Wait()); err != nil {
				t.Fatalf("unmarked shell in retired checkout: %v", err)
			}
		})
	}
}

func TestHookOwnershipSurvivesMissingWorkspace(t *testing.T) {
	first := detachedHook(t)
	second := detachedHook(t)
	useDaemonCandidates(t, first, second)
	fs, err := workspace.NewFilesystem(workspace.FilesystemOptions{Root: t.TempDir(), Hooks: workspace.Hooks{AfterCreate: "TEST_HOOK_IGNORE_TERM=1 " + first.Command, BeforeRun: second.Command}})
	if err != nil {
		t.Fatal(err)
	}
	issue := workspace.Issue{Identifier: "EXAMPLE-1"}
	firstCtx, stopFirst := workspace.WithHookProcesses(t.Context())
	secondCtx, stopSecond := workspace.WithHookProcesses(t.Context())
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
	daemon := detachedHook(t)
	useDaemonCandidates(t, daemon)
	ctx, cancel := context.WithCancel(t.Context())
	attempt, finish := workspace.WithHookProcesses(ctx)
	fs, err := workspace.NewFilesystem(workspace.FilesystemOptions{Root: t.TempDir(), Hooks: workspace.Hooks{AfterCreate: "echo setup > evidence; " + daemon.Command, BeforeRemove: "echo stopping > removed"}})
	if err != nil {
		t.Fatal(err)
	}
	issue := workspace.Issue{Identifier: "EXAMPLE-1"}
	scan := hookproc.ReapProcessScanner
	t.Cleanup(func() { hookproc.ReapProcessScanner = scan })
	hookproc.ReapProcessScanner = func(context.Context, string) ([]int, error) { return nil, nil }
	info, err := fs.Create(attempt, issue)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fs.CleanupIssue(t.Context(), issue); !errors.Is(err, workspace.ErrWorkspacePreserved) {
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
	if got := gitfixture.ReadFile(t, filepath.Join(info.Path, "evidence")); got != "setup\n" {
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

func TestObservedHookDropsDescriptor(t *testing.T) {
	for _, tc := range []struct {
		name     string
		captured bool
		other    bool
	}{
		{"captured PID and start time", true, false},
		{"observed writer only", false, false},
		{"another writer captured", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attempt, finish := workspace.WithHookProcesses(t.Context())
			t.Cleanup(func() {
				if err := finish(); err != nil {
					t.Errorf("cleanup after fixture exit: %v", err)
				}
			})
			daemon := detachedHook(t)
			useDaemonCandidates(t, daemon)
			if !tc.captured {
				hookproc.ReapProcessScanner = func(context.Context, string) ([]int, error) { return nil, nil }
			}
			hook := "unset DETENT_HOOK_OWNER DETENT_WORKSPACE; TEST_HOOK_DROP_DESCRIPTOR=1 " + daemon.Command
			if tc.other {
				other := detachedHook(t)
				useDaemonCandidates(t, other)
				hook += " && " + other.Command
			}
			fs, err := workspace.NewFilesystem(workspace.FilesystemOptions{Root: t.TempDir(), Hooks: workspace.Hooks{
				AfterCreate: hook,
			}})
			if err != nil {
				t.Fatal(err)
			}
			issue := workspace.Issue{Identifier: "EXAMPLE-1"}
			info, err := fs.Create(attempt, issue)
			if err != nil {
				t.Fatal(err)
			}
			daemon.Request(t)
			err = finish()
			if tc.captured {
				if err != nil || daemon.Alive(t) {
					t.Fatalf("recorded service exit: cleanup = %v, alive = %t", err, daemon.Alive(t))
				}
				return
			}
			var unverified *workspace.HookExitUnverifiedError
			if !errors.As(err, &unverified) || !daemon.Alive(t) {
				t.Fatalf("unverified service exit: cleanup = %v, alive = %t", err, daemon.Alive(t))
			}
			if _, err := fs.CleanupIssue(attempt, issue); !errors.As(err, &unverified) {
				t.Fatalf("cleanup retry lost unverified exit: %v", err)
			}
			if _, err := os.Stat(info.Path); err != nil {
				t.Fatalf("retained workspace: %v", err)
			}
		})
	}
}

func TestHookDaemonHostDiscovery(t *testing.T) {
	for _, tc := range []struct{ name, hookPrefix string }{
		{"inherited descriptor", ""},
		{"closed descriptor", "exec 4>&-; "},
		{"cleared environment", "unset DETENT_HOOK_OWNER DETENT_WORKSPACE; "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attempt, finish := workspace.WithHookProcesses(t.Context())
			t.Cleanup(func() {
				if err := finish(); err != nil {
					t.Error(err)
				}
			})
			daemon := detachedHook(t)
			fs, err := workspace.NewFilesystem(workspace.FilesystemOptions{Root: t.TempDir(), Hooks: workspace.Hooks{AfterCreate: tc.hookPrefix + daemon.Command}})
			if err != nil {
				t.Fatal(err)
			}
			prior := hookproc.ReapProcessScanner
			hookproc.ReapProcessScanner = hookproc.ProcessIDs
			t.Cleanup(func() { hookproc.ReapProcessScanner = prior })
			info, err := fs.Create(attempt, workspace.Issue{Identifier: "EXAMPLE-1"})
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
	for _, tc := range []struct{ name, hookPrefix string }{
		{"detached", ""},
		{"hook process group", "TEST_HOOK_KEEP_GROUP=1 "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			daemon := detachedHook(t)
			useDaemonCandidates(t, daemon)
			cacheRoot := t.TempDir()
			cache, err := toolcache.NewManaged(t.Context(), cacheRoot, filepath.Join(cacheRoot, "owner"))
			if err != nil {
				t.Fatal(err)
			}
			ctx, finish := workspace.WithHookProcesses(cache.WithContext(t.Context()))
			backend, err := workspace.NewLocalGit(workspace.LocalGitOptions{Root: t.TempDir(), SourceRoot: gitfixture.SourceRepo(t), AutoBranch: true, Hooks: workspace.Hooks{AfterCreate: tc.hookPrefix + daemon.Command}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := backend.Create(ctx, workspace.Issue{Identifier: "EXAMPLE-1"}); err != nil {
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
		})
	}
}
