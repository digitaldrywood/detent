package project

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestSweepSharedCache(t *testing.T) {
	for _, tt := range []struct {
		name                       string
		paused, running, cancelled bool
	}{
		{name: "live", running: true},
		{name: "paused", paused: true},
		{name: "pausing with workers", paused: true, running: true},
		{name: "starting"},
		{name: "cancelled", running: true, cancelled: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(workspace.SharedCacheRoot(root, "test"), "go-build")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "data")
			file, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			// Sparse logical size exercises the real fixed budget without allocating 20 GiB.
			if err := file.Truncate(workspace.SharedBuildCacheBudget + 1); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			p := &Project{cfg: globalconfig.Project{ID: "test", Paused: tt.paused}, logger: slog.New(slog.NewTextHandler(&logs, nil)), workflow: workflowconfig.Workflow{Config: workflowconfig.Config{Workspace: workflowconfig.Workspace{Root: root, CleanupIdleTTLMS: 86400000, CleanupSweepIntervalMS: 600000}}}}
			if tt.running {
				p.done = make(chan struct{})
			}
			ctx := t.Context()
			if tt.cancelled {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			got := p.SweepSharedCache(ctx, time.Now())
			if got.ObservedAt.IsZero() || got.ProjectID != "test" || got.BudgetBytes != workspace.SharedBuildCacheBudget {
				t.Fatalf("missing usage: %+v", got)
			}
			if tt.cancelled {
				if got.Error != context.Canceled.Error() {
					t.Fatalf("usage = %+v", got)
				}
			} else {
				if got.Error != "" || got.BuildBytes != 0 || got.RemovedBytes != workspace.SharedBuildCacheBudget+1 {
					t.Fatalf("usage = %+v", got)
				}
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("over-budget entry remains: %v", err)
				}
			}
			for _, field := range []string{"measured_bytes=", "budget_bytes=", "removed_bytes=", "error="} {
				if !strings.Contains(logs.String(), field) {
					t.Fatalf("missing %s in %s", field, logs.String())
				}
			}
			again := p.SweepSharedCache(t.Context(), got.ObservedAt.Add(time.Second))
			if again != got || strings.Count(logs.String(), "shared cache sweep") != 1 {
				t.Fatalf("repeated sweep within interval: %+v, %s", again, logs.String())
			}
			next := p.SweepSharedCache(t.Context(), got.ObservedAt.Add(10*time.Minute))
			if next.Error != "" || strings.Count(logs.String(), "shared cache sweep") != 2 {
				t.Fatalf("next sweep missing: %+v, %s", next, logs.String())
			}
		})
	}
}
