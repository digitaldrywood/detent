package update

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func TestPendingControllerAdoptionDoesNotWaitForIdle(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name       string
		idle       bool
		decline    bool
		automatic  bool
		background bool
	}{
		{name: "busy runner"},
		{name: "idle runner", idle: true},
		{name: "automatic tick adopts without discovery", idle: true, automatic: true},
		{name: "automatic tick retries failed adoption", idle: true, automatic: true, decline: true},
		{name: "background scheduler retries on the existing update interval", idle: true, automatic: true, background: true, decline: true},
		{name: "failed restart preserves adoption for next tick", idle: true, decline: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 10, 27, 14, 0, 0, 0, time.UTC)
			restarts := 0
			waits := 0
			accept := !test.decline
			updater := &schedulerUpdaterStub{checkErr: errors.New("release discovery unavailable"), applyStatus: Status{
				Action: ActionUpdated, LatestVersion: "1.0.1", Binary: "verified-controller",
				BinarySHA256: strings.Repeat("a", 64), VerifiedRelease: true,
			}}
			var scheduler *Scheduler
			var err error
			scheduler, err = NewScheduler(SchedulerConfig{
				Enabled:       test.automatic,
				CheckInterval: time.Minute,
				Now:           func() time.Time { return now },
				RunningBuild:  runnerauth.BuildEvidence{Version: "1.0.0"},
				Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
				Updater:       updater,
				NextDelay:     func(interval time.Duration) time.Duration { return interval },
				Wait: func(_ context.Context, delay time.Duration) bool {
					if restarts >= 2 {
						return false
					}
					waits++
					if waits > 2 || delay != time.Minute || restarts == 1 && scheduler.Status().LastError == "" {
						t.Fatalf("pending adoption wait = %s, requests = %d, failure = %q", delay, restarts, scheduler.Status().LastError)
					}
					now = now.Add(delay)
					if restarts == 1 {
						accept = true
					}
					return true
				},
				ReserveIdle: func(context.Context) (func(), bool) { return nil, test.idle },
				RequestRestart: func(binary string) bool {
					if binary != "verified-controller" {
						t.Errorf("restart binary = %q, want verified-controller", binary)
					}
					restarts++
					return accept
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, installed := scheduler.installControlLocked(t.Context(), ApplyOptions{}, "1.0.1"); !installed {
				t.Fatal("verified controller update was not installed")
			}

			tick := func() {
				now = now.Add(time.Minute)
				if test.automatic {
					if _, err := scheduler.CheckNow(t.Context()); err != nil {
						t.Fatal(err)
					}
				} else {
					scheduler.EnrolledUpdate(t.Context(), scheduler.RunningBuild(), nil)
				}
			}
			if test.background {
				scheduler.Run(t.Context())
			} else {
				tick()
			}
			if !test.background && restarts != 1 {
				t.Fatalf("restart requests after one update tick = %d, want 1 while attempts remain active", restarts)
			}
			if test.decline && !test.background {
				status := scheduler.Status()
				if status.LastError == "" {
					t.Error("failed restart was not reported")
				}
				accept = true
				tick()
				if restarts != 2 {
					t.Fatalf("restart requests after recovery tick = %d, want 2", restarts)
				}
			}
			if test.background && restarts != 2 {
				t.Fatalf("background adoption attempts = %d, want 2", restarts)
			}
			if updater.checkCalls != 0 {
				t.Errorf("pending installed adoption performed %d release discovery calls", updater.checkCalls)
			}
			if status := scheduler.Status(); status.State != "restart_requested" || status.LastError != "" {
				t.Fatalf("successful adoption status = %+v", status)
			}
		})
	}
}
