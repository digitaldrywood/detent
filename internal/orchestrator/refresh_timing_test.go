package orchestrator

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestRefreshTimingLogsEveryCompletedPhase(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	timing := newRefreshTiming(
		slog.New(slog.NewTextHandler(&logs, nil)),
		"digitaldrywood/detent",
		true,
	)
	for _, phase := range []string{
		"release",
		"active_runs",
		"tracker_fetch",
		"status_drift",
		"reconciliation",
		"rate_limits",
		"dispatch",
		"publish",
	} {
		timing.next(phase)
	}
	timing.log(context.Background(), true, &State{LastRefreshAt: time.Now()})

	got := logs.String()
	for _, want := range []string{
		`msg="project refresh timing"`,
		"project_id=digitaldrywood/detent",
		"manual=true",
		"completed=true",
		"refresh_status=ready",
		"total_duration=",
		"preflight_duration=",
		"release_duration=",
		"active_runs_duration=",
		"tracker_fetch_duration=",
		"status_drift_duration=",
		"reconciliation_duration=",
		"rate_limits_duration=",
		"dispatch_duration=",
		"publish_duration=",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("logs = %q, want %q", got, want)
		}
	}
}
