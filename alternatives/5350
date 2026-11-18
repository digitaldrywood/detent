//go:build !windows

package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/toolcache"
)

func TestLocalGitCleanupChecksRemovalHooks(t *testing.T) {
	t.Parallel()

	for _, dirty := range []bool{false, true} {
		t.Run(strconv.FormatBool(dirty), func(t *testing.T) {
			t.Parallel()
			source := initSourceRepo(t)
			publishCleanupSource(t, source)
			trace := filepath.Join(t.TempDir(), "hook-ran")
			cacheRoot := t.TempDir()
			cache, err := toolcache.NewManaged(t.Context(), cacheRoot, filepath.Join(cacheRoot, "runtime.db"))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cacheRoot, "README"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			entry := filepath.Join(cacheRoot, "00", fmt.Sprintf("%064x-d", 1))
			if err := os.Mkdir(filepath.Dir(entry), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(entry, make([]byte, 2048), 0600); err != nil {
				t.Fatal(err)
			}
			aged := time.Now().Add(-26 * time.Hour)
			if err := os.Chtimes(entry, aged, aged); err != nil {
				t.Fatal(err)
			}
			pin := filepath.Join(t.TempDir(), "active-output")
			if err := os.Link(entry, pin); err != nil {
				t.Fatal(err)
			}
			ctx := cache.WithContext(t.Context())
			reserved, registered, finished := false, false, false
			ctx = WithValidationProcessOwner(ctx, func(processCtx context.Context, _ Info, _ Issue, _ string) (ValidationProcess, error) {
				reserved = true
				return validationProcessFixture{
					started: func(procgroup.Identity) error {
						registered = true
						if report, err := cache.Maintain(processCtx, toolcache.Policy{MaxBytes: 1024}, time.Now()); err != nil || report.SizeEvictedBytes != 0 || report.RetainedBytes < 2048 {
							t.Errorf("maintenance did not yield to the running hook: %+v, %v", report, err)
						}
						if data, err := os.ReadFile(entry); err != nil || len(data) != 2048 {
							t.Errorf("running hook's cache entry changed: bytes=%d err=%v", len(data), err)
						}
						return nil
					},
					finished: func(gate.CommandResult, error) error { finished = true; return nil },
				}, nil
			})
			shell := filepath.Join(t.TempDir(), "hook-shell")
			probe := fmt.Sprintf(`{"GOHOSTOS":%q,"GOHOSTARCH":%q,"GOVERSION":"go1.26.0"}`, runtime.GOOS, runtime.GOARCH)
			script := "#!/bin/sh\nif [ \"$2\" = 'go env -json' ]; then\n  printf '%s\\n' " + shellQuote(probe) + "\n  exit 0\nfi\nexec /bin/sh \"$@\"\n"
			if err := os.WriteFile(shell, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			backend, err := NewLocalGit(LocalGitOptions{
				Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true,
				Logger: slog.New(slog.NewTextHandler(t.Output(), nil)),
				Hooks:  Hooks{Shell: shell, BeforeRemove: "test \"$GOCACHE\" = " + shellQuote(cacheRoot) + " && touch " + shellQuote(trace) + " && printf 'hook work' > README.md"},
			})
			if err != nil {
				t.Fatal(err)
			}
			issue := Issue{Identifier: "cleanup-hook", Terminal: true}
			info, err := backend.Create(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			issue.CleanupDeliveredHeadSHA = strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))
			if dirty {
				if err := os.WriteFile(filepath.Join(info.Path, "README.md"), []byte("worker work"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			result, err := backend.CleanupIssue(ctx, issue)
			if !errors.Is(err, ErrWorkspacePreserved) || result.Worktrees != 0 {
				t.Fatalf("cleanup = %+v, %v; want preservation", result, err)
			}
			_, err = os.Stat(trace)
			if dirty != errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("hook trace stat = %v, dirty = %t", err, dirty)
			}
			if reserved != !dirty || registered != !dirty || finished != !dirty {
				t.Fatalf("cleanup hook ownership: reserved=%t registered=%t finished=%t dirty=%t", reserved, registered, finished, dirty)
			}
			if err := os.Remove(pin); err != nil {
				t.Fatal(err)
			}
			if report, err := cache.Maintain(t.Context(), toolcache.Policy{MaxBytes: 1024}, time.Now()); err != nil || report.SizeEvictedBytes < 2048 || report.RetainedBytes > 1024 {
				t.Fatalf("hook retained cache protection after cleanup: %#v, %v", report, err)
			}
			if _, err := os.Stat(entry); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unprotected hook cache entry retained: %v", err)
			}
			want := "hook work"
			if dirty {
				want = "worker work"
			}
			if got := readFile(t, filepath.Join(info.Path, "README.md")); got != want {
				t.Fatalf("retained work = %q, want %q", got, want)
			}
		})
	}
}
