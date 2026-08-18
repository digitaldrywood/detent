package templates

import (
	"sort"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web/ui/primitives"
)

func refreshFreshnessKind(snapshot telemetry.Snapshot) primitives.Kind {
	if snapshot.Refresh.Stale(snapshot.GeneratedAt) {
		return primitives.KindWarn
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
		return primitives.KindWarn
	}
	if snapshotInitializing(snapshot) {
		return primitives.KindNeutral
	}
	return primitives.KindOK
}

func refreshFreshnessLabel(snapshot telemetry.Snapshot) string {
	if snapshot.Refresh.Behind() {
		return "Loop behind"
	}
	switch refreshFreshnessKind(snapshot) {
	case primitives.KindWarn:
		return "Data stale"
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

type refreshStaleDetailRow struct {
	ProjectID string
	Sources   string
	Detail    string
}

func refreshStaleSummary(snapshot telemetry.Snapshot) string {
	stale := refreshStaleSources(snapshot.Refresh, snapshot.GeneratedAt)
	if len(stale) == 0 {
		if strings.TrimSpace(snapshot.Refresh.LastError) != "" {
			return "The latest tracker refresh failed."
		}
		return "Live snapshots do not contain current tracker data."
	}
	summary := "Tracker sources"
	if projectCount := refreshStaleProjectCount(snapshot); projectCount > 0 {
		summary = boardCountLabel(projectCount, "project", "projects")
	}
	if oldest := refreshOldestSuccess(stale); !oldest.IsZero() {
		summary += ", oldest " + prPipelineAge(oldest, snapshot.GeneratedAt) + " ago"
	}
	return summary
}

func refreshStaleProjectCount(snapshot telemetry.Snapshot) int {
	projects := make(map[string]struct{})
	for _, source := range refreshStaleSources(snapshot.Refresh, snapshot.GeneratedAt) {
		projectID := strings.TrimSpace(source.ProjectID)
		if projectID == "" {
			projectID = strings.TrimSpace(snapshot.Project.ID)
		}
		if projectID != "" {
			projects[projectID] = struct{}{}
		}
	}
	return len(projects)
}

func refreshStaleDetailRows(snapshot telemetry.Snapshot) []refreshStaleDetailRow {
	stale := refreshStaleSources(snapshot.Refresh, snapshot.GeneratedAt)
	if len(stale) == 0 {
		detail := strings.TrimSpace(snapshot.Refresh.LastError)
		if detail == "" {
			detail = "The live connection is still receiving snapshots without current tracker data."
		}
		projectID := strings.TrimSpace(snapshot.Project.ID)
		if projectID == "" {
			projectID = "Tracker"
		}
		return []refreshStaleDetailRow{{
			ProjectID: projectID,
			Sources:   "tracker refresh",
			Detail:    detail,
		}}
	}
	groups := make(map[string][]telemetry.RefreshSource)
	for _, source := range stale {
		projectID := strings.TrimSpace(source.ProjectID)
		if projectID == "" {
			projectID = strings.TrimSpace(snapshot.Project.ID)
		}
		if projectID == "" {
			projectID = "Tracker"
		}
		groups[projectID] = append(groups[projectID], source)
	}
	projectIDs := make([]string, 0, len(groups))
	for projectID := range groups {
		projectIDs = append(projectIDs, projectID)
	}
	sort.Strings(projectIDs)
	rows := make([]refreshStaleDetailRow, 0, len(projectIDs))
	for _, projectID := range projectIDs {
		sources := groups[projectID]
		sort.SliceStable(sources, func(i, j int) bool {
			return refreshAlertSourceOrder(sources[i].Name) < refreshAlertSourceOrder(sources[j].Name)
		})
		names := make([]string, 0, len(sources))
		details := make([]string, 0, len(sources))
		for _, source := range sources {
			name := refreshAlertSourceName(source.Name)
			names = append(names, name)
			detail := name
			if source.LastSuccessAt != nil {
				detail += " last succeeded " + prPipelineAge(*source.LastSuccessAt, snapshot.GeneratedAt) + " ago"
			} else {
				detail += " has not succeeded"
			}
			if source.FailureStreak > 0 {
				detail += " · " + formatCount(source.FailureStreak) + " consecutive failures"
			}
			if condition := refreshSourceConditionDetail(source); condition != "" {
				detail += " · " + condition
			}
			if lastError := strings.TrimSpace(source.LastError); lastError != "" {
				detail += " · " + lastError
			}
			details = append(details, detail)
		}
		rows = append(rows, refreshStaleDetailRow{
			ProjectID: projectID,
			Sources:   strings.Join(names, "/"),
			Detail:    strings.Join(details, "; "),
		})
	}
	return rows
}

func refreshStaleHealthDetail(snapshot telemetry.Snapshot) string {
	stale := refreshStaleSources(snapshot.Refresh, snapshot.GeneratedAt)
	if len(stale) == 0 {
		if detail := strings.TrimSpace(snapshot.Refresh.LastError); detail != "" {
			return "Tracker data is stale: " + detail
		}
		return "Tracker data is stale; the live connection is still receiving snapshots without current tracker data."
	}
	parts := make([]string, 0, len(stale))
	for _, source := range stale {
		part := refreshSourceDisplayName(source.Name)
		if projectID := strings.TrimSpace(source.ProjectID); projectID != "" {
			part = projectID + " " + part
		}
		if source.LastSuccessAt != nil {
			part += " last succeeded " + prPipelineAge(*source.LastSuccessAt, snapshot.GeneratedAt) + " ago"
		} else {
			part += " has not succeeded"
		}
		if source.FailureStreak > 0 {
			part += " · " + formatCount(source.FailureStreak) + " consecutive failures"
		}
		if condition := refreshSourceConditionDetail(source); condition != "" {
			part += " · " + condition
		}
		if lastError := strings.TrimSpace(source.LastError); lastError != "" {
			part += " · " + lastError
		}
		parts = append(parts, part)
	}
	return "Tracker data is stale: " + strings.Join(parts, "; ")
}

func refreshAlertSourceName(name telemetry.RefreshSourceName) string {
	switch name {
	case telemetry.RefreshSourceCandidates:
		return "candidate"
	case telemetry.RefreshSourceDrift:
		return "drift"
	case telemetry.RefreshSourceStatuses:
		return "status"
	case telemetry.RefreshSourceProject:
		return "project"
	default:
		value := strings.TrimSpace(string(name))
		if value == "" {
			return "tracker"
		}
		return value
	}
}

func refreshAlertSourceOrder(name telemetry.RefreshSourceName) int {
	switch name {
	case telemetry.RefreshSourceCandidates:
		return 0
	case telemetry.RefreshSourceDrift:
		return 1
	case telemetry.RefreshSourceStatuses:
		return 2
	case telemetry.RefreshSourceProject:
		return 3
	default:
		return 4
	}
}

func refreshStaleSources(refresh telemetry.Refresh, now time.Time) []telemetry.RefreshSource {
	stale := make([]telemetry.RefreshSource, 0, len(refresh.Sources))
	for _, source := range refresh.Sources {
		if refreshSourceStale(refresh, source, now) {
			stale = append(stale, source)
		}
	}
	return stale
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
