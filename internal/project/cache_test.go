package project

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestSweepPausedSharedCache(t *testing.T) {
	for _, paused := range []bool{false, true} {
		t.Run(map[bool]string{false: "running", true: "paused"}[paused], func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(workspace.SharedCacheRoot(root, "test"), "go-build")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "data"), []byte("data"), 0o600); err != nil {
				t.Fatal(err)
			}
			p := &Project{cfg: globalconfig.Project{ID: "test", Paused: paused}, workflow: workflowconfig.Workflow{Config: workflowconfig.Config{Workspace: workflowconfig.Workspace{Root: root, CleanupIdleTTLMS: 86400000, CleanupSweepIntervalMS: 600000}}}}
			now := time.Now()
			got := p.SweepPausedSharedCache(t.Context(), now)
			if paused && got.BuildBytes != 4 {
				t.Fatalf("usage = %+v", got)
			}
			if !paused && !got.ObservedAt.IsZero() {
				t.Fatalf("unpaused project swept: %+v", got)
			}
			if paused {
				again := p.SweepPausedSharedCache(t.Context(), now.Add(time.Second))
				if again != got {
					t.Fatalf("repeated sweep within interval: %+v", again)
				}
			}
		})
	}
}
