package telemetry

import (
	"strings"
	"testing"
	"time"
)

func TestTodoDispatchEvidence(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 8, 22, 23, 10, 0, time.UTC)
	issue := Issue{ID: "todo", ProjectID: "pyroapex", State: "Todo", BlockedBy: []BlockedRef{{Identifier: "digitaldrywood/pyroapex#2064"}}}
	base := SchedulerDecision{IssueID: issue.ID, ProjectID: issue.ProjectID, Lane: "Todo", Result: "skipped", Reason: "blocked_by_dependency", DecisionAt: now}
	for _, tt := range []struct {
		name   string
		change func(*Snapshot)
		ready  bool
		status string
		detail string
	}{
		{name: "dependency refusal", status: "Waiting", detail: "Waiting on digitaldrywood/pyroapex#2064"},
		{name: "missing", change: func(s *Snapshot) { s.SchedulerDecisions = nil }, status: "Awaiting scheduler", detail: "unavailable"},
		{name: "stale age", change: func(s *Snapshot) { s.GeneratedAt = now.Add(2 * time.Minute) }, status: "Scheduler evidence stale", detail: "readiness unknown"},
		{name: "later sweep", change: func(s *Snapshot) { s.Dispatch.ObservedAt = now.Add(time.Second) }, status: "Scheduler evidence stale", detail: "readiness unknown"},
		{name: "future", change: func(s *Snapshot) { s.GeneratedAt = now.Add(-time.Second) }, status: "Scheduler evidence stale", detail: "readiness unknown"},
		{name: "last known", change: func(s *Snapshot) { s.LastKnown = true }, status: "Scheduler evidence stale", detail: "readiness unknown"},
		{name: "wrong project", change: func(s *Snapshot) { s.SchedulerDecisions[0].ProjectID = "other" }, status: "Awaiting scheduler", detail: "unavailable"},
		{name: "wrong lane", change: func(s *Snapshot) { s.SchedulerDecisions[0].Lane = "Rework" }, status: "Scheduler evidence stale", detail: "readiness unknown"},
		{name: "selected", change: func(s *Snapshot) {
			s.SchedulerDecisions[0].Result = "selected"
			s.SchedulerDecisions[0].Selected = true
		}, ready: true, status: "Ready", detail: "Selected by scheduler"},
		{name: "pool saturated fleet idle", change: func(s *Snapshot) {
			s.SchedulerDecisions[0].Reason = "global_capacity_full"
			s.SchedulerDecisions[0].CapacitySnapshotJSON = `{"pool":"default","global_capacity":1,"global_used":1,"shared_capacity":3,"shared_used":1,"shared_available":2}`
		}, status: "Capacity wait", detail: "Pool default: 1/1 workers used; fleet 1/3; 2 idle fleet slots unavailable to this pool"},
		{name: "malformed capacity", change: func(s *Snapshot) {
			s.SchedulerDecisions[0].Reason = "global_capacity_full"
			s.SchedulerDecisions[0].CapacitySnapshotJSON = "{"
		}, status: "Capacity wait", detail: "capacity evidence unavailable"},
		{name: "newer refusal wins", change: func(s *Snapshot) {
			s.SchedulerDecisions = append([]SchedulerDecision{{IssueID: issue.ID, ProjectID: issue.ProjectID, Lane: "Todo", Result: "selected", Selected: true, DecisionAt: now.Add(-time.Second)}}, s.SchedulerDecisions...)
		}, status: "Waiting", detail: "Waiting on digitaldrywood/pyroapex#2064"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			snapshot := Snapshot{GeneratedAt: now, BoardIssues: []Issue{issue}, SchedulerDecisions: []SchedulerDecision{base}}
			if tt.change != nil {
				tt.change(&snapshot)
			}
			got := TodoDispatchEvidence(snapshot, issue)
			if got.Ready != tt.ready || got.Status != tt.status || !strings.Contains(got.Detail, tt.detail) {
				t.Fatalf("evidence = %+v, want ready=%t status=%q detail containing %q", got, tt.ready, tt.status, tt.detail)
			}
			counts := CurrentBoardWorkload(snapshot)
			if tt.ready && (counts.Todo != 1 || counts.Waiting != 0) || !tt.ready && (counts.Todo != 0 || counts.Waiting != 1) {
				t.Fatalf("counts disagree with evidence: %+v", counts)
			}
		})
	}
}
