package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestFetchTickIssuesOwnsScan(t *testing.T) {
	for _, tt := range []struct {
		name               string
		beginErr, fetchErr error
	}{
		{name: "success"}, {name: "fetch failure", fetchErr: errors.New("fetch failed")}, {name: "cancelled wait", beginErr: context.Canceled},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tracker := &scanLifecycleConnector{beginErr: tt.beginErr, fetchErr: tt.fetchErr, t: t}
			cfg := normalizeConfig(Config{ActiveStates: []string{"Todo"}})
			o := &Orchestrator{cfg: cfg, connector: tracker, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
			state := newState(cfg)
			_, ok := o.fetchTickIssues(t.Context(), &state, time.Now(), githubBudgetReserveDecision{}, nil)
			if want := tt.beginErr == nil && tt.fetchErr == nil; ok != want {
				t.Errorf("success = %v, want %v", ok, want)
			}
			if tracker.held {
				t.Error("scan not released")
			}
			if want := tt.beginErr == nil; tracker.fetched != want || tracker.released != want {
				t.Errorf("fetched=%v released=%v, want %v", tracker.fetched, tracker.released, want)
			}
		})
	}
}

type scanLifecycleConnector struct {
	connector.Connector
	t                       *testing.T
	onBegin, onFetch        func()
	beginErr, fetchErr      error
	held, fetched, released bool
}

func (c *scanLifecycleConnector) BeginRefreshScan(context.Context) (func(), error) {
	if c.onBegin != nil {
		c.onBegin()
	}
	if c.beginErr != nil {
		return nil, c.beginErr
	}
	c.held = true
	return func() { c.held = false; c.released = true }, nil
}
func (c *scanLifecycleConnector) CombinedRefreshEnabled() bool { return true }
func (c *scanLifecycleConnector) FetchRefreshIssues(context.Context, []string, []string, connector.IssueFilterHint) connector.RefreshIssueResult {
	if !c.held {
		c.t.Error("fetch without scan ownership")
	}
	if c.onFetch != nil {
		c.onFetch()
	}
	c.fetched = true
	return connector.RefreshIssueResult{CandidateError: c.fetchErr}
}

func TestRefreshScanWaitExcludedFromWatchdog(t *testing.T) {
	for _, tt := range []struct {
		name               string
		beginErr, fetchErr error
	}{
		{name: "success"}, {name: "cancelled", beginErr: context.Canceled}, {name: "fetch failure", fetchErr: errors.New("fetch failed")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				const interval = time.Minute
				const wait = 3 * time.Minute
				var logs bytes.Buffer
				logger := slog.New(slog.NewTextHandler(&logs, nil))
				watchdog := newTickWatchdog("project", interval, logger)
				cfg := normalizeConfig(Config{ActiveStates: []string{"Todo"}})
				tracker := &scanLifecycleConnector{t: t, beginErr: tt.beginErr, fetchErr: tt.fetchErr}
				o := &Orchestrator{cfg: cfg, connector: tracker, logger: logger, tickWatchdog: watchdog}
				state := newState(cfg)
				state.PollInterval = interval
				started := time.Now()
				o.startTick(&state, started)
				// Preserve time already spent doing real work before entering the queue.
				time.Sleep(10 * time.Second)
				tracker.onBegin = func() {
					time.Sleep(wait)
					if got := watchdog.Evaluate(time.Now()); got.Status != telemetry.TickLivenessStatusReady || got.FrozenAt != nil {
						t.Errorf("queued scan liveness = %+v", got)
					}
				}
				tracker.onFetch = func() {
					if got := watchdog.Evaluate(time.Now()); got.Status != telemetry.TickLivenessStatusReady || got.MissedIntervals != 0 {
						t.Errorf("acquired scan liveness = %+v", got)
					}
				}
				timing := newRefreshTiming(logger, "project", false)
				timing.next("tracker_fetch")
				o.fetchTickIssues(t.Context(), &state, started, githubBudgetReserveDecision{}, timing)
				timing.log(t.Context(), true, &state)
				if !strings.Contains(logs.String(), "tracker_scan_wait_duration=3m0s") {
					t.Errorf("missing queue duration: %s", logs.String())
				}
				if strings.Contains(logs.String(), "tick loop frozen") {
					t.Error("healthy queue wait logged a frozen tick")
				}
				time.Sleep(2*interval - 10*time.Second - time.Nanosecond)
				if got := watchdog.Evaluate(time.Now()); got.Status != telemetry.TickLivenessStatusReady {
					t.Errorf("before threshold = %+v", got)
				}
				time.Sleep(time.Nanosecond)
				if got := watchdog.Evaluate(time.Now()); got.Status != telemetry.TickLivenessStatusNeedsAttention {
					t.Errorf("stalled work after wait = %+v", got)
				}
				o.finishTick(&state)
				next := state.NextRefreshAt
				// Completing the refresh restores the ordinary cadence; the old
				// queue wait must not postpone detection of a missed next tick.
				if got := watchdog.Evaluate(next.Add(interval)); got.Status != telemetry.TickLivenessStatusNeedsAttention {
					t.Errorf("missed next tick = %+v", got)
				}
				o.startTick(&state, time.Now())
				if got := watchdog.Evaluate(time.Now().Add(2 * interval)); got.Status != telemetry.TickLivenessStatusNeedsAttention {
					t.Errorf("next active tick = %+v", got)
				}
			})
		})
	}
}
