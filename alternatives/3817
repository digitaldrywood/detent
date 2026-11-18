package reports

import (
	"reflect"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/usagereport"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestReportClasses(t *testing.T) {
	review := "GitHub requires these review conversations to be resolved before the merge. Address each finding in Rework, then resolve its conversation.\n\n**![P2 Badge](https://img.shields.io/badge/P2-yellow)** internal/hubserver/hosted_reports.go:233 gate failed on conflict"
	for _, tt := range []struct {
		name      string
		rework    bool
		fromState string
		text      string
		want      ReportClass
	}{
		{name: "codex review body", rework: true, fromState: "Merging", text: review, want: ReportClass{Name: "Review findings to address", Kind: reportKindIssue}},
		{name: "human review return", rework: true, fromState: "Human Review", text: "", want: ReportClass{Name: "Review findings to address", Kind: reportKindIssue}},
		{name: "completion reason code is not a review", rework: true, fromState: "In Progress", text: "completed_active_review_transition", want: reportNotClassified},
		{name: "rework finalization gate", rework: true, fromState: "In Progress", text: "source finalization gate failed: exit status 2", want: ReportClass{Name: "Gate failed before landing", Kind: reportKindIssue}},
		{name: "rework landing gate", rework: true, fromState: "Merging", text: "landing gate failed: exit status 2 [earlier output truncated] goroutine 64176 [select]:\n\ttesting.(*T).Parallel(0x96d7337ed88)", want: ReportClass{Name: "Gate failed before landing", Kind: reportKindIssue}},
		{name: "rework merge resolution gate", rework: true, fromState: "Merging", text: "run agent turn: merge resolution gate failed: exit status 2", want: ReportClass{Name: "Gate failed before landing", Kind: reportKindIssue}},
		{name: "rework protected base", rework: true, fromState: "Merging", text: "landing refused: base_protected", want: ReportClass{Name: "Landing refused: protected base", Kind: reportKindInfrastructure}},
		{name: "squash conflict", rework: true, fromState: "Merging", text: "squashing fcd7735d91336d868d2536d10d33d88eb1622803 onto the base conflicts: Auto-merging web/conversation/dev/mock-hub.ts", want: ReportClass{Name: "Conflict on landing", Kind: reportKindIssue}},
		{name: "no work product", rework: true, fromState: "In Progress", text: "terminal attempt without work product", want: ReportClass{Name: "Finished without a change", Kind: reportKindIssue}},
		{name: "base moved", rework: true, fromState: "Merging", text: "base_moved: the base branch changed after landing validation", want: ReportClass{Name: "Base moved after validation", Kind: reportKindInfrastructure}},
		{name: "unrecorded rework reason", rework: true, fromState: "Blocked", text: "", want: reportNotClassified},
		{name: "unknown rework reason", rework: true, fromState: "In Progress", text: "operator asked for another pass", want: reportNotClassified},
		{name: "failure finalization gate", text: "run agent turn: source finalization gate failed: exit status 2", want: ReportClass{Name: "Finalization gate failed", Kind: reportKindIssue}},
		{name: "failure merge resolution gate", text: "run agent turn: merge resolution gate failed: exit status 2", want: ReportClass{Name: "Merge resolution gate failed", Kind: reportKindIssue}},
		{name: "worker cancellation deadline", text: "worker cancellation: deadline_exceeded", want: ReportClass{Name: "Agent turn timed out", Kind: reportKindInfrastructure}},
		{name: "agent turn deadline", text: "run agent turn: context deadline exceeded", want: ReportClass{Name: "Agent turn timed out", Kind: reportKindInfrastructure}},
		{name: "instance lock", text: "instance lock is held", want: ReportClass{Name: "Instance lock held", Kind: reportKindInfrastructure}},
		{name: "failure protected base", text: "landing refused: base_protected", want: ReportClass{Name: "Landing refused: protected base", Kind: reportKindInfrastructure}},
		{name: "failure landing conflict", text: "landing refused: conflict", want: ReportClass{Name: "Conflict on landing", Kind: reportKindIssue}},
		{name: "failure landing gate", text: "landing gate failed: exit status 2", want: ReportClass{Name: "Landing gate failed", Kind: reportKindIssue}},
		{name: "hub timeout", text: `run agent turn: hub unavailable: Post "[redacted]": context deadline exceeded (Client.Timeout exceeded while awaiting headers)`, want: ReportClass{Name: "Hub request timed out", Kind: reportKindInfrastructure}},
		{name: "hub status failure", text: "resolve landing target: list changes: Hub request failed with status 502", want: ReportClass{Name: "Hub request failed", Kind: reportKindInfrastructure}},
		{name: "reap failure", text: "run agent turn: worker process reap failed: reap orphaned workspace processes: workspace processes remained after SIGKILL: pids=[40231 40232]", want: ReportClass{Name: "Worker processes left running after stop", Kind: reportKindInfrastructure}},
		{name: "failure review conversations", text: review, want: ReportClass{Name: "Review conversations unresolved at merge", Kind: reportKindIssue}},
		{name: "workspace preparation", text: "workspace preparation failed: prepare native rework: git -C [redacted] rebase", want: ReportClass{Name: "Workspace preparation failed", Kind: reportKindInfrastructure}},
		{name: "interrupted turn", text: "codex turn_completed: codex reported an interrupted turn without error detail", want: ReportClass{Name: "Agent turn interrupted", Kind: reportKindInfrastructure}},
		{name: "unknown failure", text: "attempt failed: recorded error unavailable", want: reportNotClassified},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifyFailure(tt.text)
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
	got := ReportFailureClasses([]NativeFailure{
		{WorkItem: "wi_a", Example: "attempt failed: recorded error unavailable", Cost: 1},
		{WorkItem: "wi_a", Example: "run agent turn: source finalization gate failed: exit status 2", Cost: 2},
		{WorkItem: "wi_a", Example: "source finalization gate failed: exit status 1", Cost: 3},
		{WorkItem: "wi_b", Example: "source finalization gate failed: exit status 2", Cost: 4},
		{WorkItem: "wi_b", Example: "landing refused: conflict", Cost: 5},
	})
	want := []ReportClassTotal{
		{Class: "Finalization gate failed", Kind: reportKindIssue, Count: 3, Issues: 2, Cost: 9},
		{Class: "Conflict on landing", Kind: reportKindIssue, Count: 1, Issues: 1, Cost: 5},
		{Class: "Not classified", Kind: reportKindUnknown, Count: 1, Issues: 1, Cost: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("failure classes = %+v, want %+v", got, want)
	}
}

func TestReportReworkClassTotals(t *testing.T) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	at := func(hour int) time.Time { return base.Add(time.Duration(hour) * time.Hour) }
	w := operatortool.AnalyticsWindow{From: base, To: at(24), Bucket: 24 * time.Hour}
	returned := func(item string, hour int, reason string) QualityHistoryEvent {
		return QualityHistoryEvent{Item: item, Kind: "workflow.transitioned", At: at(hour), Data: tracker.CollaborationData{FromState: "Merging", ToState: "Rework", ReasonDetail: reason}}
	}
	landings := []NativeAnalyticsLanding{{ChangeID: "change", WorkItemID: "wi_landed", Landing: tracker.ChangeLanding{VersionID: "version", LandedAt: at(3)}}}
	quality := SummarizeAnalyticsQuality(landings, []QualityHistoryEvent{
		returned("wi_a", 1, "landing refused: conflict"),
		returned("wi_a", 10, "landing refused: base_protected"),
		returned("wi_a", 30, "landing refused: conflict"),
		returned("wi_b", 5, "squashing abc onto the base conflicts"),
		returned("wi_landed", 4, "landing refused: conflict"),
		{Item: "wi_c", Kind: "changes_requested", At: at(6)},
	}, nil, w)
	attempts := []NativeAnalyticsAttempt{
		{AttemptID: "before", WorkItemID: "wi_a", StartedAt: at(0)},
		{AttemptID: "after_conflict", WorkItemID: "wi_a", StartedAt: at(2)},
		{AttemptID: "after_protected", WorkItemID: "wi_a", StartedAt: at(11)},
		{AttemptID: "after_squash", WorkItemID: "wi_b", StartedAt: at(6)},
	}
	usage := map[string][]usagereport.Row{"before": {{Cost: 100}}, "after_conflict": {{Cost: 1}}, "after_protected": {{Cost: 2}, {Cost: 3}}, "after_squash": {{Cost: 4}}}
	got := reportReworkClasses(quality.returns, attempts, usage)
	want := []ReportClassTotal{
		{Class: "Conflict on landing", Kind: reportKindIssue, Count: 2, Issues: 2, Cost: 5},
		{Class: "Landing refused: protected base", Kind: reportKindInfrastructure, Count: 1, Issues: 1, Cost: 5},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rework classes = %+v, want %+v", got, want)
	}
	if quality.ReworkedItems != 3 || quality.InfraReturns != 1 {
		t.Fatalf("rework rate population = %+v", quality.AnalyticsQualityCounts)
	}
}
