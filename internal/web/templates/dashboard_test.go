package templates_test

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/symphony/internal/telemetry"
	"github.com/digitaldrywood/symphony/internal/web/templates"
)

func TestDashboardRendersTelemetrySnapshot(t *testing.T) {
	t.Parallel()

	perDay := 100.0
	perIssue := 10.0
	now := time.Date(2026, 5, 31, 15, 0, 0, 0, time.UTC)

	html := renderDashboard(t, templates.DashboardData{
		Title:         "Symphony",
		Version:       "v1.2.3",
		ConnectorName: "github",
		DashboardURL:  "http://localhost:4101",
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			Counts: telemetry.Counts{
				Running:   2,
				Queue:     3,
				Blocked:   1,
				Completed: 4,
			},
			Running: []telemetry.Running{
				{
					Issue: telemetry.Issue{
						ID:          "issue-35",
						Identifier:  "digitaldrywood/symphony#35",
						URL:         "https://github.com/digitaldrywood/symphony/issues/35",
						Title:       "Dashboard templates",
						Description: "Running dashboard template row with enough issue detail to preview.",
						State:       "In Progress",
					},
					SessionID:      "thread-abc123456789",
					TurnCount:      4,
					StartedAt:      now.Add(-9 * time.Minute),
					LastEventAt:    &now,
					LastEvent:      "turn_completed",
					LastMessage:    "turn completed successfully",
					RuntimeSeconds: 540,
					DiffAdded:      4,
					DiffRemoved:    2,
					DiffFiles:      3,
					DiffStatus:     "ok",
					Tokens: telemetry.Tokens{
						Input:  120_000,
						Output: 42_000,
						Total:  162_000,
					},
				},
			},
			Queue: []telemetry.Queued{
				{
					Issue: telemetry.Issue{
						ID:          "issue-36",
						Identifier:  "digitaldrywood/symphony#36",
						URL:         "https://github.com/digitaldrywood/symphony/issues/36",
						Title:       "Retry dashboard",
						Description: "Retry row issue detail preview.",
						State:       "Todo",
					},
					Attempt: 2,
					DueAt: func() *time.Time {
						dueAt := now.Add(2 * time.Minute)
						return &dueAt
					}(),
					Error: "no available orchestrator slots",
				},
			},
			Blocked: []telemetry.Blocked{
				{
					Issue: telemetry.Issue{
						ID:          "issue-37",
						Identifier:  "digitaldrywood/symphony#37",
						URL:         "https://github.com/digitaldrywood/symphony/issues/37",
						Title:       "Blocked dashboard",
						Description: "Blocked row issue detail preview.",
						State:       "Blocked",
					},
					SessionID:   "thread-blocked123456789",
					Error:       "dependency is not merged",
					BlockedAt:   func() *time.Time { blockedAt := now.Add(-3 * time.Minute); return &blockedAt }(),
					LastEventAt: func() *time.Time { lastUpdate := now.Add(-time.Minute); return &lastUpdate }(),
					LastEvent:   "turn_input_required",
					LastMessage: "waiting for operator input",
				},
			},
			Completed: []telemetry.Completed{
				{
					Issue: telemetry.Issue{
						ID:          "issue-38",
						Identifier:  "digitaldrywood/symphony#38",
						URL:         "https://github.com/digitaldrywood/symphony/issues/38",
						Title:       "Completed dashboard",
						Description: "Recent completed session issue detail preview.",
					},
					SessionID:      "thread-completed123456789",
					StartedAt:      now.Add(-12 * time.Minute),
					CompletedAt:    now.Add(-30 * time.Second),
					Turns:          5,
					RuntimeSeconds: 690,
					FinalState:     "Human Review",
					Model:          "gpt-5-codex",
					Tokens: telemetry.Tokens{
						Input:  25_000,
						Output: 5_000,
						Total:  30_000,
					},
				},
			},
			Budget: telemetry.Budget{
				Enabled:          true,
				PerDayMaxUSD:     &perDay,
				PerIssueMaxUSD:   &perIssue,
				CurrentSpendUSD:  12.5,
				ProjectedCostUSD: 0.75,
			},
			RateLimits: &telemetry.RateLimits{
				LimitName: "Codex",
				Primary: &telemetry.RateLimitBucket{
					Remaining:      800,
					Used:           200,
					Limit:          1_000,
					ResetInSeconds: 3_600,
				},
				Credits: &telemetry.RateLimitBucket{
					HasCredits: true,
					Balance:    "7.25",
				},
			},
			Tokens: telemetry.Tokens{
				Input:          150_000,
				Output:         50_000,
				Total:          200_000,
				RuntimeSeconds: 540,
			},
			Throughput: telemetry.TokenThroughput{
				TokensPerSecond: 23.5,
				WindowSeconds:   60,
				Tokens:          1410,
			},
			LifetimeTotals: telemetry.LifetimeTotals{
				Available:      true,
				InputTokens:    750_000,
				OutputTokens:   249_000,
				TotalTokens:    999_000,
				RuntimeSeconds: 7_200,
				Sessions:       12,
				Runs:           3,
			},
			TokenTrend: []telemetry.TokenTrendPoint{
				{
					At:     now.Add(-5 * time.Minute),
					Input:  20_000,
					Output: 4_000,
					Total:  24_000,
				},
				{
					At:     now,
					Input:  150_000,
					Output: 50_000,
					Total:  200_000,
				},
			},
		},
	})

	for _, want := range []string{
		"Running",
		"Queue",
		"Blocked",
		"Completed",
		"v1.2.3",
		"href=\"http://localhost:4101\"",
		"digitaldrywood/symphony#35",
		"Dashboard templates",
		"http://localhost:4101",
		"Running dashboard template row with enough issue detail to preview.",
		"turn completed successfully",
		"+4 -2 (3 files)",
		"162,000",
		"Retry queue",
		"digitaldrywood/symphony#36",
		"Retry dashboard",
		"Retry row issue detail preview.",
		"2",
		"May 31 15:02:00 UTC",
		"no available orchestrator slots",
		"Blocked sessions",
		"digitaldrywood/symphony#37",
		"Blocked dashboard",
		"Blocked row issue detail preview.",
		"May 31 14:57:00 UTC",
		"waiting for operator input",
		"dependency is not merged",
		"Recent sessions",
		"digitaldrywood/symphony#38",
		"Completed dashboard",
		"May 31 14:59:30 UTC",
		"11m 30s / 5 turns",
		"30,000",
		"Human Review",
		"gpt-5-codex",
		"$12.50",
		"$100.00",
		"Codex rate limits",
		"Primary",
		"800",
		"Credits",
		"7.25 credits",
		"available",
		"Token trend",
		"Lifetime totals",
		"999,000",
		"12",
		"2h 0m",
		"3",
		`aria-label="Token trend"`,
		"<title>Token trend</title>",
		`stroke="currentColor"`,
		"Input 14:55: 20,000 tokens",
		"Output 15:00: 50,000 tokens",
		"Board health",
		"State distribution",
		`aria-label="Board state distribution"`,
		"<title>Board state distribution</title>",
		"Todo: 3 issues",
		"In Progress: 2 issues",
		"Review: 1 issues",
		"Done: 3 issues",
		"Cumulative flow",
		`aria-label="Board cumulative flow"`,
		"<title>Board cumulative flow</title>",
		"14:59: 4 issues",
		"Agent activity",
		"Live timeline of running and recently completed Codex sessions.",
		`aria-label="Agent activity timeline"`,
		"Dashboard templates: In Progress from May 31 14:51:00 UTC to Live now",
		"Completed dashboard: Human Review from May 31 14:48:00 UTC to May 31 14:59:30 UTC",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("dashboard missing %q:\n%s", want, html)
		}
	}
}

