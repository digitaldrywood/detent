package store

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/efficiency"
)

func TestAggregateCostPerOutcome(t *testing.T) {
	t.Parallel()

	from := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)
	tests := []struct {
		name        string
		query       efficiency.CostPerOutcomeQuery
		usage       []costPerOutcomeUsage
		completions []costPerOutcomeCompletion
		assert      func(*testing.T, efficiency.CostPerOutcomeReport)
	}{
		{
			name:  "includes start and excludes end boundary",
			query: efficiency.CostPerOutcomeQuery{From: from, To: to, Bucket: 12 * time.Hour},
			usage: []costPerOutcomeUsage{
				{projectID: "detent", totalTokens: 1_000, spendUSD: 2, at: from},
				{projectID: "detent", totalTokens: 9_000, spendUSD: 9, at: to},
			},
			completions: []costPerOutcomeCompletion{
				{projectID: "detent", mergedPR: true, at: from},
				{projectID: "detent", mergedPR: true, at: to},
			},
			assert: func(t *testing.T, report efficiency.CostPerOutcomeReport) {
				t.Helper()
				got := report.Projects[0]
				if got.Current.TotalTokens != 1_000 || got.Current.MergedPRs != 1 || got.Current.ClosedIssues != 1 {
					t.Fatalf("current = %#v, want only start-boundary samples", got.Current)
				}
				if len(got.Trend) != 2 || got.Trend[0].Metrics.TokensPerMergedPR != 1_000 {
					t.Fatalf("trend = %#v, want two buckets with start sample in first", got.Trend)
				}
			},
		},
		{
			name:  "zero outcomes produce zero ratios",
			query: efficiency.CostPerOutcomeQuery{From: from, To: to, Bucket: 24 * time.Hour},
			usage: []costPerOutcomeUsage{{projectID: "detent", totalTokens: 2_000, spendUSD: 4.5, at: from.Add(time.Hour)}},
			assert: func(t *testing.T, report efficiency.CostPerOutcomeReport) {
				t.Helper()
				got := report.Projects[0].Current
				if got.TokensPerMergedPR != 0 || got.SpendPerMergedPRUSD != 0 || got.TokensPerClosedIssue != 0 || got.SpendPerClosedIssueUSD != 0 {
					t.Fatalf("zero-outcome ratios = %#v, want all zero", got)
				}
			},
		},
		{
			name:  "separates projects and outcome denominators",
			query: efficiency.CostPerOutcomeQuery{From: from, To: to, Bucket: 24 * time.Hour},
			usage: []costPerOutcomeUsage{
				{projectID: "alpha", totalTokens: 6_000, spendUSD: 12, at: from.Add(time.Hour)},
				{projectID: "beta", totalTokens: 9_000, spendUSD: 18, at: from.Add(2 * time.Hour)},
			},
			completions: []costPerOutcomeCompletion{
				{projectID: "alpha", mergedPR: true, at: from.Add(3 * time.Hour)},
				{projectID: "alpha", at: from.Add(4 * time.Hour)},
				{projectID: "beta", mergedPR: true, at: from.Add(5 * time.Hour)},
				{projectID: "beta", mergedPR: true, at: from.Add(6 * time.Hour)},
				{projectID: "beta", at: from.Add(7 * time.Hour)},
			},
			assert: func(t *testing.T, report efficiency.CostPerOutcomeReport) {
				t.Helper()
				if len(report.Projects) != 2 || report.Projects[0].ProjectID != "alpha" || report.Projects[1].ProjectID != "beta" {
					t.Fatalf("projects = %#v, want sorted alpha and beta", report.Projects)
				}
				alpha := report.Projects[0].Current
				beta := report.Projects[1].Current
				if alpha.TokensPerMergedPR != 6_000 || alpha.TokensPerClosedIssue != 3_000 || alpha.SpendPerClosedIssueUSD != 6 {
					t.Fatalf("alpha = %#v, want isolated 1 merge and 2 closes", alpha)
				}
				if beta.TokensPerMergedPR != 4_500 || beta.TokensPerClosedIssue != 3_000 || beta.SpendPerMergedPRUSD != 9 {
					t.Fatalf("beta = %#v, want isolated 2 merges and 3 closes", beta)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			report, err := aggregateCostPerOutcome(tt.query, tt.usage, tt.completions)
			if err != nil {
				t.Fatalf("aggregateCostPerOutcome() error = %v", err)
			}
			if len(report.Projects) == 0 {
				t.Fatal("aggregateCostPerOutcome() returned no projects")
			}
			tt.assert(t, report)
		})
	}
}

