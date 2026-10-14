package store

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/coordination"
)

func TestLaneLedgerWithoutWorkerAttempt(t *testing.T) {
	t.Parallel()
	for _, result := range []string{"applied", "failed", "blocked"} {
		t.Run(result, func(t *testing.T) {
			t.Parallel()
			backend := openTestStore(t, t.Context())
			at := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
			identity := IssueIdentity{ProjectID: "project", IssueID: "issue"}
			if _, _, err := backend.LatestLaneWrite(t.Context(), identity); !errors.Is(err, ErrNotFound) {
				t.Fatalf("missing write: %v", err)
			}
			if _, err := backend.LaneObservation(t.Context(), identity); !errors.Is(err, ErrNotFound) {
				t.Fatalf("missing observation: %v", err)
			}
			first, err := backend.PrepareLaneWrite(t.Context(), "project", coordination.LaneWrite{Issue: "issue", From: "Backlog", To: "Todo", Reason: "admission", WrittenAt: at})
			if err != nil || first.InstanceIdentity == "" || first.FenceToken == 0 {
				t.Fatalf("prepare: %#v, %v", first, err)
			}
			if err := backend.ResolveLaneWrite(t.Context(), first.FenceToken, result); err != nil {
				t.Fatal(err)
			}
			latest, gotResult, err := backend.LatestLaneWrite(t.Context(), identity)
			if err != nil || latest != first || gotResult != result {
				t.Fatalf("latest: %#v, %s, %v", latest, gotResult, err)
			}
			second, err := backend.PrepareLaneWrite(t.Context(), "project", first)
			if err != nil || second.FenceToken <= first.FenceToken || second.InstanceIdentity != first.InstanceIdentity {
				t.Fatalf("monotonic token: %#v, %v", second, err)
			}
			observation := LaneObservation{State: "Todo", EnteredAt: at}
			if err := backend.SaveLaneObservation(t.Context(), identity, observation); err != nil {
				t.Fatal(err)
			}
			got, err := backend.LaneObservation(t.Context(), identity)
			if err != nil || got != observation {
				t.Fatalf("observation: %#v, %v", got, err)
			}
		})
	}
}

func TestShippedOutcomesUsesIssueArtifactLookup(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	backend := openTestStore(t, ctx)
	sqliteBackend := backend.(*sqliteStore)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	phase := func(project, issue, lane, metadata string, started time.Time, number int64) {
		t.Helper()
		_, err := backend.RecordWorkflowPhaseEvent(ctx, WorkflowPhaseEvent{ProjectID: project, IssueID: issue, Identifier: "owner/repo#" + issue, IssueURL: "https://example.test/" + issue, PRNumber: &number, PhaseType: WorkflowPhaseTypeLane, PhaseName: lane, Status: "entered", StartedAt: started, MetadataJSON: metadata})
		if err != nil {
			t.Fatal(err)
		}
	}
	write := func(issue, lane, reason, result string, written time.Time) {
		t.Helper()
		prepared, err := backend.PrepareLaneWrite(ctx, "project", coordination.LaneWrite{Issue: issue, From: "Rework", To: lane, Reason: reason, WrittenAt: written})
		if err != nil {
			t.Fatal(err)
		}
		if err := backend.ResolveLaneWrite(ctx, prepared.FenceToken, result); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 1000 {
		phase("project", fmt.Sprintf("unrelated-%d", i), "Merging", `{"terminal_outcome":"artifact"}`, at, 0)
	}
	phase("project", "artifact", "Done", `{"terminal_outcome":"artifact"}`, at, 17)
	write("artifact", "Done", "ready", "applied", at)
	phase("project", "ordinary", "Merging", `{}`, at, 18)
	write("ordinary", "Merging", "ready", "applied", at)
	phase("other-project", "cross-project", "Done", `{"terminal_outcome":"artifact"}`, at, 19)
	write("cross-project", "Done", "ready", "applied", at)
	phase("project", "wrong-time", "Done", `{"terminal_outcome":"artifact"}`, at.Add(time.Second), 20)
	write("wrong-time", "Done", "ready", "applied", at)
	phase("project", "failed", "Done", `{"terminal_outcome":"artifact"}`, at, 21)
	write("failed", "Done", "ready", "failed", at)
	phase("project", "merged", "Done", `{}`, at, 22)
	write("merged", "Done", "merge_worker_programmatic_merge", "applied", at)
	phase("project", "artifact", "Done", `{"terminal_outcome":"artifact"}`, at.Add(time.Hour), 23)
	write("artifact", "Done", "ready", "applied", at.Add(time.Hour))
	got, err := backend.(ShippedOutcomeStore).ShippedOutcomes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []ShippedOutcome{
		{IssueIdentity: IssueIdentity{ProjectID: "project", IssueID: "artifact", Identifier: "owner/repo#artifact", IssueURL: "https://example.test/artifact"}, State: "Done", CompletedAt: at, PRNumber: 23},
		{IssueIdentity: IssueIdentity{ProjectID: "project", IssueID: "merged", Identifier: "owner/repo#merged", IssueURL: "https://example.test/merged"}, State: "Done", CompletedAt: at, PRNumber: 22},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("shipped outcomes = %#v, want %#v", got, want)
	}
	rows, err := sqliteBackend.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+shippedOutcomesSQL)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(detail + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.String(), "SEARCH phase USING INDEX workflow_phase_events_issue_idx (issue_id=?)") || strings.Contains(plan.String(), "workflow_phase_events_project_phase_day_idx") {
		t.Fatalf("artifact lookup is not issue-scoped:\n%s", plan.String())
	}
}
