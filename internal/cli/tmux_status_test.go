package cli

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestRunTmuxWindowStatusUpdatesCoalescesSnapshots(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	updates := make(chan telemetry.Snapshot)
	ticks := make(chan time.Time)
	status := &recordingTmuxWindowStatus{updates: make(chan telemetry.Snapshot, 1)}
	done := make(chan struct{})
	go func() {
		runTmuxWindowStatusUpdates(ctx, updates, ticks, status, slog.New(slog.NewTextHandler(io.Discard, nil)))
		close(done)
	}()

	updates <- telemetry.Snapshot{Counts: telemetry.Counts{Running: 1}}
	want := telemetry.Snapshot{Counts: telemetry.Counts{Running: 2, Queue: 3, Blocked: 4}}
	updates <- want
	select {
	case got := <-status.updates:
		t.Fatalf("Update() before tick = %#v", got)
	default:
	}
	ticks <- time.Now()
	if got := <-status.updates; got.Counts != want.Counts {
		t.Fatalf("Update() counts = %#v, want %#v", got.Counts, want.Counts)
	}
	select {
	case got := <-status.updates:
		t.Fatalf("extra Update() = %#v", got)
	default:
	}

	cancel()
	<-done
}

type recordingTmuxWindowStatus struct {
	updates chan telemetry.Snapshot
}

func (s *recordingTmuxWindowStatus) Update(_ context.Context, snapshot telemetry.Snapshot) error {
	s.updates <- snapshot
	return nil
}

func (*recordingTmuxWindowStatus) Close(context.Context) error {
	return nil
}
