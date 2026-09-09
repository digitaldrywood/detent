package orchestrator

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"testing/synctest"
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

func TestTickWatchdogCompletedSlowRefresh(t *testing.T) {
	t.Parallel()
	for _, duration := range []time.Duration{560 * time.Second, 700 * time.Second} {
		t.Run(duration.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				interval := 300 * time.Second
				var logs bytes.Buffer
				watchdog := newTickWatchdog("parable", interval, slog.New(slog.NewTextHandler(&logs, nil)))
				orch := &Orchestrator{tickWatchdog: watchdog}
				started := time.Now()
				state := State{PollInterval: interval, LastRefreshAt: started, NextRefreshAt: started.Add(interval)}
				recordRefreshSourceSuccess(&state, telemetry.RefreshSourceCandidates, started)
				orch.startTick(&state, started)
				time.Sleep(duration)
				active := watchdog.Evaluate(time.Now())
				wantActive := telemetry.TickLivenessStatusReady
				if duration >= 2*interval {
					wantActive = telemetry.TickLivenessStatusNeedsAttention
				}
				if active.Status != wantActive {
					t.Fatalf("active refresh status = %s", active.Status)
				}
				orch.finishTick(&state)
				completed := time.Now()
				wantNext := completed.Add(interval)
				if !state.NextRefreshAt.Equal(wantNext) {
					t.Fatalf("NextRefreshAt = %v, want completion plus interval %v", state.NextRefreshAt, wantNext)
				}
				if duration < 2*interval && strings.Contains(logs.String(), "tick loop") {
					t.Fatalf("slow refresh caused liveness churn: %s", logs.String())
				}
				logs.Reset()
				ticker := time.NewTicker(interval)
				defer ticker.Stop()
				resetTicker(ticker, interval)
				time.Sleep(interval - time.Second)
				got := watchdog.Evaluate(time.Now())
				if got.Status != telemetry.TickLivenessStatusReady || got.NextRefreshOverdue || got.MissedIntervals != 0 || got.FrozenAt != nil {
					t.Fatalf("legitimate polling wait = %+v", got)
				}
				if got.LastTickAt == nil || !got.LastTickAt.Equal(started) || !state.LastRefreshAt.Equal(started) {
					t.Fatal("completion changed start-time evidence")
				}
				source := state.RefreshSources[telemetry.RefreshSourceCandidates]
				if source.LastSuccessAt == nil || !source.LastSuccessAt.Equal(started) {
					t.Fatal("completion changed source freshness evidence")
				}
				next := <-ticker.C
				if !next.Equal(wantNext) {
					t.Fatalf("timer fired at %v, want %v", next, wantNext)
				}
				orch.startTick(&state, next)
				if strings.Contains(logs.String(), "tick loop") {
					t.Fatalf("completed refresh caused liveness churn: %s", logs.String())
				}
			})
		})
	}
}

func TestTickWatchdogMissedScheduledTick(t *testing.T) {
	t.Parallel()
	started := time.Date(2026, 9, 9, 0, 14, 34, 0, time.UTC)
	interval := 5 * time.Minute
	next := started.Add(560*time.Second + interval)
	for _, tt := range []struct {
		name   string
		offset time.Duration
		missed int64
		status telemetry.TickLivenessStatus
	}{
		{"waiting", -time.Second, 0, telemetry.TickLivenessStatusReady},
		{"due", 0, 1, telemetry.TickLivenessStatusReady},
		{"missed", interval, 2, telemetry.TickLivenessStatusNeedsAttention},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := newTickWatchdog("parable", interval, nil)
			w.Advance(started, started.Add(interval), interval)
			w.Schedule(next, interval)
			got := w.Evaluate(next.Add(tt.offset))
			if got.Status != tt.status || got.MissedIntervals != tt.missed {
				t.Fatalf("liveness = %+v, want status %s and missed %d", got, tt.status, tt.missed)
			}
		})
	}
}
