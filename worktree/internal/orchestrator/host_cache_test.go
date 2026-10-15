package orchestrator

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/toolcache"
)

func TestHostCacheUsesExistingReaperSweep(t *testing.T) {
	now := time.Now()
	for _, tt := range []struct {
		name     string
		last     time.Time
		want     int
		targeted bool
	}{
		{name: "startup", want: 1},
		{name: "due", last: now.Add(-2 * time.Hour), want: 1},
		{name: "not due", last: now},
		{name: "targeted cleanup cannot postpone full sweep", last: now, targeted: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			at := now
			cfg := Config{WorkspaceCleanupSweepInterval: time.Hour, WorkspaceCleanupIdleTTL: time.Hour, HostCache: toolcache.Policy{MaxAge: time.Hour, MaxBytes: 12 << 30}}
			tracker := &runningStateConnector{}
			reaper := &cleanupSweepReaper{}
			orch := &Orchestrator{cfg: cfg, connector: tracker, reaper: reaper, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), trimHostCache: func(_ context.Context, p toolcache.Policy, swept time.Time) error {
				calls++
				if p != cfg.HostCache || swept != at {
					t.Errorf("policy=%+v at=%v", p, swept)
				}
				return nil
			}}
			state := newState(cfg)
			state.LastWorkspaceCleanupAt = tt.last
			passes := []int{tt.want}
			if tt.targeted {
				idleSince := now.Add(-2 * time.Hour)
				issue := connector.Issue{ID: "blocked", Identifier: "repo#1", State: "Blocked", StageUpdatedAt: &idleSince}
				tracker.issues = []connector.Issue{issue}
				state.Blocked[issue.ID] = Blocked{Issue: issue}
				passes = []int{0, 0, 1, 1, 1, 2}
			}
			for i, want := range passes {
				if tt.targeted {
					at = now.Add(time.Duration(i+1) * 20 * time.Minute)
				}
				orch.startWorkspaceCleanup(t.Context(), &state, at)
				finishTestWorkspaceCleanup(t, orch, &state)
				if calls != want {
					t.Fatalf("pass %d calls=%d want=%d", i, calls, want)
				}
				if tt.targeted && len(reaper.issues) != i+1 {
					t.Fatalf("pass %d targeted reaps=%d want=%d", i, len(reaper.issues), i+1)
				}
			}
		})
	}
}
