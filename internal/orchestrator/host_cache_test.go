package orchestrator

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/toolcache"
)

func TestHostCacheUsesExistingReaperSweep(t *testing.T) {
	now := time.Now()
	for _, tt := range []struct {
		name string
		last time.Time
		want int
	}{
		{"startup", time.Time{}, 1}, {"due", now.Add(-2 * time.Hour), 1}, {"not due", now, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			cfg := Config{WorkspaceCleanupSweepInterval: time.Hour, HostCache: toolcache.Policy{MaxAge: time.Hour, MaxBytes: 12}}
			orch := &Orchestrator{cfg: cfg, reaper: &cleanupSweepReaper{}, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), trimHostCache: func(_ context.Context, p toolcache.Policy, at time.Time) error {
				calls++
				if p != cfg.HostCache || at != now {
					t.Fatalf("policy=%+v at=%v", p, at)
				}
				return nil
			}}
			state := newState(cfg)
			state.LastWorkspaceCleanupAt = tt.last
			orch.reapDueWorkspacesAfterRefresh(context.Background(), &state, now)
			if calls != tt.want {
				t.Fatalf("calls=%d want=%d", calls, tt.want)
			}
		})
	}
}