func TestDashboardRendersReadableAgentTimelineForConcurrentSessions(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 31, 15, 0, 0, 0, time.UTC)
	running := make([]telemetry.Running, 0, 5)
	for i := 0; i < 5; i++ {
		running = append(running, telemetry.Running{
			Issue: telemetry.Issue{
				ID:         "running-" + strconv.Itoa(i),
				Identifier: "DD-RUN-" + strconv.Itoa(i),
				Title:      "Concurrent issue " + strconv.Itoa(i),
				State:      "In Progress",
			},
			StartedAt: now.Add(-time.Duration(10-i) * time.Minute),
		})
	}

	html := renderDashboard(t, templates.DashboardData{
		Title:         "Symphony",
		ConnectorName: "github",
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			Running:     running,
		},
	})

	for _, want := range []string{
		"Agent activity",
		"DD-RUN-0",
		"DD-RUN-1",
		"DD-RUN-2",
		"DD-RUN-3",
		"DD-RUN-4",
		"min-w-[68rem]",
		"Live now",
		"bg-accent",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("dashboard missing %q:\n%s", want, html)
		}
	}
}

func TestDashboardRendersThroughputAndRuntimeTrend(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 31, 15, 0, 0, 0, time.UTC)
	html := renderDashboard(t, templates.DashboardData{
		Title:         "Symphony",
		ConnectorName: "github",
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			Completed: []telemetry.Completed{
				{
					Issue: telemetry.Issue{
						ID:         "issue-41",
						Identifier: "DD-41",
					},
					CompletedAt: now.Add(-30 * time.Second),
				},
				{
					Issue: telemetry.Issue{
						ID:         "issue-42",
						Identifier: "DD-42",
					},
					CompletedAt: now.Add(-2 * time.Minute),
				},
				{
					Issue: telemetry.Issue{
						ID:         "issue-43",
						Identifier: "DD-43",
					},
					CompletedAt: now.Add(-8 * time.Minute),
				},
			},
			Tokens: telemetry.Tokens{
				RuntimeSeconds: 5_400,
			},
			Throughput: telemetry.TokenThroughput{
				TokensPerSecond: 9.5,
				WindowSeconds:   60,
				Tokens:          570,
			},
			TokenTrend: []telemetry.TokenTrendPoint{
				{At: now.Add(-3 * time.Minute), Total: 100},
				{At: now.Add(-2 * time.Minute), Total: 220},
				{At: now.Add(-time.Minute), Total: 400},
			},
		},
	})

	for _, want := range []string{
		"Throughput",
		"Rolling tokens/sec",
		"9.5 tps",
		"Last 1m token throughput",
		"Runtime",
		"1h 30m",
		"Token throughput trend",
		`aria-label="Rolling token throughput trend"`,
		"<title>Token throughput trend</title>",
		"14:58: 2 tps",
		"14:59: 3 tps",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("dashboard missing %q:\n%s", want, html)
		}
	}
}

