package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestTickWatchdogEvaluatesLoopLiveness(t *testing.T) {
	t.Parallel()

	lastTickAt := time.Date(2026, 8, 17, 1, 21, 52, 0, time.UTC)
	interval := 8 * time.Minute
	tests := []struct {
		name         string
		now          time.Time
		advance      bool
		wantStatus   telemetry.TickLivenessStatus
		wantOverdue  bool
		wantMissed   int64
		wantFrozenAt time.Time
	}{
		{
			name:       "loop has not started",
			now:        lastTickAt,
			wantStatus: telemetry.TickLivenessStatusInitializing,
		},
		{
			name:       "healthy loop remains ready",
			now:        lastTickAt.Add(interval),
			advance:    true,
			wantStatus: telemetry.TickLivenessStatusReady,
			wantMissed: 1,
		},
		{
			name:        "past due loop is visible before watchdog threshold",
			now:         lastTickAt.Add(interval + time.Second),
			advance:     true,
			wantStatus:  telemetry.TickLivenessStatusReady,
			wantOverdue: true,
			wantMissed:  1,
		},
		{
			name:         "frozen loop needs attention after two intervals",
			now:          lastTickAt.Add(2 * interval),
			advance:      true,
			wantStatus:   telemetry.TickLivenessStatusNeedsAttention,
			wantOverdue:  true,
			wantMissed:   2,
			wantFrozenAt: lastTickAt,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			watchdog := newTickWatchdog("detent", interval, nil)
			if tt.advance {
				watchdog.Advance(lastTickAt, lastTickAt.Add(interval), interval)
			}
			got := watchdog.Evaluate(tt.now)
			if got.Status != tt.wantStatus {
				t.Fatalf("Status = %q, want %q", got.Status, tt.wantStatus)
			}
			if got.NextRefreshOverdue != tt.wantOverdue {
				t.Fatalf("NextRefreshOverdue = %v, want %v", got.NextRefreshOverdue, tt.wantOverdue)
			}
			if got.MissedIntervals != tt.wantMissed {
				t.Fatalf("MissedIntervals = %d, want %d", got.MissedIntervals, tt.wantMissed)
			}
			switch {
			case tt.wantFrozenAt.IsZero() && got.FrozenAt != nil:
				t.Fatalf("FrozenAt = %v, want nil", got.FrozenAt)
			case !tt.wantFrozenAt.IsZero() && (got.FrozenAt == nil || !got.FrozenAt.Equal(tt.wantFrozenAt)):
				t.Fatalf("FrozenAt = %v, want %v", got.FrozenAt, tt.wantFrozenAt)
			}
		})
	}
}

func TestTickWatchdogRunsOutsideTickLoop(t *testing.T) {
	t.Parallel()

	lastTickAt := time.Date(2026, 8, 17, 1, 21, 52, 0, time.UTC)
	interval := 8 * time.Minute
	watchdog := newTickWatchdog("detent", interval, nil)
	watchdog.Advance(lastTickAt, lastTickAt.Add(interval), interval)
	checks := make(chan time.Time)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		watchdog.run(ctx, checks)
	}()

	checks <- lastTickAt.Add(2 * interval)
	deadline := time.NewTimer(time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer deadline.Stop()
	defer ticker.Stop()
	for {
		if got := watchdog.Snapshot(lastTickAt.Add(2 * interval)); got.Status == telemetry.TickLivenessStatusNeedsAttention {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("watchdog did not mark frozen loop needs_attention")
		case <-ticker.C:
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("watchdog did not stop after cancellation")
	}
}
