package templates

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestThroughputRateFormatsRollingTokenTPS(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		snapshot telemetry.Snapshot
		want     string
	}{
		{
			name: "empty throughput",
			want: "0 tps",
		},
		{
			name: "integer tps",
			snapshot: telemetry.Snapshot{
				Throughput: telemetry.TokenThroughput{TokensPerSecond: 42},
			},
			want: "42 tps",
		},
		{
			name: "decimal tps",
			snapshot: telemetry.Snapshot{
				Throughput: telemetry.TokenThroughput{TokensPerSecond: 7.25},
			},
			want: "7.3 tps",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := throughputRate(tt.snapshot)
			if got != tt.want {
				t.Fatalf("throughputRate() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRuntimeStatusReflectsDraining(t *testing.T) {
	t.Parallel()

	snapshot := telemetry.Snapshot{
		GeneratedAt: time.Date(2026, 6, 12, 15, 0, 0, 0, time.UTC),
		Shutdown: telemetry.Shutdown{
			Status:            "draining",
			Draining:          true,
			SessionsRemaining: 2,
		},
	}

	if got := runtimeStatusLabel(snapshot); got != "Draining" {
		t.Fatalf("runtimeStatusLabel() = %q, want Draining", got)
	}
	if got := runtimeStatusClass(snapshot); got != "border-warning-soft bg-warning-soft text-warning" {
		t.Fatalf("runtimeStatusClass() = %q, want warning class", got)
	}
}

func TestThroughputTrendPoints(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 31, 15, 0, 30, 0, time.UTC)
	points := throughputTrendPoints(telemetry.Snapshot{
		TokenTrend: []telemetry.TokenTrendPoint{
			{At: now.Add(-8 * time.Minute), Total: 120},
			{At: now.Add(-2 * time.Minute), Total: 480},
			{At: now.Add(-30 * time.Second), Total: 570},
			{At: now.Add(10 * time.Second), Total: 690},
		},
	})

	if len(points) != 3 {
		t.Fatalf("throughputTrendPoints() len = %d, want 3", len(points))
	}

	wantValues := map[string]float64{
		"14:58:30": 1,
		"15:00":    1,
		"15:00:40": 3,
	}
	for _, point := range points {
		want := wantValues[point.Label]
		if point.Value != want {
			t.Fatalf("point %s = %v, want %v; points = %#v", point.Label, point.Value, want, points)
		}
	}
}

func TestCycleTimeHistogramChart(t *testing.T) {
	t.Parallel()

	report := telemetry.CycleTimeReport{
		Available: true,
		Buckets: []telemetry.CycleTimeBucket{
			{Label: "<1h", Count: 1},
			{Label: "1-4h", Count: 2},
		},
	}

	chart := cycleTimeHistogramChart(report)
	if chart.Title != "Cycle time histogram" || chart.AriaLabel != "Cycle time histogram" {
		t.Fatalf("chart titles = %q/%q", chart.Title, chart.AriaLabel)
	}
	if len(chart.Bars) != 2 {
		t.Fatalf("chart bars len = %d, want 2: %#v", len(chart.Bars), chart.Bars)
	}
	if chart.Bars[1].Label != "1-4h" || chart.Bars[1].Value != 2 {
		t.Fatalf("second bar = %#v, want 1-4h count 2", chart.Bars[1])
	}
	if chart.ValueSuffix != "issues" {
		t.Fatalf("ValueSuffix = %q, want issues", chart.ValueSuffix)
	}
}

func TestCycleTimeSummaryLabels(t *testing.T) {
	t.Parallel()

	report := telemetry.CycleTimeReport{
		Available:      true,
		AverageSeconds: int64(90 * time.Minute / time.Second),
		Issues: []telemetry.CycleTimeIssue{
			{Key: "digitaldrywood/detent#215"},
			{Key: "digitaldrywood/detent#216"},
		},
	}

	if got := cycleTimeAverageLabel(report); got != "1h 30m" {
		t.Fatalf("cycleTimeAverageLabel() = %q, want 1h 30m", got)
	}
	if got := cycleTimeCountLabel(report); got != "2 completed" {
		t.Fatalf("cycleTimeCountLabel() = %q, want 2 completed", got)
	}
}

func TestProjectSmallMultipleCards(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 1, 15, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		projects  []ProjectSmallMultiple
		wantOrder []string
		wantFirst projectSmallMultipleCard
	}{
		{
			name: "sorts by activity and builds compact charts",
			projects: []ProjectSmallMultiple{
				{
					ID:         "quiet",
					Name:       "Quiet",
					QueueCount: 1,
					Samples: []ProjectSmallMultipleSample{
						{At: now.Add(-time.Minute), ThroughputTokensPerSecond: 0.5, SpendUSD: 1, QueueDepth: 1},
					},
				},
				{
					ID:                        "busy",
					Name:                      "Busy",
					URL:                       "https://github.com/digitaldrywood/detent",
					Running:                   2,
					QueueCount:                3,
					Blocked:                   1,
					Completed:                 4,
					ThroughputTokensPerSecond: 7.25,
					CurrentSpendUSD:           12.5,
					Samples: []ProjectSmallMultipleSample{
						{At: now.Add(-2 * time.Minute), ThroughputTokensPerSecond: 3.5, SpendUSD: 8, QueueDepth: 2},
						{At: now.Add(-time.Minute), ThroughputTokensPerSecond: 7.25, SpendUSD: 12.5, QueueDepth: 3},
					},
				},
				{
					ID:         "queued",
					Name:       "Queued",
					QueueCount: 4,
					Samples: []ProjectSmallMultipleSample{
						{At: now.Add(-time.Minute), ThroughputTokensPerSecond: 1, SpendUSD: 2, QueueDepth: 4},
					},
				},
			},
			wantOrder: []string{"busy", "queued", "quiet"},
			wantFirst: projectSmallMultipleCard{
				ID:              "busy",
				Name:            "Busy",
				Href:            "/projects/busy",
				ExternalURL:     "https://github.com/digitaldrywood/detent",
				ActivityLabel:   "2 running / 3 queued / 1 blocked",
				ThroughputLabel: "7.3 tps",
				SpendLabel:      "$12.50",
				QueueLabel:      "3 queued",
			},
		},
		{
			name: "uses project id when display name is empty",
			projects: []ProjectSmallMultiple{
				{
					ID:              "detent",
					Running:         1,
					Samples:         []ProjectSmallMultipleSample{{At: now, QueueDepth: 0}},
					Completed:       1,
					QueueCount:      0,
					Blocked:         0,
					CurrentSpendUSD: 0,
				},
			},
			wantOrder: []string{"detent"},
			wantFirst: projectSmallMultipleCard{
				ID:              "detent",
				Name:            "detent",
				Href:            "/projects/detent",
				ActivityLabel:   "1 running / 0 queued / 0 blocked",
				ThroughputLabel: "0 tps",
				SpendLabel:      "$0.00",
				QueueLabel:      "0 queued",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := projectSmallMultipleCards(DashboardData{Projects: tt.projects})
			if len(got) != len(tt.wantOrder) {
				t.Fatalf("projectSmallMultipleCards() len = %d, want %d", len(got), len(tt.wantOrder))
			}
			for i, wantID := range tt.wantOrder {
				if got[i].ID != wantID {
					t.Fatalf("card %d ID = %q, want %q; cards = %#v", i, got[i].ID, wantID, got)
				}
			}

			first := got[0]
			if first.ID != tt.wantFirst.ID ||
				first.Name != tt.wantFirst.Name ||
				first.Href != tt.wantFirst.Href ||
				first.ExternalURL != tt.wantFirst.ExternalURL ||
				first.ActivityLabel != tt.wantFirst.ActivityLabel ||
				first.ThroughputLabel != tt.wantFirst.ThroughputLabel ||
				first.SpendLabel != tt.wantFirst.SpendLabel ||
				first.QueueLabel != tt.wantFirst.QueueLabel {
				t.Fatalf("first card = %#v, want %#v", first, tt.wantFirst)
			}
			if first.ThroughputChart.Title != "Busy throughput" && tt.wantFirst.ID == "busy" {
				t.Fatalf("ThroughputChart.Title = %q, want Busy throughput", first.ThroughputChart.Title)
			}
			if len(first.ThroughputChart.Points) == 0 || len(first.SpendChart.Points) == 0 || len(first.QueueChart.Points) == 0 {
				t.Fatalf("charts must include sparkline points: %#v", first)
			}
		})
	}
}

func TestSidebarProjectItemsUseAttentionFirstDefaultOrder(t *testing.T) {
	t.Parallel()

	got := sidebarProjectItems(DashboardShellData{Projects: []ProjectSmallMultiple{
		{ID: "paused", Name: "Paused", Paused: true},
		{ID: "idle", Name: "Idle"},
		{ID: "queued", Name: "Queued", QueueCount: 1},
		{ID: "active", Name: "Active", Running: 1},
		{ID: "blocked", Name: "Blocked", Blocked: 1},
	}})
	want := []string{"blocked", "active", "queued", "idle", "paused"}

	if len(got) != len(want) {
		t.Fatalf("sidebarProjectItems() len = %d, want %d; got %#v", len(got), len(want), got)
	}
	for i, wantID := range want {
		if got[i].ID != wantID {
			t.Fatalf("sidebarProjectItems()[%d].ID = %q, want %q; got %#v", i, got[i].ID, wantID, got)
		}
		if got[i].DefaultIndex != i {
			t.Fatalf("sidebarProjectItems()[%d].DefaultIndex = %d, want %d; got %#v", i, got[i].DefaultIndex, i, got)
		}
	}
}

func TestProjectSmallMultiplesGridClass(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		cards []projectSmallMultipleCard
		want  string
	}{
		{name: "single card", cards: []projectSmallMultipleCard{{ID: "detent"}}, want: "mt-4 grid min-w-0 gap-2"},
		{name: "multiple cards", cards: []projectSmallMultipleCard{{ID: "detent"}, {ID: "pyroapex"}}, want: "mt-4 grid min-w-0 gap-2"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := projectSmallMultiplesGridClass(tt.cards); got != tt.want {
				t.Fatalf("projectSmallMultiplesGridClass() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBudgetProjectedSpendUSD(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)

	tests := []struct {
		name    string
		now     time.Time
		current float64
		want    float64
	}{
		{
			name:    "projects current run rate to period end",
			now:     start.Add(6 * time.Hour),
			current: 12,
			want:    48,
		},
		{
			name:    "keeps current spend at period start",
			now:     start,
			current: 12,
			want:    12,
		},
		{
			name:    "does not project below current after period end",
			now:     end.Add(6 * time.Hour),
			current: 30,
			want:    30,
		},
		{
			name:    "zero spend stays zero",
			now:     start.Add(6 * time.Hour),
			current: 0,
			want:    0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := budgetProjectedSpendUSD(start, end, tt.now, tt.current)
			if got != tt.want {
				t.Fatalf("budgetProjectedSpendUSD() = %.2f, want %.2f", got, tt.want)
			}
		})
	}
}

func TestBudgetBurnDownView(t *testing.T) {
	t.Parallel()

	perDay := 100.0
	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	now := start.Add(12 * time.Hour)
	end := start.Add(24 * time.Hour)

	tests := []struct {
		name           string
		snapshot       telemetry.Snapshot
		wantAvailable  bool
		wantCurrent    string
		wantCap        string
		wantProjection string
		wantPoints     int
	}{
		{
			name: "disabled budget returns empty state",
			snapshot: telemetry.Snapshot{
				GeneratedAt: now,
			},
			wantAvailable: false,
		},
		{
			name: "enabled budget projects spend and builds chart points",
			snapshot: telemetry.Snapshot{
				GeneratedAt: now,
				Budget: telemetry.Budget{
					Enabled:         true,
					PerDayMaxUSD:    &perDay,
					CurrentSpendUSD: 25,
					PeriodStart:     start,
					PeriodEnd:       end,
					SpendPoints: []telemetry.BudgetSpendPoint{
						{At: start.Add(6 * time.Hour), SpendUSD: 10},
						{At: now, SpendUSD: 25},
					},
				},
			},
			wantAvailable:  true,
			wantCurrent:    "$25.00",
			wantCap:        "$100.00",
			wantProjection: "$50.00",
			wantPoints:     3,
		},
		{
			name: "current spend appends latest point when store samples lag",
			snapshot: telemetry.Snapshot{
				GeneratedAt: now,
				Budget: telemetry.Budget{
					Enabled:         true,
					PerDayMaxUSD:    &perDay,
					CurrentSpendUSD: 25,
					PeriodStart:     start,
					PeriodEnd:       end,
					SpendPoints: []telemetry.BudgetSpendPoint{
						{At: start.Add(6 * time.Hour), SpendUSD: 10},
					},
				},
			},
			wantAvailable:  true,
			wantCurrent:    "$25.00",
			wantCap:        "$100.00",
			wantProjection: "$50.00",
			wantPoints:     3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := budgetBurnDownView(tt.snapshot)
			if got.Available != tt.wantAvailable {
				t.Fatalf("Available = %v, want %v", got.Available, tt.wantAvailable)
			}
			if !tt.wantAvailable {
				return
			}
			if got.CurrentLabel != tt.wantCurrent {
				t.Fatalf("CurrentLabel = %q, want %q", got.CurrentLabel, tt.wantCurrent)
			}
			if got.CapLabel != tt.wantCap {
				t.Fatalf("CapLabel = %q, want %q", got.CapLabel, tt.wantCap)
			}
			if got.ProjectionLabel != tt.wantProjection {
				t.Fatalf("ProjectionLabel = %q, want %q", got.ProjectionLabel, tt.wantProjection)
			}
			if len(got.Chart.ActualPoints) != tt.wantPoints {
				t.Fatalf("ActualPoints len = %d, want %d", len(got.Chart.ActualPoints), tt.wantPoints)
			}
			if len(got.Chart.ProjectionPoints) != 2 {
				t.Fatalf("ProjectionPoints len = %d, want 2", len(got.Chart.ProjectionPoints))
			}
		})
	}
}

func TestRunningActivityRowsUseRecentEventsNewestFirst(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 31, 15, 0, 5, 0, time.UTC)
	row := telemetry.Running{
		RecentEvents: []telemetry.ActivityEvent{
			{At: now.Add(-5 * time.Second), Event: "process_started", Message: "process 4242 started"},
			{At: now.Add(-4 * time.Second), Event: "turn_started", Message: "turn started"},
			{At: now.Add(-3 * time.Second), Event: "agent_message_delta", Message: "editing dashboard"},
			{At: now.Add(-2 * time.Second), Event: "token_usage", Message: "tokens updated"},
			{At: now.Add(-time.Second), Event: "rate_limits", Message: "rate snapshot"},
			{At: now, Event: "turn_completed", Message: "turn completed"},
		},
	}

	rows := runningActivityRows(row)
	if len(rows) != 5 {
		t.Fatalf("runningActivityRows() len = %d, want 5", len(rows))
	}

	want := []struct {
		event   string
		message string
		at      string
	}{
		{event: "turn_completed", message: "turn completed", at: "15:00:05 UTC"},
		{event: "rate_limits", message: "rate snapshot", at: "15:00:04 UTC"},
		{event: "token_usage", message: "tokens updated", at: "15:00:03 UTC"},
		{event: "agent_message_delta", message: "editing dashboard", at: "15:00:02 UTC"},
		{event: "turn_started", message: "turn started", at: "15:00:01 UTC"},
	}
	for i, wantRow := range want {
		if rows[i].Event != wantRow.event || rows[i].Message != wantRow.message || rows[i].At != wantRow.at {
			t.Fatalf("row %d = %#v, want %#v", i, rows[i], wantRow)
		}
	}
}

func TestRunningActivityRowsFallBackToLatestEvent(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 5, 31, 15, 3, 4, 0, time.UTC)
	rows := runningActivityRows(telemetry.Running{
		LastEventAt: &at,
		LastEvent:   "agent_message_delta",
		LastMessage: "working through review feedback",
	})

	if len(rows) != 1 {
		t.Fatalf("runningActivityRows() len = %d, want 1", len(rows))
	}
	if rows[0].At != "15:03:04 UTC" || rows[0].Event != "agent_message_delta" || rows[0].Message != "working through review feedback" {
		t.Fatalf("runningActivityRows()[0] = %#v", rows[0])
	}
}

