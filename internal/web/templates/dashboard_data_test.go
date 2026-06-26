package templates

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/projectcolor"
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

func TestGitHubAPIHealthDerivesStatus(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 25, 14, 30, 0, 0, time.UTC)
	lastRefresh := now.Add(-30 * time.Second)
	nextRefresh := now.Add(90 * time.Second)
	resetAt := now.Add(30 * time.Minute)
	backoffUntil := now.Add(5 * time.Minute)
	expiredBackoffUntil := now.Add(-5 * time.Minute)

	tests := []struct {
		name            string
		snapshot        telemetry.Snapshot
		wantState       gitHubAPIHealthState
		wantLabel       string
		wantSummaryPart string
		wantBackoffPart string
	}{
		{
			name:            "unknown without GitHub snapshot",
			snapshot:        telemetry.Snapshot{GeneratedAt: now},
			wantState:       gitHubAPIHealthStateUnknown,
			wantLabel:       "GitHub API unknown",
			wantSummaryPart: "No GitHub rate-limit snapshot",
		},
		{
			name: "healthy with primary budgets",
			snapshot: telemetry.Snapshot{
				GeneratedAt: now,
				Refresh:     telemetry.Refresh{LastRefreshAt: &lastRefresh, NextRefreshAt: &nextRefresh},
				RateLimits: &telemetry.RateLimits{
					GitHubREST:    &telemetry.RateLimitBucket{Remaining: 4878, Used: 122, Limit: 5000, ResetAt: &resetAt},
					GitHubGraphQL: &telemetry.RateLimitBucket{Remaining: 4880, Used: 120, Limit: 5000, ResetAt: &resetAt},
				},
			},
			wantState:       gitHubAPIHealthStateHealthy,
			wantLabel:       "GitHub API healthy",
			wantSummaryPart: "REST 4,878 / 5,000",
		},
		{
			name: "warning for low primary remaining",
			snapshot: telemetry.Snapshot{
				GeneratedAt: now,
				RateLimits: &telemetry.RateLimits{
					GitHubREST:    &telemetry.RateLimitBucket{Remaining: 240, Used: 4760, Limit: 5000, ResetAt: &resetAt},
					GitHubGraphQL: &telemetry.RateLimitBucket{Remaining: 3200, Used: 1800, Limit: 5000, ResetAt: &resetAt},
				},
			},
			wantState:       gitHubAPIHealthStateWarning,
			wantLabel:       "GitHub API warning",
			wantSummaryPart: "REST 240 / 5,000",
		},
		{
			name: "secondary backoff preserves healthy primary context",
			snapshot: telemetry.Snapshot{
				GeneratedAt: now,
				RateLimits: &telemetry.RateLimits{
					GitHubREST:    &telemetry.RateLimitBucket{Remaining: 4878, Used: 122, Limit: 5000, ResetAt: &resetAt},
					GitHubGraphQL: &telemetry.RateLimitBucket{Remaining: 4880, Used: 120, Limit: 5000, ResetAt: &resetAt},
					RESTUsage: &telemetry.RESTUsage{
						RateLimited:  true,
						BackoffUntil: &backoffUntil,
						Contributors: []telemetry.RESTUsageContributor{
							{EndpointFamily: "pull requests", Count: 2, RetryAfterMS: (5 * time.Minute).Milliseconds(), RateLimited: true, LastStatus: 429},
							{EndpointFamily: "check runs", Count: 1, RateLimited: true, LastStatus: 429},
						},
					},
				},
			},
			wantState:       gitHubAPIHealthStateBackoff,
			wantLabel:       "GitHub API backoff: pull requests, check runs",
			wantSummaryPart: "Primary remaining: REST 4,878 / 5,000",
			wantBackoffPart: "retry 14:35 UTC",
		},
		{
			name: "expired secondary backoff does not mask healthy primary",
			snapshot: telemetry.Snapshot{
				GeneratedAt: now,
				RateLimits: &telemetry.RateLimits{
					GitHubREST:    &telemetry.RateLimitBucket{Remaining: 4878, Used: 122, Limit: 5000, ResetAt: &resetAt},
					GitHubGraphQL: &telemetry.RateLimitBucket{Remaining: 4880, Used: 120, Limit: 5000, ResetAt: &resetAt},
					RESTUsage: &telemetry.RESTUsage{
						RateLimited:  true,
						BackoffUntil: &expiredBackoffUntil,
						Contributors: []telemetry.RESTUsageContributor{
							{EndpointFamily: "pull requests", Count: 2, RetryAfterMS: (5 * time.Minute).Milliseconds(), RateLimited: true, LastStatus: 429},
						},
					},
				},
			},
			wantState:       gitHubAPIHealthStateHealthy,
			wantLabel:       "GitHub API healthy",
			wantSummaryPart: "REST 4,878 / 5,000",
		},
		{
			name: "primary exhausted outranks secondary backoff",
			snapshot: telemetry.Snapshot{
				GeneratedAt: now,
				RateLimits: &telemetry.RateLimits{
					GitHubREST:    &telemetry.RateLimitBucket{Remaining: 0, Used: 5000, Limit: 5000, ResetAt: &resetAt},
					GitHubGraphQL: &telemetry.RateLimitBucket{Remaining: 4880, Used: 120, Limit: 5000, ResetAt: &resetAt},
					RESTUsage:     &telemetry.RESTUsage{RateLimited: true, BackoffUntil: &backoffUntil},
				},
			},
			wantState:       gitHubAPIHealthStateExhausted,
			wantLabel:       "GitHub API exhausted",
			wantSummaryPart: "REST primary 0 / 5,000",
			wantBackoffPart: "reset 15:00 UTC",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := gitHubAPIHealth(tt.snapshot)
			if got.State != tt.wantState {
				t.Fatalf("gitHubAPIHealth().State = %q, want %q; view = %#v", got.State, tt.wantState, got)
			}
			if got.Label != tt.wantLabel {
				t.Fatalf("gitHubAPIHealth().Label = %q, want %q; view = %#v", got.Label, tt.wantLabel, got)
			}
			if !strings.Contains(got.Summary, tt.wantSummaryPart) {
				t.Fatalf("gitHubAPIHealth().Summary = %q, want substring %q; view = %#v", got.Summary, tt.wantSummaryPart, got)
			}
			if tt.wantBackoffPart != "" && !strings.Contains(got.Detail, tt.wantBackoffPart) {
				t.Fatalf("gitHubAPIHealth().Detail = %q, want substring %q; view = %#v", got.Detail, tt.wantBackoffPart, got)
			}
			if tt.name == "expired secondary backoff does not mask healthy primary" && len(got.Endpoints) != 0 {
				t.Fatalf("gitHubAPIHealth().Endpoints = %#v, want no active endpoint backoff rows", got.Endpoints)
			}
		})
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
					Color:      "#1192e8",
					Samples: []ProjectSmallMultipleSample{
						{At: now.Add(-time.Minute), ThroughputTokensPerSecond: 0.5, SpendUSD: 1, QueueDepth: 1},
					},
				},
				{
					ID:                        "busy",
					Name:                      "Busy",
					URL:                       "https://github.com/digitaldrywood/detent",
					Color:                     "#a63f7a",
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
				ProjectColor:    "#a63f7a",
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
				ProjectColor:    projectcolor.ColorForID("detent"),
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
				first.ProjectColor != tt.wantFirst.ProjectColor ||
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

func TestProjectColorMarkersUseConfiguredAndAutomaticColors(t *testing.T) {
	t.Parallel()

	projects := []ProjectSmallMultiple{
		{ID: "detent", Name: "Detent", Color: "#1192e8", Running: 1},
		{ID: "docs-site", Name: "Docs", QueueCount: 1},
	}
	cards := projectSmallMultipleCards(DashboardData{Projects: projects})
	items := sidebarProjectItems(DashboardShellData{Projects: projects})

	want := map[string]string{
		"detent":    "#1192e8",
		"docs-site": projectcolor.ColorForID("docs-site"),
	}
	for _, card := range cards {
		if card.ProjectColor != want[card.ID] {
			t.Fatalf("projectSmallMultipleCards() color for %s = %q, want %q; cards = %#v", card.ID, card.ProjectColor, want[card.ID], cards)
		}
	}
	for _, item := range items {
		if item.ProjectColor != want[item.ID] {
			t.Fatalf("sidebarProjectItems() color for %s = %q, want %q; items = %#v", item.ID, item.ProjectColor, want[item.ID], items)
		}
	}
}

func TestProjectKanbanCardsUseProjectColors(t *testing.T) {
	t.Parallel()

	board := projectKanbanBoardView(DashboardData{
		Projects: []ProjectSmallMultiple{
			{ID: "detent", Color: "#1192e8"},
			{ID: "docs-site"},
		},
		Kanban: KanbanData{States: []string{"Todo"}},
		Snapshot: telemetry.Snapshot{
			BoardIssues: []telemetry.Issue{
				{ID: "detent-issue", Identifier: "digitaldrywood/detent#1", ProjectID: "detent", Title: "Detent work", State: "Todo"},
				{ID: "docs-issue", Identifier: "digitaldrywood/docs-site#2", ProjectID: "docs-site", Title: "Docs work", State: "Todo"},
			},
		},
	})

	got := collectKanbanCards(board.Lanes)
	if len(got) != 2 {
		t.Fatalf("kanban cards len = %d, want 2; got %#v", len(got), got)
	}
	cards := board.Lanes[0].Cards
	if cards[0].ProjectID != "detent" || cards[0].ProjectColor != "#1192e8" {
		t.Fatalf("first card project marker = %q/%q, want detent/#1192e8", cards[0].ProjectID, cards[0].ProjectColor)
	}
	if cards[1].ProjectID != "docs-site" || cards[1].ProjectColor != projectcolor.ColorForID("docs-site") {
		t.Fatalf("second card project marker = %q/%q, want docs-site/%q", cards[1].ProjectID, cards[1].ProjectColor, projectcolor.ColorForID("docs-site"))
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

func TestProjectSidebarViewActiveStates(t *testing.T) {
	t.Parallel()

	views := []string{"overview", "kanban", "runs", "configuration", "diagnostics"}
	tests := []struct {
		name      string
		activeNav string
		wantView  string
	}{
		{name: "default", wantView: "overview"},
		{name: "project", activeNav: "project", wantView: "overview"},
		{name: "kanban", activeNav: "kanban", wantView: "kanban"},
		{name: "runs", activeNav: "runs", wantView: "runs"},
		{name: "settings", activeNav: "settings", wantView: "configuration"},
		{name: "configuration", activeNav: "configuration", wantView: "configuration"},
		{name: "diagnostics", activeNav: "diagnostics", wantView: "diagnostics"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			data := DashboardShellData{ProjectID: "detent", ActiveNav: tt.activeNav}
			for _, view := range views {
				want := view == tt.wantView
				if got := projectSidebarViewActive(data, view); got != want {
					t.Fatalf("projectSidebarViewActive(%q, %q) = %t, want %t", tt.activeNav, view, got, want)
				}
			}
		})
	}

	if projectSidebarViewActive(DashboardShellData{ActiveNav: "runs"}, "runs") {
		t.Fatalf("projectSidebarViewActive() must be false without project context")
	}
}

func TestSidebarStaticNavActiveRespectsProjectContext(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		data       DashboardShellData
		id         string
		wantActive bool
	}{
		{
			name:       "global settings",
			data:       DashboardShellData{ActiveNav: "settings"},
			id:         "settings",
			wantActive: true,
		},
		{
			name:       "project settings belongs to configuration",
			data:       DashboardShellData{ActiveNav: "settings", ProjectID: "detent"},
			id:         "settings",
			wantActive: false,
		},
		{
			name:       "project reports stays static",
			data:       DashboardShellData{ActiveNav: "reports", ProjectID: "detent"},
			id:         "reports",
			wantActive: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := sidebarStaticNavActive(tt.data, tt.id); got != tt.wantActive {
				t.Fatalf("sidebarStaticNavActive(%q) = %t, want %t", tt.id, got, tt.wantActive)
			}
		})
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
					PullRequest: &telemetry.PullRequest{
						Number: 22,
						URL:    "https://github.com/digitaldrywood/detent/pull/22",
					},
				},
				StartedAt: now.Add(-4 * time.Minute),
			},
			{
				Issue: telemetry.Issue{
					ID:         "issue-1",
					Identifier: "DD-1",
					URL:        "https://github.com/digitaldrywood/detent/issues/1",
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
					URL:        "https://github.com/digitaldrywood/detent/issues/3",
					Title:      "Completed issue",
					PullRequest: &telemetry.PullRequest{
						Number: 3,
						State:  "OPEN",
					},
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
	if rows[1].Title != "Completed issue" || rows[1].State != "completed" {
		t.Fatalf("completed open PR timeline row = %#v", rows[1])
	}
	if rows[1].IssueURL != "https://github.com/digitaldrywood/detent/issues/3" {
		t.Fatalf("completed issue URL = %q, want issue URL", rows[1].IssueURL)
	}
	if rows[1].PullRequestURL != "https://github.com/digitaldrywood/detent/pull/3" || rows[1].PullRequestNumber != 3 {
		t.Fatalf("completed PR metadata = %q/%d, want derived PR #3 URL", rows[1].PullRequestURL, rows[1].PullRequestNumber)
	}
	if rows[2].IssueURL != "https://github.com/digitaldrywood/detent/issues/1" || rows[2].PullRequestURL != "" {
		t.Fatalf("issue-only timeline row links = %q/%q", rows[2].IssueURL, rows[2].PullRequestURL)
	}
	if rows[3].IssueURL != "" || rows[3].PullRequestURL != "https://github.com/digitaldrywood/detent/pull/22" || rows[3].PullRequestNumber != 22 {
		t.Fatalf("PR-only timeline row links = %q/%q/%d", rows[3].IssueURL, rows[3].PullRequestURL, rows[3].PullRequestNumber)
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
	backlogAt := now.Add(-7 * time.Minute)
	todoAt := now.Add(-6 * time.Minute)
	runningAt := now.Add(-5 * time.Minute)
	blockedAt := now.Add(-4 * time.Minute)
	reviewAt := now.Add(-3 * time.Minute)
	doneAt := now.Add(-2 * time.Minute)

	board := projectKanbanBoardView(DashboardData{
		Kanban: KanbanData{
			States: []string{"Backlog", "Todo", "In Progress", "Blocked", "Human Review", "Merging", "Done", "Cancelled"},
		},
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			BoardIssues: []telemetry.Issue{
				{
					ID:             "backlog",
					Identifier:     "digitaldrywood/detent#10",
					ProjectID:      "detent",
					Title:          "Backlog issue",
					State:          "Backlog",
					StageUpdatedAt: &backlogAt,
				},
				{
					ID:         "todo",
					Identifier: "digitaldrywood/detent#11",
					ProjectID:  "detent",
					Title:      "Stale board issue",
					State:      "Backlog",
				},
			},
			Pipeline: []telemetry.Issue{
				{
					ID:             "review",
					Identifier:     "digitaldrywood/detent#12",
					URL:            "https://github.com/digitaldrywood/detent/issues/12",
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
					URL:            "https://github.com/digitaldrywood/detent/issues/15",
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

	if board.TotalLabel != "6" {
		t.Fatalf("TotalLabel = %q, want 6", board.TotalLabel)
	}
	if board.EmptyCountLabel != "2" {
		t.Fatalf("EmptyCountLabel = %q, want 2", board.EmptyCountLabel)
	}

	got := collectKanbanCards(board.AllLanes)
	want := []kanbanCardSnapshot{
		{Lane: "Backlog", IssueNumber: "#10", Title: "Backlog issue", TimeInStage: "7m 0s", Metadata: "No linked PR"},
		{Lane: "Todo", IssueNumber: "#11", Title: "Todo issue", TimeInStage: "6m 0s", Metadata: "No linked PR"},
		{Lane: "In Progress", IssueNumber: "#13", Title: "Running issue", TimeInStage: "5m 0s", Labels: "bug", Assignees: "bob", Metadata: "No linked PR"},
		{Lane: "Blocked", IssueNumber: "#14", Title: "Blocked issue", TimeInStage: "4m 0s", Blockers: "digitaldrywood/detent#401 In Progress", Metadata: "No linked PR"},
		{Lane: "Human Review", IssueNumber: "#12", Title: "Review lane PR", URL: "https://github.com/digitaldrywood/detent/issues/12", CIStatus: "pass", CodexReviewState: "clean", TimeInStage: "3m 0s", Labels: "enhancement, stage:s6", Assignees: "alice", ClearedBlockers: "digitaldrywood/detent#10 Done", Metadata: "PR #142"},
		{Lane: "Done", IssueNumber: "#15", Title: "Done lane PR", URL: "https://github.com/digitaldrywood/detent/issues/15", CIStatus: "pass", CodexReviewState: "clean", TimeInStage: "2m 0s", Metadata: "PR #145"},
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
	wantEmpty := []string{"Merging", "Cancelled"}
	if len(gotEmpty) != len(wantEmpty) {
		t.Fatalf("empty lanes len = %d, want %d; got %#v", len(gotEmpty), len(wantEmpty), gotEmpty)
	}
	for i, wantTitle := range wantEmpty {
		if gotEmpty[i] != wantTitle {
			t.Fatalf("empty lane %d = %q, want %q; got %#v", i, gotEmpty[i], wantTitle, gotEmpty)
		}
	}
}

func TestProjectKanbanCompactChipsSummarizeSecondaryMetadata(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		card projectKanbanCard
		want []string
	}{
		{
			name: "issue-only",
			card: projectKanbanCard{
				TimeInStage: "5m 0s",
			},
			want: []string{"5m 0s"},
		},
		{
			name: "pr-linked",
			card: projectKanbanCard{
				HasPullRequest:   true,
				CIStatus:         "pass",
				CodexReviewState: "clean",
				TimeInStage:      "4m 0s",
			},
			want: []string{"4m 0s", "CI pass", "Codex clean"},
		},
		{
			name: "conflicting-pr",
			card: projectKanbanCard{
				HasPullRequest: true,
				MergeableState: "dirty",
				ConflictReason: "PR #38 mergeStateStatus DIRTY",
				TimeInStage:    "4m 0s",
			},
			want: []string{"4m 0s", "CI n/a", "Codex n/a", "Conflict"},
		},
		{
			name: "blocked",
			card: projectKanbanCard{
				TimeInStage: "3m 0s",
				Blockers:    []string{"digitaldrywood/detent#415 Todo"},
			},
			want: []string{"3m 0s", "1 blocker"},
		},
		{
			name: "review",
			card: projectKanbanCard{
				HasPullRequest:   true,
				CIStatus:         "pass",
				CodexReviewState: "P2",
				TimeInStage:      "2m 0s",
				Labels:           []string{"enhancement", "ui"},
				Assignees:        []string{"release-captain"},
			},
			want: []string{"2m 0s", "CI pass", "Codex P2", "1 assignee", "2 labels"},
		},
		{
			name: "merge-lane-active",
			card: projectKanbanCard{
				TimeInStage:     "1m 0s",
				MergeLaneStatus: "Merging now",
				MergeLaneDetail: "Active merge worker for PR #143; running checks",
				MergeLaneClass:  "border-accent-soft bg-accent-soft text-accent",
			},
			want: []string{"1m 0s", "Merging now"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := projectKanbanCompactChipLabels(projectKanbanCompactChips(tt.card))
			if len(got) != len(tt.want) {
				t.Fatalf("compact chips len = %d, want %d; got %#v", len(got), len(tt.want), got)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("compact chip %d = %q, want %q; got %#v", i, got[i], tt.want[i], got)
				}
			}
		})
	}
}

func TestProjectKanbanBoardShowsMergeLaneStatus(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 24, 18, 0, 0, 0, time.UTC)
	activeAt := now.Add(-3 * time.Minute)
	queuedAt := now.Add(-2 * time.Minute)

	board := projectKanbanBoardView(DashboardData{
		Kanban: KanbanData{
			States: []string{"Merging"},
		},
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			Pipeline: []telemetry.Issue{
				{
					ID:             "active",
					Identifier:     "digitaldrywood/detent#143",
					Title:          "Active merge",
					State:          "Merging",
					StageUpdatedAt: &activeAt,
					PullRequest: &telemetry.PullRequest{
						Number: 143,
						URL:    "https://github.com/digitaldrywood/detent/pull/143",
					},
				},
				{
					ID:             "queued",
					Identifier:     "digitaldrywood/detent#144",
					Title:          "Queued merge",
					State:          "Merging",
					StageUpdatedAt: &queuedAt,
					PullRequest: &telemetry.PullRequest{
						Number: 144,
						URL:    "https://github.com/digitaldrywood/detent/pull/144",
					},
				},
			},
			Running: []telemetry.Running{
				{
					Issue: telemetry.Issue{
						ID:         "active",
						Identifier: "digitaldrywood/detent#143",
						Title:      "Active merge",
						State:      "Merging",
						PullRequest: &telemetry.PullRequest{
							Number: 143,
							URL:    "https://github.com/digitaldrywood/detent/pull/143",
						},
					},
					StartedAt: activeAt,
					LastEvent: "running checks",
				},
			},
			Queue: []telemetry.Queued{
				{
					Issue: telemetry.Issue{
						ID:             "queued",
						Identifier:     "digitaldrywood/detent#144",
						Title:          "Queued merge",
						State:          "Merging",
						StageUpdatedAt: &queuedAt,
						PullRequest: &telemetry.PullRequest{
							Number: 144,
							URL:    "https://github.com/digitaldrywood/detent/pull/144",
						},
					},
					Attempt: 1,
					Error:   "project_state_capacity_full",
				},
			},
		},
	})

	got := collectKanbanCards(board.AllLanes)
	want := []kanbanCardSnapshot{
		{Lane: "Merging", IssueNumber: "#143", Title: "Active merge", CIStatus: "pending", CodexReviewState: "clean", TimeInStage: "3m 0s", Metadata: "PR #143", MergeLaneStatus: "Merging now", MergeLaneDetail: "Active merge worker for PR #143; running checks"},
		{Lane: "Merging", IssueNumber: "#144", Title: "Queued merge", CIStatus: "pending", CodexReviewState: "clean", TimeInStage: "2m 0s", Metadata: "PR #144", MergeLaneStatus: "Queued #2", MergeLaneDetail: "Waiting: project_state_capacity_full; 2nd in merge queue; waiting for repo merge lane behind PR #143"},
	}
	if len(got) != len(want) {
		t.Fatalf("kanban cards len = %d, want %d; got %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("kanban card %d = %#v, want %#v", i, got[i], want[i])
		}
	}
}

func TestProjectKanbanBoardScopesMergeLaneStatusByProject(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 24, 20, 0, 0, 0, time.UTC)
	activeAt := now.Add(-5 * time.Minute)
	queuedAt := now.Add(-2 * time.Minute)

	board := projectKanbanBoardView(DashboardData{
		Kanban: KanbanData{
			States: []string{"Merging"},
		},
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			Pipeline: []telemetry.Issue{
				{
					ID:             "local-1",
					Identifier:     "digitaldrywood/detent#143",
					ProjectID:      "detent",
					Title:          "Active merge",
					State:          "Merging",
					StageUpdatedAt: &activeAt,
					PullRequest: &telemetry.PullRequest{
						Number: 143,
						URL:    "https://github.com/digitaldrywood/detent/pull/143",
					},
				},
				{
					ID:             "local-1",
					Identifier:     "digitaldrywood/docs-site#27",
					ProjectID:      "docs-site",
					Title:          "Queued docs merge",
					State:          "Merging",
					StageUpdatedAt: &queuedAt,
					PullRequest: &telemetry.PullRequest{
						Number: 27,
						URL:    "https://github.com/digitaldrywood/docs-site/pull/27",
					},
				},
			},
			Running: []telemetry.Running{
				{
					Issue: telemetry.Issue{
						ID:         "local-1",
						Identifier: "digitaldrywood/detent#143",
						ProjectID:  "detent",
						Title:      "Active merge",
						State:      "Merging",
						PullRequest: &telemetry.PullRequest{
							Number: 143,
							URL:    "https://github.com/digitaldrywood/detent/pull/143",
						},
					},
					StartedAt: activeAt,
					LastEvent: "running checks",
				},
			},
		},
	})

	got := collectKanbanCards(board.AllLanes)
	want := []kanbanCardSnapshot{
		{Lane: "Merging", IssueNumber: "#143", Title: "Active merge", CIStatus: "pending", CodexReviewState: "clean", TimeInStage: "5m 0s", Metadata: "PR #143", MergeLaneStatus: "Merging now", MergeLaneDetail: "Active merge worker for PR #143; running checks"},
		{Lane: "Merging", IssueNumber: "#27", Title: "Queued docs merge", CIStatus: "pending", CodexReviewState: "clean", TimeInStage: "2m 0s", Metadata: "PR #27", MergeLaneStatus: "Queued #1", MergeLaneDetail: "1st in merge queue; waiting for repo merge lane"},
	}
	if len(got) != len(want) {
		t.Fatalf("kanban cards len = %d, want %d; got %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("kanban card %d = %#v, want %#v", i, got[i], want[i])
		}
	}
}

func TestProjectKanbanCardForIssueShowsPullRequestConflictReason(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 24, 13, 0, 0, 0, time.UTC)
	issue := telemetry.Issue{
		ID:         "conflicting-pr",
		Identifier: "digitaldrywood/creswoodcorners-phone#32",
		Title:      "Resolve PR conflicts",
		State:      "Rework",
		PullRequest: &telemetry.PullRequest{
			Number:         38,
			URL:            "https://github.com/digitaldrywood/creswoodcorners-phone/pull/38",
			State:          "OPEN",
			MergeableState: "DIRTY",
			CIStatus:       "success",
		},
	}

	card := projectKanbanCardForIssue(DashboardData{}, issue, "Rework", now.Add(-time.Minute), now)

	if card.MergeableState != "dirty" {
		t.Fatalf("MergeableState = %q, want dirty", card.MergeableState)
	}
	if card.ConflictReason != "PR #38 mergeStateStatus DIRTY" {
		t.Fatalf("ConflictReason = %q, want PR #38 mergeStateStatus DIRTY", card.ConflictReason)
	}
	chips := projectKanbanCompactChips(card)
	if got := projectKanbanCompactChipLabels(chips); !slices.Contains(got, "Conflict") {
		t.Fatalf("compact chips = %#v, want Conflict", got)
	}
	for _, chip := range chips {
		if chip.Label == "Conflict" && chip.Title != "PR #38 mergeStateStatus DIRTY" {
			t.Fatalf("Conflict chip title = %q, want PR #38 mergeStateStatus DIRTY", chip.Title)
		}
	}
}

