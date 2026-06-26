package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/store/sqlc"
)

func TestOpenSQLiteAppliesMigrationsAndPragmas(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "detent.db")

	backend, err := Open(ctx, Config{
		Backend: BackendSQLite,
		Path:    dbPath,
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})

	sqliteBackend, ok := backend.(*sqliteStore)
	if !ok {
		t.Fatalf("Open() returned %T, want *sqliteStore", backend)
	}

	if got := queryString(t, sqliteBackend.db, "PRAGMA journal_mode"); got != "wal" {
		t.Fatalf("journal_mode = %q, want wal", got)
	}
	if got := queryInt(t, sqliteBackend.db, "PRAGMA busy_timeout"); got != 5000 {
		t.Fatalf("busy_timeout = %d, want 5000", got)
	}
	if got := queryInt(t, sqliteBackend.db, "SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name IN ('detent_runs', 'codex_sessions', 'fair_share_usage', 'usage_events', 'workflow_phase_events')"); got != 5 {
		t.Fatalf("migrated table count = %d, want 5", got)
	}
}

func TestSQLiteQueriesRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "detent.db")

	backend, err := Open(ctx, Config{
		Backend:     BackendSQLite,
		Path:        dbPath,
		BusyTimeout: 2500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})

	run, err := backend.Queries().CreateDetentRun(ctx, sqlc.CreateDetentRunParams{
		StartedAt:            "2026-05-30T12:00:00Z",
		StoppedAt:            sql.NullString{},
		RestartReason:        sql.NullString{},
		PeakConcurrentAgents: 3,
		SessionsLaunched:     1,
		InputTokens:          120,
		OutputTokens:         30,
		TotalTokens:          150,
		RuntimeSeconds:       90,
	})
	if err != nil {
		t.Fatalf("CreateDetentRun() error = %v", err)
	}

	session, err := backend.Queries().CreateCodexSession(ctx, sqlc.CreateCodexSessionParams{
		RunID:          sql.NullInt64{Int64: run.ID, Valid: true},
		IssueID:        sql.NullString{String: "I_kwDOSskuwc8AAAABD42cNw", Valid: true},
		Identifier:     sql.NullString{String: "digitaldrywood/detent#5", Valid: true},
		IssueURL:       sql.NullString{String: "https://github.com/digitaldrywood/detent/issues/5", Valid: true},
		StartedAt:      sql.NullString{String: "2026-05-30T12:01:00Z", Valid: true},
		CompletedAt:    sql.NullString{String: "2026-05-30T12:02:00Z", Valid: true},
		Turns:          2,
		InputTokens:    100,
		OutputTokens:   20,
		TotalTokens:    120,
		RuntimeSeconds: 60,
		FinalState:     sql.NullString{String: "Human Review", Valid: true},
		Model:          sql.NullString{String: "gpt-5", Valid: true},
	})
	if err != nil {
		t.Fatalf("CreateCodexSession() error = %v", err)
	}

	got, err := backend.Queries().GetCodexSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("GetCodexSession() error = %v", err)
	}

	if got.RunID.Int64 != run.ID {
		t.Fatalf("session run_id = %d, want %d", got.RunID.Int64, run.ID)
	}
	if got.Identifier.String != "digitaldrywood/detent#5" {
		t.Fatalf("session identifier = %q, want digitaldrywood/detent#5", got.Identifier.String)
	}
}

func TestStatsStoreRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		run  RunStart
	}{
		{
			name: "persists run and session stats",
			run: RunStart{
				StartedAt:            time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC),
				PeakConcurrentAgents: 1,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			backend := openTestStore(t, ctx)

			runID, err := backend.StartRun(ctx, tt.run)
			if err != nil {
				t.Fatalf("StartRun() error = %v", err)
			}

			if err := backend.UpdateRun(ctx, runID, RunUpdate{
				PeakConcurrentAgents: 3,
				SessionsLaunched:     1,
				InputTokens:          100,
				OutputTokens:         25,
				TotalTokens:          125,
				RuntimeSeconds:       240,
			}); err != nil {
				t.Fatalf("UpdateRun() error = %v", err)
			}

			sessionID, err := backend.StartSession(ctx, SessionStart{
				RunID:      runID,
				IssueID:    "I_kwDOSskuwc8AAAABD42c3Q",
				Identifier: "digitaldrywood/detent#6",
				IssueURL:   "https://github.com/digitaldrywood/detent/issues/6",
				StartedAt:  time.Date(2026, 5, 30, 12, 1, 0, 0, time.UTC),
				Model:      "gpt-5",
			})
			if err != nil {
				t.Fatalf("StartSession() error = %v", err)
			}

			if err := backend.FinishSession(ctx, sessionID, SessionFinish{
				CompletedAt:    time.Date(2026, 5, 30, 12, 5, 0, 0, time.UTC),
				Turns:          2,
				InputTokens:    100,
				OutputTokens:   25,
				TotalTokens:    125,
				RuntimeSeconds: 240,
				FinalState:     "Human Review",
			}); err != nil {
				t.Fatalf("FinishSession() error = %v", err)
			}

			if err := backend.StopRun(ctx, runID, RunStop{
				StoppedAt:            time.Date(2026, 5, 30, 12, 5, 0, 0, time.UTC),
				RestartReason:        "complete",
				PeakConcurrentAgents: 3,
				SessionsLaunched:     1,
				InputTokens:          100,
				OutputTokens:         25,
				TotalTokens:          125,
				RuntimeSeconds:       240,
			}); err != nil {
				t.Fatalf("StopRun() error = %v", err)
			}

			run, err := backend.Queries().GetDetentRun(ctx, runID)
			if err != nil {
				t.Fatalf("GetDetentRun() error = %v", err)
			}
			if run.StartedAt != "2026-05-30T12:00:00Z" {
				t.Fatalf("run started_at = %q, want 2026-05-30T12:00:00Z", run.StartedAt)
			}
			if run.StoppedAt.String != "2026-05-30T12:05:00Z" {
				t.Fatalf("run stopped_at = %q, want 2026-05-30T12:05:00Z", run.StoppedAt.String)
			}
			if run.TotalTokens != 125 {
				t.Fatalf("run total_tokens = %d, want 125", run.TotalTokens)
			}

			session, err := backend.Queries().GetCodexSession(ctx, sessionID)
			if err != nil {
				t.Fatalf("GetCodexSession() error = %v", err)
			}
			if session.RunID.Int64 != runID {
				t.Fatalf("session run_id = %d, want %d", session.RunID.Int64, runID)
			}
			if session.CompletedAt.String != "2026-05-30T12:05:00Z" {
				t.Fatalf("session completed_at = %q, want 2026-05-30T12:05:00Z", session.CompletedAt.String)
			}
			if session.FinalState.String != "Human Review" {
				t.Fatalf("session final_state = %q, want Human Review", session.FinalState.String)
			}
			if session.Model.String != "gpt-5" {
				t.Fatalf("session model = %q, want gpt-5", session.Model.String)
			}

			spend, err := backend.DailyTokenSpend(ctx, time.Date(2026, 5, 30, 0, 0, 0, 0, time.UTC))
			if err != nil {
				t.Fatalf("DailyTokenSpend() error = %v", err)
			}
			if spend.InputTokens != 100 || spend.OutputTokens != 25 || spend.TotalTokens != 125 || spend.Sessions != 1 {
				t.Fatalf("DailyTokenSpend() = %#v", spend)
			}
			if len(spend.ByModel) != 1 || spend.ByModel[0].Model != "gpt-5" {
				t.Fatalf("DailyTokenSpend().ByModel = %#v", spend.ByModel)
			}

			issueSpend, err := backend.IssueTokenSpend(ctx, IssueIdentity{IssueID: "I_kwDOSskuwc8AAAABD42c3Q"})
			if err != nil {
				t.Fatalf("IssueTokenSpend() error = %v", err)
			}
			if issueSpend.InputTokens != 100 || issueSpend.OutputTokens != 25 || issueSpend.TotalTokens != 125 || issueSpend.Sessions != 1 {
				t.Fatalf("IssueTokenSpend() = %#v", issueSpend)
			}
			if len(issueSpend.ByModel) != 1 || issueSpend.ByModel[0].Model != "gpt-5" {
				t.Fatalf("IssueTokenSpend().ByModel = %#v", issueSpend.ByModel)
			}

			identifierSpend, err := backend.IssueTokenSpend(ctx, IssueIdentity{Identifier: "digitaldrywood/detent#6"})
			if err != nil {
				t.Fatalf("IssueTokenSpend(identifier) error = %v", err)
			}
			if identifierSpend.TotalTokens != 125 {
				t.Fatalf("IssueTokenSpend(identifier).TotalTokens = %d, want 125", identifierSpend.TotalTokens)
			}

			urlSpend, err := backend.IssueTokenSpend(ctx, IssueIdentity{IssueURL: "https://github.com/digitaldrywood/detent/issues/6"})
			if err != nil {
				t.Fatalf("IssueTokenSpend(url) error = %v", err)
			}
			if urlSpend.TotalTokens != 125 {
				t.Fatalf("IssueTokenSpend(url).TotalTokens = %d, want 125", urlSpend.TotalTokens)
			}

			lifetime, err := backend.LifetimeTotals(ctx)
			if err != nil {
				t.Fatalf("LifetimeTotals() error = %v", err)
			}
			if lifetime.InputTokens != 100 || lifetime.OutputTokens != 25 || lifetime.TotalTokens != 125 || lifetime.RuntimeSeconds != 240 {
				t.Fatalf("LifetimeTotals() token/runtime totals = %#v", lifetime)
			}
			if lifetime.Sessions != 1 || lifetime.Runs != 1 {
				t.Fatalf("LifetimeTotals() sessions/runs = %#v, want 1/1", lifetime)
			}
		})
	}
}

