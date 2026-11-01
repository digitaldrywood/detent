package orchestrator

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
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

func TestRefreshTimingGraphQLPoints(t *testing.T) {
	for _, cost := range []int64{0, 3, 12} {
		t.Run(strconv.FormatInt(cost, 10), func(t *testing.T) {
			var logs bytes.Buffer
			timing := newRefreshTiming(slog.New(slog.NewTextHandler(&logs, nil)), "project", false)
			ctx, points := connector.WithGraphQLPoints(t.Context())
			timing.points = points
			timing.next("tracker_fetch")
			connector.RecordGraphQLPoints(ctx, cost)
			connector.RecordGraphQLPoints(ctx, cost)
			timing.next("reconciliation")
			connector.RecordGraphQLPoints(ctx, 1)
			timing.log(ctx, true, nil)
			for _, want := range []string{fmt.Sprintf("tracker_fetch_graphql_points=%d", 2*cost), "reconciliation_graphql_points=1"} {
				if !strings.Contains(logs.String(), want) {
					t.Errorf("missing %s in %s", want, logs.String())
				}
			}
		})
	}
}