func TestDashboardRendersBudgetHistoryAndDailyCap(t *testing.T) {
	t.Parallel()

	perDay := 100.0
	now := time.Date(2026, 5, 31, 15, 0, 0, 0, time.UTC)
	html := renderDashboard(t, templates.DashboardData{
		Title:         "Symphony",
		ConnectorName: "github",
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			Budget: telemetry.Budget{
				Enabled:         true,
				PerDayMaxUSD:    &perDay,
				CurrentSpendUSD: 12.5,
				Days: []telemetry.BudgetDay{
					{Date: "2026-05-25", SpendUSD: 4},
					{Date: "2026-05-26", SpendUSD: 6.5},
					{Date: "2026-05-27", SpendUSD: 0},
					{Date: "2026-05-28", SpendUSD: 10},
					{Date: "2026-05-29", SpendUSD: 8.25},
					{Date: "2026-05-30", SpendUSD: 15},
					{Date: "2026-05-31", SpendUSD: 12.5},
				},
			},
		},
	})

	for _, want := range []string{
		"Spend today",
		"$12.50 / $100.00",
		`aria-label="Daily budget usage"`,
		`style="width: 13%;"`,
		"Budget history",
		`aria-label="Spend over the last seven days"`,
		`title="2026-05-25: $4.00"`,
		`title="2026-05-30: $15.00"`,
		`style="height: 100%;"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("dashboard missing %q:\n%s", want, html)
		}
	}
}

func TestDashboardRendersHealthIndicators(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 31, 15, 0, 0, 0, time.UTC)
	html := renderDashboard(t, templates.DashboardData{
		Title:         "Symphony",
		ConnectorName: "github",
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			LifetimeTotals: telemetry.LifetimeTotals{
				DegradedReason: "read runtime store lifetime totals: disk unavailable",
			},
		},
	})

	for _, want := range []string{
		`aria-label="Runtime status"`,
		"Live",
		"Stats degraded",
		`title="read runtime store lifetime totals: disk unavailable"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("dashboard missing %q:\n%s", want, html)
		}
	}
	snapshotIndex := strings.Index(html, `id="snapshot"`)
	healthIndex := strings.Index(html, `aria-label="Runtime status"`)
	if snapshotIndex == -1 || healthIndex == -1 || healthIndex < snapshotIndex {
		t.Fatalf("dashboard health indicators must render inside the SSE snapshot surface:\n%s", html)
	}

	offlineHTML := renderDashboard(t, templates.DashboardData{
		Title:         "Symphony",
		ConnectorName: "github",
	})
	if !strings.Contains(offlineHTML, "Offline") {
		t.Fatalf("dashboard missing offline status:\n%s", offlineHTML)
	}
}

