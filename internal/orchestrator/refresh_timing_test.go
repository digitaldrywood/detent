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

func TestRefreshSubstepTiming(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		stage    string
		duration time.Duration
	}{
		{"reconciliation", 61 * time.Second},
		{"reconciliation", 296 * time.Second},
		{"workspace_cleanup", 290 * time.Second},
	} {
		t.Run(tt.stage+tt.duration.String(), func(t *testing.T) {
			var logs bytes.Buffer
			timing := newRefreshTiming(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})), "detent", false)
			timing.next(tt.stage)
			timing.step("fetch")
			started := timing.stepStarted
			timing.finishStep(started.Add(tt.duration))
			timing.step("apply")
			timing.next("publish")
			for _, want := range []string{"sub-step started", "sub-step completed", "stage=" + tt.stage, "step=fetch", "duration=" + tt.duration.String(), "step=apply"} {
				if !strings.Contains(logs.String(), want) {
					t.Errorf("logs missing %q: %s", want, logs.String())
				}
			}
			if timing.stepName != "" {
				t.Error("stage transition retained open step")
			}
		})
	}
}
