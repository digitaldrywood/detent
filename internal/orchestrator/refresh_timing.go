package orchestrator

import (
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/digitaldrywood/detent/internal/telemetry"
)

type refreshTiming struct {
	progress     *atomic.Pointer[telemetry.RefreshProgress]
	logger       *slog.Logger
	projectID    string
	message      string
	manual       bool
	startedAt    time.Time
	phaseStarted time.Time
	phase        string
	stepName     string
	stepStarted  time.Time
	phases       []any
}

func newRefreshTiming(logger *slog.Logger, projectID string, manual bool) *refreshTiming {
	now := time.Now()
	return &refreshTiming{
		logger:       logger,
		message:      "project refresh timing",
		projectID:    strings.TrimSpace(projectID),
		manual:       manual,
		startedAt:    now,
		phaseStarted: now,
		phase:        "preflight",
	}
}

func (t *refreshTiming) next(phase string) {
	if t == nil {
		return
	}
	now := time.Now()
	if t.phase != strings.TrimSpace(phase) {
		t.finishPhase(now)
	}
	t.phase = strings.TrimSpace(phase)
	t.phaseStarted = now
	if t.progress != nil {
		t.progress.Store(&telemetry.RefreshProgress{Stage: t.phase, StartedAt: t.startedAt, StageStartedAt: now})
	}
	if t.logger != nil {
		t.logger.Info("project refresh stage", "project_id", t.projectID, "stage", t.phase, "elapsed", now.Sub(t.startedAt))
	}
}

func (t *refreshTiming) log(ctx context.Context, completed bool, state *State) time.Duration {
	if t == nil {
		return 0
	}
	now := time.Now()
	t.finishPhase(now)
	duration := now.Sub(t.startedAt)
	if t.logger == nil {
		return duration
	}
	attrs := []any{
		"project_id", t.projectID,
		"manual", t.manual,
		"completed", completed,
		"total_duration", duration,
	}
	if state != nil {
		attrs = append(attrs,
			"refresh_status", refreshTimingStatus(state),
			"last_error", strings.TrimSpace(state.LastRefreshError),
		)
	}
	attrs = append(attrs, t.phases...)
	t.logger.InfoContext(ctx, t.message, attrs...)
	return duration
}

func refreshTimingStatus(state *State) string {
	if state == nil {
		return "unknown"
	}
	if strings.TrimSpace(state.LastRefreshError) != "" || !state.LastRefreshErrorAt.IsZero() {
		return "degraded"
	}
	if state.LastRefreshAt.IsZero() {
		return "initializing"
	}
	return "ready"
}

func (t *refreshTiming) finishPhase(now time.Time) {
	t.finishStep(now)
	if t.phase == "" {
		return
	}
	t.phases = append(t.phases, t.phase+"_duration", now.Sub(t.phaseStarted))
	t.phase = ""
}

// step reports progress at info level without changing refresh stage ownership.
// The previous step is timed before the next starts, including on early returns.
func (t *refreshTiming) step(name string) {
	if t == nil {
		return
	}
	now := time.Now()
	t.finishStep(now)
	t.stepName, t.stepStarted = name, now
	if t.logger != nil {
		t.logger.Info("project refresh sub-step started", "project_id", t.projectID, "stage", t.phase, "step", name)
	}
}

func (t *refreshTiming) finishStep(now time.Time) {
	if t.stepName == "" {
		return
	}
	if t.logger != nil {
		t.logger.Info("project refresh sub-step completed", "project_id", t.projectID, "stage", t.phase, "step", t.stepName, "duration", now.Sub(t.stepStarted))
	}
	t.stepName = ""
}
