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

func TestRefreshEfficiencyReceiptTracksIncompleteIssueAtSessionIntervals(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	backend := openTestStore(t, ctx)
	live, ok := backend.(efficiency.LiveRecorder)
	if !ok {
		t.Fatal("store does not implement efficiency.LiveRecorder")
	}
	base := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	issueID := "issue-live"
	identifier := "digitaldrywood/detent#1926"
	issueURL := "https://github.com/digitaldrywood/detent/issues/1926"
	observation := efficiency.Observation{
		ProjectID:               "detent",
		IssueID:                 issueID,
		Identifier:              identifier,
		IssueURL:                issueURL,
		ObservedAt:              base.Add(5 * time.Minute),
		RefreshIntervalSessions: 5,
		Thresholds:              efficiency.Thresholds{TokensMultiple: 2, SessionsMultiple: 2, DwellMultiple: 2},
	}

	appendEfficiencySessions(t, ctx, backend, issueID, identifier, issueURL, base, 1, 4)
	if receipt, refreshed, err := live.RefreshEfficiencyReceipt(ctx, observation); err != nil {
		t.Fatalf("RefreshEfficiencyReceipt() below threshold error = %v", err)
	} else if refreshed {
		t.Fatalf("RefreshEfficiencyReceipt() below threshold = %#v, true; want no refresh", receipt)
	}

	appendEfficiencySessions(t, ctx, backend, issueID, identifier, issueURL, base, 5, 5)
	receipt, refreshed, err := live.RefreshEfficiencyReceipt(ctx, observation)
	if err != nil {
		t.Fatalf("RefreshEfficiencyReceipt() at threshold error = %v", err)
	}
	if !refreshed || !receipt.InProgress || receipt.Sessions != 5 || receipt.Attempts != 5 || receipt.Redispatches != 4 || receipt.TotalTokens != 500 {
		t.Fatalf("live receipt = %#v, want refreshed 5-session incomplete receipt", receipt)
	}
	if !receipt.CompletedAt.IsZero() || !receipt.RefreshedAt.Equal(observation.ObservedAt) {
		t.Fatalf("live receipt timestamps = completed %s refreshed %s, want zero/%s", receipt.CompletedAt, receipt.RefreshedAt, observation.ObservedAt)
	}

	completed, err := backend.ListEfficiencyReceipts(ctx, efficiency.Query{ProjectID: "detent"})
	if err != nil {
		t.Fatalf("ListEfficiencyReceipts() completed-only error = %v", err)
	}
	if len(completed) != 0 {
		t.Fatalf("completed-only receipts = %#v, want live receipt excluded", completed)
	}
	liveReceipts, err := backend.ListEfficiencyReceipts(ctx, efficiency.Query{ProjectID: "detent", IncludeInProgress: true})
	if err != nil {
		t.Fatalf("ListEfficiencyReceipts() including live error = %v", err)
	}
	if len(liveReceipts) != 1 || !liveReceipts[0].InProgress {
		t.Fatalf("receipts including live = %#v, want one incomplete receipt", liveReceipts)
	}

	appendEfficiencySessions(t, ctx, backend, issueID, identifier, issueURL, base, 6, 9)
	observation.ObservedAt = base.Add(9 * time.Minute)
	if got, refreshed, err := live.RefreshEfficiencyReceipt(ctx, observation); err != nil {
		t.Fatalf("RefreshEfficiencyReceipt() before next interval error = %v", err)
	} else if refreshed || got.Sessions != 5 {
		t.Fatalf("RefreshEfficiencyReceipt() before next interval = %#v, %t; want cached 5-session receipt", got, refreshed)
	}

	appendEfficiencySessions(t, ctx, backend, issueID, identifier, issueURL, base, 10, 10)
	observation.ObservedAt = base.Add(10 * time.Minute)
	receipt, refreshed, err = live.RefreshEfficiencyReceipt(ctx, observation)
	if err != nil {
		t.Fatalf("RefreshEfficiencyReceipt() at next interval error = %v", err)
	}
	if !refreshed || receipt.Sessions != 10 || receipt.Redispatches != 9 {
		t.Fatalf("second live receipt = %#v, want refreshed 10-session receipt", receipt)
	}

	completedAt := base.Add(20 * time.Minute)
	final, err := backend.CompleteEfficiencyReceipt(ctx, efficiency.Completion{
		ProjectID:   "detent",
		IssueID:     issueID,
		Identifier:  identifier,
		IssueURL:    issueURL,
		CompletedAt: completedAt,
	})
	if err != nil {
		t.Fatalf("CompleteEfficiencyReceipt() error = %v", err)
	}
	if final.InProgress || !final.CompletedAt.Equal(completedAt) || final.Sessions != 10 {
		t.Fatalf("final receipt = %#v, want completed 10-session receipt", final)
	}
	completed, err = backend.ListEfficiencyReceipts(ctx, efficiency.Query{ProjectID: "detent"})
	if err != nil {
		t.Fatalf("ListEfficiencyReceipts() after completion error = %v", err)
	}
	if len(completed) != 1 || completed[0].InProgress {
		t.Fatalf("completed receipts = %#v, want finalized receipt", completed)
	}
}

type efficiencySeed struct {
	issueID       string
	identifier    string
	prNumber      *int64
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
		PRNumber:    seed.prNumber,
		CompletedAt: seed.completedAt,
		Thresholds:  efficiency.Thresholds{TokensMultiple: 2, SessionsMultiple: 2, DwellMultiple: 2},
	})
	if err != nil {
		t.Fatalf("CompleteEfficiencyReceipt() error = %v", err)
	}
	return receipt
}

func appendEfficiencySessions(
	t *testing.T,
	ctx context.Context,
	backend Store,
	issueID string,
	identifier string,
	issueURL string,
	base time.Time,
	from int,
	to int,
) {
	t.Helper()

	for attempt := from; attempt <= to; attempt++ {
		startedAt := base.Add(time.Duration(attempt-1) * time.Minute)
		attemptID, err := backend.StartWorkAttempt(ctx, WorkAttemptStart{
			ProjectID:     "detent",
			IssueID:       issueID,
			Identifier:    identifier,
			IssueURL:      issueURL,
			WorkerType:    "agent",
			Lane:          "In Progress",
			AttemptNumber: attempt,
			StartedAt:     startedAt,
		})
		if err != nil {
			t.Fatalf("StartWorkAttempt(%d) error = %v", attempt, err)
		}
		sessionID, err := backend.StartSession(ctx, SessionStart{
			ProjectID:     "detent",
			WorkAttemptID: attemptID,
			IssueID:       issueID,
			Identifier:    identifier,
			IssueURL:      issueURL,
			StartedAt:     startedAt,
			Model:         "gpt-5",
		})
		if err != nil {
			t.Fatalf("StartSession(%d) error = %v", attempt, err)
		}
		if err := backend.FinishSession(ctx, sessionID, SessionFinish{
			CompletedAt:       startedAt.Add(time.Minute),
			InputTokens:       80,
			CachedInputTokens: 60,
			OutputTokens:      20,
			TotalTokens:       100,
			FinalState:        "completed",
			Model:             "gpt-5",
		}); err != nil {
			t.Fatalf("FinishSession(%d) error = %v", attempt, err)
		}
	}
}