func TestCostPerOutcomeReadsUsageAndReceipts(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	backend := openTestStore(t, ctx)
	from := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	prNumber := int64(1398)
	if _, err := backend.RecordUsageEvent(ctx, UsageEvent{ProjectID: "detent", IssueID: "issue-1398", PRNumber: &prNumber, Model: "gpt-5", TotalTokens: 12_000, CostUSD: 3.75, StartedAt: from.Add(time.Hour), FinishedAt: from.Add(2 * time.Hour), Outcome: "completed"}); err != nil {
		t.Fatalf("RecordUsageEvent() error = %v", err)
	}
	seedEfficiencyIssue(t, ctx, backend, efficiencySeed{issueID: "issue-1398", identifier: "digitaldrywood/detent#1398", prNumber: &prNumber, startedAt: from.Add(time.Hour), completedAt: from.Add(3 * time.Hour), attempts: 1, sessionTokens: []int64{100}, cachedTokens: []int64{80}, costUSD: 0})

	report, err := backend.CostPerOutcome(ctx, efficiency.CostPerOutcomeQuery{ProjectID: "detent", From: from, To: from.Add(24 * time.Hour), Bucket: time.Hour})
	if err != nil {
		t.Fatalf("CostPerOutcome() error = %v", err)
	}
	if len(report.Projects) != 1 {
		t.Fatalf("projects = %d, want 1", len(report.Projects))
	}
	got := report.Projects[0].Current
	if got.TotalTokens != 12_000 || got.MergedPRs != 1 || got.ClosedIssues != 1 || math.Abs(got.SpendUSD-3.75) > 0.000001 {
		t.Fatalf("current = %#v, want persisted usage and receipt outcome", got)
	}
}

func TestCostPerOutcomeIncludesWholeSecondUsageBeforeFractionalEnd(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	backend := openTestStore(t, ctx)
	from := time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC)
	finishedAt := from.Add(time.Second)
	if _, err := backend.RecordUsageEvent(ctx, UsageEvent{ProjectID: "detent", Model: "gpt-5", TotalTokens: 1_398, StartedAt: from, FinishedAt: finishedAt, Outcome: "completed"}); err != nil {
		t.Fatalf("RecordUsageEvent() error = %v", err)
	}

	report, err := backend.CostPerOutcome(ctx, efficiency.CostPerOutcomeQuery{From: from, To: finishedAt.Add(500 * time.Millisecond), Bucket: time.Second})
	if err != nil {
		t.Fatalf("CostPerOutcome() error = %v", err)
	}
	if len(report.Projects) != 1 || report.Projects[0].Current.TotalTokens != 1_398 {
		t.Fatalf("projects = %#v, want whole-second usage before fractional end", report.Projects)
	}
}

func TestCostPerOutcomeQueriesUseWindowIndexes(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	backend := openTestStore(t, ctx)
	sqliteBackend, ok := backend.(*sqliteStore)
	if !ok {
		t.Fatalf("openTestStore() returned %T, want *sqliteStore", backend)
	}
	tests := []struct {
		name      string
		statement string
		args      []any
		index     string
	}{
		{name: "unscoped usage", statement: costPerOutcomeUsageSQL, args: []any{"2026-07-10T00:00:00Z", "2026-07-11T00:00:00Z"}, index: "usage_events_finished_at_idx"},
		{name: "scoped usage", statement: costPerOutcomeProjectUsageSQL, args: []any{"detent", "2026-07-10T00:00:00Z", "2026-07-11T00:00:00Z"}, index: "usage_events_project_finished_at_idx"},
		{name: "unscoped completions", statement: costPerOutcomeCompletionsSQL, args: []any{"2026-07-10T00:00:00Z", "2026-07-11T00:00:00Z"}, index: "efficiency_receipts_completed_at_idx"},
		{name: "scoped completions", statement: costPerOutcomeProjectCompletionsSQL, args: []any{"detent", "2026-07-10T00:00:00Z", "2026-07-11T00:00:00Z"}, index: "efficiency_receipts_project_completed_idx"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows, err := sqliteBackend.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+tt.statement, tt.args...)
			if err != nil {
				t.Fatalf("EXPLAIN QUERY PLAN error = %v", err)
			}
			defer rows.Close()
			var plan strings.Builder
			for rows.Next() {
				var id, parent, unused int
				var detail string
				if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
					t.Fatalf("scan query plan error = %v", err)
				}
				plan.WriteString(detail)
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("read query plan error = %v", err)
			}
			if !strings.Contains(plan.String(), tt.index) {
				t.Fatalf("query plan = %q, want index %q", plan.String(), tt.index)
			}
		})
	}
}
