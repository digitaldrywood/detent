package templates_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/symphony-go/internal/telemetry"
	"github.com/digitaldrywood/symphony-go/internal/web/templates"
)

func TestDashboardRendersTelemetrySnapshot(t *testing.T) {
	t.Parallel()

	perDay := 100.0
	perIssue := 10.0
	now := time.Date(2026, 5, 31, 15, 0, 0, 0, time.UTC)

	html := renderDashboard(t, templates.DashboardData{
		Title:         "Symphony",
		ConnectorName: "github",
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
						Identifier: "digitaldrywood/symphony-go#35",
						URL:        "https://github.com/digitaldrywood/symphony-go/issues/35",
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
		},
		TokenSparkline: []templates.TokenSparklinePoint{
			{Label: "14:55", Value: 20_000},
			{Label: "15:00", Value: 200_000},
		},
	})

	for _, want := range []string{
		"Running",
		"Queue",
		"Blocked",
		"Completed",
		"digitaldrywood/symphony-go#35",
		"Dashboard templates",
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
		"Token sparkline",
		"14:55: 20,000 tokens",
		"15:00: 200,000 tokens",
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
						Identifier: "digitaldrywood/symphony-go#35",
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
		"No active issue sessions.",
		"Budget disabled",
		"No Codex rate-limit snapshot.",
		"No token activity yet.",
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

	if !strings.Contains(html, "Running session details are not available for this snapshot.") {
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
