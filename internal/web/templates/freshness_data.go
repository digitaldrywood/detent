package templates

import (
	"sort"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web/ui/primitives"
)

func refreshFreshnessKind(snapshot telemetry.Snapshot) primitives.Kind {
	if refreshSnapshotFailed(snapshot) {
		return primitives.KindErr
	}
	if snapshot.Refresh.Status == telemetry.RefreshStatusInitializing {
		return primitives.KindNeutral
	}
	if snapshot.LastKnown {
		return primitives.KindNeutral
	}
	if snapshot.Refresh.Stale(snapshot.GeneratedAt) {
		return primitives.KindNeutral
	}
	if snapshot.Refresh.Behind() {
		return primitives.KindNeutral
	}
	if len(snapshot.Refresh.Sources) > 0 {
		if refreshOldestSuccess(snapshot.Refresh.Sources).IsZero() {
			return primitives.KindNeutral
		}
		return primitives.KindOK
	}
	if snapshotDegraded(snapshot) {
		return primitives.KindNeutral
	}
	return primitives.KindOK
}

func refreshFreshnessLabel(snapshot telemetry.Snapshot) string {
	if refreshSnapshotFailed(snapshot) {
		return "Refresh failed"
	}
	if snapshot.LastKnown {
		return "Last-known data"
	}
	if snapshot.Refresh.Behind() {
		return "Loop behind"
	}
	switch refreshFreshnessKind(snapshot) {
	case primitives.KindErr:
		return "Refresh failed"
	case primitives.KindOK:
		return "Data current"
	default:
		return "Waiting for data"
	}
}

func refreshFreshnessSummary(snapshot telemetry.Snapshot) string {
	label := refreshFreshnessLabel(snapshot)
	oldest := refreshOldestSuccess(snapshot.Refresh.Sources)
	if oldest.IsZero() {
		if snapshot.Refresh.LastRefreshAt != nil {
			oldest = *snapshot.Refresh.LastRefreshAt
		}
	}
	if oldest.IsZero() {
		return label
	}
	return label + " · " + prPipelineAge(oldest, snapshot.GeneratedAt) + " ago"
}

func refreshSourceStale(refresh telemetry.Refresh, source telemetry.RefreshSource, now time.Time) bool {
	if source.Degraded {
		return true
	}
	threshold := refresh.FailureThreshold
	if threshold <= 0 {
		threshold = 3
	}
	if source.FailureStreak >= threshold {
		return true
	}
	if source.LastSuccessAt == nil || refresh.StaleAfterSeconds <= 0 || now.IsZero() {
		return false
	}
	return now.After(source.LastSuccessAt.Add(time.Duration(refresh.StaleAfterSeconds) * time.Second))
}

func refreshOldestSuccess(sources []telemetry.RefreshSource) time.Time {
	var oldest time.Time
	for _, source := range sources {
		if source.LastSuccessAt == nil {
			continue
		}
		if oldest.IsZero() || source.LastSuccessAt.Before(oldest) {
			oldest = source.LastSuccessAt.UTC()
		}
	}
	return oldest
}

func refreshSourceDisplayName(name telemetry.RefreshSourceName) string {
	switch name {
	case telemetry.RefreshSourceCandidates:
		return "candidate fetch"
	case telemetry.RefreshSourceStatuses:
		return "status fetch"
	case telemetry.RefreshSourceDrift:
		return "drift fetch"
	case telemetry.RefreshSourceProject:
		return "project refresh"
	default:
		return strings.TrimSpace(string(name)) + " fetch"
	}
}

func refreshFailureDetail(failure telemetry.RefreshFailure) string {
	details := make([]string, 0, 4)
	if failure.Source != "" {
		details = append(details, refreshSourceDisplayName(failure.Source))
	}
	if failure.FailureStreak > 0 {
		details = append(details, formatCount(failure.FailureStreak)+" consecutive failures")
	}
	if condition := strings.TrimSpace(failure.Condition); condition != "" {
		details = append(details, condition)
	}
	if lastError := strings.TrimSpace(failure.LastError); lastError != "" {
		details = append(details, lastError)
	}
	return strings.Join(details, " · ")
}

