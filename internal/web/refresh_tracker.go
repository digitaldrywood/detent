package web

import (
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/telemetry"
)

type manualRefreshTracker struct {
	mu     sync.Mutex
	latest *telemetry.RefreshAttempt
}

func newManualRefreshTracker() *manualRefreshTracker {
	return &manualRefreshTracker{}
}

func (t *manualRefreshTracker) recordResponse(response RefreshResponse) {
	if t == nil {
		return
	}
	attempt := telemetry.RefreshAttempt{
		ID:          strings.TrimSpace(response.RequestID),
		Status:      response.Status,
		RequestedAt: optionalRefreshTime(response.RequestedAt),
		Operations:  append([]string(nil), response.Operations...),
		Coalesced:   response.Coalesced,
		LastError:   strings.TrimSpace(response.LastError),
		LastErrorAt: cloneTimePtr(response.LastErrorAt),
	}
	if attempt.Status == "" {
		if response.Refused {
			attempt.Status = telemetry.RefreshAttemptStatusRefused
		} else if response.Coalesced {
			attempt.Status = telemetry.RefreshAttemptStatusCoalesced
		} else if response.Queued {
			attempt.Status = telemetry.RefreshAttemptStatusInProgress
		}
	}
	if attempt.IsZero() {
		return
	}
	t.recordAttempt(attempt)
}

func (t *manualRefreshTracker) apply(snapshot telemetry.Snapshot) telemetry.Snapshot {
	if t == nil {
		return snapshot
	}
	if snapshot.Refresh.Manual != nil {
		t.recordAttempt(*snapshot.Refresh.Manual)
	}
	t.mu.Lock()
	latest := cloneRefreshAttemptPtr(t.latest)
	t.mu.Unlock()
	if latest == nil {
		return snapshot
	}
	if snapshot.Refresh.Manual == nil || refreshAttemptNewer(latest, snapshot.Refresh.Manual) {
		snapshot.Refresh.Manual = latest
	}
	return snapshot
}

func (t *manualRefreshTracker) recordAttempt(attempt telemetry.RefreshAttempt) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.latest == nil || refreshAttemptNewer(&attempt, t.latest) {
		t.latest = cloneRefreshAttemptPtr(&attempt)
	}
}

func (s *Server) withManualRefresh(snapshot telemetry.Snapshot) telemetry.Snapshot {
	if s == nil || s.refreshes == nil {
		return snapshot
	}
	return s.refreshes.apply(snapshot)
}

func refreshAttemptNewer(candidate *telemetry.RefreshAttempt, current *telemetry.RefreshAttempt) bool {
	if candidate == nil {
		return false
	}
	if current == nil {
		return true
	}
	if strings.TrimSpace(candidate.ID) != "" && strings.TrimSpace(candidate.ID) == strings.TrimSpace(current.ID) {
		if refreshAttemptRank(candidate.Status) != refreshAttemptRank(current.Status) {
			return refreshAttemptRank(candidate.Status) > refreshAttemptRank(current.Status)
		}
		return refreshAttemptLatestTime(candidate).After(refreshAttemptLatestTime(current))
	}
	return refreshAttemptSortTime(candidate).After(refreshAttemptSortTime(current))
}

func refreshAttemptRank(status telemetry.RefreshAttemptStatus) int {
	switch status {
	case telemetry.RefreshAttemptStatusSucceeded, telemetry.RefreshAttemptStatusFailed:
		return 3
	case telemetry.RefreshAttemptStatusRefused:
		return 3
	case telemetry.RefreshAttemptStatusCoalesced:
		return 2
	case telemetry.RefreshAttemptStatusInProgress:
		return 1
	default:
		return 0
	}
}

func refreshAttemptSortTime(attempt *telemetry.RefreshAttempt) time.Time {
	if attempt == nil {
		return time.Time{}
	}
	for _, value := range []*time.Time{attempt.RequestedAt, attempt.StartedAt, attempt.CompletedAt, attempt.LastErrorAt} {
		if value != nil && !value.IsZero() {
			return value.UTC()
		}
	}
	return time.Time{}
}

func refreshAttemptLatestTime(attempt *telemetry.RefreshAttempt) time.Time {
	if attempt == nil {
		return time.Time{}
	}
	latest := time.Time{}
	for _, value := range []*time.Time{attempt.RequestedAt, attempt.StartedAt, attempt.CompletedAt, attempt.LastErrorAt} {
		if value == nil || value.IsZero() {
			continue
		}
		if latest.IsZero() || value.After(latest) {
			latest = value.UTC()
		}
	}
	return latest
}

func cloneRefreshAttemptPtr(attempt *telemetry.RefreshAttempt) *telemetry.RefreshAttempt {
	if attempt == nil {
		return nil
	}
	cloned := *attempt
	cloned.RequestedAt = cloneTimePtr(attempt.RequestedAt)
	cloned.StartedAt = cloneTimePtr(attempt.StartedAt)
	cloned.CompletedAt = cloneTimePtr(attempt.CompletedAt)
	cloned.LastErrorAt = cloneTimePtr(attempt.LastErrorAt)
	cloned.Operations = append([]string(nil), attempt.Operations...)
	return &cloned
}

func cloneTimePtr(value *time.Time) *time.Time {
	if value == nil || value.IsZero() {
		return nil
	}
	cloned := value.UTC()
	return &cloned
}

func optionalRefreshTime(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	value = value.UTC()
	return &value
}
