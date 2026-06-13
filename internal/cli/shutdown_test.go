package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hub"
	projectpkg "github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestShutdownControllerQueuesRequests(t *testing.T) {
	t.Parallel()

	controller := NewShutdownController()
	controller.RequestDrain()
	controller.RequestForce()

	if got := <-controller.Requests(); got != ShutdownRequestDrain {
		t.Fatalf("first request = %v, want drain", got)
	}
	if got := <-controller.Requests(); got != ShutdownRequestForce {
		t.Fatalf("second request = %v, want force", got)
	}
}

func TestShutdownControllerInterruptRequestsDrainThenForce(t *testing.T) {
	t.Parallel()

	controller := NewShutdownController()
	if controller.RequestInterrupt() {
		t.Fatal("inactive interrupt was handled")
	}

	deactivate := controller.activate()
	defer deactivate()

	if !controller.RequestInterrupt() {
		t.Fatal("active interrupt was not handled")
	}
	if got := <-controller.Requests(); got != ShutdownRequestDrain {
		t.Fatalf("first interrupt = %v, want drain", got)
	}

	if !controller.RequestInterrupt() {
		t.Fatal("second interrupt was not handled")
	}
	if got := <-controller.Requests(); got != ShutdownRequestForce {
		t.Fatalf("second interrupt = %v, want force", got)
	}
}

func TestShutdownControllerDrainMakesNextInterruptForce(t *testing.T) {
	t.Parallel()

	controller := NewShutdownController()
	deactivate := controller.activate()
	defer deactivate()

	controller.RequestDrain()
	if got := <-controller.Requests(); got != ShutdownRequestDrain {
		t.Fatalf("explicit drain = %v, want drain", got)
	}

	if !controller.RequestInterrupt() {
		t.Fatal("interrupt after drain was not handled")
	}
	if got := <-controller.Requests(); got != ShutdownRequestForce {
		t.Fatalf("interrupt after drain = %v, want force", got)
	}
}

func TestRunWithShutdownZeroSessionsExitsGracefully(t *testing.T) {
	t.Parallel()

	controller := NewShutdownController()
	started := make(chan struct{})
	var output bytes.Buffer
	errs := make(chan error, 1)

	go func() {
		errs <- runWithShutdown(context.Background(), runningShutdownConfig{
			Controller:       controller,
			Registry:         projectpkg.NewRegistry(),
			SnapshotHub:      hub.New[telemetry.Snapshot](),
			Output:           &output,
			ProgressInterval: time.Millisecond,
			HardTimeout:      time.Second,
			Now: func() time.Time {
				return time.Date(2026, 6, 12, 15, 0, 0, 0, time.UTC)
			},
		}, func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		})
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("server did not start")
	}
	controller.RequestDrain()

	select {
	case err := <-errs:
		if err != nil {
			t.Fatalf("runWithShutdown() error = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for shutdown")
	}
	if got := output.String(); !strings.Contains(got, "shutdown requested — no agent sessions in flight") {
		t.Fatalf("output missing zero-session notice:\n%s", got)
	}
}

func TestRunWithShutdownZeroSessionsExitsWhenServeIgnoresCancellation(t *testing.T) {
	t.Parallel()

	controller := NewShutdownController()
	started := make(chan struct{})
	errs := make(chan error, 1)

	go func() {
		errs <- runWithShutdown(context.Background(), runningShutdownConfig{
			Controller:       controller,
			Registry:         projectpkg.NewRegistry(),
			SnapshotHub:      hub.New[telemetry.Snapshot](),
			Output:           io.Discard,
			ProgressInterval: time.Millisecond,
			HardTimeout:      20 * time.Millisecond,
			Now: func() time.Time {
				return time.Date(2026, 6, 12, 15, 0, 0, 0, time.UTC)
			},
		}, func(context.Context) error {
			close(started)
			select {}
		})
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("server did not start")
	}
	controller.RequestDrain()

	select {
	case err := <-errs:
		if err != nil {
			t.Fatalf("runWithShutdown() error = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for shutdown")
	}
}

func TestRunWithShutdownMarksControllerActive(t *testing.T) {
	t.Parallel()

	controller := NewShutdownController()
	ctx, cancel := context.WithCancel(context.Background())
	active := make(chan bool, 1)
	errs := make(chan error, 1)

	go func() {
		errs <- runWithShutdown(ctx, runningShutdownConfig{
			Controller:  controller,
			Registry:    projectpkg.NewRegistry(),
			SnapshotHub: hub.New[telemetry.Snapshot](),
			HardTimeout: time.Second,
		}, func(ctx context.Context) error {
			active <- controller.Active()
			<-ctx.Done()
			return ctx.Err()
		})
	}()

	select {
	case got := <-active:
		if !got {
			t.Fatal("controller active = false while runWithShutdown is serving")
		}
	case <-time.After(time.Second):
		t.Fatal("server did not start")
	}
	cancel()

	select {
	case err := <-errs:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("runWithShutdown() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for shutdown")
	}
	if controller.Active() {
		t.Fatal("controller active = true after runWithShutdown returned")
	}
}

func TestRunWithShutdownForceReturnsForcedError(t *testing.T) {
	t.Parallel()

	controller := NewShutdownController()
	started := make(chan struct{})
	var output bytes.Buffer
	errs := make(chan error, 1)

	go func() {
		errs <- runWithShutdown(context.Background(), runningShutdownConfig{
			Controller:  controller,
			Registry:    projectpkg.NewRegistry(),
			SnapshotHub: hub.New[telemetry.Snapshot](),
			Output:      &output,
			HardTimeout: time.Second,
			Now: func() time.Time {
				return time.Date(2026, 6, 12, 15, 0, 0, 0, time.UTC)
			},
		}, func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		})
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("server did not start")
	}
	controller.RequestForce()

	select {
	case err := <-errs:
		if !errors.Is(err, ErrShutdownForced) {
			t.Fatalf("runWithShutdown() error = %v, want ErrShutdownForced", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for force shutdown")
	}
	if got := output.String(); !strings.Contains(got, "force quit requested — interrupting 0 agent sessions") {
		t.Fatalf("output missing force notice:\n%s", got)
	}
}

func TestShutdownBannerFormatsRunningSessions(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	writeShutdownBanner(&output, []telemetry.Running{
		{
			Issue: telemetry.Issue{
				ID:         "issue-365",
				Identifier: "digitaldrywood/detent#365",
				Title:      "docs(onboarding): tighten setup",
				State:      "In Progress",
			},
			RuntimeSeconds: 724,
		},
	})

	got := output.String()
	for _, want := range []string{
		"shutdown requested — 1 agent session in flight",
		"#365 docs(onboarding)",
		"In Progress",
		"12m 4s",
		"draining: no new work will be dispatched",
		"press Ctrl+C again to force quit",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("banner missing %q:\n%s", want, got)
		}
	}
}