func TestAgentTimelineRows(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 31, 15, 0, 0, 0, time.UTC)
	snapshot := telemetry.Snapshot{
		GeneratedAt: now,
		Running: []telemetry.Running{
			{
				Issue: telemetry.Issue{
					ID:         "issue-2",
					Identifier: "DD-2",
					Title:      "Second running issue",
					State:      "Merging",
				},
				StartedAt: now.Add(-4 * time.Minute),
			},
			{
				Issue: telemetry.Issue{
					ID:         "issue-1",
					Identifier: "DD-1",
					Title:      "First running issue",
					State:      "In Progress",
				},
				StartedAt: now.Add(-8 * time.Minute),
			},
		},
		Completed: []telemetry.Completed{
			{
				Issue: telemetry.Issue{
					ID:         "issue-3",
					Identifier: "DD-3",
					Title:      "Completed issue",
				},
				StartedAt:   now.Add(-10 * time.Minute),
				CompletedAt: now.Add(-2 * time.Minute),
				FinalState:  "completed",
			},
			{
				Issue: telemetry.Issue{
					ID:         "issue-4",
					Identifier: "DD-4",
				},
				StartedAt:   now.Add(-12 * time.Minute),
				CompletedAt: now.Add(-11 * time.Minute),
				FinalState:  "failed",
			},
		},
	}

	rows := agentTimelineRows(snapshot)
	if len(rows) != 4 {
		t.Fatalf("agentTimelineRows() len = %d, want 4", len(rows))
	}

	wantOrder := []string{"DD-4", "DD-3", "DD-1", "DD-2"}
	for i, want := range wantOrder {
		if rows[i].Identifier != want {
			t.Fatalf("row %d identifier = %q, want %q; rows = %#v", i, rows[i].Identifier, want, rows)
		}
	}

	tests := []struct {
		name      string
		row       agentTimelineRow
		wantState string
		wantStart string
		wantEnd   string
		wantWidth string
		wantClass string
	}{
		{
			name:      "failed completed row",
			row:       rows[0],
			wantState: "failed",
			wantStart: "0.00%",
			wantEnd:   "8.33%",
			wantWidth: "8.33%",
			wantClass: "bg-danger",
		},
		{
			name:      "completed row",
			row:       rows[1],
			wantState: "completed",
			wantStart: "16.67%",
			wantEnd:   "83.33%",
			wantWidth: "66.67%",
			wantClass: "bg-success",
		},
		{
			name:      "running row",
			row:       rows[3],
			wantState: "Merging",
			wantStart: "66.67%",
			wantEnd:   "100.00%",
			wantWidth: "33.33%",
			wantClass: "bg-accent",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if tt.row.State != tt.wantState {
				t.Fatalf("State = %q, want %q", tt.row.State, tt.wantState)
			}
			if tt.row.StartPercent != tt.wantStart {
				t.Fatalf("StartPercent = %q, want %q", tt.row.StartPercent, tt.wantStart)
			}
			if tt.row.EndPercent != tt.wantEnd {
				t.Fatalf("EndPercent = %q, want %q", tt.row.EndPercent, tt.wantEnd)
			}
			if len(tt.row.Segments) != 1 {
				t.Fatalf("Segments len = %d, want 1", len(tt.row.Segments))
			}
			segment := tt.row.Segments[0]
			if segment.Width != tt.wantWidth {
				t.Fatalf("segment.Width = %q, want %q", segment.Width, tt.wantWidth)
			}
			if segment.Class != tt.wantClass {
				t.Fatalf("segment.Class = %q, want %q", segment.Class, tt.wantClass)
			}
		})
	}
}

