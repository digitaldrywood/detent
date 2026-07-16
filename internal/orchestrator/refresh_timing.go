package orchestrator

import (
	"context"
	"log/slog"
	"strings"
	"time"
)

type refreshTiming struct {
	logger       *slog.Logger
	projectID    string
	manual       bool
	startedAt    time.Time
	phaseStarted time.Time
	phase        string
	phases       []any
}

func newRefreshTiming(logger *slog.Logger, projectID string, manual bool) *refreshTiming {
	now := time.Now()
	return &refreshTiming{
		logger:       logger,
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
	t.finishPhase(now)
	t.phase = strings.TrimSpace(phase)
	t.phaseStarted = now
}

func (t *refreshTiming) log(ctx context.Context, completed bool, state *State) {
	if t == nil || t.logger == nil {
		return
	}
	now := time.Now()
	t.finishPhase(now)
	attrs := []any{
		"project_id", t.projectID,
		"manual", t.manual,
		"completed", completed,
		"total_duration", now.Sub(t.startedAt),
	}
	if state != nil {
		attrs = append(attrs,
			"refresh_status", refreshTimingStatus(state),
			"last_error", strings.TrimSpace(state.LastRefreshError),
		)
	}
	attrs = append(attrs, t.phases...)
	t.logger.InfoContext(ctx, "project refresh timing", attrs...)
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
	if t.phase == "" {
		return
	}
	t.phases = append(t.phases, t.phase+"_duration", now.Sub(t.phaseStarted))
	t.phase = ""
}
