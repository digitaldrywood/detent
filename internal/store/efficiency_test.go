package store

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/efficiency"
)

func TestEfficiencyReceiptReconcilesRawRowsAndFlagsOutlier(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	backend := openTestStore(t, ctx)
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	normal := seedEfficiencyIssue(t, ctx, backend, efficiencySeed{
		issueID:       "issue-normal",
		identifier:    "digitaldrywood/detent#1200",
		startedAt:     base,
		completedAt:   base.Add(10 * time.Minute),
		attempts:      2,
		sessionTokens: []int64{120, 80},
		cachedTokens:  []int64{90, 50},
		costUSD:       0.25,
		working:       300,
		gate:          120,
		merge:         60,
		parked:        30,
		ciReruns:      1,
		breakerTrips:  1,
	})
	if normal.Sessions != 2 || normal.Attempts != 2 {
		t.Fatalf("normal sessions/attempts = %d/%d, want 2/2", normal.Sessions, normal.Attempts)
	}
	if normal.TotalTokens != 200 || normal.InputTokens != 160 || normal.CachedInputTokens != 140 || normal.OutputTokens != 40 {
		t.Fatalf("normal token totals = %#v, want raw session sums", normal)
	}
	if math.Abs(normal.EstimatedCostUSD-0.25) > 0.000001 {
		t.Fatalf("normal estimated cost = %f, want 0.25", normal.EstimatedCostUSD)
	}
	if normal.WorkingSeconds != 390 || normal.GateWaitSeconds != 120 || normal.MergeTrainSeconds != 60 || normal.ParkedSeconds != 30 {
		t.Fatalf("normal dwell = %#v, want 390/120/60/30", normal)
	}
	if normal.Redispatches != 1 || normal.CIReruns != 1 || normal.BreakerTrips != 1 {
		t.Fatalf("normal retry counts = %#v, want 1/1/1", normal)
	}
	if normal.Anomalous() {
		t.Fatalf("normal receipt marked anomalous: %#v", normal)
	}

	outlier := seedEfficiencyIssue(t, ctx, backend, efficiencySeed{
		issueID:       "issue-outlier",
		identifier:    "digitaldrywood/detent#1201",
		startedAt:     base.Add(24 * time.Hour),
		completedAt:   base.Add(24*time.Hour + 40*time.Minute),
		attempts:      5,
		sessionTokens: []int64{500, 500, 500, 500, 500},
		cachedTokens:  []int64{100, 100, 100, 100, 100},
		costUSD:       5,
		working:       1500,
		gate:          300,
		merge:         120,
		parked:        240,
	})
	if !outlier.TokensAnomaly || !outlier.SessionsAnomaly || !outlier.DwellAnomaly {
		t.Fatalf("outlier anomaly flags = %#v, want all true", outlier)
	}

	stored, err := backend.EfficiencyReceipt(ctx, "detent", outlier.IssueID, outlier.Identifier)
	if err != nil {
		t.Fatalf("EfficiencyReceipt() error = %v", err)
	}
	if stored.TotalTokens != outlier.TotalTokens || !stored.Anomalous() {
		t.Fatalf("stored receipt = %#v, want persisted outlier", stored)
	}

	rollup, err := backend.EfficiencyRollup(ctx, efficiency.Query{
		ProjectID: "detent",
		From:      base.Add(-time.Hour),
		To:        base.Add(48 * time.Hour),
	})
	if err != nil {
		t.Fatalf("EfficiencyRollup() error = %v", err)
	}
	if rollup.Current.Issues != 2 || rollup.Current.Anomalies != 1 {
		t.Fatalf("rollup issues/anomalies = %d/%d, want 2/1", rollup.Current.Issues, rollup.Current.Anomalies)
	}
	if rollup.Current.TokensPerIssue.P50 != 200 || rollup.Current.TokensPerIssue.P90 != 2500 {
		t.Fatalf("rollup token percentiles = %#v, want 200/2500", rollup.Current.TokensPerIssue)
	}
	if len(rollup.CacheTrend) != 2 {
		t.Fatalf("rollup cache trend len = %d, want 2", len(rollup.CacheTrend))
	}
}

type efficiencySeed struct {
	issueID       string
	identifier    string
	startedAt     time.Time
	completedAt   time.Time
	attempts      int
	sessionTokens []int64
	cachedTokens  []int64
	costUSD       float64
	working       int64
	gate          int64
	merge         int64
	parked        int64
	ciReruns      int
	breakerTrips  int
}

