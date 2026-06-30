package web

import (
	"strings"
	"sync"

	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web/templates"
)

type kanbanRefreshFeedbackTracker struct {
	mu     sync.Mutex
	states map[string]telemetry.RefreshStatus
}

func newKanbanRefreshFeedbackTracker() *kanbanRefreshFeedbackTracker {
	return &kanbanRefreshFeedbackTracker{states: map[string]telemetry.RefreshStatus{}}
}

func (t *kanbanRefreshFeedbackTracker) apply(key string, data templates.KanbanData, snapshot telemetry.Snapshot) templates.KanbanData {
	if t == nil {
		return data
	}
	key = strings.TrimSpace(key)
	if key == "" {
		key = "fleet"
	}
	status := snapshot.Refresh.ReadinessStatus()

	t.mu.Lock()
	previous, ok := t.states[key]
	t.states[key] = status
	t.mu.Unlock()

	if !ok || strings.TrimSpace(data.Feedback) != "" {
		return data
	}
	switch {
	case previous != telemetry.RefreshStatusDegraded && status == telemetry.RefreshStatusDegraded:
		data.Feedback = kanbanRefreshDegradedFeedback(snapshot)
		data.FeedbackKind = "warning"
	case previous == telemetry.RefreshStatusDegraded && status == telemetry.RefreshStatusReady:
		data.Feedback = "Tracker refresh recovered."
		data.FeedbackKind = "success"
	}
	return data
}

func kanbanRefreshDegradedFeedback(snapshot telemetry.Snapshot) string {
	reason := strings.Join(strings.Fields(snapshot.Refresh.LastError), " ")
	if reason == "" {
		return "Tracker refresh degraded."
	}
	return "Tracker refresh degraded: " + reason
}
