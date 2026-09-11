package demofixtures

import "github.com/digitaldrywood/detent/internal/telemetry"

func demoBoardCountsSnapshot(variant string) telemetry.Snapshot {
	live := telemetry.SnapshotSection{Source: telemetry.SnapshotSourceLive, Complete: true, ObservedAt: demoBaseTime}
	snapshot := telemetry.Snapshot{GeneratedAt: demoBaseTime, Tracker: live, Runtime: live,
		Refresh: telemetry.Refresh{Status: telemetry.RefreshStatusReady, LastRefreshAt: &demoBaseTime}}
	ids := []string{"dogfood", "billing-api", "parable"}
	if variant == "board-counts-single-cached" {
		ids = ids[:1]
	}
	for i, id := range ids {
		project := telemetry.ProjectSnapshot{Project: telemetry.Project{ID: id, DisplayName: id}, Tracker: live, Runtime: live,
			Refresh: snapshot.Refresh}
		if variant == "board-counts-unknown" || (variant == "board-counts-partial" && i == 2) {
			project.Tracker = telemetry.SnapshotSection{Source: telemetry.SnapshotSourceUnknown}
			project.Runtime = project.Tracker
			project.Refresh.Status = telemetry.RefreshStatusDegraded
			project.Dispatch.WaitReason = "Authorization selector declined: issues are authorized for other hosts."
			snapshot.Tracker = telemetry.SnapshotSection{Source: telemetry.SnapshotSourceMixed}
			snapshot.Refresh.Status = telemetry.RefreshStatusPartial
		} else {
			snapshot.SchedulerDecisions = append(snapshot.SchedulerDecisions, telemetry.SchedulerDecision{ProjectID: id, IssueID: id + "-ready", Lane: "Todo", DecisionAt: demoBaseTime, Result: "selected", Selected: true})
			snapshot.BoardIssues = append(snapshot.BoardIssues,
				telemetry.Issue{ID: id + "-ready", ProjectID: id, State: "Todo", Title: "Ready work"},
				telemetry.Issue{ID: id + "-blocked", ProjectID: id, State: "Blocked", Title: "Blocked work"})
		}
		if variant == "board-counts-single-cached" {
			project.Tracker.Source = telemetry.SnapshotSourceCached
			project.Refresh.Status = telemetry.RefreshStatusDegraded
			project.Refresh.LastError = "Cached snapshot after tracker refresh failure."
		}
		if variant == "board-counts-complete-degraded" && id == "parable" {
			project.Refresh.Status = telemetry.RefreshStatusDegraded
			project.Refresh.LastError = "Tracker refresh failed with complete sections retained."
		}
		snapshot.Projects = append(snapshot.Projects, project)
	}
	for i := range 8 {
		snapshot.BoardIssues = append(snapshot.BoardIssues, telemetry.Issue{ID: string(rune('a' + i)), ProjectID: "dogfood", State: "Todo", Title: "Waiting for dispatch"})
	}
	return snapshot
}