func TestProjectKanbanCardForIssueCopiesDescriptionPreview(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 16, 14, 0, 0, 0, time.UTC)
	issue := telemetry.Issue{
		ID:          "readable-card",
		Identifier:  "digitaldrywood/detent#525",
		Title:       "Make compact kanban cards readable",
		Description: "  Titles need their own line.\nHover should show enough issue context for triage.  ",
		State:       "Todo",
	}

	card := projectKanbanCardForIssue(DashboardData{}, issue, "Todo", now.Add(-time.Minute), now)

	if card.Description != "Titles need their own line. Hover should show enough issue context for triage." {
		t.Fatalf("Description = %q", card.Description)
	}
}

func TestProjectKanbanCardForIssueOnlyKeepsActiveBlockers(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 20, 17, 0, 0, 0, time.UTC)
	tests := []struct {
		name         string
		state        string
		blockedBy    []telemetry.BlockedRef
		wantBlockers []string
		wantCleared  []string
	}{
		{
			name:  "terminal dependency is cleared",
			state: "Merging",
			blockedBy: []telemetry.BlockedRef{
				{Identifier: "digitaldrywood/detent#429", State: "Done"},
			},
			wantCleared: []string{"digitaldrywood/detent#429 Done"},
		},
		{
			name:  "non-terminal dependency stays active",
			state: "Todo",
			blockedBy: []telemetry.BlockedRef{
				{Identifier: "digitaldrywood/detent#430", State: "In Progress"},
			},
			wantBlockers: []string{"digitaldrywood/detent#430 In Progress"},
		},
		{
			name:  "unresolved dependency stays active",
			state: "Blocked",
			blockedBy: []telemetry.BlockedRef{
				{Identifier: "digitaldrywood/detent#431"},
			},
			wantBlockers: []string{"digitaldrywood/detent#431"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			issue := telemetry.Issue{
				ID:         "issue",
				Identifier: "digitaldrywood/detent#594",
				Title:      "Fix blocker rendering",
				State:      tt.state,
				BlockedBy:  tt.blockedBy,
			}
			card := projectKanbanCardForIssue(DashboardData{}, issue, tt.state, now.Add(-time.Minute), now)
			if len(card.Blockers) != len(tt.wantBlockers) {
				t.Fatalf("Blockers len = %d, want %d; got %#v", len(card.Blockers), len(tt.wantBlockers), card.Blockers)
			}
			for i := range tt.wantBlockers {
				if card.Blockers[i] != tt.wantBlockers[i] {
					t.Fatalf("Blockers[%d] = %q, want %q; got %#v", i, card.Blockers[i], tt.wantBlockers[i], card.Blockers)
				}
			}
			if len(card.ClearedBlockers) != len(tt.wantCleared) {
				t.Fatalf("ClearedBlockers len = %d, want %d; got %#v", len(card.ClearedBlockers), len(tt.wantCleared), card.ClearedBlockers)
			}
			for i := range tt.wantCleared {
				if card.ClearedBlockers[i] != tt.wantCleared[i] {
					t.Fatalf("ClearedBlockers[%d] = %q, want %q; got %#v", i, card.ClearedBlockers[i], tt.wantCleared[i], card.ClearedBlockers)
				}
			}
		})
	}
}