func TestBudgetCostEvents(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	backend := openTestStore(t, ctx)

	events := []UsageEvent{
		{
			ProjectID:      "detent",
			Model:          "gpt-5",
			CostUSD:        1.25,
			StartedAt:      time.Date(2026, 6, 1, 5, 0, 0, 0, time.UTC),
			FinishedAt:     time.Date(2026, 6, 1, 5, 1, 0, 0, time.UTC),
			Outcome:        "completed",
			RuntimeSeconds: 60,
		},
		{
			ProjectID:      "pyroapex",
			Model:          "gpt-5",
			CostUSD:        3.5,
			StartedAt:      time.Date(2026, 6, 1, 6, 0, 0, 0, time.UTC),
			FinishedAt:     time.Date(2026, 6, 1, 6, 1, 0, 0, time.UTC),
			Outcome:        "completed",
			RuntimeSeconds: 60,
		},
		{
			ProjectID:      "detent",
			Model:          "gpt-5",
			CostUSD:        2.75,
			StartedAt:      time.Date(2026, 6, 1, 7, 0, 0, 0, time.UTC),
			FinishedAt:     time.Date(2026, 6, 1, 7, 1, 0, 0, time.UTC),
			Outcome:        "completed",
			RuntimeSeconds: 60,
		},
		{
			ProjectID:      "detent",
			Model:          "gpt-5",
			CostUSD:        9,
			StartedAt:      time.Date(2026, 6, 2, 1, 0, 0, 0, time.UTC),
			FinishedAt:     time.Date(2026, 6, 2, 1, 1, 0, 0, time.UTC),
			Outcome:        "completed",
			RuntimeSeconds: 60,
		},
	}

	for _, event := range events {
		if _, err := backend.RecordUsageEvent(ctx, event); err != nil {
			t.Fatalf("RecordUsageEvent() error = %v", err)
		}
	}

	got, err := backend.BudgetCostEvents(ctx, BudgetCostQuery{
		ProjectIDs: []string{"detent"},
		From:       time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		To:         time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("BudgetCostEvents() error = %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("BudgetCostEvents() len = %d, want 2: %#v", len(got), got)
	}
	if got[0].ProjectID != "detent" || got[0].CostUSD != 1.25 || got[1].CostUSD != 2.75 {
		t.Fatalf("BudgetCostEvents() = %#v, want detent costs in time order", got)
	}
}

func TestCycleTimeReportFromCompletedSessions(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	backend := openTestStore(t, ctx)
	base := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)

	seedCycleSession(t, ctx, backend, cycleSessionSeed{
		IssueID:     "issue-215",
		Identifier:  "digitaldrywood/detent#215",
		StartedAt:   base.Add(-time.Hour),
		CompletedAt: base.Add(-30 * time.Minute),
		FinalState:  "failed",
	})
	seedCycleSession(t, ctx, backend, cycleSessionSeed{
		IssueID:     "issue-215",
		Identifier:  "digitaldrywood/detent#215",
		StartedAt:   base,
		CompletedAt: base.Add(90 * time.Minute),
		FinalState:  "completed",
	})
	seedCycleSession(t, ctx, backend, cycleSessionSeed{
		IssueID:     "issue-216",
		Identifier:  "digitaldrywood/detent#216",
		StartedAt:   base.Add(30 * time.Minute),
		CompletedAt: base.Add(2 * time.Hour),
		FinalState:  "failed",
	})
	seedCycleSession(t, ctx, backend, cycleSessionSeed{
		IssueID:     "issue-215",
		Identifier:  "digitaldrywood/detent#215",
		StartedAt:   base.Add(2 * time.Hour),
		CompletedAt: base.Add(3 * time.Hour),
		FinalState:  "completed",
	})
	seedCycleSession(t, ctx, backend, cycleSessionSeed{
		IssueID:     "issue-217",
		Identifier:  "digitaldrywood/detent#217",
		StartedAt:   base.Add(-24 * time.Hour),
		CompletedAt: base.Add(24 * time.Hour),
		FinalState:  "Human Review",
	})

	report, err := backend.CycleTimeReport(ctx)
	if err != nil {
		t.Fatalf("CycleTimeReport() error = %v", err)
	}

	if len(report.Issues) != 2 {
		t.Fatalf("CycleTimeReport().Issues len = %d, want 2: %#v", len(report.Issues), report.Issues)
	}
	if report.Issues[0].Key != "digitaldrywood/detent#217" || report.Issues[0].DurationSeconds != int64(48*time.Hour/time.Second) {
		t.Fatalf("first issue = %#v, want #217 at 48h", report.Issues[0])
	}
	if report.Issues[1].Key != "digitaldrywood/detent#215" || report.Issues[1].DurationSeconds != int64(4*time.Hour/time.Second) || report.Issues[1].Sessions != 3 {
		t.Fatalf("second issue = %#v, want #215 at 4h across 3 sessions", report.Issues[1])
	}
	if report.AverageSeconds != int64((48*time.Hour+4*time.Hour)/2/time.Second) {
		t.Fatalf("AverageSeconds = %d, want 93600", report.AverageSeconds)
	}
	if len(report.Buckets) != 5 || report.Buckets[2].Count != 1 || report.Buckets[4].Count != 1 {
		t.Fatalf("Buckets = %#v, want counts in 4-8h and 1-3d", report.Buckets)
	}
}

func TestCycleTimeSeconds(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		start time.Time
		end   time.Time
		want  int64
		ok    bool
	}{
		{name: "same instant is zero seconds", start: base, end: base, want: 0, ok: true},
		{name: "whole seconds between timestamps", start: base, end: base.Add(90*time.Minute + 12*time.Second), want: 5412, ok: true},
		{name: "missing start is invalid", end: base, ok: false},
		{name: "missing end is invalid", start: base, ok: false},
		{name: "end before start is invalid", start: base, end: base.Add(-time.Second), ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := cycleTimeSeconds(tt.start, tt.end)
			if ok != tt.ok || got != tt.want {
				t.Fatalf("cycleTimeSeconds() = %d, %v; want %d, %v", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestCycleTimeBuckets(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		issues []CycleTimeIssue
		want   []CycleTimeBucket
	}{
		{name: "no durations returns no buckets"},
		{
			name: "assigns fixed lead time ranges and trims trailing empties",
			issues: []CycleTimeIssue{
				{Key: "fast", DurationSeconds: int64(30 * time.Minute / time.Second)},
				{Key: "medium", DurationSeconds: int64(2 * time.Hour / time.Second)},
				{Key: "same range", DurationSeconds: int64(3 * time.Hour / time.Second)},
				{Key: "slow", DurationSeconds: int64(9 * 24 * time.Hour / time.Second)},
			},
			want: []CycleTimeBucket{
				{Label: "<1h", MinSeconds: 0, MaxSeconds: 3600, Count: 1},
				{Label: "1-4h", MinSeconds: 3600, MaxSeconds: 14400, Count: 2},
				{Label: "4-8h", MinSeconds: 14400, MaxSeconds: 28800},
				{Label: "8-24h", MinSeconds: 28800, MaxSeconds: 86400},
				{Label: "1-3d", MinSeconds: 86400, MaxSeconds: 259200},
				{Label: "3-7d", MinSeconds: 259200, MaxSeconds: 604800},
				{Label: "7d+", MinSeconds: 604800, Count: 1},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := cycleTimeBuckets(tt.issues)
			if len(got) != len(tt.want) {
				t.Fatalf("cycleTimeBuckets() len = %d, want %d: %#v", len(got), len(tt.want), got)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("bucket %d = %#v, want %#v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestWorkflowMetricsStoreRoundTripAndAggregates(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	backend := openTestStore(t, ctx)
	base := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)

	events := []WorkflowPhaseEvent{
		{
			ProjectID:         " detent ",
			IssueID:           " issue-722 ",
			Identifier:        " digitaldrywood/detent#722 ",
			IssueURL:          " https://github.com/digitaldrywood/detent/issues/722 ",
			PhaseType:         WorkflowPhaseTypeLane,
			PhaseName:         " In Progress ",
			PreviousPhaseName: "Todo",
			Status:            " exited ",
			StartedAt:         base,
			FinishedAt:        base.Add(10 * time.Minute),
			Reason:            "transition_to:Human Review",
		},
		{
			ProjectID:  "detent",
			IssueID:    "issue-723",
			Identifier: "digitaldrywood/detent#723",
			PhaseType:  WorkflowPhaseTypeLane,
			PhaseName:  "In Progress",
			Status:     "exited",
			StartedAt:  base.Add(time.Hour),
			FinishedAt: base.Add(time.Hour + 20*time.Minute),
		},
		{
			ProjectID:      "detent",
			IssueID:        "issue-722",
			Identifier:     "digitaldrywood/detent#722",
			PhaseType:      WorkflowPhaseTypeAgentSession,
			PhaseName:      "agent_active",
			Status:         "completed",
			StartedAt:      base.Add(time.Minute),
			FinishedAt:     base.Add(9 * time.Minute),
			Turns:          3,
			InputTokens:    1000,
			OutputTokens:   250,
			TotalTokens:    1250,
			MetadataJSON:   `{"session_id":42}`,
			EndpointFamily: "codex",
		},
	}
	for _, event := range events {
		if _, err := backend.RecordWorkflowPhaseEvent(ctx, event); err != nil {
			t.Fatalf("RecordWorkflowPhaseEvent() error = %v", err)
		}
	}

	report, err := backend.WorkflowMetricsReport(ctx, WorkflowMetricsQuery{
		ProjectID: "detent",
		From:      base.Add(-time.Minute),
		To:        base.Add(2 * time.Hour),
	})
	if err != nil {
		t.Fatalf("WorkflowMetricsReport() error = %v", err)
	}

	if len(report.Lanes) != 1 {
		t.Fatalf("WorkflowMetricsReport().Lanes len = %d, want 1: %#v", len(report.Lanes), report.Lanes)
	}
	lane := report.Lanes[0]
	if lane.ProjectID != "detent" || lane.PhaseName != "In Progress" || lane.Count != 2 {
		t.Fatalf("lane metric = %#v, want detent In Progress count 2", lane)
	}
	if lane.AverageSeconds != 900 || lane.P50Seconds != 600 || lane.P90Seconds != 1200 || lane.P95Seconds != 1200 {
		t.Fatalf("lane durations = %#v, want average 900 p50 600 p90/p95 1200", lane)
	}

	if len(report.SubPhases) != 1 {
		t.Fatalf("WorkflowMetricsReport().SubPhases len = %d, want 1: %#v", len(report.SubPhases), report.SubPhases)
	}
	subphase := report.SubPhases[0]
	if subphase.PhaseType != string(WorkflowPhaseTypeAgentSession) || subphase.PhaseName != "agent_active" || subphase.TotalSeconds != 480 {
		t.Fatalf("subphase metric = %#v, want 480s agent_active", subphase)
	}

	timeline, err := backend.IssueWorkflowTimeline(ctx, IssueIdentity{Identifier: "digitaldrywood/detent#722"})
	if err != nil {
		t.Fatalf("IssueWorkflowTimeline() error = %v", err)
	}
	if len(timeline.Events) != 2 {
		t.Fatalf("IssueWorkflowTimeline().Events len = %d, want 2: %#v", len(timeline.Events), timeline.Events)
	}
	if timeline.Events[0].ProjectID != "detent" || timeline.Events[0].PhaseName != "In Progress" || timeline.Events[0].DurationSeconds != 600 {
		t.Fatalf("timeline first event = %#v, want normalized In Progress lane", timeline.Events[0])
	}
	if timeline.Events[1].Turns != 3 || timeline.Events[1].TotalTokens != 1250 || timeline.Events[1].MetadataJSON != `{"session_id":42}` {
		t.Fatalf("timeline agent event = %#v, want turns/tokens/metadata", timeline.Events[1])
	}
}

func TestFairShareStoreRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	backend := openTestStore(t, ctx)
	dispatchedAt := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)

	if err := backend.RecordFairShareDispatch(ctx, FairShareDispatch{
		ProjectID:      " alpha ",
		Weight:         2,
		RuntimeSeconds: 30,
		DispatchedAt:   dispatchedAt,
	}); err != nil {
		t.Fatalf("RecordFairShareDispatch() first error = %v", err)
	}
	if err := backend.RecordFairShareDispatch(ctx, FairShareDispatch{
		ProjectID:      "alpha",
		Weight:         2,
		RuntimeSeconds: 45,
		DispatchedAt:   dispatchedAt.Add(time.Minute),
	}); err != nil {
		t.Fatalf("RecordFairShareDispatch() second error = %v", err)
	}

	usage, err := backend.ListFairShareUsage(ctx)
	if err != nil {
		t.Fatalf("ListFairShareUsage() error = %v", err)
	}
	if len(usage) != 1 {
		t.Fatalf("usage len = %d, want 1: %#v", len(usage), usage)
	}

	got := usage[0]
	if got.ProjectID != "alpha" {
		t.Fatalf("ProjectID = %q, want alpha", got.ProjectID)
	}
	if got.Weight != 2 {
		t.Fatalf("Weight = %d, want 2", got.Weight)
	}
	if got.Dispatches != 2 {
		t.Fatalf("Dispatches = %d, want 2", got.Dispatches)
	}
	if got.RuntimeSeconds != 75 {
		t.Fatalf("RuntimeSeconds = %d, want 75", got.RuntimeSeconds)
	}
	if !got.UpdatedAt.Equal(dispatchedAt.Add(time.Minute)) {
		t.Fatalf("UpdatedAt = %s, want %s", got.UpdatedAt, dispatchedAt.Add(time.Minute))
	}
}

func TestUsageLedgerRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		event UsageEvent
	}{
		{
			name: "persists usage event across reopen",
			event: UsageEvent{
				ProjectID:      " detent ",
				RunID:          11,
				SessionID:      42,
				IssueID:        " I_kwDOSskuwc8AAAABD6psJQ ",
				Identifier:     " digitaldrywood/detent#117 ",
				PRNumber:       new(int64(91)),
				Model:          " gpt-5-codex ",
				InputTokens:    123,
				OutputTokens:   45,
				TotalTokens:    168,
				CostUSD:        0.00123,
				RuntimeSeconds: 73,
				StartedAt:      time.Date(2026, 5, 31, 13, 0, 0, 0, time.UTC),
				FinishedAt:     time.Date(2026, 5, 31, 13, 1, 13, 0, time.UTC),
				Outcome:        " completed ",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			dbPath := filepath.Join(t.TempDir(), "detent.db")

			backend, err := Open(ctx, Config{
				Backend: BackendSQLite,
				Path:    dbPath,
			})
			if err != nil {
				t.Fatalf("Open() error = %v", err)
			}

			eventID, err := backend.RecordUsageEvent(ctx, tt.event)
			if err != nil {
				t.Fatalf("RecordUsageEvent() error = %v", err)
			}

			got, err := backend.Queries().GetUsageEvent(ctx, eventID)
			if err != nil {
				t.Fatalf("GetUsageEvent() error = %v", err)
			}
			if got.ProjectID != "detent" {
				t.Fatalf("ProjectID = %q, want detent", got.ProjectID)
			}
			if got.RunID.Int64 != 11 || got.SessionID.Int64 != 42 {
				t.Fatalf("run/session = %d/%d, want 11/42", got.RunID.Int64, got.SessionID.Int64)
			}
			if got.IssueID.String != "I_kwDOSskuwc8AAAABD6psJQ" || got.Identifier.String != "digitaldrywood/detent#117" {
				t.Fatalf("issue identity = %q/%q", got.IssueID.String, got.Identifier.String)
			}
			if got.PrNumber.Int64 != 91 {
				t.Fatalf("pr_number = %d, want 91", got.PrNumber.Int64)
			}
			if got.Model != "gpt-5-codex" {
				t.Fatalf("model = %q, want gpt-5-codex", got.Model)
			}
			if got.InputTokens != 123 || got.OutputTokens != 45 || got.TotalTokens != 168 || got.RuntimeSeconds != 73 {
				t.Fatalf("tokens/runtime = %d/%d/%d/%d", got.InputTokens, got.OutputTokens, got.TotalTokens, got.RuntimeSeconds)
			}
			if got.CostUsd != 0.00123 {
				t.Fatalf("cost_usd = %.12f, want 0.001230000000", got.CostUsd)
			}
			if got.StartedAt != "2026-05-31T13:00:00Z" || got.FinishedAt != "2026-05-31T13:01:13Z" {
				t.Fatalf("timestamps = %q/%q", got.StartedAt, got.FinishedAt)
			}
			if got.EventDay != "2026-05-31" {
				t.Fatalf("event_day = %q, want 2026-05-31", got.EventDay)
			}
			if got.Outcome != "completed" {
				t.Fatalf("outcome = %q, want completed", got.Outcome)
			}

			if err := backend.Close(); err != nil {
				t.Fatalf("Close() error = %v", err)
			}

			reopened, err := Open(ctx, Config{
				Backend: BackendSQLite,
				Path:    dbPath,
			})
			if err != nil {
				t.Fatalf("reopen Open() error = %v", err)
			}
			t.Cleanup(func() {
				if err := reopened.Close(); err != nil {
					t.Fatalf("reopened Close() error = %v", err)
				}
			})

			persisted, err := reopened.Queries().GetUsageEvent(ctx, eventID)
			if err != nil {
				t.Fatalf("GetUsageEvent() after reopen error = %v", err)
			}
			if persisted.TotalTokens != 168 {
				t.Fatalf("persisted total_tokens = %d, want 168", persisted.TotalTokens)
			}
			if persisted.CostUsd != 0.00123 {
				t.Fatalf("persisted cost_usd = %.12f, want 0.001230000000", persisted.CostUsd)
			}
		})
	}
}

func TestUsageReportAggregates(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	backend := openTestStore(t, ctx)
	seedUsageReportEvents(t, ctx, backend)

	tests := []struct {
		name  string
		query UsageReportQuery
		want  []UsageReportRow
	}{
		{
			name:  "by day with inclusive range",
			query: UsageReportQuery{By: UsageReportByDay, From: dateOnly(2026, 5, 31), To: dateOnly(2026, 6, 1)},
			want: []UsageReportRow{
				{
					Key:            "2026-05-31",
					InputTokens:    150,
					OutputTokens:   75,
					TotalTokens:    225,
					RuntimeSeconds: 45,
					Events:         2,
				},
				{
					Key:            "2026-06-01",
					InputTokens:    70,
					OutputTokens:   30,
					TotalTokens:    100,
					RuntimeSeconds: 25,
					Events:         1,
				},
			},
		},
		{
			name:  "by project",
			query: UsageReportQuery{By: UsageReportByProject, From: dateOnly(2026, 5, 31), To: dateOnly(2026, 6, 1)},
			want: []UsageReportRow{
				{
					Key:            "detent",
					InputTokens:    220,
					OutputTokens:   105,
					TotalTokens:    325,
					RuntimeSeconds: 70,
					Events:         3,
				},
			},
		},
		{
			name:  "by issue",
			query: UsageReportQuery{By: UsageReportByIssue},
			want: []UsageReportRow{
				{
					Key:            "digitaldrywood/detent#117",
					InputTokens:    100,
					OutputTokens:   50,
					TotalTokens:    150,
					RuntimeSeconds: 30,
					Events:         1,
				},
				{
					Key:            "digitaldrywood/detent#119",
					InputTokens:    120,
					OutputTokens:   55,
					TotalTokens:    175,
					RuntimeSeconds: 40,
					Events:         2,
				},
				{
					Key:            "unassigned",
					InputTokens:    5,
					OutputTokens:   2,
					TotalTokens:    7,
					RuntimeSeconds: 3,
					Events:         1,
				},
			},
		},
		{
			name:  "by PR",
			query: UsageReportQuery{By: UsageReportByPR},
			want: []UsageReportRow{
				{
					Key:            "detent#133",
					InputTokens:    100,
					OutputTokens:   50,
					TotalTokens:    150,
					RuntimeSeconds: 30,
					Events:         1,
				},
				{
					Key:            "detent#141",
					InputTokens:    120,
					OutputTokens:   55,
					TotalTokens:    175,
					RuntimeSeconds: 40,
					Events:         2,
				},
				{
					Key:            "pyroapex#141",
					InputTokens:    5,
					OutputTokens:   2,
					TotalTokens:    7,
					RuntimeSeconds: 3,
					Events:         1,
				},
			},
		},
		{
			name:  "by model",
			query: UsageReportQuery{By: UsageReportByModel, From: dateOnly(2026, 5, 31), To: dateOnly(2026, 6, 1)},
			want: []UsageReportRow{
				{
					Key:            "gpt-5.4",
					InputTokens:    150,
					OutputTokens:   75,
					TotalTokens:    225,
					RuntimeSeconds: 45,
					Events:         2,
				},
				{
					Key:            "gpt-5.4-mini",
					InputTokens:    70,
					OutputTokens:   30,
					TotalTokens:    100,
					RuntimeSeconds: 25,
					Events:         1,
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			report, err := backend.UsageReport(ctx, tt.query)
			if err != nil {
				t.Fatalf("UsageReport() error = %v", err)
			}
			assertUsageRows(t, report.Rows, tt.want)
		})
	}
}

func TestUsageReportRejectsInvalidRange(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	backend := openTestStore(t, ctx)

	_, err := backend.UsageReport(ctx, UsageReportQuery{
		By:   UsageReportByDay,
		From: dateOnly(2026, 6, 2),
		To:   dateOnly(2026, 6, 1),
	})
	if err == nil {
		t.Fatal("UsageReport() error = nil, want invalid date range")
	}
}

func TestOpenRejectsUnsupportedBackend(t *testing.T) {
	t.Parallel()

	_, err := Open(context.Background(), Config{
		Backend: Backend("postgres"),
		Path:    filepath.Join(t.TempDir(), "detent.db"),
	})
	if err == nil {
		t.Fatal("Open() error = nil, want unsupported backend error")
	}
}

func TestOpenUsesSQLiteBackendByDefault(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	backend, err := Open(ctx, Config{
		Path:        filepath.Join(t.TempDir(), "detent.db"),
		BusyTimeout: 2500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})

	sqliteBackend, ok := backend.(*sqliteStore)
	if !ok {
		t.Fatalf("Open() returned %T, want *sqliteStore", backend)
	}
	if got := queryInt(t, sqliteBackend.db, "PRAGMA busy_timeout"); got != 2500 {
		t.Fatalf("busy_timeout = %d, want 2500", got)
	}
}

func TestOpenSQLiteRejectsMissingPath(t *testing.T) {
	t.Parallel()

	_, err := Open(context.Background(), Config{
		Backend: BackendSQLite,
	})
	if err == nil {
		t.Fatal("Open() error = nil, want missing path error")
	}
}

func TestBusyTimeoutMillis(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		timeout time.Duration
		want    int64
	}{
		{name: "default for zero", timeout: 0, want: 5000},
		{name: "default for negative", timeout: -time.Second, want: 5000},
		{name: "minimum positive", timeout: time.Nanosecond, want: 1},
		{name: "configured duration", timeout: 3 * time.Second, want: 3000},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := busyTimeoutMillis(tt.timeout); got != tt.want {
				t.Fatalf("busyTimeoutMillis(%s) = %d, want %d", tt.timeout, got, tt.want)
			}
		})
	}
}

func openTestStore(t *testing.T, ctx context.Context) Store {
	t.Helper()

	backend, err := Open(ctx, Config{
		Backend: BackendSQLite,
		Path:    filepath.Join(t.TempDir(), "detent.db"),
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})
	return backend
}

type cycleSessionSeed struct {
	IssueID     string
	Identifier  string
	StartedAt   time.Time
	CompletedAt time.Time
	FinalState  string
}

func seedCycleSession(t *testing.T, ctx context.Context, backend Store, seed cycleSessionSeed) {
	t.Helper()

	sessionID, err := backend.StartSession(ctx, SessionStart{
		IssueID:    seed.IssueID,
		Identifier: seed.Identifier,
		StartedAt:  seed.StartedAt,
		Model:      "gpt-5-codex",
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}
	if err := backend.FinishSession(ctx, sessionID, SessionFinish{
		CompletedAt:    seed.CompletedAt,
		RuntimeSeconds: int64(seed.CompletedAt.Sub(seed.StartedAt) / time.Second),
		FinalState:     seed.FinalState,
		Model:          "gpt-5-codex",
	}); err != nil {
		t.Fatalf("FinishSession() error = %v", err)
	}
}

func seedUsageReportEvents(t *testing.T, ctx context.Context, backend Store) {
	t.Helper()

	events := []UsageEvent{
		{
			ProjectID:      "detent",
			IssueID:        "issue-117",
			Identifier:     "digitaldrywood/detent#117",
			PRNumber:       new(int64(133)),
			Model:          "gpt-5.4",
			InputTokens:    100,
			OutputTokens:   50,
			TotalTokens:    150,
			RuntimeSeconds: 30,
			StartedAt:      time.Date(2026, 5, 31, 9, 0, 0, 0, time.UTC),
			FinishedAt:     time.Date(2026, 5, 31, 9, 1, 0, 0, time.UTC),
			Outcome:        "completed",
		},
		{
			ProjectID:      "detent",
			IssueID:        "issue-119",
			Identifier:     "digitaldrywood/detent#119",
			PRNumber:       new(int64(141)),
			Model:          "gpt-5.4",
			InputTokens:    50,
			OutputTokens:   25,
			TotalTokens:    75,
			RuntimeSeconds: 15,
			StartedAt:      time.Date(2026, 5, 31, 10, 0, 0, 0, time.UTC),
			FinishedAt:     time.Date(2026, 5, 31, 10, 1, 0, 0, time.UTC),
			Outcome:        "completed",
		},
		{
			ProjectID:      "detent",
			IssueID:        "issue-119",
			Identifier:     "digitaldrywood/detent#119",
			PRNumber:       new(int64(141)),
			Model:          "gpt-5.4-mini",
			InputTokens:    70,
			OutputTokens:   30,
			TotalTokens:    100,
			RuntimeSeconds: 25,
			StartedAt:      time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC),
			FinishedAt:     time.Date(2026, 6, 1, 11, 1, 0, 0, time.UTC),
			Outcome:        "completed",
		},
		{
			ProjectID:      "pyroapex",
			PRNumber:       new(int64(141)),
			Model:          "",
			InputTokens:    5,
			OutputTokens:   2,
			TotalTokens:    7,
			RuntimeSeconds: 3,
			StartedAt:      time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC),
			FinishedAt:     time.Date(2026, 6, 2, 12, 1, 0, 0, time.UTC),
			Outcome:        "completed",
		},
	}

	for _, event := range events {
		if _, err := backend.RecordUsageEvent(ctx, event); err != nil {
			t.Fatalf("RecordUsageEvent() error = %v", err)
		}
	}
}

func assertUsageRows(t *testing.T, got []UsageReportRow, want []UsageReportRow) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("rows len = %d, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Key != want[i].Key ||
			got[i].InputTokens != want[i].InputTokens ||
			got[i].OutputTokens != want[i].OutputTokens ||
			got[i].TotalTokens != want[i].TotalTokens ||
			got[i].RuntimeSeconds != want[i].RuntimeSeconds ||
			got[i].Events != want[i].Events {
			t.Fatalf("row %d = %#v, want %#v", i, got[i], want[i])
		}
	}
}

func dateOnly(year int, month time.Month, day int) time.Time {
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}

func queryString(t *testing.T, db *sql.DB, query string) string {
	t.Helper()

	var value string
	if err := db.QueryRow(query).Scan(&value); err != nil {
		t.Fatalf("querying %q: %v", query, err)
	}
	return value
}

func queryInt(t *testing.T, db *sql.DB, query string) int64 {
	t.Helper()

	var value int64
	if err := db.QueryRow(query).Scan(&value); err != nil {
		t.Fatalf("querying %q: %v", query, err)
	}
	return value
}