func TestProjectKanbanBoardGroupsSnapshotRowsByConfiguredStates(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 1, 15, 0, 0, 0, time.UTC)
	todoAt := now.Add(-6 * time.Minute)
	runningAt := now.Add(-5 * time.Minute)
	blockedAt := now.Add(-4 * time.Minute)
	reviewAt := now.Add(-3 * time.Minute)
	doneAt := now.Add(-2 * time.Minute)

	board := projectKanbanBoardView(DashboardData{
		WorkflowStates: []string{"Backlog", "Todo", "In Progress", "Blocked", "Human Review", "Merging", "Done", "Cancelled"},
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			Pipeline: []telemetry.Issue{
				{
					ID:             "review",
					Identifier:     "digitaldrywood/detent#12",
					ProjectID:      "detent",
					Title:          "Review lane PR",
					State:          "Human Review",
					Labels:         []string{"enhancement", "stage:s6"},
					Assignees:      []string{"alice"},
					BlockedBy:      []telemetry.BlockedRef{{Identifier: "digitaldrywood/detent#10", State: "Done"}},
					StageUpdatedAt: &reviewAt,
					PullRequest: &telemetry.PullRequest{
						Number:           142,
						URL:              "https://github.com/digitaldrywood/detent/pull/142",
						CIStatus:         "success",
						CodexReviewState: "clean",
					},
				},
				{
					ID:             "done",
					Identifier:     "digitaldrywood/detent#15",
					ProjectID:      "detent",
					Title:          "Done lane PR",
					State:          "Done",
					StageUpdatedAt: &doneAt,
					PullRequest: &telemetry.PullRequest{
						Number: 145,
						State:  "MERGED",
					},
				},
			},
			Running: []telemetry.Running{
				{
					Issue: telemetry.Issue{
						ID:         "running",
						Identifier: "digitaldrywood/detent#13",
						ProjectID:  "detent",
						Title:      "Running issue",
						State:      "In Progress",
						Labels:     []string{"bug"},
						Assignees:  []string{"bob"},
					},
					StartedAt: runningAt,
				},
			},
			Queue: []telemetry.Queued{
				{
					Issue: telemetry.Issue{
						ID:             "todo",
						Identifier:     "digitaldrywood/detent#11",
						ProjectID:      "detent",
						Title:          "Todo issue",
						StageUpdatedAt: &todoAt,
					},
					Attempt: 1,
				},
			},
			Blocked: []telemetry.Blocked{
				{
					Issue: telemetry.Issue{
						ID:         "blocked",
						Identifier: "digitaldrywood/detent#14",
						ProjectID:  "detent",
						Title:      "Blocked issue",
						State:      "Blocked",
						BlockedBy:  []telemetry.BlockedRef{{Identifier: "digitaldrywood/detent#401", State: "In Progress"}},
					},
					BlockedAt: &blockedAt,
				},
			},
		},
	})

	if board.TotalLabel != "5" {
		t.Fatalf("TotalLabel = %q, want 5", board.TotalLabel)
	}
	if board.EmptyCountLabel != "3" {
		t.Fatalf("EmptyCountLabel = %q, want 3", board.EmptyCountLabel)
	}

	got := collectKanbanCards(board.Lanes)
	want := []kanbanCardSnapshot{
		{Lane: "Todo", IssueNumber: "#11", Title: "Todo issue", TimeInStage: "6m 0s", Metadata: "No linked PR"},
		{Lane: "In Progress", IssueNumber: "#13", Title: "Running issue", TimeInStage: "5m 0s", Labels: "bug", Assignees: "bob", Metadata: "No linked PR"},
		{Lane: "Blocked", IssueNumber: "#14", Title: "Blocked issue", TimeInStage: "4m 0s", Blockers: "digitaldrywood/detent#401 In Progress", Metadata: "No linked PR"},
		{Lane: "Human Review", IssueNumber: "#142", Title: "Review lane PR", URL: "https://github.com/digitaldrywood/detent/pull/142", CIStatus: "pass", CodexReviewState: "clean", TimeInStage: "3m 0s", Labels: "enhancement, stage:s6", Assignees: "alice", Blockers: "digitaldrywood/detent#10 Done", Metadata: "PR #142"},
		{Lane: "Done", IssueNumber: "#145", Title: "Done lane PR", CIStatus: "pass", CodexReviewState: "clean", TimeInStage: "2m 0s", Metadata: "PR #145"},
	}
	if len(got) != len(want) {
		t.Fatalf("kanban cards len = %d, want %d; got %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("kanban card %d = %#v, want %#v", i, got[i], want[i])
		}
	}

	gotEmpty := collectKanbanLaneTitles(board.EmptyLanes)
	wantEmpty := []string{"Backlog", "Merging", "Cancelled"}
	if len(gotEmpty) != len(wantEmpty) {
		t.Fatalf("empty lanes len = %d, want %d; got %#v", len(gotEmpty), len(wantEmpty), gotEmpty)
	}
	for i, wantTitle := range wantEmpty {
		if gotEmpty[i] != wantTitle {
			t.Fatalf("empty lane %d = %q, want %q; got %#v", i, gotEmpty[i], wantTitle, gotEmpty)
		}
	}
}