func TestProjectKanbanCardForIssueUsesProjectTerminalStates(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 20, 19, 0, 0, 0, time.UTC)
	data := DashboardData{
		Kanban: KanbanData{
			TerminalStates: []string{"Done"},
			TerminalStatesByProject: map[string][]string{
				"custom": {"Released"},
			},
		},
	}

	card := projectKanbanCardForIssue(data, telemetry.Issue{
		ID:         "issue",
		Identifier: "digitaldrywood/custom#594",
		ProjectID:  "custom",
		Title:      "Custom terminal dependency",
		State:      "Merging",
		BlockedBy: []telemetry.BlockedRef{
			{Identifier: "digitaldrywood/custom#429", State: "Released"},
		},
	}, "Merging", now.Add(-time.Minute), now)

	if len(card.Blockers) != 0 {
		t.Fatalf("Blockers = %#v, want none for project terminal state", card.Blockers)
	}
	if got, want := strings.Join(card.ClearedBlockers, ", "), "digitaldrywood/custom#429 Released"; got != want {
		t.Fatalf("ClearedBlockers = %q, want %q", got, want)
	}
}

func TestProjectKanbanBoardDoesNotTreatCompletedSessionsAsCurrentDone(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 13, 15, 0, 0, 0, time.UTC)
	board := projectKanbanBoardView(DashboardData{
		Kanban: KanbanData{
			States: []string{"Todo", "Done"},
		},
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

func TestProjectKanbanBoardDoesNotProjectCompletedOpenPRSession(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 20, 13, 0, 0, 0, time.UTC)
	board := projectKanbanBoardView(DashboardData{
		Kanban: KanbanData{
			States: []string{"Backlog", "Todo", "In Progress", "Blocked", "Human Review", "Rework", "Merging", "Done", "Cancelled"},
		},
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			Completed: []telemetry.Completed{
				{
					Issue: telemetry.Issue{
						ID:         "issue-550",
						Identifier: "digitaldrywood/detent#550",
						URL:        "https://github.com/digitaldrywood/detent/issues/550",
						Title:      "Keep completed implementation visible",
						PullRequest: &telemetry.PullRequest{
							Number:           552,
							URL:              "https://github.com/digitaldrywood/detent/pull/552",
							State:            "OPEN",
							CIStatus:         "success",
							CodexReviewState: "clean",
						},
					},
					CompletedAt: now.Add(-2 * time.Minute),
					FinalState:  "completed",
				},
			},
		},
	})

	got := collectKanbanCards(board.Lanes)
	if len(got) != 0 {
		t.Fatalf("kanban cards len = %d, want 0; got %#v", len(got), got)
	}
	if got := collectKanbanLaneTitles(board.AllLanes); containsString(got, "Handoff") {
		t.Fatalf("all lanes = %#v, want no unconfigured Handoff lane", got)
	}
}

