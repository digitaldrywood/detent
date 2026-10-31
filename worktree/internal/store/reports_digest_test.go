package store

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/coordination"
	"github.com/digitaldrywood/detent/internal/efficiency"
)

// Catches success/import receipts being promoted to shipping evidence, and loss
// of accepted artifact/operational deliveries without an efficiency receipt.
func TestDailyDigestOutcomeProvenance(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, reason, result, target, outcome string
		receipt                               bool
		want                                  int64
	}{
		{"merged", "pull_request_merged", "applied", "Done", "", false, 1},
		{"historical programmatic ledger time", "merge_worker_programmatic_merge", "applied", "Done", "", true, 1},
		{"closed", "issue_closed_completed", "applied", "Done", "", true, 1},
		{"operational", "operational_completion", "applied", "Done", "", false, 1},
		{"configured artifact", "ready", "applied", "Published", "artifact", false, 1},
		{"imported Done", "closed_completed_observed_done", "applied", "Done", "", true, 0},
		{"abandoned", "issue_closed_not_planned", "applied", "Cancelled", "", true, 0},
		{"failed write", "pull_request_merged", "failed", "Done", "", true, 0},
		{"success without shipping", "worker_success", "applied", "Done", "", true, 0},
		{"intermediate ready", "ready", "applied", "Merging", "", true, 0},
		{"unverified ready", "ready", "applied", "Done", "", true, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			backend := openTestStore(t, ctx)
			at := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
			ledger := backend.(LaneLedgerStore)
			write, err := ledger.PrepareLaneWrite(ctx, "project", coordination.LaneWrite{Issue: "issue", From: "In Progress", To: tt.target, Reason: tt.reason, WrittenAt: at})
			if err != nil {
				t.Fatal(err)
			}
			if err := ledger.ResolveLaneWrite(ctx, write.FenceToken, tt.result); err != nil {
				t.Fatal(err)
			}
			if tt.outcome != "" {
				if _, err := backend.RecordWorkflowPhaseEvent(ctx, WorkflowPhaseEvent{ProjectID: "project", IssueID: "issue", PhaseType: WorkflowPhaseTypeLane, PhaseName: tt.target, Status: "entered", StartedAt: at, MetadataJSON: `{"terminal_outcome":"artifact"}`}); err != nil {
					t.Fatal(err)
				}
			}
			if tt.receipt {
				if _, err := backend.CompleteEfficiencyReceipt(ctx, efficiency.Completion{ProjectID: "project", IssueID: "issue", CompletedAt: at}); err != nil {
					t.Fatal(err)
				}
			}
			window := DailyDigestWindow{Date: "2026-09-30", From: at.Add(-12 * time.Hour), To: at.Add(12 * time.Hour)}
			for _, refresh := range []bool{false, true} {
				if refresh { // A later imported receipt must not revoke or move shipping.
					if _, err := backend.CompleteEfficiencyReceipt(ctx, efficiency.Completion{ProjectID: "project", IssueID: "issue", CompletedAt: at.AddDate(0, 0, 1)}); err != nil {
						t.Fatal(err)
					}
				}
				days, err := backend.DailyDigest(ctx, []DailyDigestWindow{window})
				if err != nil {
					t.Fatal(err)
				}
				day := days[0]
				if day.IssuesShipped != tt.want || day.ShippedByProject["project"] != tt.want {
					t.Fatalf("shipped=%d projects=%v, want %d", day.IssuesShipped, day.ShippedByProject, tt.want)
				}
				wantReceipts := int64(0)
				if tt.receipt && !refresh {
					wantReceipts = tt.want
				}
				if day.Efficiency.Issues != wantReceipts {
					t.Fatalf("cohort coverage=%d, want %d", day.Efficiency.Issues, wantReceipts)
				}
				for _, ids := range [][]string{{"project"}, {"foreign"}, {}} {
					scoped := window
					scoped.ProjectIDs = ids
					got, err := backend.DailyDigest(ctx, []DailyDigestWindow{scoped})
					if err != nil {
						t.Fatal(err)
					}
					want := int64(0)
					if len(ids) > 0 && ids[0] == "project" {
						want = tt.want
					}
					if got[0].IssuesShipped != want {
						t.Fatalf("scope %v shipped=%d want=%d", ids, got[0].IssuesShipped, want)
					}
				}

			}
		})
	}
}
