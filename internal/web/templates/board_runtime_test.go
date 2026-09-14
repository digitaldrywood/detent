package templates

import (
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestBoardRuntimePrecedesTrackerSnapshot(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 14, 15, 35, 33, 0, time.UTC)
	for _, tt := range []struct {
		name             string
		session, attempt bool
		age              time.Duration
		wait             string
	}{
		{name: "session starts one second after waiting snapshot", session: true, age: time.Second},
		{name: "session outlives stale scheduler evidence", session: true, age: 5 * time.Minute},
		{name: "session overrides retained wait", session: true, age: time.Second, wait: "Waiting for capacity"},
		{name: "active attempt before session publication", attempt: true, age: time.Second},
	} {
		t.Run(tt.name, func(t *testing.T) {
			data := boardTestData()
			issue := telemetry.Issue{ID: "2610", Identifier: "digitaldrywood/detent#2610", ProjectID: "detent", State: "Todo", Title: "Live runtime"}
			now := at.Add(tt.age)
			data.Snapshot = telemetry.Snapshot{GeneratedAt: now, Project: telemetry.Project{ID: "detent"}, BoardIssues: []telemetry.Issue{issue}, Tracker: telemetry.SnapshotSection{Source: telemetry.SnapshotSourceLive, ObservedAt: at, Complete: true}, Runtime: telemetry.SnapshotSection{Source: telemetry.SnapshotSourceLive, ObservedAt: now, Complete: true}, SchedulerDecisions: []telemetry.SchedulerDecision{{ProjectID: "detent", IssueID: issue.ID, Lane: "Todo", Result: "skipped", Reason: "global_capacity_full", DecisionAt: at}}}
			if tt.session {
				data.Snapshot.Running = []telemetry.Running{{Issue: issue, StartedAt: at.Add(time.Second)}}
			}
			if tt.attempt {
				data.Snapshot.WorkAttempts = []telemetry.WorkAttempt{{ProjectID: "detent", IssueID: issue.ID, Status: "active", StartedAt: now}}
			}
			card := projectKanbanCard{ProjectID: "detent", IssueID: issue.ID, Identifier: issue.Identifier, Title: issue.Title, Stage: "Todo", WaitDetail: tt.wait}
			view := boardCardViewFromCard(data, projectKanbanLane{Title: "Todo"}, card, false, "fleet", "")
			html := renderBoardComponent(t, boardCardView2(view))
			if !view.Running || view.Waiting || view.Work.ReadinessKey != "running" || !strings.Contains(html, ">Running<") {
				t.Errorf("card does not show Running: running=%v waiting=%v readiness=%s signals=%+v", view.Running, view.Waiting, view.Work.ReadinessKey, view.Signals)
			}
			if strings.Contains(html, "readiness unknown") || strings.Contains(html, ">Waiting<") {
				t.Error("live card renders stale readiness")
			}
			if !strings.Contains(html, "Tracker snapshot") || !strings.Contains(html, at.Format(time.RFC3339)) {
				t.Error("card does not show tracker observation separately")
			}
		})
	}
}

func TestBoardDataCurrentUsesNewestProjectObservation(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 14, 15, 35, 33, 0, time.UTC)
	for _, tt := range []struct {
		name             string
		runtime, tracker time.Time
		want             time.Time
	}{
		{"runtime newer", at.Add(time.Minute), at, at.Add(time.Minute)},
		{"tracker newer", at, at.Add(time.Minute), at.Add(time.Minute)},
		{"tracker only", time.Time{}, at, at},
	} {
		t.Run(tt.name, func(t *testing.T) {
			snapshot := telemetry.Snapshot{GeneratedAt: at.Add(time.Minute), Tracker: telemetry.SnapshotSection{ObservedAt: tt.tracker}, Runtime: telemetry.SnapshotSection{ObservedAt: tt.runtime}}
			if got := appLiveStatusAt(DashboardShellData{Snapshot: snapshot}); !got.Equal(tt.want) {
				t.Errorf("data current = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBoardRuntimeIdentityAndLiveness(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, project, status             string
		stale, completed, lastKnown, want bool
	}{
		{name: "active", project: "one", status: "active", want: true},
		{name: "other project", project: "two", status: "active"},
		{name: "stale attempt", project: "one", status: "active", stale: true},
		{name: "completed attempt", project: "one", status: "active", completed: true},
		{name: "terminal status", project: "one", status: "succeeded"},
		{name: "last known", project: "one", status: "active", lastKnown: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			attempt := telemetry.WorkAttempt{ProjectID: tt.project, IssueID: "1", Status: tt.status, Stale: tt.stale}
			if tt.completed {
				at := time.Now()
				attempt.CompletedAt = &at
			}
			snapshot := telemetry.Snapshot{LastKnown: tt.lastKnown, WorkAttempts: []telemetry.WorkAttempt{attempt}}
			if got := boardCardIsRunning(snapshot, projectKanbanCard{ProjectID: "one", IssueID: "1"}); got != tt.want {
				t.Errorf("running = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBoardFreshnessPerProject(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 14, 15, 40, 33, 0, time.UTC)
	old := now.Add(-5 * time.Minute)
	for _, tt := range []struct {
		name    string
		second  time.Time
		want    time.Time
		current bool
	}{
		{"both observed", now.Add(-time.Second), now.Add(-time.Second), true},
		{"other project stale", old, old, false},
		{"other project unknown", time.Time{}, time.Time{}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			snapshot := telemetry.Snapshot{GeneratedAt: now, Projects: []telemetry.ProjectSnapshot{
				{Project: telemetry.Project{ID: "one"}, Tracker: telemetry.SnapshotSection{ObservedAt: old}, Runtime: telemetry.SnapshotSection{ObservedAt: now}},
				{Project: telemetry.Project{ID: "two"}, Tracker: telemetry.SnapshotSection{ObservedAt: tt.second}},
			}}
			if got := appLiveStatusAt(DashboardShellData{Snapshot: snapshot}); !got.Equal(tt.want) {
				t.Errorf("stamp = %v, want %v", got, tt.want)
			}
			if got := boardObservationsCurrent(snapshot); got != tt.current {
				t.Errorf("current = %v, want %v", got, tt.current)
			}
			tracker, runtime, _ := boardProjectObservations(snapshot, "two")
			if !tracker.Equal(tt.second) || !runtime.IsZero() {
				t.Error("project observations leaked across projects")
			}
		})
	}
}

func TestBoardLiveDecisionPrecedesRetainedBlocker(t *testing.T) {
	t.Parallel()
	now := time.Now()
	data := boardTestData()
	data.Snapshot = telemetry.Snapshot{GeneratedAt: now, SchedulerDecisions: []telemetry.SchedulerDecision{{ProjectID: "detent", IssueID: "1", Lane: "Todo", Result: "selected", Selected: true, DecisionAt: now}}}
	card := projectKanbanCard{ProjectID: "detent", IssueID: "1", Stage: "Todo", Blockers: []string{"closed-dependency"}}
	view := boardCardViewFromCard(data, projectKanbanLane{Title: "Todo"}, card, false, "fleet", "")
	if view.Work.ReadinessKey != "ready" || view.Waiting || len(view.Signals) != 1 || view.Signals[0].Text != "Ready" {
		t.Errorf("selected card: readiness=%s waiting=%v signals=%+v", view.Work.ReadinessKey, view.Waiting, view.Signals)
	}
}
