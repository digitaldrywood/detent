package templates

import (
	"time"

	"github.com/digitaldrywood/detent/internal/telemetry"
)

// boardProjectObservations keeps the tracker clock available independently of
// the newest observation used for the board's data-current stamp.
func boardProjectObservations(snapshot telemetry.Snapshot, projectID string) (time.Time, time.Time, telemetry.Refresh) {
	tracker, runtime, refresh := snapshot.Tracker, snapshot.Runtime, snapshot.Refresh
	for _, project := range snapshot.Projects {
		if project.Project.ID == projectID {
			tracker, runtime, refresh = project.Tracker, project.Runtime, project.Refresh
			break
		}
	}
	at := tracker.ObservedAt
	if at.IsZero() {
		at = refreshOldestSuccess(refresh.Sources)
	}
	if at.IsZero() && refresh.LastRefreshAt != nil {
		at = *refresh.LastRefreshAt
	}
	return at, runtime.ObservedAt, refresh
}

func boardNewestObservation(tracker, runtime time.Time) time.Time {
	if runtime.After(tracker) {
		return runtime
	}
	return tracker
}

// A fleet stamp uses the oldest of the per-project newest observations: one
// busy project must not make an unobserved project's data look current.
func boardDataObservedAt(snapshot telemetry.Snapshot) time.Time {
	if len(snapshot.Projects) == 0 {
		tracker, runtime, _ := boardProjectObservations(snapshot, snapshot.Project.ID)
		return boardNewestObservation(tracker, runtime)
	}
	var oldest time.Time
	for _, project := range snapshot.Projects {
		tracker, runtime, _ := boardProjectObservations(snapshot, project.Project.ID)
		at := boardNewestObservation(tracker, runtime)
		if at.IsZero() {
			return time.Time{}
		}
		if oldest.IsZero() || at.Before(oldest) {
			oldest = at
		}
	}
	return oldest
}

func boardObservationsCurrent(snapshot telemetry.Snapshot) bool {
	if snapshot.LastKnown || snapshot.GeneratedAt.IsZero() {
		return false
	}
	current := func(projectID string) bool {
		tracker, runtime, refresh := boardProjectObservations(snapshot, projectID)
		at := boardNewestObservation(tracker, runtime)
		maxAge := time.Duration(refresh.StaleAfterSeconds) * time.Second
		if maxAge <= 0 {
			maxAge = max(2*time.Duration(refresh.PollIntervalSeconds)*time.Second, time.Minute)
		}
		age := snapshot.GeneratedAt.Sub(at)
		return !at.IsZero() && age >= 0 && age <= maxAge
	}
	if len(snapshot.Projects) == 0 {
		return current(snapshot.Project.ID)
	}
	for _, project := range snapshot.Projects {
		if !current(project.Project.ID) {
			return false
		}
	}
	return true
}
