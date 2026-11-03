//go:build unix

package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/testenv"
)

func TestHookDaemonHelper(t *testing.T) { testenv.RunHookDaemonHelper(t) }

func TestHookDaemonTeardown(t *testing.T) {
	for _, mode := range []string{"remove", "quarantine", "failed creation", "quarantine SIGKILL", "missing path"} {
		t.Run(mode, func(t *testing.T) {
			source := initSourceRepo(t)
			root := t.TempDir()
			useRealProcessScan(t, root)
			command, identity := testenv.HookDaemonCommand(t)
			if mode == "quarantine SIGKILL" {
				command = "DETENT_HOOK_DAEMON_RESIST=1 " + command
			}
			backend, err := NewLocalGit(LocalGitOptions{Root: root, SourceRoot: source, AutoBranch: true, Hooks: Hooks{AfterCreate: command}})
			if err != nil {
				t.Fatal(err)
			}
			issue := Issue{Identifier: "example-1", WorkspaceSession: true}
			info, err := backend.Create(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			daemon := identity()
			neighborCommand, neighborIdentity := testenv.HookDaemonCommand(t)
			backend.hooks.AfterCreate = neighborCommand
			if _, err := backend.Create(t.Context(), Issue{Identifier: "example-2", WorkspaceSession: true}); err != nil {
				t.Fatal(err)
			}
			neighbor := neighborIdentity()
			preserved := filepath.Join(info.Path, "evidence")
			if err := os.WriteFile(preserved, []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "missing path":
				if err := os.RemoveAll(info.Path); err != nil {
					t.Fatal(err)
				}
				backend.processSource = scratchEnvironmentProcessIDs
				_, err = backend.CleanupIssue(t.Context(), issue)
			case "remove":
				if err := os.Remove(preserved); err != nil {
					t.Fatal(err)
				}
				_, err = backend.CleanupIssue(t.Context(), issue)
			case "quarantine", "quarantine SIGKILL":
				var path string
				path, err = backend.quarantineWorktree(t.Context(), info.Path)
				preserved = filepath.Join(path, "evidence")
			case "failed creation":
				if err := os.Remove(filepath.Join(info.Path, ".git")); err != nil {
					t.Fatal(err)
				}
				var path string
				var ok bool
				path, ok, err = backend.quarantineFailedWorkspace(t.Context(), info.Path)
				if !ok {
					t.Fatal("workspace was not quarantined")
				}
				preserved = filepath.Join(path, "evidence")
			}
			if err != nil {
				t.Fatal(err)
			}
			observations, err := procgroup.Observe([]procgroup.Identity{daemon})
			if err != nil || observations[0].Alive {
				t.Fatalf("hook daemon remains: %v, %v", observations, err)
			}
			if alive, err := procgroup.Alive(neighbor); err != nil || !alive {
				t.Fatalf("neighbor stopped: %t, %v", alive, err)
			}
			if mode != "remove" && mode != "missing path" {
				if data, err := os.ReadFile(preserved); err != nil || string(data) != "keep" {
					t.Fatalf("quarantine lost files: %q, %v", data, err)
				}
			}
		})
	}
}

func TestHookResourceExhaustionRetainsDiagnostic(t *testing.T) {
	for _, output := range []string{"could not create shared memory segment: No space left on device", "Too many open files", "Address already in use"} {
		t.Run(output, func(t *testing.T) {
			root := t.TempDir()
			backend, err := NewFilesystem(FilesystemOptions{Root: root, Hooks: Hooks{AfterCreate: "printf '%s' " + shellQuote(output) + "; exit 1"}})
			if err != nil {
				t.Fatal(err)
			}
			_, err = backend.Create(context.Background(), Issue{Identifier: "example"})
			var hook *HookError
			if !errors.As(err, &hook) || hook.Output != output {
				t.Fatalf("hook diagnostic = %v", err)
			}
		})
	}
}