func healthRefreshRows(snapshot telemetry.Snapshot) []healthRow {
	groups := map[string][]telemetry.RefreshSource{}
	for _, source := range snapshot.Refresh.Sources {
		projectID := strings.TrimSpace(source.ProjectID)
		if projectID == "" {
			projectID = strings.TrimSpace(snapshot.Project.ID)
		}
		groups[projectID] = append(groups[projectID], source)
	}
	if len(groups) == 0 && snapshotHasRefreshSignal(snapshot.Refresh) {
		kind := refreshFreshnessKind(snapshot)
		status := refreshHealthStatus(kind)
		if snapshot.Refresh.Behind() {
			status = "Behind"
		}
		return []healthRow{{
			ID:        "health-tracker",
			Component: "Tracker freshness",
			Kind:      kind,
			Status:    status,
			Detail:    refreshFreshnessSummary(snapshot),
			Resets:    "—",
		}}
	}
	projectIDs := make([]string, 0, len(groups))
	for projectID := range groups {
		projectIDs = append(projectIDs, projectID)
	}
	sort.Strings(projectIDs)
	rows := make([]healthRow, 0, len(projectIDs))
	for _, projectID := range projectIDs {
		sources := groups[projectID]
		sort.SliceStable(sources, func(i, j int) bool {
			return refreshSourceOrder(sources[i].Name) < refreshSourceOrder(sources[j].Name)
		})
		kind := primitives.KindOK
		status := "Current"
		if snapshot.Refresh.Behind() {
			kind = primitives.KindNeutral
			status = "Behind"
		}
		details := make([]string, 0, len(sources))
		for _, source := range sources {
			detail := refreshSourceDisplayName(source.Name)
			if source.LastSuccessAt == nil {
				detail += " pending"
			} else {
				detail += " " + prPipelineAge(*source.LastSuccessAt, snapshot.GeneratedAt) + " ago"
			}
			if source.FailureStreak > 0 {
				detail += " · " + formatCount(source.FailureStreak) + " consecutive failures"
			}
			if condition := refreshSourceConditionDetail(source); condition != "" {
				detail += " · " + condition
			}
			if refreshSourceStale(snapshot.Refresh, source, snapshot.GeneratedAt) {
				kind = primitives.KindWarn
				if lastError := strings.TrimSpace(source.LastError); lastError != "" {
					detail += ": " + lastError
				}
			}
			details = append(details, detail)
		}
		if kind == primitives.KindWarn {
			status = refreshHealthStatus(kind)
		}
		component := "Tracker freshness"
		if projectID != "" {
			component += " · " + projectID
		}
		rows = append(rows, healthRow{
			ID:        "health-tracker-" + boardCardSlug(projectID),
			Component: component,
			Kind:      kind,
			Status:    status,
			Detail:    strings.Join(details, " · "),
			Resets:    "—",
		})
	}
	return rows
}

func refreshSourceConditionDetail(source telemetry.RefreshSource) string {
	condition := strings.TrimSpace(source.Condition)
	if condition == "" {
		return ""
	}
	connectorName := strings.TrimSpace(source.Connector)
	if connectorName == "" {
		connectorName = "tracker"
	} else {
		connectorName += " tracker"
	}
	return connectorName + " · " + condition
}

func refreshHealthStatus(kind primitives.Kind) string {
	if kind == primitives.KindWarn {
		return "Stale"
	}
	if kind == primitives.KindNeutral {
		return "Initializing"
	}
	return "Current"
}

func refreshSourceOrder(name telemetry.RefreshSourceName) int {
	switch name {
	case telemetry.RefreshSourceCandidates:
		return 0
	case telemetry.RefreshSourceStatuses:
		return 1
	case telemetry.RefreshSourceDrift:
		return 2
	case telemetry.RefreshSourceProject:
		return 3
	default:
		return 4
	}
}