func TestProjectKanbanBoardDoesNotTreatCompletedSessionsAsCurrentDone(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 13, 15, 0, 0, 0, time.UTC)
	board := projectKanbanBoardView(DashboardData{
		WorkflowStates: []string{"Todo", "Done"},
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			Completed: []telemetry.Completed{
				{
					Issue: telemetry.Issue{
						ID:         "issue-396",
						Identifier: "digitaldrywood/detent#396",
						Title:      "Completed session only",
					},
					CompletedAt: now,
					FinalState:  "Done",
				},
			},
		},
	})

	if len(board.Lanes) != 0 {
		t.Fatalf("visible lanes len = %d, want 0; lanes = %#v", len(board.Lanes), board.Lanes)
	}
	if got := collectKanbanLaneTitles(board.EmptyLanes); len(got) != 2 || got[0] != "Todo" || got[1] != "Done" {
		t.Fatalf("empty lanes = %#v, want Todo and Done", got)
	}
}

func TestPRPipelineLanesMapSnapshotRows(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 1, 15, 0, 0, 0, time.UTC)
	reviewAt := now.Add(-2 * time.Hour)
	mergeAt := now.Add(-15 * time.Minute)
	doneAt := now.Add(-45 * time.Minute)
	oldDoneAt := now.Add(-25 * time.Hour)

	tests := []struct {
		name     string
		snapshot telemetry.Snapshot
		want     []pipelineCardSnapshot
	}{
		{
			name: "tracker pipeline issues",
			snapshot: telemetry.Snapshot{
				GeneratedAt: now,
				Pipeline: []telemetry.Issue{
					{
						ID:         "review",
						Identifier: "digitaldrywood/detent#12",
						Title:      "Review lane PR",
						State:      "Human Review",
						UpdatedAt:  &reviewAt,
						PullRequest: &telemetry.PullRequest{
							Number:           142,
							URL:              "https://github.com/digitaldrywood/detent/pull/142",
							CIStatus:         "success",
							CodexReviewState: "clean",
						},
					},
					{
						ID:         "merge",
						Identifier: "digitaldrywood/detent#13",
						Title:      "Merge lane PR",
						State:      "Merging",
						UpdatedAt:  &mergeAt,
						PullRequest: &telemetry.PullRequest{
							Number:            143,
							CIStatus:          "pending",
							CIDurationSeconds: 510,
							QuietWaitSeconds:  600,
							SlowChecks: []telemetry.PullRequestCheck{
								{Name: "GoReleaser Snapshot", DurationSeconds: 247},
							},
							RunningChecks:    []string{"Test Coverage"},
							CodexReviewState: "P2",
						},
					},
					{
						ID:         "done",
						Identifier: "digitaldrywood/detent#14",
						Title:      "Done lane PR",
						State:      "Done",
						UpdatedAt:  &doneAt,
						PullRequest: &telemetry.PullRequest{
							Number:           144,
							State:            "MERGED",
							CodexReviewState: "P1",
						},
					},
					{
						ID:         "done-unverified",
						Identifier: "digitaldrywood/detent#16",
						Title:      "Done lane unverified PR",
						State:      "Done",
						UpdatedAt:  &doneAt,
						PullRequest: &telemetry.PullRequest{
							Number: 145,
						},
					},
					{
						ID:         "old-done",
						Identifier: "digitaldrywood/detent#15",
						Title:      "Old done PR",
						State:      "Done",
						UpdatedAt:  &oldDoneAt,
					},
					{
						ID:             "cancelled-today",
						Identifier:     "digitaldrywood/detent#17",
						Title:          "Cancelled today PR",
						State:          "Cancelled",
						UpdatedAt:      &oldDoneAt,
						StageUpdatedAt: &doneAt,
						PullRequest: &telemetry.PullRequest{
							Number:           146,
							State:            "MERGED",
							CodexReviewState: "clean",
						},
					},
				},
			},
			want: []pipelineCardSnapshot{
				{Lane: "Human Review", IssueNumber: "#142", Title: "Review lane PR", CIStatus: "pass", CodexReviewState: "clean", TimeInStage: "2h 0m"},
				{Lane: "Merging", IssueNumber: "#143", Title: "Merge lane PR", CIStatus: "pending", CodexReviewState: "P2", TimeInStage: "15m 0s", WaitDetail: "quiet 10m 0s / CI 8m 30s / slow GoReleaser Snapshot 4m 7s / running Test Coverage"},
				{Lane: "Done today", IssueNumber: "#144", Title: "Done lane PR", CIStatus: "pass", CodexReviewState: "P1", TimeInStage: "45m 0s"},
				{Lane: "Done today", IssueNumber: "#145", Title: "Done lane unverified PR", CIStatus: "pending", CodexReviewState: "clean", TimeInStage: "45m 0s"},
				{Lane: "Done today", IssueNumber: "#146", Title: "Cancelled today PR", CIStatus: "pass", CodexReviewState: "clean", TimeInStage: "45m 0s"},
			},
		},
		{
			name: "runtime fallback rows",
			snapshot: telemetry.Snapshot{
				GeneratedAt: now,
				Running: []telemetry.Running{
					{
						Issue: telemetry.Issue{
							ID:         "merge-session",
							Identifier: "digitaldrywood/detent#21",
							Title:      "Merge session",
							State:      "Merging",
						},
						StartedAt: now.Add(-5 * time.Minute),
					},
				},
			},
			want: []pipelineCardSnapshot{
				{Lane: "Merging", IssueNumber: "#21", Title: "Merge session", CIStatus: "pending", CodexReviewState: "clean", TimeInStage: "5m 0s"},
			},
		},
		{
			name: "empty lanes",
			snapshot: telemetry.Snapshot{
				GeneratedAt: now,
			},
			want: []pipelineCardSnapshot{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := collectPipelineCards(prPipelineLanes(tt.snapshot))
			if len(got) != len(tt.want) {
				t.Fatalf("pipeline cards len = %d, want %d; got %#v", len(got), len(tt.want), got)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("pipeline card %d = %#v, want %#v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestPRPipelineLanesDoNotTreatCompletedSessionAsCurrentDone(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 13, 15, 0, 0, 0, time.UTC)
	snapshot := telemetry.Snapshot{
		GeneratedAt: now,
		Blocked: []telemetry.Blocked{
			{
				Issue: telemetry.Issue{
					ID:         "issue-396",
					Identifier: "digitaldrywood/detent#396",
					Title:      "Blocked after completed session",
					State:      "Blocked",
				},
			},
		},
		Completed: []telemetry.Completed{
			{
				Issue: telemetry.Issue{
					ID:         "issue-396",
					Identifier: "digitaldrywood/detent#396",
					Title:      "Blocked after completed session",
				},
				CompletedAt: now.Add(-5 * time.Minute),
				FinalState:  "completed",
			},
		},
	}

	got := collectPipelineCards(prPipelineLanes(snapshot))
	if len(got) != 0 {
		t.Fatalf("pipeline cards len = %d, want 0; got %#v", len(got), got)
	}
}

func TestPRPipelineLanesCapDoneTodayToRecentCards(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 1, 15, 0, 0, 0, time.UTC)
	issues := make([]telemetry.Issue, 0, 12)
	for i := range 12 {
		updatedAt := now.Add(-time.Duration(11-i) * time.Minute)
		issues = append(issues, telemetry.Issue{
			ID:         "done-" + strconv.Itoa(i),
			Identifier: "digitaldrywood/detent#" + strconv.Itoa(200+i),
			Title:      "Done PR " + strconv.Itoa(i),
			State:      "Done",
			UpdatedAt:  &updatedAt,
			PullRequest: &telemetry.PullRequest{
				Number: 200 + i,
				State:  "MERGED",
			},
		})
	}

	lanes := prPipelineLanes(telemetry.Snapshot{
		GeneratedAt: now,
		Pipeline:    issues,
	})
	doneLane := lanes[2]
	if doneLane.Title != "Done today" {
		t.Fatalf("lane[2] = %q, want Done today", doneLane.Title)
	}
	if len(doneLane.Cards) != 10 {
		t.Fatalf("Done today cards len = %d, want 10; cards = %#v", len(doneLane.Cards), doneLane.Cards)
	}
	if doneLane.Cards[0].IssueNumber != "#211" {
		t.Fatalf("newest card = %q, want #211; cards = %#v", doneLane.Cards[0].IssueNumber, doneLane.Cards)
	}
	for _, card := range doneLane.Cards {
		if card.IssueNumber == "#200" || card.IssueNumber == "#201" {
			t.Fatalf("Done today should drop oldest cards, found %s in %#v", card.IssueNumber, doneLane.Cards)
		}
	}
}

func TestPipelineNowUsesStageUpdatedAt(t *testing.T) {
	t.Parallel()

	issueUpdatedAt := time.Date(2026, 5, 31, 10, 0, 0, 0, time.UTC)
	stageUpdatedAt := time.Date(2026, 6, 1, 12, 30, 0, 0, time.UTC)
	got := pipelineNow(telemetry.Snapshot{
		Pipeline: []telemetry.Issue{{
			ID:             "done",
			UpdatedAt:      &issueUpdatedAt,
			StageUpdatedAt: &stageUpdatedAt,
		}},
	})
	if !got.Equal(stageUpdatedAt) {
		t.Fatalf("pipelineNow() = %v, want %v", got, stageUpdatedAt)
	}
}

type pipelineCardSnapshot struct {
	Lane             string
	IssueNumber      string
	Title            string
	CIStatus         string
	CodexReviewState string
	TimeInStage      string
	WaitDetail       string
}

func collectPipelineCards(lanes []prPipelineLane) []pipelineCardSnapshot {
	out := []pipelineCardSnapshot{}
	for _, lane := range lanes {
		for _, card := range lane.Cards {
			out = append(out, pipelineCardSnapshot{
				Lane:             lane.Title,
				IssueNumber:      card.IssueNumber,
				Title:            card.Title,
				CIStatus:         card.CIStatus,
				CodexReviewState: card.CodexReviewState,
				TimeInStage:      card.TimeInStage,
				WaitDetail:       card.WaitDetail,
			})
		}
	}
	return out
}

type kanbanCardSnapshot struct {
	Lane             string
	IssueNumber      string
	Title            string
	URL              string
	CIStatus         string
	CodexReviewState string
	TimeInStage      string
	Labels           string
	Assignees        string
	Blockers         string
	Metadata         string
}

func collectKanbanCards(lanes []projectKanbanLane) []kanbanCardSnapshot {
	out := []kanbanCardSnapshot{}
	for _, lane := range lanes {
		for _, card := range lane.Cards {
			out = append(out, kanbanCardSnapshot{
				Lane:             lane.Title,
				IssueNumber:      card.IssueNumber,
				Title:            card.Title,
				URL:              card.URL,
				CIStatus:         card.CIStatus,
				CodexReviewState: card.CodexReviewState,
				TimeInStage:      card.TimeInStage,
				Labels:           strings.Join(card.Labels, ", "),
				Assignees:        strings.Join(card.Assignees, ", "),
				Blockers:         strings.Join(card.Blockers, ", "),
				Metadata:         card.PullRequestLabel,
			})
		}
	}
	return out
}

func collectKanbanLaneTitles(lanes []projectKanbanLane) []string {
	out := make([]string, 0, len(lanes))
	for _, lane := range lanes {
		out = append(out, lane.Title)
	}
	return out
}
