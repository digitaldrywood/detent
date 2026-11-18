package workspace

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	commandshell "github.com/digitaldrywood/detent/internal/shell"
)

func TestMissingHookShellDoesNotRun(t *testing.T) {
	t.Parallel()
	for _, backend := range []string{"local_git", "filesystem"} {
		t.Run(backend, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			shellName := filepath.Join(root, "missing-shell")
			hooks := Hooks{Shell: shellName}
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			run := (&LocalGit{root: root, hooks: hooks, logger: logger}).executeHook
			if backend == "filesystem" {
				run = (&Filesystem{root: root, hooks: hooks, logger: logger}).runHook
			}
			err := run(context.Background(), "before_run", "echo ran > marker", Info{Path: root}, Issue{})
			var hook *HookError
			var unavailable *commandshell.UnavailableError
			if !errors.As(err, &hook) || !errors.As(err, &unavailable) || unavailable.Shell != shellName || hook.ExitCode != -1 {
				t.Fatalf("hook error = %v, want unavailable shell %q without exit", err, shellName)
			}
			if _, err := os.Stat(filepath.Join(root, "marker")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("hook ran: marker stat = %v", err)
			}
		})
	}
}