func seedEfficiencyIssue(t *testing.T, ctx context.Context, backend Store, seed efficiencySeed) efficiency.Receipt {
	t.Helper()

	url := "https://github.com/digitaldrywood/detent/issues/" + seed.issueID
	attemptIDs := make([]int64, 0, seed.attempts)
	for attempt := 1; attempt <= seed.attempts; attempt++ {
		attemptID, err := backend.StartWorkAttempt(ctx, WorkAttemptStart{
			ProjectID:     "detent",
			IssueID:       seed.issueID,
			Identifier:    seed.identifier,
			IssueURL:      url,
			WorkerType:    "agent",
			Lane:          "In Progress",
			AttemptNumber: attempt,
			StartedAt:     seed.startedAt.Add(time.Duration(attempt-1) * time.Minute),
		})
		if err != nil {
			t.Fatalf("StartWorkAttempt() error = %v", err)
		}
		attemptIDs = append(attemptIDs, attemptID)
	}
	for index, totalTokens := range seed.sessionTokens {
		inputTokens := totalTokens * 4 / 5
		outputTokens := totalTokens - inputTokens
		sessionID, err := backend.StartSession(ctx, SessionStart{
			WorkAttemptID: attemptIDs[min(index, len(attemptIDs)-1)],
			IssueID:       seed.issueID,
			Identifier:    seed.identifier,
			IssueURL:      url,
			StartedAt:     seed.startedAt.Add(time.Duration(index) * time.Minute),
			Model:         "gpt-5",
		})
		if err != nil {
			t.Fatalf("StartSession() error = %v", err)
		}
		if err := backend.FinishSession(ctx, sessionID, SessionFinish{
			CompletedAt:       seed.startedAt.Add(time.Duration(index+1) * time.Minute),
			InputTokens:       inputTokens,
			CachedInputTokens: seed.cachedTokens[index],
			OutputTokens:      outputTokens,
			TotalTokens:       totalTokens,
			FinalState:        "completed",
			Model:             "gpt-5",
		}); err != nil {
			t.Fatalf("FinishSession() error = %v", err)
		}
	}
	if _, err := backend.RecordUsageEvent(ctx, UsageEvent{
		ProjectID:  "detent",
		IssueID:    seed.issueID,
		Identifier: seed.identifier,
		Model:      "gpt-5",
		CostUSD:    seed.costUSD,
		StartedAt:  seed.startedAt,
		FinishedAt: seed.completedAt,
		Outcome:    "completed",
	}); err != nil {
		t.Fatalf("RecordUsageEvent() error = %v", err)
	}
	dwell := []struct {
		name    string
		seconds int64
	}{
		{name: "In Progress", seconds: seed.working},
		{name: "Human Review", seconds: seed.gate},
		{name: "Merging", seconds: seed.merge},
		{name: "Blocked", seconds: seed.parked},
	}
	cursor := seed.startedAt
	for _, lane := range dwell {
		if lane.seconds == 0 {
			continue
		}
		finishedAt := cursor.Add(time.Duration(lane.seconds) * time.Second)
		if _, err := backend.RecordWorkflowPhaseEvent(ctx, WorkflowPhaseEvent{
			ProjectID:  "detent",
			IssueID:    seed.issueID,
			Identifier: seed.identifier,
			IssueURL:   url,
			PhaseType:  WorkflowPhaseTypeLane,
			PhaseName:  lane.name,
			Status:     "exited",
			StartedAt:  cursor,
			FinishedAt: finishedAt,
		}); err != nil {
			t.Fatalf("RecordWorkflowPhaseEvent(%s) error = %v", lane.name, err)
		}
		cursor = finishedAt
	}
	for range seed.ciReruns {
		if _, err := backend.RecordWorkflowPhaseEvent(ctx, WorkflowPhaseEvent{ProjectID: "detent", IssueID: seed.issueID, PhaseType: WorkflowPhaseTypeReview, PhaseName: "ci_rerun", Status: "completed", StartedAt: cursor, FinishedAt: cursor}); err != nil {
			t.Fatalf("RecordWorkflowPhaseEvent(ci_rerun) error = %v", err)
		}
	}
	for range seed.breakerTrips {
		if _, err := backend.RecordWorkflowPhaseEvent(ctx, WorkflowPhaseEvent{ProjectID: "detent", IssueID: seed.issueID, PhaseType: WorkflowPhaseTypeLane, PhaseName: "Blocked", Status: "entered", Reason: "instant_fail_circuit_breaker", StartedAt: cursor}); err != nil {
			t.Fatalf("RecordWorkflowPhaseEvent(breaker) error = %v", err)
		}
	}
	receipt, err := backend.CompleteEfficiencyReceipt(ctx, efficiency.Completion{
		ProjectID:   "detent",
		IssueID:     seed.issueID,
		Identifier:  seed.identifier,
		IssueURL:    url,
		CompletedAt: seed.completedAt,
		Thresholds:  efficiency.Thresholds{TokensMultiple: 2, SessionsMultiple: 2, DwellMultiple: 2},
	})
	if err != nil {
		t.Fatalf("CompleteEfficiencyReceipt() error = %v", err)
	}
	return receipt
}