func TestProjectKanbanBoardLeavesConfiguredHandoffLaneEmptyForCompletedSession(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 20, 13, 0, 0, 0, time.UTC)
	board := projectKanbanBoardView(DashboardData{
		Kanban: KanbanData{
			States: []string{"Todo", "Handoff", "Done"},
		},
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			Completed: []telemetry.Completed{
				{
					Issue: telemetry.Issue{
						ID:         "issue-550",
						Identifier: "digitaldrywood/detent#550",
						Title:      "Keep completed implementation visible",
						PullRequest: &telemetry.PullRequest{
							Number: 552,
							State:  "OPEN",
						},
					},
					CompletedAt: now.Add(-2 * time.Minute),
					FinalState:  "completed",
				},
			},
		},
	})

	got := collectKanbanCards(board.Lanes)
	if len(got) != 0 {
		t.Fatalf("kanban cards len = %d, want 0; got %#v", len(got), got)
	}
	if got := collectKanbanLaneTitles(board.EmptyLanes); !containsString(got, "Handoff") {
		t.Fatalf("empty lanes = %#v, want configured Handoff lane", got)
	}
}

func TestProjectKanbanBoardUsesTrackerStateWhenCompletedSessionAlsoExists(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 20, 13, 0, 0, 0, time.UTC)
	stageAt := now.Add(-30 * time.Second)
	board := projectKanbanBoardView(DashboardData{
		Kanban: KanbanData{
			States: []string{"Todo", "Human Review", "Merging", "Done"},
		},
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			Pipeline: []telemetry.Issue{
				{
					ID:             "issue-550",
					Identifier:     "digitaldrywood/detent#550",
					Title:          "Keep completed implementation visible",
					State:          "Human Review",
					StageUpdatedAt: &stageAt,
					PullRequest: &telemetry.PullRequest{
						Number:           552,
						State:            "OPEN",
						CIStatus:         "success",
						CodexReviewState: "clean",
					},
				},
			},
			Completed: []telemetry.Completed{
				{
					Issue: telemetry.Issue{
						ID:         "issue-550",
						Identifier: "digitaldrywood/detent#550",
						Title:      "Keep completed implementation visible",
						PullRequest: &telemetry.PullRequest{
							Number:           552,
							State:            "OPEN",
							CIStatus:         "success",
							CodexReviewState: "clean",
						},
					},
					CompletedAt: now.Add(-2 * time.Minute),
					FinalState:  "completed",
				},
			},
		},
	})

	got := collectKanbanCards(board.Lanes)
	want := []kanbanCardSnapshot{
		{Lane: "Human Review", IssueNumber: "#550", Title: "Keep completed implementation visible", CIStatus: "pass", CodexReviewState: "clean", TimeInStage: "30s", Metadata: "PR #552"},
	}
	if len(got) != len(want) {
		t.Fatalf("kanban cards len = %d, want %d; got %#v", len(got), len(want), got)
	}
	if got[0] != want[0] {
		t.Fatalf("kanban card = %#v, want %#v", got[0], want[0])
	}
}