func TestDashboardRendersDensityControls(t *testing.T) {
	t.Parallel()

	html := renderDashboard(t, templates.DashboardData{
		Title:         "Symphony",
		ConnectorName: "github",
		Snapshot: telemetry.Snapshot{
			Running: []telemetry.Running{
				{
					Issue: telemetry.Issue{
						ID:         "density-issue",
						Identifier: "DD-DENSE",
						Title:      "Density controls",
					},
				},
			},
		},
	})

	for _, want := range []string{
		`aria-label="Dashboard density"`,
		`data-density-choice="comfortable"`,
		`data-density-choice="compact"`,
		`aria-pressed="true"`,
		`symphony.dashboard.density`,
		`dashboard-table`,
		`table-fixed`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("dashboard missing %q:\n%s", want, html)
		}
	}
}

func TestDashboardRendersIssueAndSessionControls(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 31, 15, 0, 0, 0, time.UTC)
	html := renderDashboard(t, templates.DashboardData{
		Title:         "Symphony",
		ConnectorName: "github",
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			Running: []telemetry.Running{
				{
					Issue: telemetry.Issue{
						ID:         "issue-running-url",
						Identifier: "digitaldrywood/symphony#91",
						URL:        "https://github.com/digitaldrywood/symphony/issues/91",
						Title:      "Running URL controls",
					},
					SessionID: "thread-running-url",
				},
			},
			Queue: []telemetry.Queued{
				{
					Issue: telemetry.Issue{
						ID:         "issue-retry-url",
						Identifier: "digitaldrywood/symphony#92",
						URL:        "https://github.com/digitaldrywood/symphony/issues/92",
						Title:      "Retry URL controls",
					},
					Attempt: 1,
				},
			},
			Blocked: []telemetry.Blocked{
				{
					Issue: telemetry.Issue{
						ID:         "issue-blocked-url",
						Identifier: "digitaldrywood/symphony#93",
						URL:        "https://github.com/digitaldrywood/symphony/issues/93",
						Title:      "Blocked URL controls",
					},
					SessionID: "thread-blocked-url",
				},
			},
			Completed: []telemetry.Completed{
				{
					Issue: telemetry.Issue{
						ID:         "issue-recent-url",
						Identifier: "digitaldrywood/symphony#94",
						URL:        "https://github.com/digitaldrywood/symphony/issues/94",
						Title:      "Recent URL controls",
					},
					SessionID:   "thread-recent-url",
					CompletedAt: now,
				},
			},
		},
	})

	for _, want := range []string{
		`data-copy="https://github.com/digitaldrywood/symphony/issues/91"`,
		`data-copy="https://github.com/digitaldrywood/symphony/issues/92"`,
		`data-copy="https://github.com/digitaldrywood/symphony/issues/93"`,
		`data-copy="https://github.com/digitaldrywood/symphony/issues/94"`,
		`href="https://github.com/digitaldrywood/symphony/issues/91"`,
		`href="https://github.com/digitaldrywood/symphony/issues/92"`,
		`href="https://github.com/digitaldrywood/symphony/issues/93"`,
		`href="https://github.com/digitaldrywood/symphony/issues/94"`,
		`href="/api/v1/digitaldrywood%2Fsymphony%2391"`,
		`href="/api/v1/digitaldrywood%2Fsymphony%2392"`,
		`href="/api/v1/digitaldrywood%2Fsymphony%2393"`,
		`data-copy="thread-running-url"`,
		`data-copy="thread-blocked-url"`,
		`data-copy="thread-recent-url"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("dashboard missing %q:\n%s", want, html)
		}
	}

	if strings.Contains(html, `href="/api/v1/digitaldrywood%2Fsymphony%2394"`) {
		t.Fatalf("dashboard rendered JSON details for completed session:\n%s", html)
	}
}

func TestDashboardRendersUnknownDiffStatusAsPending(t *testing.T) {
	t.Parallel()

	html := renderDashboard(t, templates.DashboardData{
		Title:         "Symphony",
		ConnectorName: "github",
		Snapshot: telemetry.Snapshot{
			Running: []telemetry.Running{
				{
					Issue: telemetry.Issue{
						ID:         "issue-35",
						Identifier: "digitaldrywood/symphony#35",
						State:      "In Progress",
					},
				},
			},
		},
	})

	if !strings.Contains(html, "pending") {
		t.Fatalf("dashboard missing pending diff status:\n%s", html)
	}
	if strings.Contains(html, ">error<") {
		t.Fatalf("dashboard rendered missing diff status as error:\n%s", html)
	}
}

func TestDashboardRendersEmptyStates(t *testing.T) {
	t.Parallel()

	html := renderDashboard(t, templates.DashboardData{
		Title:         "Symphony",
		ConnectorName: "memory",
	})

	for _, want := range []string{
		"Waiting for first telemetry snapshot.",
		"No active issue sessions.",
		"No issues are currently backing off.",
		"No blocked sessions.",
		"No completed sessions recorded.",
		"No board states recorded.",
		"No board progress history yet.",
		"Budget disabled",
		"No Codex rate-limit snapshot.",
		"No token trend yet.",
		"Lifetime totals unavailable.",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("dashboard missing %q:\n%s", want, html)
		}
	}
}

func TestDashboardIncludesMotionAndThemeHooks(t *testing.T) {
	t.Parallel()

	html := renderDashboard(t, templates.DashboardData{
		Title:         "Symphony",
		ConnectorName: "github",
	})

	for _, want := range []string{
		`document.documentElement.classList.toggle("dark"`,
		`id="snapshot"`,
		`sse-swap="snapshot"`,
		`hx-swap="innerHTML settle:160ms"`,
		`sse-surface`,
		`sse-tick`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("dashboard missing %q:\n%s", want, html)
		}
	}
}

func TestDashboardDistinguishesMissingRunningDetails(t *testing.T) {
	t.Parallel()

	html := renderDashboard(t, templates.DashboardData{
		Title:         "Symphony",
		ConnectorName: "github",
		Snapshot: telemetry.Snapshot{
			Counts: telemetry.Counts{
				Running: 2,
			},
		},
	})

	if !strings.Contains(html, "Running session details are unavailable.") {
		t.Fatalf("dashboard missing running details placeholder:\n%s", html)
	}
	if strings.Contains(html, "No active issue sessions.") {
		t.Fatalf("dashboard rendered empty running state for summary-only snapshot:\n%s", html)
	}
}

func TestDashboardDistinguishesMissingWorkQueueDetails(t *testing.T) {
	t.Parallel()

	html := renderDashboard(t, templates.DashboardData{
		Title:         "Symphony",
		ConnectorName: "github",
		Snapshot: telemetry.Snapshot{
			Counts: telemetry.Counts{
				Queue:     2,
				Blocked:   1,
				Completed: 3,
			},
		},
	})

	for _, want := range []string{
		"Retry queue details are unavailable.",
		"Blocked session details are unavailable.",
		"Completed session details are unavailable.",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("dashboard missing %q:\n%s", want, html)
		}
	}

	for _, empty := range []string{
		"No issues are currently backing off.",
		"No blocked sessions.",
		"No completed sessions recorded.",
	} {
		if strings.Contains(html, empty) {
			t.Fatalf("dashboard rendered empty state %q for summary-only snapshot:\n%s", empty, html)
		}
	}
}

func renderDashboard(t *testing.T, data templates.DashboardData) string {
	t.Helper()

	var buf bytes.Buffer
	if err := templates.Dashboard(data).Render(context.Background(), &buf); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	return buf.String()
}
