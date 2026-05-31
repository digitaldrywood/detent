package templates_test

import (
	"bytes"
	"context"
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
						ID:         "issue-35",
						Identifier: "digitaldrywood/symphony#35",
						URL:        "https://github.com/digitaldrywood/symphony/issues/35",
						Title:      "Dashboard templates",
						State:      "In Progress",
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
		"turn completed successfully",
		"+4 -2 (3 files)",
		"162,000",
		"$12.50",
		"$100.00",
		"Codex rate limits",
		"Primary",
		"800",
		"Credits",
		"7.25 credits",
		"available",
		"Token trend",
		`aria-label="Token trend"`,
		"<title>Token trend</title>",
		`stroke="currentColor"`,
		"Input 14:55: 20,000 tokens",
		"Output 15:00: 50,000 tokens",
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
		},
	})

	for _, want := range []string{
		"Throughput",
		"Current completions/min",
		"0.4 completions/min",
		"Runtime",
		"1h 30m",
		"Throughput trend",
		`aria-label="Rolling throughput trend"`,
		"<title>Throughput trend</title>",
		"14:52: 1 completions/min",
		"14:58: 1 completions/min",
		"14:59: 1 completions/min",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("dashboard missing %q:\n%s", want, html)
		}
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
		"Budget disabled",
		"No Codex rate-limit snapshot.",
		"No token trend yet.",
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

func renderDashboard(t *testing.T, data templates.DashboardData) string {
	t.Helper()

	var buf bytes.Buffer
	if err := templates.Dashboard(data).Render(context.Background(), &buf); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	return buf.String()
}
