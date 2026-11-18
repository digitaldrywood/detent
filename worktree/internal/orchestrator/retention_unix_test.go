//go:build !windows

package orchestrator

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/testutil/fakehub"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestRetentionUnremovableQuarantineWarnsOnce(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "workspaces")
	backend, err := workspace.NewLocalGit(workspace.LocalGitOptions{Root: root, SourceRoot: e2eInitSourceRepo(t)})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	parent := filepath.Join(root, ".detent/quarantine")
	quarantine := filepath.Join(parent, "workspace-20260910T120000.000000000Z")
	if err := os.MkdirAll(quarantine, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(quarantine, "file"), []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(parent, 0o700); err != nil {
			t.Error(err)
		}
	})
	var logs bytes.Buffer
	cfg := normalizeConfig(Config{WorkspaceCleanupSweepInterval: time.Hour})
	o := &Orchestrator{cfg: cfg, connector: fakehub.New(fakehub.Config{}), reaper: &localRetentionReaper{LocalGit: backend}, logger: slog.New(slog.NewTextHandler(&logs, nil))}
	state := newState(cfg)
	for pass := range 3 {
		o.startWorkspaceCleanup(t.Context(), &state, now.Add(time.Duration(pass)*time.Hour))
		finishTestWorkspaceCleanup(t, o, &state)
	}
	if got := strings.Count(logs.String(), "workspace retention failed"); got != 1 {
		t.Fatalf("warnings=%d want=1: %s", got, logs.String())
	}
	if !strings.Contains(logs.String(), filepath.Join(".detent/quarantine", filepath.Base(quarantine))) {
		t.Fatalf("warning missing quarantine path: %s", logs.String())
	}
}