func TestCompletedOpenPRSessionDoesNotCreateWorkflowCards(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 20, 13, 0, 0, 0, time.UTC)
	snapshot := telemetry.Snapshot{
		GeneratedAt: now,
		Completed: []telemetry.Completed{
			{
				Issue: telemetry.Issue{
					ID:         "issue-550",
					Identifier: "digitaldrywood/detent#550",
					Title:      "Keep completed implementation visible",
					PullRequest: &telemetry.PullRequest{
						Number:   552,
						State:    "OPEN",
						CIStatus: "success",
					},
				},
				CompletedAt: now.Add(-2 * time.Minute),
				FinalState:  "completed",
			},
		},
	}

	if got := projectKanbanIssues(snapshot); len(got) != 0 {
		t.Fatalf("projectKanbanIssues() len = %d, want 0; got %#v", len(got), got)
	}
	if got := collectPipelineCards(prPipelineLanes(snapshot)); len(got) != 0 {
		t.Fatalf("pipeline cards len = %d, want 0; got %#v", len(got), got)
	}
}

func TestProjectKanbanBoardHidesTerminalLanesByDefault(t *testing.T) {
	t.Parallel()

	board := projectKanbanBoardView(DashboardData{
		Kanban: KanbanData{
			States:         []string{"Todo", "In Progress", "Done", "Cancelled", "Archived"},
			TerminalStates: []string{"Done", "Cancelled", "Archived"},
		},
		Snapshot: telemetry.Snapshot{
			BoardIssues: []telemetry.Issue{
				{
					ID:         "todo",
					Identifier: "digitaldrywood/detent#496",
					Title:      "Fix empty-lane toggle",
					State:      "Todo",
				},
				{
					ID:         "done",
					Identifier: "digitaldrywood/detent#497",
					Title:      "Completed work",
					State:      "Done",
				},
				{
					ID:         "cancelled",
					Identifier: "digitaldrywood/detent#498",
					Title:      "Cancelled work",
					State:      "Cancelled",
				},
				{
					ID:         "archived",
					Identifier: "digitaldrywood/detent#499",
					Title:      "Archived work",
					State:      "Archived",
				},
			},
		},
	})

	got := map[string]bool{}
	for _, lane := range board.AllLanes {
		got[lane.Title] = lane.DefaultVisible
	}
	want := map[string]bool{
		"Todo":        true,
		"In Progress": false,
		"Done":        false,
		"Cancelled":   false,
		"Archived":    false,
	}
	if len(got) != len(want) {
		t.Fatalf("default visible lanes len = %d, want %d; got %#v", len(got), len(want), got)
	}
	for state, wantVisible := range want {
		if got[state] != wantVisible {
			t.Fatalf("%s DefaultVisible = %t, want %t; got %#v", state, got[state], wantVisible, got)
		}
	}
	if got := collectKanbanLaneTitles(board.Lanes); len(got) != 1 || got[0] != "Todo" {
		t.Fatalf("visible lanes = %#v, want Todo only", got)
	}
	if board.TotalLabel != "4" {
		t.Fatalf("TotalLabel = %q, want 4", board.TotalLabel)
	}
}

