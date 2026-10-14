package hubserver

import (
	"slices"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestReportClasses(t *testing.T) {
	review := "GitHub requires these review conversations to be resolved before the merge. Address each finding in Rework, then resolve its conversation.\n\n**![P2 Badge](https://img.shields.io/badge/P2-yellow)** internal/hubserver/hosted_reports.go:233 gate failed on conflict"
	for _, tt := range []struct {
		name      string
		rework    bool
		fromState string
		text      string
		want      reportClass
	}{
		{name: "codex review body", rework: true, fromState: "Merging", text: review, want: reportClass{"Review findings to address", reportKindIssue}},
		{name: "human review return", rework: true, fromState: "Human Review", text: "", want: reportClass{"Review findings to address", reportKindIssue}},
		{name: "review transition", rework: true, fromState: "In Progress", text: "completed active review transition", want: reportClass{"Review findings to address", reportKindIssue}},
		{name: "rework finalization gate", rework: true, fromState: "In Progress", text: "source finalization gate failed: exit status 2", want: reportClass{"Gate failed before landing", reportKindInfrastructure}},
		{name: "rework landing gate", rework: true, fromState: "Merging", text: "landing gate failed: exit status 2 [earlier output truncated] goroutine 64176 [select]:\n\ttesting.(*T).Parallel(0x96d7337ed88)", want: reportClass{"Gate failed before landing", reportKindInfrastructure}},
		{name: "rework protected base", rework: true, fromState: "Merging", text: "landing refused: base_protected", want: reportClass{"Landing refused: protected base", reportKindInfrastructure}},
		{name: "squash conflict", rework: true, fromState: "Merging", text: "squashing fcd7735d91336d868d2536d10d33d88eb1622803 onto the base conflicts: Auto-merging web/conversation/dev/mock-hub.ts", want: reportClass{"Conflict on landing", reportKindIssue}},
		{name: "no work product", rework: true, fromState: "In Progress", text: "terminal attempt without work product", want: reportClass{"Finished without a change", reportKindIssue}},
		{name: "base moved", rework: true, fromState: "Merging", text: "base_moved: the base branch changed after landing validation", want: reportClass{"Base moved after validation", reportKindInfrastructure}},
		{name: "unrecorded rework reason", rework: true, fromState: "Blocked", text: "", want: reportNotClassified},
		{name: "unknown rework reason", rework: true, fromState: "In Progress", text: "operator asked for another pass", want: reportNotClassified},
		{name: "failure finalization gate", text: "run agent turn: source finalization gate failed: exit status 2", want: reportClass{"Finalization gate failed", reportKindInfrastructure}},
		{name: "failure protected base", text: "landing refused: base_protected", want: reportClass{"Landing refused: protected base", reportKindInfrastructure}},
		{name: "failure landing conflict", text: "landing refused: conflict", want: reportClass{"Conflict on landing", reportKindIssue}},
		{name: "failure landing gate", text: "landing gate failed: exit status 2", want: reportClass{"Landing gate failed", reportKindInfrastructure}},
		{name: "hub timeout", text: `run agent turn: hub unavailable: Post "[redacted]": context deadline exceeded (Client.Timeout exceeded while awaiting headers)`, want: reportClass{"Hub request timed out", reportKindInfrastructure}},
		{name: "hub status failure", text: "resolve landing target: list changes: Hub request failed with status 502", want: reportClass{"Hub request failed", reportKindInfrastructure}},
		{name: "reap failure", text: "run agent turn: worker process reap failed: reap orphaned workspace processes: workspace processes remained after SIGKILL: pids=[40231 40232]", want: reportClass{"Worker processes left running after stop", reportKindInfrastructure}},
		{name: "failure review conversations", text: review, want: reportClass{"Review conversations unresolved at merge", reportKindIssue}},
		{name: "workspace preparation", text: "workspace preparation failed: prepare native rework: git -C [redacted] rebase", want: reportClass{"Workspace preparation failed", reportKindInfrastructure}},
		{name: "interrupted turn", text: "codex turn_completed: codex reported an interrupted turn without error detail", want: reportClass{"Agent turn interrupted", reportKindInfrastructure}},
		{name: "unknown failure", text: "attempt failed: recorded error unavailable", want: reportNotClassified},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyFailure(tt.text)
			if tt.rework {
				got = classifyReworkReason(tt.fromState, tt.text)
			}
			if got != tt.want {
				t.Fatalf("class = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestReportFailureClassTotals(t *testing.T) {
	got := reportFailureClasses([]nativeFailure{
		{workItem: "wi_a", example: "attempt failed: recorded error unavailable", cost: 1},
		{workItem: "wi_a", example: "run agent turn: source finalization gate failed: exit status 2", cost: 2},
		{workItem: "wi_a", example: "source finalization gate failed: exit status 1", cost: 3},
		{workItem: "wi_b", example: "source finalization gate failed: exit status 2", cost: 4},
		{workItem: "wi_b", example: "landing refused: conflict", cost: 5},
	})
	want := []reportClassTotal{
		{Class: "Finalization gate failed", Kind: reportKindInfrastructure, Count: 3, Issues: 2, Cost: 9},
		{Class: "Conflict on landing", Kind: reportKindIssue, Count: 1, Issues: 1, Cost: 5},
		{Class: "Not classified", Kind: reportKindUnknown, Count: 1, Issues: 1, Cost: 1},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("failure classes = %+v, want %+v", got, want)
	}
}

func TestReportReworkClassTotals(t *testing.T) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	at := func(hour int) time.Time { return base.Add(time.Duration(hour) * time.Hour) }
	returned := func(hour int, reason string) analyticsResidenceEvent {
		return analyticsResidenceEvent{at: at(hour), data: tracker.CollaborationData{FromState: "Merging", ToState: "Rework", ReasonDetail: reason}}
	}
	timelines := []analyticsResidenceTimeline{
		{issue: analyticsResidenceIssue{id: "wi_a"}, rework: []analyticsResidenceEvent{returned(1, "landing refused: conflict"), returned(10, "landing refused: base_protected"), returned(30, "landing refused: conflict")}},
		{issue: analyticsResidenceIssue{id: "wi_b"}, rework: []analyticsResidenceEvent{returned(5, "squashing abc onto the base conflicts")}},
	}
	attempts := []nativeAnalyticsAttempt{
		{AttemptID: "before", WorkItemID: "wi_a", StartedAt: at(0)},
		{AttemptID: "after_conflict", WorkItemID: "wi_a", StartedAt: at(2)},
		{AttemptID: "after_protected", WorkItemID: "wi_a", StartedAt: at(11)},
		{AttemptID: "after_squash", WorkItemID: "wi_b", StartedAt: at(6)},
	}
	usage := map[string][]usageRow{"before": {{Cost: 100}}, "after_conflict": {{Cost: 1}}, "after_protected": {{Cost: 2}, {Cost: 3}}, "after_squash": {{Cost: 4}}}
	got := reportReworkClasses(timelines, attempts, usage, at(0), at(24))
	want := []reportClassTotal{
		{Class: "Conflict on landing", Kind: reportKindIssue, Count: 2, Issues: 2, Cost: 5},
		{Class: "Landing refused: protected base", Kind: reportKindInfrastructure, Count: 1, Issues: 1, Cost: 5},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("rework classes = %+v, want %+v", got, want)
	}
}