func TestPRPipelineLanesMapSnapshotRows(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 1, 15, 0, 0, 0, time.UTC)
	reviewAt := now.Add(-2 * time.Hour)
	mergeAt := now.Add(-15 * time.Minute)
	doneAt := now.Add(-45 * time.Minute)
	oldDoneAt := now.Add(-25 * time.Hour)
	retryAt := now.Add(5 * time.Minute)

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
							Number:                  142,
							URL:                     "https://github.com/digitaldrywood/detent/pull/142",
							CIStatus:                "success",
							CodexReviewState:        "clean",
							HydrationDegradedReason: "stale_cached_pull_request",
							HydrationNextRetryAt:    &retryAt,
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
							CIQueueSeconds:    120,
							CIDurationSeconds: 510,
							QuietWaitSeconds:  600,
							SlowChecks: []telemetry.PullRequestCheck{
								{Name: "GoReleaser Snapshot", DurationSeconds: 247, QueueSeconds: 60},
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
				{Lane: "Human Review", IssueNumber: "#142", Title: "Review lane PR", CIStatus: "pass", CodexReviewState: "clean", TimeInStage: "2h 0m", WaitDetail: "PR hydration using stale cached data until 15:05 UTC"},
				{Lane: "Merging", IssueNumber: "#143", Title: "Merge lane PR", CIStatus: "pending", CodexReviewState: "P2", TimeInStage: "15m 0s", WaitDetail: "quiet 10m 0s / queued 2m 0s / CI 8m 30s / slow GoReleaser Snapshot 4m 7s (queued 1m 0s) / running Test Coverage", MergeLaneStatus: "Queued #1", MergeLaneDetail: "1st in merge queue; waiting for repo merge lane"},
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
				{Lane: "Merging", IssueNumber: "#21", Title: "Merge session", CIStatus: "pending", CodexReviewState: "clean", TimeInStage: "5m 0s", MergeLaneStatus: "Merging now", MergeLaneDetail: "Active merge worker"},
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

func TestPRPipelineLanesShowMergeLaneStatus(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 24, 19, 0, 0, 0, time.UTC)
	activeAt := now.Add(-4 * time.Minute)
	queuedAt := now.Add(-2 * time.Minute)
	snapshot := telemetry.Snapshot{
		GeneratedAt: now,
		Pipeline: []telemetry.Issue{
			{
				ID:             "active",
				Identifier:     "digitaldrywood/detent#143",
				Title:          "Active merge",
				State:          "Merging",
				StageUpdatedAt: &activeAt,
				PullRequest: &telemetry.PullRequest{
					Number: 143,
					URL:    "https://github.com/digitaldrywood/detent/pull/143",
				},
			},
			{
				ID:             "queued",
				Identifier:     "digitaldrywood/detent#144",
				Title:          "Queued merge",
				State:          "Merging",
				StageUpdatedAt: &queuedAt,
				PullRequest: &telemetry.PullRequest{
					Number: 144,
					URL:    "https://github.com/digitaldrywood/detent/pull/144",
				},
			},
		},
		Running: []telemetry.Running{
			{
				Issue: telemetry.Issue{
					ID:         "active",
					Identifier: "digitaldrywood/detent#143",
					Title:      "Active merge",
					State:      "Merging",
					PullRequest: &telemetry.PullRequest{
						Number: 143,
						URL:    "https://github.com/digitaldrywood/detent/pull/143",
					},
				},
				StartedAt: activeAt,
				LastEvent: "squash merging",
			},
		},
	}

	got := collectPipelineCards(prPipelineLanes(snapshot))
	want := []pipelineCardSnapshot{
		{Lane: "Merging", IssueNumber: "#144", Title: "Queued merge", CIStatus: "pending", CodexReviewState: "clean", TimeInStage: "2m 0s", MergeLaneStatus: "Queued #2", MergeLaneDetail: "2nd in merge queue; waiting for repo merge lane behind PR #143"},
		{Lane: "Merging", IssueNumber: "#143", Title: "Active merge", CIStatus: "pending", CodexReviewState: "clean", TimeInStage: "4m 0s", MergeLaneStatus: "Merging now", MergeLaneDetail: "Active merge worker for PR #143; squash merging"},
	}
	if len(got) != len(want) {
		t.Fatalf("pipeline cards len = %d, want %d; got %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("pipeline card %d = %#v, want %#v", i, got[i], want[i])
		}
	}
}

func TestPRPipelineLanesPreserveTrackerRefreshRows(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 20, 13, 0, 0, 0, time.UTC)
	stageAt := now.Add(-2 * time.Minute)
	tests := []struct {
		name  string
		state string
	}{
		{name: "handoff", state: "Handoff"},
		{name: "pending tracker refresh", state: "Pending Tracker Refresh"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := collectPipelineCards(prPipelineLanes(telemetry.Snapshot{
				GeneratedAt: now,
				Pipeline: []telemetry.Issue{
					{
						ID:             "issue-550",
						Identifier:     "digitaldrywood/detent#550",
						Title:          "Keep tracker row visible",
						State:          tt.state,
						StageUpdatedAt: &stageAt,
						PullRequest: &telemetry.PullRequest{
							Number:           552,
							State:            "OPEN",
							CIStatus:         "success",
							CodexReviewState: "clean",
						},
					},
				},
			}))
			want := []pipelineCardSnapshot{
				{Lane: "Human Review", IssueNumber: "#552", Title: "Keep tracker row visible", CIStatus: "pass", CodexReviewState: "clean", TimeInStage: "2m 0s"},
			}
			if len(got) != len(want) {
				t.Fatalf("pipeline cards len = %d, want %d; got %#v", len(got), len(want), got)
			}
			if got[0] != want[0] {
				t.Fatalf("pipeline card = %#v, want %#v", got[0], want[0])
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

func TestPRPipelineLanesDoNotProjectCompletedOpenPRSession(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 20, 13, 0, 0, 0, time.UTC)
	snapshot := telemetry.Snapshot{
		GeneratedAt: now,
		Completed: []telemetry.Completed{
			{
				Issue: telemetry.Issue{
					ID:         "issue-550",
					Identifier: "digitaldrywood/detent#550",
					Title:      "Keep completed implementation visible",
					PullRequest: &telemetry.PullRequest{
						Number:           552,
						URL:              "https://github.com/digitaldrywood/detent/pull/552",
						State:            "OPEN",
						CIStatus:         "success",
						CodexReviewState: "clean",
					},
				},
				CompletedAt: now.Add(-2 * time.Minute),
				FinalState:  "completed",
			},
		},
	}

	got := collectPipelineCards(prPipelineLanes(snapshot))
	if len(got) != 0 {
		t.Fatalf("pipeline cards len = %d, want 0; got %#v", len(got), got)
	}
}

func TestPRPipelineLanesUseTrackerStateWhenCompletedSessionAlsoExists(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 20, 13, 0, 0, 0, time.UTC)
	stageAt := now.Add(-30 * time.Second)
	snapshot := telemetry.Snapshot{
		GeneratedAt: now,
		Pipeline: []telemetry.Issue{
			{
				ID:             "issue-550",
				Identifier:     "digitaldrywood/detent#550",
				Title:          "Keep completed implementation visible",
				State:          "Merging",
				StageUpdatedAt: &stageAt,
				PullRequest: &telemetry.PullRequest{
					Number:           552,
					State:            "OPEN",
					CIStatus:         "success",
					CodexReviewState: "clean",
				},
			},
		},
		Completed: []telemetry.Completed{
			{
				Issue: telemetry.Issue{
					ID:         "issue-550",
					Identifier: "digitaldrywood/detent#550",
					Title:      "Keep completed implementation visible",
					PullRequest: &telemetry.PullRequest{
						Number:           552,
						State:            "OPEN",
						CIStatus:         "success",
						CodexReviewState: "clean",
					},
				},
				CompletedAt: now.Add(-2 * time.Minute),
				FinalState:  "completed",
			},
		},
	}

	got := collectPipelineCards(prPipelineLanes(snapshot))
	want := []pipelineCardSnapshot{
		{Lane: "Merging", IssueNumber: "#552", Title: "Keep completed implementation visible", CIStatus: "pass", CodexReviewState: "clean", TimeInStage: "30s", MergeLaneStatus: "Queued #1", MergeLaneDetail: "1st in merge queue; waiting for repo merge lane"},
	}
	if len(got) != len(want) {
		t.Fatalf("pipeline cards len = %d, want %d; got %#v", len(got), len(want), got)
	}
	if got[0] != want[0] {
		t.Fatalf("pipeline card = %#v, want %#v", got[0], want[0])
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

func TestPRPipelineMergeSummaryTracksActiveAndRecentDurations(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 26, 13, 0, 0, 0, time.UTC)
	activeEnteredAt := now.Add(-9 * time.Minute)
	activeStartedAt := now.Add(-6 * time.Minute)
	queuedEnteredAt := now.Add(-12 * time.Minute)
	recentFastCompletedAt := now.Add(-2 * time.Hour)
	recentSlowCompletedAt := now.Add(-time.Hour)
	oldCompletedAt := now.Add(-25 * time.Hour)

	summary := prPipelineMergeSummary(telemetry.Snapshot{
		GeneratedAt: now,
		Pipeline: []telemetry.Issue{
			{
				ID:         "active",
				Identifier: "digitaldrywood/detent#721",
				State:      "Merging",
				MergeTiming: &telemetry.MergeTiming{
					EnteredMergingAt:          &activeEnteredAt,
					MergeWorkerSlotAcquiredAt: &activeStartedAt,
					MergeStartedAt:            &activeStartedAt,
				},
			},
			{
				ID:         "queued",
				Identifier: "digitaldrywood/detent#722",
				State:      "Merging",
				MergeTiming: &telemetry.MergeTiming{
					EnteredMergingAt: &queuedEnteredAt,
				},
			},
		},
		Completed: []telemetry.Completed{
			{
				Issue: telemetry.Issue{
					ID:         "recent-fast",
					Identifier: "digitaldrywood/detent#723",
					MergeTiming: &telemetry.MergeTiming{
						ActiveMergeDurationSeconds: int64((4 * time.Minute).Seconds()),
						TotalMergingSeconds:        int64((7 * time.Minute).Seconds()),
					},
				},
				CompletedAt: recentFastCompletedAt,
			},
			{
				Issue: telemetry.Issue{
					ID:         "recent-slow",
					Identifier: "digitaldrywood/detent#724",
					MergeTiming: &telemetry.MergeTiming{
						ActiveMergeDurationSeconds: int64((6 * time.Minute).Seconds()),
						TotalMergingSeconds:        int64((9 * time.Minute).Seconds()),
					},
				},
				CompletedAt: recentSlowCompletedAt,
			},
			{
				Issue: telemetry.Issue{
					ID:         "old",
					Identifier: "digitaldrywood/detent#725",
					MergeTiming: &telemetry.MergeTiming{
						ActiveMergeDurationSeconds: int64((20 * time.Minute).Seconds()),
						TotalMergingSeconds:        int64((30 * time.Minute).Seconds()),
					},
				},
				CompletedAt: oldCompletedAt,
			},
		},
	})

	if summary.ActiveElapsed != "6m 0s" || !summary.ActiveWarning {
		t.Fatalf("active summary = (%q, %v), want 6m warning", summary.ActiveElapsed, summary.ActiveWarning)
	}
	if summary.QueueWait != "12m 0s" || !summary.QueueWarning {
		t.Fatalf("queue summary = (%q, %v), want 12m warning", summary.QueueWait, summary.QueueWarning)
	}
	if summary.RecentCount != "2" || summary.ActiveP50 != "4m 0s" || summary.ActiveP90 != "6m 0s" {
		t.Fatalf("active percentiles = %#v, want recent count 2 and active p50/p90", summary)
	}
	if summary.TotalP50 != "7m 0s" || summary.TotalP90 != "9m 0s" {
		t.Fatalf("total percentiles = %#v, want total p50/p90", summary)
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
	MergeLaneStatus  string
	MergeLaneDetail  string
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
				MergeLaneStatus:  card.MergeLaneStatus,
				MergeLaneDetail:  card.MergeLaneDetail,
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
	WaitDetail       string
	Labels           string
	Assignees        string
	Blockers         string
	ClearedBlockers  string
	Metadata         string
	MergeLaneStatus  string
	MergeLaneDetail  string
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
				WaitDetail:       card.WaitDetail,
				Labels:           strings.Join(card.Labels, ", "),
				Assignees:        strings.Join(card.Assignees, ", "),
				Blockers:         strings.Join(card.Blockers, ", "),
				ClearedBlockers:  strings.Join(card.ClearedBlockers, ", "),
				Metadata:         card.PullRequestLabel,
				MergeLaneStatus:  card.MergeLaneStatus,
				MergeLaneDetail:  card.MergeLaneDetail,
			})
		}
	}
	return out
}

func projectKanbanCompactChipLabels(chips []projectKanbanCompactChip) []string {
	out := make([]string, 0, len(chips))
	for _, chip := range chips {
		out = append(out, chip.Label)
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

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
