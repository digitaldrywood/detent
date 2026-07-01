package templates_test

import (
	"bytes"
	"context"
	"io"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"

	"github.com/digitaldrywood/detent/internal/projectcolor"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web/templates"
)

const projectKanbanFixedLaneClass = `project-kanban-lane grid min-h-[12rem] w-[var(--project-kanban-lane-width,18rem)] min-w-[var(--project-kanban-lane-width,18rem)] max-w-[var(--project-kanban-lane-width,18rem)] basis-[var(--project-kanban-lane-width,18rem)] shrink-0 content-start overflow-hidden rounded-md border border-border bg-muted/60 p-2`

func TestDashboardRendersTelemetrySnapshot(t *testing.T) {
	t.Parallel()

	perDay := 100.0
	perIssue := 10.0
	now := time.Date(2026, 5, 31, 15, 0, 0, 0, time.UTC)
	pipelineUpdatedAt := now.Add(-20 * time.Minute)
	leaseRenewedAt := now.Add(-30 * time.Second)
	leaseExpiresAt := now.Add(90 * time.Second)

	html := renderDashboard(t, templates.DashboardData{
		Title:         "Detent",
		Version:       "v1.2.3",
		ConnectorName: "github",
		DashboardURL:  "http://localhost:4101",
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			Instance: telemetry.Instance{
				Name:                    "release-captain",
				GitHubLogin:             "detent-bot",
				AuthorizationScope:      "assignee in @me (detent-bot, release-captain)",
				AuthorizationConfigured: true,
			},
			Counts: telemetry.Counts{
				Running:   2,
				Queue:     3,
				Blocked:   1,
				Completed: 4,
			},
			Pipeline: []telemetry.Issue{
				{
					ID:         "pipeline-39",
					Identifier: "digitaldrywood/detent#39",
					URL:        "https://github.com/digitaldrywood/detent/issues/39",
					Title:      "Review pipeline lane",
					State:      "Human Review",
					UpdatedAt:  &pipelineUpdatedAt,
					PullRequest: &telemetry.PullRequest{
						Number:           142,
						URL:              "https://github.com/digitaldrywood/detent/pull/142",
						CIStatus:         "pass",
						CodexReviewState: "P2",
					},
				},
			},
			Running: []telemetry.Running{
				{
					Issue: telemetry.Issue{
						ID:          "issue-35",
						Identifier:  "digitaldrywood/detent#35",
						URL:         "https://github.com/digitaldrywood/detent/issues/35",
						Title:       "Dashboard templates",
						Description: "Running dashboard template row with enough issue detail to preview.",
						State:       "In Progress",
						Owner:       "alpha",
						LeaseRenewedAt: func() *time.Time {
							return &leaseRenewedAt
						}(),
						LeaseExpiresAt: func() *time.Time {
							return &leaseExpiresAt
						}(),
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
						Identifier:  "digitaldrywood/detent#36",
						URL:         "https://github.com/digitaldrywood/detent/issues/36",
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
						Identifier:  "digitaldrywood/detent#37",
						URL:         "https://github.com/digitaldrywood/detent/issues/37",
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
						Identifier:  "digitaldrywood/detent#38",
						URL:         "https://github.com/digitaldrywood/detent/issues/38",
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
				GitHubGraphQL: &telemetry.RateLimitBucket{
					Remaining: 4880,
					Used:      120,
					Limit:     5_000,
					Cost:      8,
					ResetAt:   func() *time.Time { resetAt := now.Add(time.Hour); return &resetAt }(),
				},
				GitHubREST: &telemetry.RateLimitBucket{
					Remaining: 4878,
					Used:      122,
					Limit:     5_000,
					Cost:      2,
					ResetAt:   func() *time.Time { resetAt := now.Add(time.Hour); return &resetAt }(),
				},
				GraphQLCost: &telemetry.GraphQLCost{
					TotalQueries: 3,
					TotalCost:    8,
					Contributors: []telemetry.GraphQLCostContributor{
						{QueryType: "candidate_issues", Count: 2, Cost: 5},
						{QueryType: "running_states", Count: 1, Cost: 3},
					},
				},
				RESTUsage: &telemetry.RESTUsage{
					TotalRequests: 2,
					RateLimited:   true,
					Contributors: []telemetry.RESTUsageContributor{
						{EndpointFamily: "label issues", Count: 1, Remaining: 4879, Limit: 5_000, LastStatus: 200},
						{EndpointFamily: "issue comments", Count: 1, Remaining: 4878, Limit: 5_000, RetryAfterMS: 120_000, RateLimited: true, LastStatus: 403},
					},
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
			CycleTime: telemetry.CycleTimeReport{
				Available:      true,
				AverageSeconds: int64(90 * time.Minute / time.Second),
				Issues: []telemetry.CycleTimeIssue{
					{Key: "digitaldrywood/detent#139", DurationSeconds: int64(45 * time.Minute / time.Second)},
					{Key: "digitaldrywood/detent#215", DurationSeconds: int64(2 * time.Hour / time.Second)},
				},
				Buckets: []telemetry.CycleTimeBucket{
					{Label: "<1h", Count: 1},
					{Label: "1-4h", Count: 1},
				},
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
		`href="/"`,
		`href="/reports"`,
		`href="/settings"`,
		"release-captain",
		"detent-bot",
		"assignee in @me (detent-bot, release-captain)",
		"digitaldrywood/detent#35",
		"Dashboard templates",
		"Running dashboard template row with enough issue detail to preview.",
		"Owner alpha",
		"Lease expires May 31 15:01:30 UTC",
		"turn completed successfully",
		"+4 -2 (3 files)",
		"162,000",
		"Retry queue",
		"digitaldrywood/detent#36",
		"Retry dashboard",
		"Retry row issue detail preview.",
		"2",
		"May 31 15:02:00 UTC",
		"no available orchestrator slots",
		"Blocked sessions",
		"digitaldrywood/detent#37",
		"Blocked dashboard",
		"Blocked row issue detail preview.",
		"May 31 14:57:00 UTC",
		"waiting for operator input",
		"dependency is not merged",
		"Recent sessions",
		"digitaldrywood/detent#38",
		"Completed dashboard",
		"May 31 14:59:30 UTC",
		"11m 30s / 5 turns",
		"30,000",
		"Human Review",
		"gpt-5-codex",
		"$12.50",
		"$100.00",
		"Rate limits",
		"Primary",
		"800",
		"GitHub GraphQL",
		"4,880",
		"cost 8",
		"GitHub REST",
		"4,878",
		"REST budget",
		"2 requests",
		"label issues",
		"issue comments",
		"rate limited",
		"GraphQL budget",
		"4,880 / 5,000",
		"1h 0m to reset",
		"3 queries",
		"candidate_issues",
		"5 points",
		"running_states",
		"3 points",
		"Credits",
		"7.25 credits",
		"available",
		"Token trend",
		"Lifetime totals",
		"999,000",
		"12",
		"2h 0m",
		"3",
		"Cycle time",
		"2 completed",
		"1h 30m",
		`aria-label="Cycle time histogram"`,
		"<title>Cycle time histogram</title>",
		"<title>&lt;1h: 1 issues</title>",
		"<title>1-4h: 1 issues</title>",
		`aria-label="Token trend"`,
		"<title>Token trend</title>",
		`stroke="currentColor"`,
		"Input 14:55: 20,000 tokens",
		"Output 15:00: 50,000 tokens",
		"Board health",
		"Current issue states",
		`aria-label="Current issue state distribution"`,
		"<title>Current issue state distribution</title>",
		"Todo: 3 issues",
		"In Progress: 2 issues",
		"Review: 1 issues",
		"Session completions",
		`aria-label="Completed sessions over time"`,
		"<title>Completed sessions over time</title>",
		"14:59: 4 sessions",
		"PR pipeline",
		"Live merge-train lanes",
		"#142",
		"Review pipeline lane",
		"CI pass",
		"Codex P2",
		"20m 0s",
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

func TestDashboardRendersTrackerStatusDriftWarning(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 1, 15, 0, 0, 0, time.UTC)
	html := renderDashboard(t, templates.DashboardData{
		Title:         "Detent",
		ConnectorName: "github",
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			Refresh: telemetry.Refresh{
				LastRefreshAt: &now,
			},
			Counts: telemetry.Counts{Running: 1},
			Running: []telemetry.Running{{
				Issue: telemetry.Issue{
					ID:         "issue-running",
					Identifier: "digitaldrywood/detent#782",
					Title:      "Normal board row remains visible",
					State:      "In Progress",
				},
			}},
			TrackerDrift: telemetry.TrackerDrift{
				UntrackedOpen: []telemetry.Issue{{
					ID:         "I_771",
					Identifier: "digitaldrywood/detent#771",
					URL:        "https://github.com/digitaldrywood/detent/issues/771",
					Title:      "Untracked issue",
					Labels:     []string{"bug"},
				}},
				OpenTerminal: []telemetry.Issue{{
					ID:         "I_583",
					Identifier: "digitaldrywood/detent#583",
					URL:        "https://github.com/digitaldrywood/detent/issues/583",
					Title:      "Done but open",
					State:      "Done",
					Labels:     []string{"detent:done"},
				}},
			},
		},
	})

	for _, want := range []string{
		`aria-label="Tracker status drift"`,
		"Tracker status drift",
		"2 cleanup issues",
		"Untracked open issues",
		"Open terminal issues",
		`href="https://github.com/digitaldrywood/detent/issues/771"`,
		"#771",
		"Untracked issue",
		`href="https://github.com/digitaldrywood/detent/issues/583"`,
		"#583",
		"Done but open",
		"detent:done",
		"Normal board row remains visible",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("dashboard missing %q:\n%s", want, html)
		}
	}
}

func TestDashboardRendersSidebarGitHubAPIHealth(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 25, 14, 30, 0, 0, time.UTC)
	lastRefresh := now.Add(-30 * time.Second)
	nextRefresh := now.Add(90 * time.Second)
	resetAt := now.Add(30 * time.Minute)
	backoffUntil := now.Add(5 * time.Minute)
	html := renderDashboard(t, templates.DashboardData{
		Title:         "Detent",
		ConnectorName: "github",
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			Refresh: telemetry.Refresh{
				LastRefreshAt: &lastRefresh,
				NextRefreshAt: &nextRefresh,
			},
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
			LifetimeTotals: telemetry.LifetimeTotals{Available: true},
		},
	})

	for _, want := range []string{
		`id="github-api-health"`,
		`sse-swap="github-api-health"`,
		`hx-swap="morph:outerHTML"`,
		`aria-label="Health: GitHub secondary throttle active for pull requests/check runs.`,
		`data-preserve-details="github-api-health"`,
		"Health",
		"Backoff",
		"GitHub secondary throttle active for pull requests/check runs",
		"Primary REST quota is healthy: 4,878/5,000 remaining",
		"GitHub secondary endpoint throttle is active for pull requests/check runs.",
		"Retrying at 14:35 UTC",
		"REST primary",
		"GraphQL primary",
		"4,878 / 5,000 remaining",
		"4,880 / 5,000 remaining",
		"Last tracker refresh",
		"14:29:30 UTC",
		"Next tracker refresh",
		"14:31:30 UTC",
		"pull requests",
		"check runs",
		"429",
		"retry 14:35 UTC",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("dashboard missing GitHub API health marker %q:\n%s", want, html)
		}
	}
	assertTemplateHealthInSidebar(t, html)
	if strings.Contains(html, `class="relative w-full max-w-full rounded-md border text-sm shadow-sm sm:w-fit`) {
		t.Fatalf("dashboard rendered page-level GitHub API health banner:\n%s", html)
	}
}

func TestDashboardRendersSidebarGitHubAPIHealthStateMetadata(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 25, 14, 30, 0, 0, time.UTC)
	resetAt := now.Add(30 * time.Minute)
	backoffUntil := now.Add(5 * time.Minute)

	tests := []struct {
		name      string
		snapshot  telemetry.Snapshot
		wantState string
		wantLabel string
	}{
		{
			name:      "unknown",
			snapshot:  telemetry.Snapshot{GeneratedAt: now},
			wantState: "unknown",
			wantLabel: "GitHub API unknown",
		},
		{
			name: "healthy",
			snapshot: telemetry.Snapshot{
				GeneratedAt: now,
				RateLimits: &telemetry.RateLimits{
					GitHubREST:    &telemetry.RateLimitBucket{Remaining: 4878, Used: 122, Limit: 5000, ResetAt: &resetAt},
					GitHubGraphQL: &telemetry.RateLimitBucket{Remaining: 4880, Used: 120, Limit: 5000, ResetAt: &resetAt},
				},
			},
			wantState: "healthy",
			wantLabel: "GitHub API healthy",
		},
		{
			name: "warning",
			snapshot: telemetry.Snapshot{
				GeneratedAt: now,
				RateLimits: &telemetry.RateLimits{
					GitHubREST:    &telemetry.RateLimitBucket{Remaining: 240, Used: 4760, Limit: 5000, ResetAt: &resetAt},
					GitHubGraphQL: &telemetry.RateLimitBucket{Remaining: 3200, Used: 1800, Limit: 5000, ResetAt: &resetAt},
				},
			},
			wantState: "warning",
			wantLabel: "GitHub primary quota low",
		},
		{
			name: "secondary backoff",
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
						},
					},
				},
			},
			wantState: "backoff",
			wantLabel: "GitHub secondary throttle active for pull requests",
		},
		{
			name: "primary exhausted",
			snapshot: telemetry.Snapshot{
				GeneratedAt: now,
				RateLimits: &telemetry.RateLimits{
					GitHubREST:    &telemetry.RateLimitBucket{Remaining: 0, Used: 5000, Limit: 5000, ResetAt: &resetAt},
					GitHubGraphQL: &telemetry.RateLimitBucket{Remaining: 4880, Used: 120, Limit: 5000, ResetAt: &resetAt},
				},
			},
			wantState: "exhausted",
			wantLabel: "GitHub primary quota exhausted",
		},
		{
			name: "graphql exhausted status",
			snapshot: telemetry.Snapshot{
				GeneratedAt: now,
				RateLimits: &telemetry.RateLimits{
					GitHubREST:    &telemetry.RateLimitBucket{Remaining: 4878, Used: 122, Limit: 5000, ResetAt: &resetAt},
					GitHubGraphQL: &telemetry.RateLimitBucket{Status: telemetry.RateLimitStatusExhausted},
				},
			},
			wantState: "exhausted",
			wantLabel: "GitHub primary quota exhausted",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			html := renderDashboard(t, templates.DashboardData{
				Title:         "Detent",
				ConnectorName: "github",
				Snapshot:      tt.snapshot,
			})

			for _, want := range []string{
				`id="github-api-health"`,
				`data-preserve-details="github-api-health"`,
				`data-github-api-health-state="` + tt.wantState + `"`,
				tt.wantLabel,
				"Health",
				gitHubAPIHealthStateTestLabel(tt.wantState),
			} {
				if !strings.Contains(html, want) {
					t.Fatalf("dashboard missing GitHub API health marker %q:\n%s", want, html)
				}
			}
			assertTemplateHealthInSidebar(t, html)
		})
	}
}

func TestProjectRunsSnapshotRendersIssueAndPullRequestActions(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 21, 15, 0, 0, 0, time.UTC)
	html := renderProjectRunsSnapshot(t, templates.DashboardData{
		ProjectID:   "detent",
		ProjectName: "Detent",
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			Running: []telemetry.Running{
				{
					Issue: telemetry.Issue{
						ID:         "issue-only",
						Identifier: "digitaldrywood/detent#601",
						URL:        "https://github.com/digitaldrywood/detent/issues/601",
						Title:      "Issue-only run",
						State:      "In Progress",
					},
					StartedAt: now.Add(-3 * time.Minute),
				},
				{
					Issue: telemetry.Issue{
						ID:         "pr-only",
						Identifier: "digitaldrywood/detent#602",
						Title:      "PR-only run",
						State:      "Human Review",
						PullRequest: &telemetry.PullRequest{
							Number: 702,
							URL:    "https://github.com/digitaldrywood/detent/pull/702",
						},
					},
					StartedAt: now.Add(-2 * time.Minute),
				},
			},
			Completed: []telemetry.Completed{
				{
					Issue: telemetry.Issue{
						ID:         "issue-plus-pr",
						Identifier: "digitaldrywood/detent#603",
						URL:        "https://github.com/digitaldrywood/detent/issues/603",
						Title:      "Issue plus PR run",
						PullRequest: &telemetry.PullRequest{
							Number: 703,
							URL:    "https://github.com/digitaldrywood/detent/pull/703",
						},
					},
					SessionID:   "thread-issue-plus-pr",
					StartedAt:   now.Add(-6 * time.Minute),
					CompletedAt: now.Add(-1 * time.Minute),
					FinalState:  "Human Review",
				},
			},
		},
	})

	agentActivity := dashboardSection(t, html, "Agent activity", "PR pipeline")
	for _, want := range []string{
		`href="https://github.com/digitaldrywood/detent/issues/601" target="_blank" rel="noopener noreferrer" title="Open issue digitaldrywood/detent#601" aria-label="Open issue digitaldrywood/detent#601"`,
		`href="https://github.com/digitaldrywood/detent/pull/702" target="_blank" rel="noopener noreferrer" title="Open PR #702" aria-label="Open PR #702"`,
		`href="https://github.com/digitaldrywood/detent/issues/603" target="_blank" rel="noopener noreferrer" title="Open issue digitaldrywood/detent#603" aria-label="Open issue digitaldrywood/detent#603"`,
		`href="https://github.com/digitaldrywood/detent/pull/703" target="_blank" rel="noopener noreferrer" title="Open PR #703" aria-label="Open PR #703"`,
	} {
		if !strings.Contains(agentActivity, want) {
			t.Fatalf("agent activity missing %q:\n%s", want, agentActivity)
		}
	}
	for href, wantCount := range map[string]int{
		`href="https://github.com/digitaldrywood/detent/issues/601"`: 2,
		`href="https://github.com/digitaldrywood/detent/pull/702"`:   2,
		`href="https://github.com/digitaldrywood/detent/issues/603"`: 2,
		`href="https://github.com/digitaldrywood/detent/pull/703"`:   2,
	} {
		if got := strings.Count(agentActivity, href); got != wantCount {
			t.Fatalf("agent activity %s count = %d, want %d:\n%s", href, got, wantCount, agentActivity)
		}
	}
	if strings.Contains(agentActivity, `href="https://github.com/digitaldrywood/detent/issues/602"`) {
		t.Fatalf("PR-only agent activity row rendered an issue link:\n%s", agentActivity)
	}

	recentSessions := dashboardTail(t, html, "Recent sessions")
	for href, wantCount := range map[string]int{
		`href="https://github.com/digitaldrywood/detent/issues/603"`: 2,
		`href="https://github.com/digitaldrywood/detent/pull/703"`:   2,
	} {
		if got := strings.Count(recentSessions, href); got != wantCount {
			t.Fatalf("recent sessions %s count = %d, want %d:\n%s", href, got, wantCount, recentSessions)
		}
	}
	if !strings.Contains(recentSessions, `data-copy="https://github.com/digitaldrywood/detent/issues/603"`) {
		t.Fatalf("recent sessions removed issue copy action:\n%s", recentSessions)
	}
}

func TestDashboardRendersBlockedIssueWithCompletedSessionAsCurrentBlocked(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 13, 15, 0, 0, 0, time.UTC)
	html := renderDashboard(t, templates.DashboardData{
		Title:         "Detent",
		ConnectorName: "github",
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			Counts: telemetry.Counts{
				Blocked:   1,
				Completed: 1,
			},
			Blocked: []telemetry.Blocked{
				{
					Issue: telemetry.Issue{
						ID:         "issue-396",
						Identifier: "digitaldrywood/detent#396",
						Title:      "Blocked after completed session",
						State:      "Blocked",
					},
					Error: "blocked by project status",
				},
			},
			Completed: []telemetry.Completed{
				{
					Issue: telemetry.Issue{
						ID:         "issue-396",
						Identifier: "digitaldrywood/detent#396",
						Title:      "Blocked after completed session",
					},
					StartedAt:   now.Add(-15 * time.Minute),
					CompletedAt: now.Add(-5 * time.Minute),
					FinalState:  "completed",
				},
			},
		},
	})

	blockedSection := dashboardSection(t, html, "Blocked sessions", "Recent sessions")
	for _, want := range []string{
		"digitaldrywood/detent#396",
		"Blocked after completed session",
		"Blocked",
		"blocked by project status",
	} {
		if !strings.Contains(blockedSection, want) {
			t.Fatalf("blocked section missing %q:\n%s", want, blockedSection)
		}
	}

	doneLane := dashboardSection(t, html, `aria-label="Done today lane"`, "Running issues")
	if !strings.Contains(doneLane, "No PRs finished today.") {
		t.Fatalf("done lane should be empty:\n%s", doneLane)
	}
	if strings.Contains(doneLane, "digitaldrywood/detent#396") {
		t.Fatalf("done lane rendered blocked issue from completed session:\n%s", doneLane)
	}

	recentSection := dashboardSection(t, html, "Recent sessions", "Token throughput")
	for _, want := range []string{
		"digitaldrywood/detent#396",
		"Final state",
		"completed",
	} {
		if !strings.Contains(recentSection, want) {
			t.Fatalf("recent session section missing %q:\n%s", want, recentSection)
		}
	}

	boardSection := dashboardSection(t, html, "Board health", "Cycle time")
	for _, want := range []string{
		"Current issue states",
		"Blocked: 1 issues",
		"Session completions",
		"14:55: 1 sessions",
	} {
		if !strings.Contains(boardSection, want) {
			t.Fatalf("board section missing %q:\n%s", want, boardSection)
		}
	}
	if strings.Contains(boardSection, "Done: 1 issues") {
		t.Fatalf("board state distribution counted completed session as current Done:\n%s", boardSection)
	}
}

func TestDashboardPrioritizesOperationalSections(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 1, 15, 0, 0, 0, time.UTC)
	updatedAt := now.Add(-time.Minute)
	html := renderDashboard(t, templates.DashboardData{
		Title:         "Detent",
		ConnectorName: "github",
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			Pipeline: []telemetry.Issue{
				{
					ID:         "review",
					Identifier: "digitaldrywood/detent#284",
					Title:      "Review dashboard order",
					State:      "Human Review",
					UpdatedAt:  &updatedAt,
					PullRequest: &telemetry.PullRequest{
						Number:   284,
						CIStatus: "success",
					},
				},
			},
			Running: []telemetry.Running{
				{
					Issue: telemetry.Issue{
						ID:         "running",
						Identifier: "digitaldrywood/detent#285",
						Title:      "Active dashboard work",
						State:      "In Progress",
					},
					StartedAt: now.Add(-5 * time.Minute),
				},
			},
		},
		Projects: []templates.ProjectSmallMultiple{
			{
				ID:                        "detent",
				Name:                      "Detent",
				ThroughputTokensPerSecond: 1.5,
				Samples: []templates.ProjectSmallMultipleSample{
					{At: now, ThroughputTokensPerSecond: 1.5},
				},
			},
		},
	})

	for _, want := range []string{
		"dashboard-topbar",
		`aria-label="Dashboard health"`,
		`aria-label="Agent activity timeline"`,
		`aria-label="Pull request pipeline"`,
		`aria-label="Fleet grid"`,
		`aria-label="Board health"`,
		`aria-label="Cycle time"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("dashboard missing %q:\n%s", want, html)
		}
	}
	if strings.Contains(html, ">Operations dashboard</h1>") {
		t.Fatalf("dashboard should use the slim navbar, not the oversized dashboard h1:\n%s", html)
	}
	for _, want := range []string{
		`aria-label="Detent dashboard"`,
		`data-tui-sidebar-layout`,
		`data-tui-sidebar-collapsible="icon"`,
		`data-tui-sidebar-trigger`,
		`data-tui-sidebar-target="dashboard-sidebar"`,
		`href="/"`,
		">Detent</span>",
		`<h1 class="sr-only">Fleet</h1>`,
		`href="/reports"`,
		`href="/settings"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("dashboard slim navbar missing %q:\n%s", want, html)
		}
	}
	for _, forbidden := range []string{
		`>Dashboard</a>`,
		`aria-current="page">Dashboard</a>`,
		`dashboard-topbar-chip`,
		`href="http://localhost:4000"`,
	} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("dashboard slim navbar rendered forbidden marker %q:\n%s", forbidden, html)
		}
	}

	healthIndex := strings.Index(html, `aria-label="Dashboard health"`)
	activityIndex := strings.Index(html, `aria-label="Agent activity timeline"`)
	metricsIndex := strings.Index(html, `aria-label="Dashboard statistics"`)
	pipelineIndex := strings.Index(html, `aria-label="Pull request pipeline"`)
	projectsIndex := strings.Index(html, `aria-label="Fleet grid"`)
	boardIndex := strings.Index(html, `aria-label="Board health"`)
	cycleIndex := strings.Index(html, `aria-label="Cycle time"`)
	if healthIndex < 0 || metricsIndex < 0 || activityIndex < 0 || pipelineIndex < 0 || projectsIndex < 0 || boardIndex < 0 || cycleIndex < 0 {
		t.Fatalf("dashboard section indexes missing: health=%d metrics=%d activity=%d pipeline=%d projects=%d board=%d cycle=%d\n%s", healthIndex, metricsIndex, activityIndex, pipelineIndex, projectsIndex, boardIndex, cycleIndex, html)
	}
	if healthIndex >= activityIndex || activityIndex >= metricsIndex || metricsIndex >= projectsIndex || projectsIndex >= pipelineIndex || pipelineIndex >= boardIndex || boardIndex >= cycleIndex {
		t.Fatalf("dashboard sections are not ordered as health, activity, stats, projects, pipeline, analytics: health=%d activity=%d metrics=%d projects=%d pipeline=%d board=%d cycle=%d\n%s", healthIndex, activityIndex, metricsIndex, projectsIndex, pipelineIndex, boardIndex, cycleIndex, html)
	}
}

func TestDashboardRendersBoundedPRPipelineLanes(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 1, 15, 0, 0, 0, time.UTC)
	updatedAt := now.Add(-10 * time.Minute)
	html := renderDashboard(t, templates.DashboardData{
		Title:         "Detent",
		ConnectorName: "github",
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			Pipeline: []telemetry.Issue{
				{
					ID:         "review",
					Identifier: "digitaldrywood/detent#284",
					Title:      "Bounded pipeline lane",
					State:      "Human Review",
					UpdatedAt:  &updatedAt,
					PullRequest: &telemetry.PullRequest{
						Number:   284,
						CIStatus: "success",
					},
				},
			},
		},
	})

	for _, want := range []string{
		`aria-label="Human Review lane"`,
		`aria-label="Merging lane"`,
		`aria-label="Done today lane"`,
		"pr-pipeline-lane-scroll",
		"max-h-[24rem]",
		"overflow-y-auto",
		"Nothing is merging.",
		"No PRs finished today.",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("dashboard missing %q:\n%s", want, html)
		}
	}
}

func TestDashboardRendersProjectKanbanReadOnlyBoard(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 1, 15, 0, 0, 0, time.UTC)
	backlogAt := now.Add(-9 * time.Minute)
	todoAt := now.Add(-8 * time.Minute)
	reviewAt := now.Add(-3 * time.Minute)
	html := renderProjectKanbanPage(t, templates.DashboardData{
		Title:         "Detent",
		ConnectorName: "github",
		ProjectID:     "detent",
		ProjectName:   "Detent",
		Kanban: templates.KanbanData{
			Mode:   "read_only",
			States: []string{"Backlog", "Todo", "In Progress", "Human Review", "Done"},
		},
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			BoardIssues: []telemetry.Issue{
				{
					ID:             "backlog",
					Identifier:     "digitaldrywood/detent#476",
					ProjectID:      "detent",
					Title:          "Backlog board item",
					State:          "Backlog",
					StageUpdatedAt: &backlogAt,
				},
			},
			Pipeline: []telemetry.Issue{
				{
					ID:             "review",
					Identifier:     "digitaldrywood/detent#478",
					ProjectID:      "detent",
					Title:          "Render per-project Kanban board",
					State:          "Human Review",
					Labels:         []string{"enhancement", "ui"},
					Assignees:      []string{"release-captain"},
					BlockedBy:      []telemetry.BlockedRef{{Identifier: "digitaldrywood/detent#415", State: "Done"}},
					StageUpdatedAt: &reviewAt,
					PullRequest: &telemetry.PullRequest{
						Number:           512,
						URL:              "https://github.com/digitaldrywood/detent/pull/512",
						CIStatus:         "success",
						CodexReviewState: "P2",
					},
				},
			},
			Queue: []telemetry.Queued{
				{
					Issue: telemetry.Issue{
						ID:             "todo",
						Identifier:     "digitaldrywood/detent#477",
						ProjectID:      "detent",
						Title:          "Read workflow state",
						StageUpdatedAt: &todoAt,
					},
					Attempt: 1,
				},
			},
		},
	})

	if !strings.Contains(html, `id="snapshot" class="sse-surface grid min-w-0 content-start" sse-swap="snapshot" hx-swap="morph:innerHTML"`) {
		t.Fatalf("dashboard snapshot must keep morph swap:\n%s", html)
	}

	for _, want := range []string{
		`aria-label="Project views"`,
		`data-dashboard-view="overview"`,
		`data-dashboard-view="kanban"`,
		`data-dashboard-view="runs"`,
		`data-dashboard-view="configuration"`,
		`data-dashboard-view="diagnostics"`,
		`href="/projects/detent/kanban"`,
		`href="/projects/detent/runs"`,
		`href="/projects/detent/configuration"`,
		`href="/projects/detent/diagnostics"`,
		`id="project-kanban"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("project navigation missing %q:\n%s", want, html)
		}
	}
	if strings.Contains(html, `href="/projects/detent#project-kanban"`) {
		t.Fatalf("project navigation still links Kanban to dashboard anchor:\n%s", html)
	}
	kanbanIndex := strings.Index(html, `data-dashboard-view="kanban"`)
	if kanbanIndex < 0 {
		t.Fatalf("project Kanban nav missing:\n%s", html)
	}
	kanbanNav := html[max(0, kanbanIndex-256):min(len(html), kanbanIndex+256)]
	for _, want := range []string{
		`data-tui-sidebar-active="true"`,
		`aria-current="page"`,
	} {
		if !strings.Contains(kanbanNav, want) {
			t.Fatalf("project Kanban nav missing active marker %q:\n%s", want, kanbanNav)
		}
	}

	section := projectKanbanSection(t, html)
	for _, want := range []string{
		"Project Kanban",
		"Read-only",
		"This board is currently read-only.",
		"To move cards from Detent, enable Kanban integration in WORKFLOW.md under the existing server block.",
		"Run",
		"detent doctor --port 0",
		"first and confirm status-label, issue-field, and ProjectV2 write checks pass.",
		`aria-label="Kanban integration config snippet"`,
		`kanban:
  mode: integration`,
		"Add",
		"allowed_transitions",
		"when you want broad manual status editing instead of conservative defaults.",
		`data-project-kanban-visibility-key="project:detent`,
		`data-project-kanban-visibility-menu`,
		`data-preserve-details="project-kanban-visibility-project-detent"`,
		`data-project-kanban-visibility-checkbox`,
		`name="visible_lane" value="in-progress"`,
		`data-project-kanban-visibility-action="all"`,
		`data-project-kanban-visibility-action="reset"`,
		`Reset to defaults`,
		`data-project-kanban-visibility-row`,
		`data-project-kanban-visibility-status`,
		`data-project-kanban-visibility-reset`,
		`data-project-kanban-visibility-close`,
		`aria-label="Close lane visibility"`,
		`data-project-kanban-lane-title="Todo"`,
		`data-project-kanban-lane-pinned="false"`,
		`data-project-kanban-pin-toggle`,
		`data-project-kanban-pin-active="false"`,
		`data-project-kanban-pin-icon="unpinned"`,
		`data-project-kanban-pin-icon="pinned"`,
		`aria-label="Pin Todo lane"`,
		`data-project-kanban-empty-lane`,
		`data-project-kanban-lane-visible="false"`,
		`project-kanban-lanes mt-3 flex min-w-0 max-w-full flex-nowrap items-start justify-start gap-3 overflow-x-auto pb-2`,
		"Todo (1)",
		"In Progress (0)",
		"Human Review (1)",
		`data-preserve-scroll="project-kanban-human-review"`,
		`href="https://github.com/digitaldrywood/detent/pull/512"`,
		"#512",
		"Render per-project Kanban board",
		"CI pass",
		"Codex P2",
		"enhancement",
		"ui",
		"release-captain",
		"digitaldrywood/detent#415 Done",
		"Backlog board item",
		"No linked PR",
		"Read workflow state",
	} {
		if !strings.Contains(section, want) {
			t.Fatalf("project kanban missing %q:\n%s", want, section)
		}
	}
	for _, forbidden := range []string{
		`draggable=`,
		`hx-post=`,
		`data-kanban-drop`,
		`Comment`,
		`id="project-kanban-show-empty"`,
		`Show 2 empty states`,
	} {
		if strings.Contains(section, forbidden) {
			t.Fatalf("project kanban rendered mutation affordance %q:\n%s", forbidden, section)
		}
	}

	backlogIndex := strings.Index(section, `aria-label="Backlog lane"`)
	todoIndex := strings.Index(section, `aria-label="Todo lane"`)
	reviewIndex := strings.Index(section, `aria-label="Human Review lane"`)
	if backlogIndex < 0 || todoIndex < 0 || reviewIndex < 0 || backlogIndex >= todoIndex || todoIndex >= reviewIndex {
		t.Fatalf("kanban lanes are not in configured order: backlog=%d todo=%d review=%d\n%s", backlogIndex, todoIndex, reviewIndex, section)
	}
}

func TestProjectKanbanStartupReadinessSuppressesEmptyLaneCopyAndActions(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 22, 9, 0, 0, 0, time.UTC)
	html := renderProjectKanbanPage(t, templates.DashboardData{
		Title:       "Detent",
		ProjectID:   "detent",
		ProjectName: "Detent",
		Kanban: templates.KanbanData{
			Mode:   "integration",
			States: []string{"Todo", "In Progress", "Human Review"},
			AllowedTransitions: map[string][]string{
				"Todo": {"In Progress"},
			},
		},
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			Refresh: telemetry.Refresh{
				Status: telemetry.RefreshStatusInitializing,
			},
		},
	})

	section := projectKanbanSection(t, html)
	for _, want := range []string{
		"Loading tracker state...",
		`aria-label="Snapshot readiness"`,
		`data-project-kanban-lane-title="Todo"`,
		`data-project-kanban-lane-title="In Progress"`,
	} {
		if !strings.Contains(section, want) {
			t.Fatalf("startup project Kanban missing %q:\n%s", want, section)
		}
	}
	for _, forbidden := range []string{
		"No issues in this state.",
		`data-kanban-drop-state`,
		`data-kanban-action`,
		`hx-post="/api/v1/kanban/move"`,
		`hx-post="/api/v1/kanban/comment"`,
		`id="kanban-feedback"`,
	} {
		if strings.Contains(section, forbidden) {
			t.Fatalf("startup project Kanban rendered loaded affordance %q:\n%s", forbidden, section)
		}
	}
}

func TestProjectKanbanLoadedEmptySnapshotRendersEmptyLaneCopy(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 22, 9, 5, 0, 0, time.UTC)
	lastRefreshAt := now.Add(-15 * time.Second)
	html := renderProjectKanbanPage(t, templates.DashboardData{
		Title:       "Detent",
		ProjectID:   "detent",
		ProjectName: "Detent",
		Kanban: templates.KanbanData{
			Mode:   "read_only",
			States: []string{"Todo", "In Progress"},
		},
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			Refresh: telemetry.Refresh{
				Status:        telemetry.RefreshStatusReady,
				LastRefreshAt: &lastRefreshAt,
			},
		},
	})

	section := projectKanbanSection(t, html)
	for _, want := range []string{
		"No issues in this state.",
		`data-project-kanban-empty-lane`,
		`0 cards`,
	} {
		if !strings.Contains(section, want) {
			t.Fatalf("loaded empty project Kanban missing %q:\n%s", want, section)
		}
	}
	if strings.Contains(section, "Loading tracker state...") {
		t.Fatalf("loaded empty project Kanban rendered startup state:\n%s", section)
	}
}

func TestProjectKanbanLoadedSnapshotRendersCardsWithoutLoadingState(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 22, 9, 10, 0, 0, time.UTC)
	lastRefreshAt := now.Add(-20 * time.Second)
	html := renderProjectKanbanPage(t, templates.DashboardData{
		Title:       "Detent",
		ProjectID:   "detent",
		ProjectName: "Detent",
		Kanban: templates.KanbanData{
			Mode:   "integration",
			States: []string{"Todo", "In Progress"},
			AllowedTransitions: map[string][]string{
				"Todo": {"In Progress"},
			},
		},
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			Refresh: telemetry.Refresh{
				Status:        telemetry.RefreshStatusReady,
				LastRefreshAt: &lastRefreshAt,
			},
			BoardIssues: []telemetry.Issue{
				{
					ID:         "issue-610",
					Identifier: "digitaldrywood/detent#610",
					ProjectID:  "detent",
					URL:        "https://github.com/digitaldrywood/detent/issues/610",
					Title:      "Show startup loading states",
					State:      "Todo",
				},
			},
		},
	})

	section := projectKanbanSection(t, html)
	for _, want := range []string{
		`data-project-kanban-card="digitaldrywood/detent#610"`,
		`data-kanban-action="move"`,
		`data-kanban-drop-state="Todo"`,
		"Show startup loading states",
	} {
		if !strings.Contains(section, want) {
			t.Fatalf("loaded project Kanban missing %q:\n%s", want, section)
		}
	}
	for _, forbidden := range []string{
		"Loading tracker state...",
	} {
		if strings.Contains(section, forbidden) {
			t.Fatalf("loaded project Kanban rendered %q:\n%s", forbidden, section)
		}
	}
}

func TestProjectSnapshotsAvoidEmptyStatesBeforeReadiness(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 22, 9, 15, 0, 0, time.UTC)
	data := templates.DashboardData{
		Title:       "Detent",
		ProjectID:   "detent",
		ProjectName: "Detent",
		Kanban: templates.KanbanData{
			Mode:   "read_only",
			States: []string{"Todo", "In Progress"},
		},
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			Refresh: telemetry.Refresh{
				Status: telemetry.RefreshStatusInitializing,
			},
		},
	}

	for name, html := range map[string]string{
		"diagnostics": renderProjectDiagnosticsPage(t, data),
		"overview":    renderDashboard(t, data),
		"runs":        renderProjectRunsSnapshot(t, data),
	} {
		if !strings.Contains(html, "Loading tracker state...") {
			t.Fatalf("%s missing startup state:\n%s", name, html)
		}
		for _, forbidden := range []string{
			`aria-label="Diagnostics operations summary"`,
			"Tracker data and runtime telemetry are available.",
			"No active issue sessions.",
			"No agent activity recorded.",
			"No issues are currently backing off.",
			"No blocked sessions.",
			"No completed sessions recorded.",
			"No board states recorded.",
			`>0 cards<`,
			`>0 running<`,
		} {
			if strings.Contains(html, forbidden) {
				t.Fatalf("%s rendered empty/zero state %q before readiness:\n%s", name, forbidden, html)
			}
		}
	}
}

func TestProjectSnapshotsRenderFirstRefreshFailureState(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 22, 9, 20, 0, 0, time.UTC)
	lastErrorAt := now.Add(-5 * time.Second)
	data := templates.DashboardData{
		Title:       "Detent",
		ProjectID:   "detent",
		ProjectName: "Detent",
		Kanban: templates.KanbanData{
			Mode:   "integration",
			States: []string{"Todo", "In Progress"},
		},
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			Project: telemetry.Project{
				ID:          "detent",
				DisplayName: "Detent",
			},
			Refresh: telemetry.Refresh{
				Status:      telemetry.RefreshStatusDegraded,
				LastError:   "github tracker unavailable",
				LastErrorAt: &lastErrorAt,
			},
		},
	}

	for name, html := range map[string]string{
		"diagnostics": renderProjectDiagnosticsPage(t, data),
		"kanban":      renderProjectKanbanPage(t, data),
		"overview":    renderDashboard(t, data),
		"runs":        renderProjectRunsSnapshot(t, data),
	} {
		for _, want := range []string{
			"Tracker refresh failed.",
			"Detent could not load the first tracker snapshot.",
			"github tracker unavailable",
		} {
			if !strings.Contains(html, want) {
				t.Fatalf("%s missing degraded state %q:\n%s", name, want, html)
			}
		}
		if strings.Contains(html, "Tracker refresh degraded.") {
			t.Fatalf("%s rendered prior-snapshot degraded copy for first refresh failure:\n%s", name, html)
		}
		for _, forbidden := range []string{
			`aria-label="Diagnostics operations summary"`,
			"Tracker data and runtime telemetry are available.",
			"No issues in this state.",
			"No active issue sessions.",
			`>0 cards<`,
			`id="kanban-feedback"`,
		} {
			if strings.Contains(html, forbidden) {
				t.Fatalf("%s rendered loaded state %q after first refresh failure:\n%s", name, forbidden, html)
			}
		}
	}
}

func TestProjectSnapshotsKeepFirstRefreshFailureWithLocalStats(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 22, 9, 22, 0, 0, time.UTC)
	lastErrorAt := now.Add(-5 * time.Second)
	data := templates.DashboardData{
		Title:       "Detent",
		ProjectID:   "detent",
		ProjectName: "Detent",
		Kanban: templates.KanbanData{
			Mode:   "integration",
			States: []string{"Todo", "In Progress"},
		},
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			Project: telemetry.Project{
				ID:          "detent",
				DisplayName: "Detent",
			},
			Projects: []telemetry.ProjectSnapshot{
				{
					Project: telemetry.Project{
						ID:          "detent",
						DisplayName: "Detent",
					},
					Refresh: telemetry.Refresh{
						Status:      telemetry.RefreshStatusDegraded,
						LastError:   "github tracker unavailable",
						LastErrorAt: &lastErrorAt,
					},
					Tokens: telemetry.Tokens{
						Input:  120,
						Output: 30,
						Total:  150,
					},
					Throughput: telemetry.TokenThroughput{
						TokensPerSecond: 2.5,
						WindowSeconds:   60,
						Tokens:          150,
					},
				},
			},
			Refresh: telemetry.Refresh{
				Status:      telemetry.RefreshStatusDegraded,
				LastError:   "github tracker unavailable",
				LastErrorAt: &lastErrorAt,
			},
			Tokens: telemetry.Tokens{
				Input:  120,
				Output: 30,
				Total:  150,
			},
			LifetimeTotals: telemetry.LifetimeTotals{
				Available:      true,
				InputTokens:    120,
				OutputTokens:   30,
				TotalTokens:    150,
				RuntimeSeconds: 45,
				Sessions:       1,
				Runs:           1,
			},
			CycleTime: telemetry.CycleTimeReport{
				Available:      true,
				AverageSeconds: 600,
			},
			TokenTrend: []telemetry.TokenTrendPoint{
				{
					At:     now,
					Input:  120,
					Output: 30,
					Total:  150,
				},
			},
		},
	}

	for name, html := range map[string]string{
		"kanban":   renderProjectKanbanPage(t, data),
		"overview": renderDashboard(t, data),
		"runs":     renderProjectRunsSnapshot(t, data),
	} {
		for _, want := range []string{
			"Tracker refresh failed.",
			"Detent could not load the first tracker snapshot.",
			"github tracker unavailable",
		} {
			if !strings.Contains(html, want) {
				t.Fatalf("%s missing first-refresh failure copy %q:\n%s", name, want, html)
			}
		}
		if strings.Contains(html, "Tracker refresh degraded.") {
			t.Fatalf("%s rendered prior-snapshot degraded copy from local stats:\n%s", name, html)
		}
	}
}

func TestProjectSnapshotsRenderDegradedRefreshWithPriorSnapshotState(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 22, 9, 25, 0, 0, time.UTC)
	lastRefreshAt := now.Add(-30 * time.Second)
	lastErrorAt := now.Add(-5 * time.Second)
	data := templates.DashboardData{
		Title:       "Detent",
		ProjectID:   "detent",
		ProjectName: "Detent",
		Kanban: templates.KanbanData{
			Mode:   "integration",
			States: []string{"Todo", "In Progress"},
		},
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			Refresh: telemetry.Refresh{
				Status:        telemetry.RefreshStatusDegraded,
				LastRefreshAt: &lastRefreshAt,
				LastError:     "workspace cleanup candidate fetch failed for states=done,cancelled: fetch github pull request: github transient error: status 502",
				LastErrorAt:   &lastErrorAt,
			},
			BoardIssues: []telemetry.Issue{
				{
					ID:         "issue-680",
					Identifier: "digitaldrywood/detent#680",
					ProjectID:  "detent",
					URL:        "https://github.com/digitaldrywood/detent/issues/680",
					Title:      "Summarize degraded refresh errors",
					State:      "Todo",
				},
			},
		},
	}

	for name, html := range map[string]string{
		"kanban":   renderProjectKanbanPage(t, data),
		"overview": renderDashboard(t, data),
		"runs":     renderProjectRunsSnapshot(t, data),
	} {
		for _, want := range []string{
			"Tracker refresh degraded.",
			"GitHub returned a transient 502 while fetching workspace cleanup candidates. Detent will retry on the next refresh.",
			"Last successful refresh: Jun 22 09:24:30 UTC.",
			"Force refresh",
			`hx-post="/api/v1/refresh"`,
		} {
			if !strings.Contains(html, want) {
				t.Fatalf("%s missing degraded refresh copy %q:\n%s", name, want, html)
			}
		}
		if name == "kanban" {
			if !strings.Contains(html, `hx-target="#github-api-manual-refresh-status"`) {
				t.Fatalf("kanban missing sidebar health manual refresh target:\n%s", html)
			}
			if !strings.Contains(html, `name="manual_refresh_status_id" value="github-api-manual-refresh-status"`) {
				t.Fatalf("kanban missing sidebar health manual refresh status id:\n%s", html)
			}
		} else if !strings.Contains(html, `hx-target="#manual-refresh-status"`) {
			t.Fatalf("%s missing page manual refresh target:\n%s", name, html)
		}
		for _, forbidden := range []string{
			"Detent could not load the first tracker snapshot.",
			"Tracker refresh failed.",
		} {
			if strings.Contains(html, forbidden) {
				t.Fatalf("%s rendered first-load copy %q for degraded refresh:\n%s", name, forbidden, html)
			}
		}
	}

	kanbanHTML := renderProjectKanbanPage(t, data)
	for _, want := range []string{
		`data-project-kanban-card="digitaldrywood/detent#680"`,
		"Summarize degraded refresh errors",
		"Todo (1)",
		"Degraded",
	} {
		if !strings.Contains(kanbanHTML, want) {
			t.Fatalf("kanban degraded prior snapshot missing %q:\n%s", want, kanbanHTML)
		}
	}
	kanbanSection := projectKanbanSection(t, kanbanHTML)
	for _, forbidden := range []string{
		`aria-label="Snapshot readiness"`,
		`hx-target="#manual-refresh-status"`,
		"GitHub returned a transient 502 while fetching workspace cleanup candidates.",
	} {
		if strings.Contains(kanbanSection, forbidden) {
			t.Fatalf("kanban degraded prior snapshot rendered board readiness chrome %q:\n%s", forbidden, kanbanSection)
		}
	}
	for _, forbidden := range []string{
		`data-kanban-action="move"`,
		`hx-post="/api/v1/kanban/move"`,
		`draggable="true"`,
	} {
		if strings.Contains(kanbanHTML, forbidden) {
			t.Fatalf("kanban degraded prior snapshot rendered mutation affordance %q:\n%s", forbidden, kanbanHTML)
		}
	}
}

func TestProjectSnapshotsRenderManualRefreshRefusal(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 22, 9, 27, 0, 0, time.UTC)
	lastRefreshAt := now.Add(-30 * time.Second)
	lastErrorAt := now.Add(-5 * time.Second)
	manualRequestedAt := now.Add(-2 * time.Second)
	data := templates.DashboardData{
		Title:       "Detent",
		ProjectID:   "detent",
		ProjectName: "Detent",
		Kanban: templates.KanbanData{
			Mode:   "integration",
			States: []string{"Todo", "In Progress"},
		},
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			Refresh: telemetry.Refresh{
				Status:        telemetry.RefreshStatusDegraded,
				LastRefreshAt: &lastRefreshAt,
				LastError:     "fetch candidate issues failed: github transient error: status 502",
				LastErrorAt:   &lastErrorAt,
				Manual: &telemetry.RefreshAttempt{
					ID:          "manual-refused",
					Status:      telemetry.RefreshAttemptStatusRefused,
					RequestedAt: &manualRequestedAt,
					LastError:   "GitHub GraphQL backoff is active until Jun 22 09:32:00 UTC. Force refresh was refused to preserve hard rate-limit constraints.",
					LastErrorAt: &lastErrorAt,
					Operations:  []string{"poll", "reconcile"},
				},
			},
			BoardIssues: []telemetry.Issue{
				{
					ID:         "issue-681",
					Identifier: "digitaldrywood/detent#681",
					ProjectID:  "detent",
					Title:      "Keep degraded board visible",
					State:      "Todo",
				},
			},
		},
	}

	html := renderDashboard(t, data)
	for _, want := range []string{
		"Tracker refresh degraded.",
		"Force refresh",
		"Refresh refused",
		"GitHub GraphQL backoff is active until Jun 22 09:32:00 UTC.",
		`id="manual-refresh-status"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("dashboard missing manual refresh refusal %q:\n%s", want, html)
		}
	}
	for _, forbidden := range []string{
		"Loading tracker state...",
		"Detent could not load the first tracker snapshot.",
	} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("dashboard rendered %q while prior degraded snapshot is available:\n%s", forbidden, html)
		}
	}
}

func TestProjectSnapshotsSummarizeGitHubTransientHTMLReadinessError(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 22, 9, 30, 0, 0, time.UTC)
	lastRefreshAt := now.Add(-time.Minute)
	lastErrorAt := now.Add(-5 * time.Second)
	htmlBody := "<!DOCTYPE html><html><head><title>Unicorn! · GitHub</title></head><body>upstream unavailable</body></html>"
	data := templates.DashboardData{
		Title:       "Detent",
		ProjectID:   "detent",
		ProjectName: "Detent",
		Kanban: templates.KanbanData{
			Mode:   "read_only",
			States: []string{"Todo", "In Progress"},
		},
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			Refresh: telemetry.Refresh{
				Status:        telemetry.RefreshStatusDegraded,
				LastRefreshAt: &lastRefreshAt,
				LastError:     "workspace cleanup candidate fetch failed for states=done,cancelled: fetch github pull request: github transient error: status 502: " + htmlBody,
				LastErrorAt:   &lastErrorAt,
			},
		},
	}

	html := renderDashboard(t, data)
	for _, want := range []string{
		"Tracker refresh degraded.",
		"GitHub returned a transient 502 while fetching workspace cleanup candidates. Detent will retry on the next refresh.",
		"Last successful refresh: Jun 22 09:29:00 UTC.",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("dashboard missing sanitized degraded refresh copy %q:\n%s", want, html)
		}
	}
	for _, forbidden := range []string{
		"<!DOCTYPE html>",
		"&lt;!DOCTYPE html&gt;",
		"Unicorn",
		"upstream unavailable",
		"github transient error",
	} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("dashboard rendered raw upstream error %q:\n%s", forbidden, html)
		}
	}
}

func TestDashboardRendersFleetKanbanNavForMultiProjectOnly(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		activeNav  string
		projects   []templates.ProjectSmallMultiple
		wantKanban bool
		wantActive bool
	}{
		{
			name:      "multi project shows fleet kanban",
			activeNav: "fleet",
			projects: []templates.ProjectSmallMultiple{
				{ID: "detent", Name: "Detent"},
				{ID: "docs-site", Name: "Docs Site"},
			},
			wantKanban: true,
		},
		{
			name:      "multi project fleet kanban active",
			activeNav: "kanban",
			projects: []templates.ProjectSmallMultiple{
				{ID: "detent", Name: "Detent"},
				{ID: "docs-site", Name: "Docs Site"},
			},
			wantKanban: true,
			wantActive: true,
		},
		{
			name:      "single project hides fleet kanban",
			activeNav: "fleet",
			projects: []templates.ProjectSmallMultiple{
				{ID: "detent", Name: "Detent"},
			},
		},
		{
			name:       "empty project list hides fleet kanban",
			activeNav:  "fleet",
			wantKanban: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			html := renderDashboard(t, templates.DashboardData{
				Title:     "Detent",
				ActiveNav: tt.activeNav,
				Projects:  tt.projects,
			})
			hasKanban := strings.Contains(html, `href="/kanban"`)
			if hasKanban != tt.wantKanban {
				t.Fatalf("fleet Kanban nav visible = %t, want %t:\n%s", hasKanban, tt.wantKanban, html)
			}
			if !tt.wantKanban {
				return
			}
			if !strings.Contains(html, `aria-label="Fleet Kanban"`) {
				t.Fatalf("fleet Kanban nav missing aria label:\n%s", html)
			}
			if tt.wantActive {
				assertActiveSidebarLink(t, html, "/kanban")
				assertInactiveSidebarLink(t, html, "/")
				return
			}
			assertInactiveSidebarLink(t, html, "/kanban")
			assertActiveSidebarLink(t, html, "/")
		})
	}
}

func TestDashboardRendersFleetKanbanReadOnlyBoard(t *testing.T) {
	t.Parallel()

	docsColor := projectcolor.ColorForID("docs-site")
	html := renderProjectKanbanPage(t, templates.DashboardData{
		Title:     "Detent / Kanban",
		ActiveNav: "kanban",
		Projects: []templates.ProjectSmallMultiple{
			{ID: "detent", Name: "Detent", Color: "#1192e8", Running: 1},
			{ID: "docs-site", Name: "Docs Site", QueueCount: 1},
		},
		Kanban: templates.KanbanData{
			Mode:   "read_only",
			States: []string{"Todo", "In Progress"},
		},
		Snapshot: telemetry.Snapshot{
			BoardIssues: []telemetry.Issue{
				{ID: "detent-card", Identifier: "digitaldrywood/detent#542", ProjectID: "detent", URL: "https://github.com/digitaldrywood/detent/issues/542", Title: "Add fleet Kanban", State: "Todo"},
				{ID: "docs-card", Identifier: "digitaldrywood/docs-site#12", ProjectID: "docs-site", URL: "https://github.com/digitaldrywood/docs-site/issues/12", Title: "Document fleet board", State: "In Progress"},
			},
		},
	})

	for _, want := range []string{
		`aria-label="Fleet Kanban"`,
		`sse-connect="/events?view=kanban"`,
		`data-project-kanban-visibility-key="fleet"`,
		`data-preserve-details="project-kanban-visibility-fleet"`,
		`data-project-kanban-visibility-menu`,
		`data-project-kanban-card="digitaldrywood/detent#542"`,
		`data-project-kanban-card="digitaldrywood/docs-site#12"`,
		`data-project-color="#1192e8"`,
		`data-project-color="` + docsColor + `"`,
		`aria-label="Project detent color marker"`,
		`aria-label="Project docs-site color marker"`,
		`aria-label="Open detent Kanban"`,
		`aria-label="Open docs-site Kanban"`,
		`href="/projects/detent/kanban"`,
		`href="/projects/docs-site/kanban"`,
		projectKanbanFixedLaneClass,
		`>detent</span>`,
		`>docs-site</span>`,
		"Add fleet Kanban",
		"Document fleet board",
		"Read-only",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("fleet Kanban missing %q:\n%s", want, html)
		}
	}
	for _, forbidden := range []string{
		"This board is currently read-only.",
		"To move cards from Detent, enable Kanban integration in WORKFLOW.md under the existing server block.",
		"detent doctor --port 0",
		`data-kanban-action`,
		`hx-post="/api/v1/kanban/move"`,
		`hx-post="/api/v1/kanban/comment"`,
		`id="kanban-feedback"`,
	} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("fleet Kanban rendered mutation affordance %q:\n%s", forbidden, html)
		}
	}
}

func TestDashboardRendersFleetKanbanMoveForEligibleProjectCards(t *testing.T) {
	t.Parallel()

	html := renderProjectKanbanPage(t, templates.DashboardData{
		Title:     "Detent / Kanban",
		ActiveNav: "kanban",
		Projects: []templates.ProjectSmallMultiple{
			{ID: "detent", Name: "Detent", Color: "#1192e8"},
			{ID: "docs-site", Name: "Docs Site"},
		},
		Kanban: templates.KanbanData{
			Mode:   "read_only",
			States: []string{"Todo", "In Progress", "Done"},
			Projects: map[string]templates.KanbanProjectData{
				"detent": {
					Mode:   "integration",
					States: []string{"Todo", "In Progress", "Done"},
					AllowedTransitions: map[string][]string{
						"Todo": {"In Progress"},
						"Done": {},
					},
				},
				"docs-site": {
					Mode:   "read_only",
					States: []string{"Todo", "In Progress"},
				},
			},
		},
		Snapshot: telemetry.Snapshot{
			BoardIssues: []telemetry.Issue{
				{ID: "detent-card", Identifier: "digitaldrywood/detent#542", ProjectID: "detent", URL: "https://github.com/digitaldrywood/detent/issues/542", Title: "Eligible fleet move", State: "Todo"},
				{ID: "docs-card", Identifier: "digitaldrywood/docs-site#12", ProjectID: "docs-site", URL: "https://github.com/digitaldrywood/docs-site/issues/12", Title: "Read only fleet card", State: "Todo"},
				{ID: "unknown-card", Identifier: "digitaldrywood/unknown#7", ProjectID: "unknown", URL: "https://github.com/digitaldrywood/unknown/issues/7", Title: "Unknown project card", State: "Todo"},
				{ID: "done-card", Identifier: "digitaldrywood/detent#544", ProjectID: "detent", URL: "https://github.com/digitaldrywood/detent/issues/544", Title: "No transition card", State: "Done"},
				{Identifier: "digitaldrywood/detent#545", ProjectID: "detent", URL: "https://github.com/digitaldrywood/detent/issues/545", Title: "PR only fleet card", State: "Todo", PullRequest: &telemetry.PullRequest{Number: 145, URL: "https://github.com/digitaldrywood/detent/pull/145"}},
			},
		},
	})

	for _, want := range []string{
		"Integration",
		`id="kanban-feedback"`,
		`hx-get="/api/v1/kanban/move?`,
		`kanban_board=fleet`,
		`project_id=detent`,
		`issue_id=detent-card`,
		`current_state=Todo`,
		`aria-label="Move #542"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("fleet Kanban missing %q:\n%s", want, html)
		}
	}
	if got := strings.Count(html, `hx-get="/api/v1/kanban/move?`); got != 1 {
		t.Fatalf("fleet Kanban move trigger count = %d, want 1:\n%s", got, html)
	}

	for _, title := range []string{
		"Read only fleet card",
		"Unknown project card",
		"No transition card",
		"PR only fleet card",
	} {
		card := compactKanbanCardSection(t, html, title)
		if strings.Contains(card, `hx-get="/api/v1/kanban/move?`) {
			t.Fatalf("fleet Kanban rendered unsafe move action for %q:\n%s", title, card)
		}
	}
	for _, forbidden := range []string{
		`draggable="true"`,
		`data-kanban-drop-state=`,
		`data-kanban-drag-move-form>`,
		`hx-post="/api/v1/kanban/remove"`,
		`hx-get="/api/v1/kanban/comment?`,
	} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("fleet Kanban rendered project-only affordance %q:\n%s", forbidden, html)
		}
	}
}

func TestDashboardRendersCompactProjectKanbanCards(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 16, 13, 0, 0, 0, time.UTC)
	issueOnlyAt := now.Add(-5 * time.Minute)
	prLinkedAt := now.Add(-4 * time.Minute)
	blockedAt := now.Add(-3 * time.Minute)
	reviewAt := now.Add(-2 * time.Minute)
	html := renderProjectKanbanPage(t, templates.DashboardData{
		Title:       "Detent",
		ProjectID:   "detent",
		ProjectName: "Detent",
		Kanban: templates.KanbanData{
			Mode:   "read_only",
			States: []string{"Todo", "Blocked", "Human Review"},
		},
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			BoardIssues: []telemetry.Issue{
				{
					ID:             "issue-only",
					Identifier:     "digitaldrywood/detent#500",
					ProjectID:      "detent",
					URL:            "https://github.com/digitaldrywood/detent/issues/500",
					Title:          "Issue-only compact card",
					Description:    "Issue-only summary should appear in the hover preview.",
					State:          "Todo",
					StageUpdatedAt: &issueOnlyAt,
				},
				{
					ID:             "pr-linked",
					Identifier:     "digitaldrywood/detent#501",
					ProjectID:      "detent",
					URL:            "https://github.com/digitaldrywood/detent/issues/501",
					Title:          "PR-linked compact card",
					State:          "Todo",
					StageUpdatedAt: &prLinkedAt,
					PullRequest: &telemetry.PullRequest{
						Number:           601,
						URL:              "https://github.com/digitaldrywood/detent/pull/601",
						MergeableState:   "DIRTY",
						CIStatus:         "success",
						CodexReviewState: "clean",
					},
				},
			},
			Pipeline: []telemetry.Issue{
				{
					ID:             "review",
					Identifier:     "digitaldrywood/detent#503",
					ProjectID:      "detent",
					URL:            "https://github.com/digitaldrywood/detent/issues/503",
					Title:          "Review compact card",
					State:          "Human Review",
					Labels:         []string{"enhancement", "ui"},
					Assignees:      []string{"release-captain"},
					StageUpdatedAt: &reviewAt,
					PullRequest: &telemetry.PullRequest{
						Number:           603,
						URL:              "https://github.com/digitaldrywood/detent/pull/603",
						CIStatus:         "success",
						CodexReviewState: "P2",
					},
				},
			},
			Blocked: []telemetry.Blocked{
				{
					Issue: telemetry.Issue{
						ID:             "blocked",
						Identifier:     "digitaldrywood/detent#502",
						ProjectID:      "detent",
						URL:            "https://github.com/digitaldrywood/detent/issues/502",
						Title:          "Blocked compact card",
						State:          "Blocked",
						BlockedBy:      []telemetry.BlockedRef{{Identifier: "digitaldrywood/detent#415", State: "Todo"}},
						StageUpdatedAt: &blockedAt,
					},
					BlockedAt: &blockedAt,
				},
			},
		},
	})

	section := projectKanbanSection(t, html)
	for _, want := range []string{
		"project-kanban-card-compact",
		projectKanbanFixedLaneClass,
		`project-kanban-lane-scroll mt-2 grid auto-rows-max min-w-0 max-w-full max-h-[32rem] gap-1.5 overflow-x-hidden overflow-y-auto pr-1`,
		`project-kanban-card project-kanban-card-compact w-full min-w-0 max-w-full overflow-hidden rounded-md border border-border bg-card p-1.5`,
		`data-kanban-card-details`,
		`data-preserve-details="project-kanban-details-digitaldrywood-detent-500"`,
		`data-kanban-card-expanded`,
		`data-help-trigger`,
		`data-help-term="project-kanban-card-preview-digitaldrywood-detent-500"`,
		`data-help-title="Issue-only compact card"`,
		`data-help-description="Issue-only summary should appear in the hover preview."`,
		`line-clamp-2 break-words text-[13px] font-normal leading-snug text-foreground`,
		`aria-label="Kanban card #500 Issue-only compact card"`,
		`aria-label="Kanban card #501 PR-linked compact card"`,
		`aria-label="Kanban card #502 Blocked compact card"`,
		`aria-label="Kanban card #503 Review compact card"`,
		`aria-label="Show details for #500"`,
		`href="https://github.com/digitaldrywood/detent/issues/501"`,
		`href="https://github.com/digitaldrywood/detent/pull/601"`,
		"#500",
		"#501",
		"#502",
		"#503",
		"PR #601",
		"PR #603",
		"Merge state",
		"dirty",
		"Conflict",
		"PR #601 mergeStateStatus DIRTY",
		"CI pass",
		"Codex clean",
		"Codex P2",
		"1 blocker",
		"1 assignee",
		"2 labels",
		"digitaldrywood/detent#415 Todo",
		"release-captain",
		"enhancement",
		"ui",
	} {
		if !strings.Contains(section, want) {
			t.Fatalf("compact project kanban missing %q:\n%s", want, section)
		}
	}

	for _, title := range []string{
		"Issue-only compact card",
		"PR-linked compact card",
		"Blocked compact card",
		"Review compact card",
	} {
		card := compactKanbanCardSection(t, section, title)
		firstDetails := strings.Index(card, `data-kanban-card-details`)
		if firstDetails < 0 {
			t.Fatalf("card %q missing details disclosure:\n%s", title, card)
		}
		defaultView := card[:firstDetails]
		for _, forbidden := range []string{
			`<textarea`,
			`name="target_state"`,
			`>Labels<`,
			`>Assignees<`,
			`>Blockers<`,
			`truncate text-sm font-medium text-foreground`,
		} {
			if strings.Contains(defaultView, forbidden) {
				t.Fatalf("card %q rendered expanded metadata by default marker %q:\n%s", title, forbidden, card)
			}
		}
	}
}

func TestDashboardProjectKanbanDoesNotRenderClearedBlockersAsActive(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 20, 18, 0, 0, 0, time.UTC)
	stageUpdatedAt := now.Add(-9 * time.Minute)
	html := renderProjectKanbanPage(t, templates.DashboardData{
		Title:       "Detent",
		ProjectID:   "detent",
		ProjectName: "Detent",
		Kanban: templates.KanbanData{
			Mode:   "integration",
			States: []string{"Merging", "Done"},
		},
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			Pipeline: []telemetry.Issue{
				{
					ID:             "merging",
					Identifier:     "digitaldrywood/detent#594",
					ProjectID:      "detent",
					URL:            "https://github.com/digitaldrywood/detent/issues/594",
					Title:          "Merging card with cleared blocker",
					State:          "Merging",
					BlockedBy:      []telemetry.BlockedRef{{Identifier: "digitaldrywood/detent#429", State: "Done"}},
					StageUpdatedAt: &stageUpdatedAt,
					PullRequest: &telemetry.PullRequest{
						Number:           595,
						URL:              "https://github.com/digitaldrywood/detent/pull/595",
						CIStatus:         "success",
						CodexReviewState: "clean",
					},
				},
			},
		},
	})

	card := compactKanbanCardSection(t, projectKanbanSection(t, html), "Merging card with cleared blocker")
	for _, forbidden := range []string{
		"1 blocker",
		`>Blockers<`,
		"border-danger-soft bg-danger-soft text-danger",
	} {
		if strings.Contains(card, forbidden) {
			t.Fatalf("cleared blocker rendered as active marker %q:\n%s", forbidden, card)
		}
	}
	for _, want := range []string{
		`>Cleared blockers<`,
		"digitaldrywood/detent#429 Done",
	} {
		if !strings.Contains(card, want) {
			t.Fatalf("cleared blocker detail missing %q:\n%s", want, card)
		}
	}
}

func TestDashboardProjectKanbanControlsStayInsideLane(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 19, 16, 0, 0, 0, time.UTC)
	stageUpdatedAt := now.Add(-7 * time.Minute)
	longProjectID := "detent-super-long-project-id-with-extra-segments"
	html := renderProjectKanbanPage(t, templates.DashboardData{
		Title:       "Detent",
		ProjectID:   "detent",
		ProjectName: "Detent",
		Kanban: templates.KanbanData{
			Mode:   "integration",
			States: []string{"Todo", "Blocked"},
			AllowedTransitions: map[string][]string{
				"Todo": {"Blocked"},
			},
		},
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			BoardIssues: []telemetry.Issue{
				{
					ID:          "dense",
					Identifier:  "digitaldrywood/detent#543",
					ProjectID:   longProjectID,
					URL:         "https://github.com/digitaldrywood/detent/issues/543",
					Title:       "Dense layout card with metadata",
					Description: "Compact Kanban card should constrain long metadata without widening its lane.",
					State:       "Todo",
					Labels: []string{
						"release-readiness-with-long-label",
						"layout-regression-that-should-wrap",
					},
					Assignees: []string{"release-captain-with-long-handle"},
					BlockedBy: []telemetry.BlockedRef{
						{Identifier: "digitaldrywood/detent#542-with-extra-context", State: "Human Review"},
					},
					StageUpdatedAt: &stageUpdatedAt,
					PullRequest: &telemetry.PullRequest{
						Number:           1543,
						URL:              "https://github.com/digitaldrywood/detent/pull/1543",
						CIStatus:         "success-with-extra-detail",
						CodexReviewState: "p2-with-extra-detail",
					},
				},
			},
		},
	})

	section := projectKanbanSection(t, html)
	for _, want := range []string{
		`project-kanban-lanes mt-3 flex min-w-0 max-w-full flex-nowrap items-start justify-start gap-3 overflow-x-auto pb-2`,
		`data-preserve-scroll="project-kanban-lanes"`,
		projectKanbanFixedLaneClass,
		`flex min-h-7 w-full min-w-0 max-w-full items-center gap-1.5 overflow-hidden`,
		`min-w-0 shrink truncate text-xs font-semibold text-foreground`,
		`inline-flex size-6 flex-none items-center justify-center`,
		`project-kanban-empty-lane`,
		`dashboard-empty-state min-w-0 max-w-full overflow-hidden`,
	} {
		if !strings.Contains(section, want) {
			t.Fatalf("project Kanban compact lane contract missing %q:\n%s", want, section)
		}
	}
	for _, forbidden := range []string{
		`project-kanban-lanes mt-3 grid min-w-0 max-w-full items-start justify-start gap-2 overflow-x-hidden`,
		`[grid-template-columns:repeat(auto-fill,minmax(14rem,16rem))]`,
		`project-kanban-lane grid min-h-[12rem] w-full min-w-0 max-w-full`,
	} {
		if strings.Contains(section, forbidden) {
			t.Fatalf("project Kanban compact lane contract still renders %q:\n%s", forbidden, section)
		}
	}

	card := compactKanbanCardSection(t, section, "Dense layout card with metadata")
	for _, want := range []string{
		`project-kanban-card project-kanban-card-compact w-full min-w-0 max-w-full overflow-hidden rounded-md border border-border bg-card p-1.5`,
		`flex w-full min-w-0 max-w-full items-center justify-between gap-1.5 overflow-hidden`,
		`flex min-w-0 flex-1 basis-0 items-center gap-1.5 overflow-hidden`,
		`flex min-w-0 max-w-full flex-1 basis-0 items-center justify-end gap-1 overflow-hidden`,
		`inline-flex h-5 min-w-0 max-w-[6rem] items-center truncate`,
		`inline-flex h-5 min-w-0 max-w-[5.5rem] items-center truncate`,
		`mt-1 flex min-w-0 max-w-full flex-wrap items-center gap-1 text-[11px]`,
		`inline-flex h-4 min-w-0 max-w-full items-center truncate`,
		`group mt-1 min-w-0 max-w-full overflow-hidden border-t border-border pt-1`,
		`mt-2 grid min-w-0 max-w-full gap-2 text-xs`,
		`flex min-w-0 max-w-full items-center justify-between gap-2 overflow-hidden`,
		`flex min-w-0 max-w-full flex-wrap gap-1.5 overflow-hidden`,
		`mt-3 flex min-w-0 max-w-full flex-wrap items-center gap-1.5`,
		longProjectID,
		"release-readiness-with-long-label",
		"release-captain-with-long-handle",
		"digitaldrywood/detent#542-with-extra-context Human Review",
	} {
		if !strings.Contains(card, want) {
			t.Fatalf("project Kanban compact card contract missing %q:\n%s", want, card)
		}
	}
}

func TestDashboardProjectKanbanVisibilityControllerSurvivesHTMXSwaps(t *testing.T) {
	t.Parallel()

	html := renderProjectKanbanPage(t, templates.DashboardData{
		Title:       "Detent",
		ProjectID:   "detent",
		ProjectName: "Detent",
		Kanban: templates.KanbanData{
			Mode:   "read_only",
			States: []string{"Todo", "Done"},
		},
		Snapshot: telemetry.Snapshot{
			BoardIssues: []telemetry.Issue{
				{
					ID:         "todo",
					Identifier: "digitaldrywood/detent#496",
					Title:      "Fix empty-lane toggle",
					State:      "Todo",
				},
			},
		},
	})

	for _, want := range []string{
		`detent.ui.projectKanban.visibleLanes.`,
		`localStorage`,
		`htmx:afterSwap`,
		`htmx:afterSettle`,
		`htmx:sseBeforeMessage`,
		`function visibilitySnapshotTarget(event)`,
		`details[data-preserve-details][open]`,
		`function closeVisibilityMenusExcept(activeMenu)`,
		`[data-project-kanban-visibility-close]`,
		`function toggleLanePin(button)`,
		`storageVersion = 4`,
		`function defaultLaneIDs(board)`,
		`function visibilityOverrides(board)`,
		`function legacyVisibilityOverrides(board, parsed)`,
		`parsed.v === 3 || parsed.v === 2 || parsed.v === 1`,
		`function resetVisibilityOverrides(board)`,
		`function setLaneOverride(board, id, state)`,
		`function laneOverrideForVisibility(lane, visible)`,
		`function resetLaneOverride(button)`,
		`show: show`,
		`hide: hide`,
		`"default"`,
		`"show"`,
		`"hide"`,
		`data-project-kanban-lane-default-visible`,
		`data-project-kanban-lane-visibility-state`,
		`data-project-kanban-visibility-state`,
		`[data-project-kanban-pin-toggle]`,
		`data-project-kanban-lane-pinned`,
		`aria-pressed`,
		`event.key === "Escape"`,
		`document.addEventListener("toggle"`,
		`event.preventDefault()`,
		`data-project-kanban-lane-visible`,
		`data-project-kanban-visibility-key`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("dashboard missing persistent kanban visibility controller %q:\n%s", want, html)
		}
	}
}

func TestDashboardProjectKanbanVisibilityTriStateControls(t *testing.T) {
	t.Parallel()

	html := renderProjectKanbanPage(t, templates.DashboardData{
		Title:       "Detent",
		ProjectID:   "detent",
		ProjectName: "Detent",
		Kanban: templates.KanbanData{
			Mode:           "read_only",
			States:         []string{"Todo", "In Progress", "Cancelled"},
			TerminalStates: []string{"Cancelled"},
		},
		Snapshot: telemetry.Snapshot{
			BoardIssues: []telemetry.Issue{
				{
					ID:         "todo",
					Identifier: "digitaldrywood/detent#496",
					Title:      "Queued work",
					State:      "Todo",
				},
				{
					ID:         "started",
					Identifier: "digitaldrywood/detent#497",
					Title:      "Active work",
					State:      "In Progress",
				},
				{
					ID:         "cancelled",
					Identifier: "digitaldrywood/detent#584",
					Title:      "Cancelled work",
					State:      "Cancelled",
				},
			},
		},
	})
	section := projectKanbanSection(t, html)

	for _, want := range []string{
		`data-project-kanban-visibility-action="reset"`,
		`Reset to defaults`,
		`data-project-kanban-visibility-row`,
		`data-project-kanban-visibility-lane="in-progress"`,
		`data-project-kanban-visibility-status`,
		`data-project-kanban-visibility-reset`,
		`aria-label="Reset In Progress lane to default visibility"`,
		`title="Reset lane to default visibility"`,
		`Default visible`,
		`Default hidden`,
		`name="visible_lane" value="in-progress"`,
		`data-project-kanban-visibility-default="true"`,
		`name="visible_lane" value="cancelled"`,
		`data-project-kanban-visibility-default="false"`,
	} {
		if !strings.Contains(section, want) {
			t.Fatalf("project Kanban visibility controls missing %q:\n%s", want, section)
		}
	}
	for _, forbidden := range []string{
		`data-project-kanban-visibility-action="active"`,
		`Active only`,
	} {
		if strings.Contains(section, forbidden) {
			t.Fatalf("project Kanban visibility controls still contain %q:\n%s", forbidden, section)
		}
	}

	for _, want := range []string{
		`storageVersion = 4`,
		`function visibilityOverrides(board)`,
		`function saveVisibilityOverrides(board, overrides)`,
		`function legacyVisibilityOverrides(board, parsed)`,
		`parsed.v === 3 || parsed.v === 2 || parsed.v === 1`,
		`function effectiveLaneVisibility(lane, overrides)`,
		`function setLaneOverride(board, id, state)`,
		`function laneOverrideForVisibility(lane, visible)`,
		`function resetLaneOverride(button)`,
		`function resetVisibilityOverrides(board)`,
		`document.addEventListener("htmx:afterSwap", applyBoards)`,
		`document.addEventListener("htmx:afterSettle", applyBoards)`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("dashboard missing tri-state visibility script %q:\n%s", want, html)
		}
	}
}

func TestDashboardProjectKanbanPopulatedTerminalLaneStartsHiddenAndUsable(t *testing.T) {
	t.Parallel()

	html := renderProjectKanbanPage(t, templates.DashboardData{
		Title:       "Detent",
		ProjectID:   "detent",
		ProjectName: "Detent",
		Kanban: templates.KanbanData{
			Mode:           "read_only",
			States:         []string{"Todo", "Done", "Cancelled"},
			TerminalStates: []string{"Done", "Cancelled"},
		},
		Snapshot: telemetry.Snapshot{
			BoardIssues: []telemetry.Issue{
				{
					ID:         "todo",
					Identifier: "digitaldrywood/detent#496",
					Title:      "Active work",
					State:      "Todo",
				},
				{
					ID:         "cancelled",
					Identifier: "digitaldrywood/detent#584",
					Title:      "Cancelled work",
					State:      "Cancelled",
				},
			},
		},
	})
	section := projectKanbanSection(t, html)

	cancelledLane := regexp.MustCompile(`<section[^>]*data-project-kanban-lane-title="Cancelled"[^>]*>`).FindString(section)
	if cancelledLane == "" {
		t.Fatalf("cancelled lane missing:\n%s", section)
	}
	for _, want := range []string{
		`data-project-kanban-lane-empty="false"`,
		`data-project-kanban-lane-visible="false"`,
	} {
		if !strings.Contains(cancelledLane, want) {
			t.Fatalf("cancelled lane missing %q:\n%s", want, cancelledLane)
		}
	}

	cancelledInput := regexp.MustCompile(`<input[^>]*value="cancelled"[^>]*>`).FindString(section)
	if cancelledInput == "" {
		t.Fatalf("cancelled visibility checkbox missing:\n%s", section)
	}
	for _, want := range []string{
		`data-project-kanban-visibility-default="false"`,
	} {
		if !strings.Contains(cancelledInput, want) {
			t.Fatalf("cancelled checkbox missing %q:\n%s", want, cancelledInput)
		}
	}
	for _, forbidden := range []string{`checked`, `disabled`} {
		if strings.Contains(cancelledInput, forbidden) {
			t.Fatalf("cancelled checkbox must remain usable and unchecked, found %q:\n%s", forbidden, cancelledInput)
		}
	}
	for _, want := range []string{
		`<span class="rounded-full bg-muted px-1.5 py-0.5 font-mono" data-project-kanban-visibility-count>1/3</span>`,
		`Default hidden`,
		`Cancelled (1)`,
		`Cancelled work`,
	} {
		if !strings.Contains(section, want) {
			t.Fatalf("project Kanban missing %q:\n%s", want, section)
		}
	}
}

func TestDashboardProjectKanbanVisibilityKeyIgnoresLiveExtraLanes(t *testing.T) {
	t.Parallel()

	baseHTML := renderProjectKanbanPage(t, templates.DashboardData{
		ProjectID:   "detent",
		ProjectName: "Detent",
		Kanban: templates.KanbanData{
			Mode:   "read_only",
			States: []string{"Todo", "Done"},
		},
		Snapshot: telemetry.Snapshot{
			BoardIssues: []telemetry.Issue{
				{
					ID:         "todo",
					Identifier: "digitaldrywood/detent#496",
					Title:      "Todo card",
					State:      "Todo",
				},
			},
		},
	})
	extraLaneHTML := renderProjectKanbanPage(t, templates.DashboardData{
		ProjectID:   "detent",
		ProjectName: "Detent",
		Kanban: templates.KanbanData{
			Mode:   "read_only",
			States: []string{"Todo", "Done", "Escalated"},
		},
		Snapshot: telemetry.Snapshot{
			BoardIssues: []telemetry.Issue{
				{
					ID:         "escalated",
					Identifier: "digitaldrywood/detent#497",
					Title:      "Escalated card",
					State:      "Escalated",
				},
			},
		},
	})

	baseKey := projectKanbanVisibilityKeyFromHTML(t, baseHTML)
	extraLaneKey := projectKanbanVisibilityKeyFromHTML(t, extraLaneHTML)
	if baseKey != "project:detent" || extraLaneKey != baseKey {
		t.Fatalf("visibility key should remain scoped to project: base=%q extra=%q", baseKey, extraLaneKey)
	}
}

func TestDashboardKanbanIntegrationControls(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	data := templates.DashboardData{
		Title:       "Detent",
		ProjectID:   "detent",
		ProjectName: "Detent",
		Kanban: templates.KanbanData{
			Mode:   "integration",
			States: []string{"Todo", "In Progress", "Human Review"},
		},
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			Project:     telemetry.Project{ID: "detent", DisplayName: "Detent"},
			Pipeline: []telemetry.Issue{
				{
					ID:         "I_kw1",
					Identifier: "digitaldrywood/detent#1",
					ProjectID:  "detent",
					Title:      "Movable issue",
					State:      "Todo",
					URL:        "https://github.com/digitaldrywood/detent/issues/1",
					PullRequest: &telemetry.PullRequest{
						Number: 42,
						URL:    "https://github.com/digitaldrywood/frontend/pull/42",
					},
				},
				{
					Identifier: "digitaldrywood/detent#2",
					ProjectID:  "detent",
					Title:      "PR only",
					State:      "Human Review",
					PullRequest: &telemetry.PullRequest{
						Number: 43,
						URL:    "https://github.com/digitaldrywood/detent/pull/43",
					},
				},
			},
		},
	}

	html := renderProjectKanbanPage(t, data)
	for _, want := range []string{
		`aria-live="polite"`,
		`id="kanban-action-dialog"`,
		`id="kanban-dialog-content"`,
		`data-kanban-card`,
		`draggable="true"`,
		`data-kanban-drop-state="In Progress"`,
		`data-tui-dialog-trigger`,
		`data-tui-dialog-target="kanban-action-dialog"`,
		`hx-get="/api/v1/kanban/move?`,
		`hx-get="/api/v1/kanban/comment?`,
		`hx-post="/api/v1/kanban/move"`,
		`hx-post="/api/v1/kanban/remove"`,
		`hx-confirm="Remove this item from project?"`,
		`hx-target="#kanban-feedback"`,
		`hx-target="#kanban-dialog-content"`,
		`data-kanban-drag-move-form`,
		`Remove from project`,
		`event.dataTransfer.setDragImage(card`,
		`pr_repository=digitaldrywood%2Ffrontend`,
		`Cannot move PR-only card`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("integration dashboard missing %q:\n%s", want, html)
		}
	}
	for _, forbidden := range []string{
		"This board is currently read-only.",
		"To move cards from Detent, enable Kanban integration in WORKFLOW.md under the existing server block.",
		"detent doctor --port 0",
	} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("integration dashboard rendered read-only setup notice %q:\n%s", forbidden, html)
		}
	}

	data.Kanban.Mode = "read_only"
	html = renderProjectKanbanPage(t, data)
	for _, forbidden := range []string{
		`hx-post="/api/v1/kanban/move"`,
		`hx-post="/api/v1/kanban/remove"`,
		`hx-post="/api/v1/kanban/comment"`,
		`hx-get="/api/v1/kanban/move`,
		`hx-get="/api/v1/kanban/comment`,
		`draggable="true"`,
		`data-kanban-action`,
	} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("read-only dashboard rendered %q:\n%s", forbidden, html)
		}
	}
}

func TestDashboardKanbanIntegrationFiltersMoveTargets(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	html := renderProjectKanbanPage(t, templates.DashboardData{
		Title:       "Detent",
		ProjectID:   "detent",
		ProjectName: "Detent",
		Kanban: templates.KanbanData{
			Mode:   "integration",
			States: []string{"Todo", "In Progress", "Blocked", "Human Review", "Cancelled"},
			AllowedTransitions: map[string][]string{
				"In Progress": {"Blocked", "Cancelled"},
			},
		},
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			Project:     telemetry.Project{ID: "detent", DisplayName: "Detent"},
			Pipeline: []telemetry.Issue{
				{
					ID:         "I_kw1",
					Identifier: "digitaldrywood/detent#1",
					ProjectID:  "detent",
					Title:      "Active issue",
					State:      "In Progress",
				},
			},
		},
	})

	section := projectKanbanSection(t, html)
	for _, want := range []string{
		`data-kanban-drop-key="blocked"`,
		`data-kanban-drop-key="cancelled"`,
	} {
		if !strings.Contains(section, want) {
			t.Fatalf("integration dashboard missing allowed move marker %q:\n%s", want, section)
		}
	}

	card := compactKanbanCardSection(t, section, "Active issue")
	for _, want := range []string{
		`data-kanban-allowed-targets="blocked cancelled"`,
		`hx-get="/api/v1/kanban/move?`,
		`current_state=In+Progress`,
		`hx-post="/api/v1/kanban/move"`,
		`hx-target="#kanban-feedback"`,
		`data-kanban-drag-move-form`,
		`name="kanban_drag" value="true"`,
		`data-kanban-drag-target-state`,
	} {
		if !strings.Contains(card, want) {
			t.Fatalf("integration card missing allowed move marker %q:\n%s", want, card)
		}
	}
	for _, forbidden := range []string{
		`data-kanban-allowed-targets="blocked cancelled humanreview"`,
		`data-kanban-allowed-targets="blocked cancelled inprogress"`,
		`<option`,
		`hx-get="/api/v1/kanban/move" hx-target="#kanban-dialog-content" hx-swap="innerHTML" data-kanban-drag-move-form`,
	} {
		if strings.Contains(card, forbidden) {
			t.Fatalf("integration card rendered disallowed move marker %q:\n%s", forbidden, card)
		}
	}
}

func TestDashboardKanbanDragDropPropagatesTargetState(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	html := renderProjectKanbanPage(t, templates.DashboardData{
		Title:       "Detent",
		ProjectID:   "detent",
		ProjectName: "Detent",
		Kanban: templates.KanbanData{
			Mode:   "integration",
			States: []string{"Todo", "In Progress", "Blocked"},
			AllowedTransitions: map[string][]string{
				"Todo": {"In Progress"},
			},
		},
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			Project:     telemetry.Project{ID: "detent", DisplayName: "Detent"},
			Pipeline: []telemetry.Issue{
				{
					ID:         "I_kw1",
					Identifier: "digitaldrywood/detent#1",
					ProjectID:  "detent",
					Title:      "Drag target card",
					State:      "Todo",
				},
			},
		},
	})

	card := compactKanbanCardSection(t, html, "Drag target card")
	for _, want := range []string{
		`data-kanban-allowed-targets="inprogress"`,
		`data-kanban-drag-move-form`,
		`name="kanban_drag" value="true"`,
		`name="target_state" value="" data-kanban-drag-target-state`,
		`hx-post="/api/v1/kanban/move"`,
		`hx-target="#kanban-feedback"`,
		`name="current_state" value="Todo"`,
	} {
		if !strings.Contains(card, want) {
			t.Fatalf("drag card missing %q:\n%s", want, card)
		}
	}

	for _, want := range []string{
		`const targetState = form ? form.querySelector("[data-kanban-drag-target-state]") : null;`,
		`function markedDraggedCard()`,
		`function activeDragIssueID(event)`,
		`card.dataset.kanbanDragging = "true";`,
		`const issueID = activeDragIssueID(event);`,
		`targetState.value = lane.dataset.kanbanDropState || "";`,
		`feedback("Move blocked by transition policy.");`,
		`lane.dataset.kanbanDropAllowed = allowed ? "true" : "false";`,
		`lane.dataset.projectKanbanLaneVisible = "true";`,
		`lane.dataset.kanbanDropWasVisible = lane.dataset.projectKanbanLaneVisible || "";`,
		`lane.setAttribute("aria-disabled", allowed ? "false" : "true");`,
		`event.dataTransfer.dropEffect = allowed ? "move" : "none";`,
		`document.body.addEventListener("htmx:responseError"`,
		`form.requestSubmit();`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("drag script missing %q:\n%s", want, html)
		}
	}
	for _, forbidden := range []string{
		`openActionDialog();`,
		`hx-get="/api/v1/kanban/move" hx-target="#kanban-dialog-content" hx-swap="innerHTML" data-kanban-drag-move-form`,
	} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("drag script/card rendered dialog submit path %q:\n%s", forbidden, html)
		}
	}
}

func TestDashboardPreservesSnapshotScrollContainersAcrossSSEMorph(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 1, 15, 0, 0, 0, time.UTC)
	stageAt := now.Add(-time.Minute)
	html := renderDashboard(t, templates.DashboardData{
		Title:         "Detent",
		ConnectorName: "github",
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			Pipeline: []telemetry.Issue{
				{
					ID:         "review",
					Identifier: "DD-REVIEW",
					Title:      "Review pipeline card",
					State:      "Human Review",
					UpdatedAt:  &stageAt,
				},
				{
					ID:         "merging",
					Identifier: "DD-MERGING",
					Title:      "Merging pipeline card",
					State:      "Merging",
					UpdatedAt:  &stageAt,
				},
				{
					ID:         "done",
					Identifier: "DD-DONE",
					Title:      "Done pipeline card",
					State:      "Done",
					UpdatedAt:  &stageAt,
				},
			},
			Running: []telemetry.Running{
				{
					Issue: telemetry.Issue{
						ID:         "running",
						Identifier: "DD-RUNNING",
						Title:      "Running issue",
						State:      "In Progress",
					},
					StartedAt: now.Add(-5 * time.Minute),
				},
			},
			Queue: []telemetry.Queued{
				{
					Issue: telemetry.Issue{
						ID:         "queued",
						Identifier: "DD-QUEUED",
						Title:      "Queued issue",
					},
					Attempt: 1,
				},
			},
			Blocked: []telemetry.Blocked{
				{
					Issue: telemetry.Issue{
						ID:         "blocked",
						Identifier: "DD-BLOCKED",
						Title:      "Blocked issue",
						State:      "Blocked",
					},
				},
			},
			Completed: []telemetry.Completed{
				{
					Issue: telemetry.Issue{
						ID:         "completed",
						Identifier: "DD-COMPLETED",
						Title:      "Completed issue",
					},
					CompletedAt: now,
				},
			},
		},
	})

	for _, want := range []string{
		`data-preserve-scroll="agent-activity"`,
		`data-preserve-scroll="pr-pipeline-human-review"`,
		`data-preserve-scroll="pr-pipeline-merging"`,
		`data-preserve-scroll="pr-pipeline-done-today"`,
		`data-preserve-scroll="running-issues"`,
		`data-preserve-scroll="retry-queue"`,
		`data-preserve-scroll="blocked-sessions"`,
		`data-preserve-scroll="recent-sessions"`,
		`document.addEventListener("htmx:beforeSwap"`,
		`document.addEventListener("htmx:afterSettle"`,
		"scrollTop",
		"scrollLeft",
		`detailsSelector = "details[data-preserve-details]"`,
		"detailsOpen",
		"rememberDetails",
		"restoreDetails",
		"HTMLDetailsElement",
		"element.open = open",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("dashboard missing scroll preservation marker %q:\n%s", want, html)
		}
	}
}

func TestProjectDiagnosticsPageRendersTabbedOperationsView(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	html := renderProjectDiagnosticsPage(t, templates.DashboardData{
		Title:         "Diagnostics",
		ConnectorName: "github",
		ProjectID:     "detent",
		ProjectName:   "Detent",
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			Project:     telemetry.Project{ID: "detent", DisplayName: "Detent"},
			Refresh: telemetry.Refresh{
				Status:        telemetry.RefreshStatusReady,
				LastRefreshAt: &now,
			},
			WorkflowMetrics: telemetry.WorkflowMetrics{
				Available: true,
				RuntimeStore: telemetry.RuntimeStoreEvidence{
					Backend:         "sqlite",
					Status:          "healthy",
					Healthy:         true,
					Path:            "tmp/detent.db",
					MigrationStatus: "applied through 6",
					Tables: []telemetry.RuntimeStoreTableEvidence{
						{Name: "workflow_phase_events", RowCount: 12, Scope: "project"},
						{Name: "usage_events", RowCount: 4, Scope: "project"},
					},
					WorkflowPhaseEvents: telemetry.RuntimeStoreWorkflowPhaseEvents{
						RowCount:         12,
						OldestFinishedAt: &now,
						NewestFinishedAt: &now,
					},
				},
				Windows: []telemetry.WorkflowMetricsWindow{
					{
						Label: "24h",
						From:  now.Add(-24 * time.Hour),
						To:    now,
						Lanes: []telemetry.WorkflowPhaseMetric{
							{PhaseName: "In Progress", Count: 2, AverageSeconds: 600, P50Seconds: 540, P90Seconds: 720, P95Seconds: 720, Bottleneck: true},
						},
					},
				},
				ActiveBottleneck: telemetry.WorkflowBottleneck{
					Label:   "Human Review waiting longest",
					Detail:  "digitaldrywood/detent#755 has waited 2h in Human Review.",
					Seconds: 7200,
					Count:   1,
				},
			},
		},
	})

	for _, want := range []string{
		`role="tablist"`,
		`aria-label="Diagnostics sections"`,
		`id="diagnostics-tab-overview"`,
		`aria-controls="diagnostics-panel-overview"`,
		`aria-selected="true"`,
		`role="tabpanel"`,
		`id="diagnostics-panel-workflow-timing"`,
		`aria-labelledby="diagnostics-tab-workflow-timing"`,
		"Overview",
		"Workflow timing",
		"Active work",
		"Queues &amp; blockers",
		"GitHub/API",
		"Runtime store",
		"Raw/Debug",
		`detent.ui.diagnostics.selectedTab.detent`,
		`window.localStorage`,
		`document.addEventListener("htmx:afterSettle"`,
		`event.key === "ArrowRight"`,
		`event.key === "ArrowLeft"`,
		`event.key === "Home"`,
		`event.key === "End"`,
		`data-preserve-scroll="diagnostics-workflow-timing"`,
		"SQLite-backed history",
		"tmp/detent.db",
		"workflow_phase_events",
		"applied through 6",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("project diagnostics page missing %q:\n%s", want, html)
		}
	}
}

func TestProjectDiagnosticsPageRendersWorkflowDiagnosticPromptCopyControls(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	lanes := []telemetry.WorkflowPhaseMetric{
		workflowDiagnosticTestLane("In Progress", 180, 60, 120),
		workflowDiagnosticTestLane("Human Review", 240, 30, 210),
		workflowDiagnosticTestLane("Merging", 600, 120, 480),
		workflowDiagnosticTestLane("Rework", 300, 180, 120),
	}
	lanes[2].Bottleneck = true
	html := renderProjectDiagnosticsPage(t, templates.DashboardData{
		Title:         "Diagnostics",
		ConnectorName: "github",
		ProjectID:     "detent",
		ProjectName:   "Detent",
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			Project:     telemetry.Project{ID: "detent", DisplayName: "Detent"},
			WorkflowMetrics: telemetry.WorkflowMetrics{
				Available: true,
				RuntimeStore: telemetry.RuntimeStoreEvidence{
					Backend: "sqlite",
					Status:  "healthy",
					Healthy: true,
				},
				Windows: []telemetry.WorkflowMetricsWindow{
					{
						Label: "24h",
						From:  now.Add(-24 * time.Hour),
						To:    now,
						Lanes: lanes,
						SubPhases: []telemetry.WorkflowPhaseMetric{
							{PhaseType: "agent_session", PhaseName: "agent_active", Count: 1, TotalSeconds: 120, TotalTokens: 600, Turns: 1},
							{PhaseType: "local_check", PhaseName: "make check", Count: 1, TotalSeconds: 90},
							{PhaseType: "ci", PhaseName: "ci", Count: 1, TotalSeconds: 180},
							{PhaseType: "github_backoff", PhaseName: "github_backoff", Count: 1, TotalSeconds: 60},
							{PhaseType: "merge_queue", PhaseName: "merge_queue", Count: 1, TotalSeconds: 240},
						},
					},
				},
				OldestCards: []telemetry.WorkflowLaneAge{
					{ProjectID: "detent", IssueID: "issue-759", Identifier: "digitaldrywood/detent#759", URL: "https://github.com/digitaldrywood/detent/issues/759", State: "Merging", AgeSeconds: 3600},
				},
				ActiveBottleneck: telemetry.WorkflowBottleneck{
					Kind:       "merge_queue",
					Label:      "Merge queue",
					Detail:     "issues waiting or actively merging",
					ProjectID:  "detent",
					IssueID:    "issue-759",
					Identifier: "digitaldrywood/detent#759",
					Seconds:    3600,
					Count:      1,
				},
			},
		},
	})

	for _, want := range []string{
		`aria-label="Copy diagnostic prompt"`,
		`aria-label="Copy diagnostic prompt for In Progress"`,
		`aria-label="Copy diagnostic prompt for Human Review"`,
		`aria-label="Copy diagnostic prompt for Merging"`,
		`aria-label="Copy diagnostic prompt for Rework"`,
		`data-copy="Detent workflow lane diagnostic request`,
		"Wait vs active:",
		"Representative run identifiers",
		"navigator.clipboard.writeText(this.dataset.copy)",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("diagnostics missing workflow diagnostic prompt control %q:\n%s", want, html)
		}
	}
}

func TestDashboardShellRendersSharedAppChrome(t *testing.T) {
	t.Parallel()

	html := renderDashboardShell(t, templates.DashboardShellData{
		Title:           "Detent settings",
		ApplicationName: "Detent",
		InstanceName:    "release-captain",
		Assets: templates.AssetPaths{
			Stylesheet:      "/assets/detent.css",
			ChartJS:         "/assets/chart.js",
			DashboardCharts: "/assets/dashboard-charts.js",
		},
		Projects: []templates.ProjectSmallMultiple{
			{ID: "detent", Name: "Detent", Running: 2},
		},
		ActiveNav:              "settings",
		SidebarCollapsed:       true,
		IncludeDashboardCharts: true,
	})

	for _, want := range []string{
		`<title>Detent settings</title>`,
		`<link rel="stylesheet" href="/assets/detent.css"`,
		`src="/assets/chart.js"`,
		`src="/assets/dashboard-charts.js"`,
		`data-tui-sidebar-layout`,
		`id="dashboard-sidebar"`,
		`data-tui-sidebar-state="collapsed"`,
		`/static/js/templui/sidebar.min.js`,
		`/static/js/templui/dialog.min.js`,
		`/static/js/templui/popover.min.js`,
		`data-tui-sheet`,
		`data-tui-sidebar-target="dashboard-sidebar"`,
		`href="/projects/detent"`,
		`href="/settings"`,
		`function applyProjectSidebarActiveState()`,
		`window.addEventListener("hashchange", applyProjectSidebarActiveState)`,
		`[data-dashboard-view-nav], [data-dashboard-static-nav]`,
		`data-tui-sidebar-active="true" aria-current="page"`,
		`data-shell-test-child`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("dashboard shell missing %q:\n%s", want, html)
		}
	}

	for _, wantSingle := range []string{
		`data-tui-sidebar-layout`,
		`/static/js/templui/sidebar.min.js`,
		`/static/js/templui/dialog.min.js`,
		`/static/js/templui/popover.min.js`,
	} {
		if got := strings.Count(html, wantSingle); got != 1 {
			t.Fatalf("dashboard shell rendered %q %d times, want 1:\n%s", wantSingle, got, html)
		}
	}

	for _, forbidden := range []string{
		"dashboard-nav flex min-w-0 items-center gap-4",
		"dashboard-nav-link",
		`aria-label="Primary"`,
	} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("dashboard shell rendered retired nav marker %q:\n%s", forbidden, html)
		}
	}
}

func workflowDiagnosticTestLane(lane string, average int64, active int64, wait int64) telemetry.WorkflowPhaseMetric {
	return telemetry.WorkflowPhaseMetric{
		ProjectID:      "detent",
		PhaseType:      "lane",
		PhaseName:      lane,
		Count:          2,
		TotalSeconds:   average * 2,
		AverageSeconds: average,
		P50Seconds:     average,
		P90Seconds:     average + 60,
		P95Seconds:     average + 120,
		ActiveSeconds:  active,
		WaitSeconds:    wait,
		Representatives: []telemetry.WorkflowRepresentativeRun{
			{RunID: 42, SessionID: 84, Identifier: "digitaldrywood/detent#759", FinishedAt: time.Date(2026, 6, 28, 11, 0, 0, 0, time.UTC)},
		},
	}
}

func TestDashboardSidebarActiveScriptDistinguishesFleetKanban(t *testing.T) {
	t.Parallel()

	html := renderDashboardShell(t, templates.DashboardShellData{
		Title: "Detent",
		Projects: []templates.ProjectSmallMultiple{
			{ID: "detent", Name: "Detent"},
			{ID: "docs-site", Name: "Docs Site"},
		},
		ActiveNav: "kanban",
	})

	for _, want := range []string{
		`function staticNavForLocation()`,
		`if (path === "/kanban")`,
		`root.querySelector("[data-dashboard-static-nav='" + staticNav + "']")`,
		`function projectViewForLocation()`,
		`if (!path.startsWith("/projects/"))`,
		`if (path.endsWith("/kanban"))`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("dashboard sidebar active script missing %q:\n%s", want, html)
		}
	}

	projectViewStart := strings.Index(html, `function projectViewForLocation()`)
	if projectViewStart < 0 {
		t.Fatalf("dashboard sidebar active script missing projectViewForLocation:\n%s", html)
	}
	projectViewEnd := strings.Index(html[projectViewStart:], `function setActive`)
	if projectViewEnd < 0 {
		t.Fatalf("dashboard sidebar active script missing setActive after projectViewForLocation:\n%s", html)
	}
	projectViewScript := html[projectViewStart : projectViewStart+projectViewEnd]
	projectGuardIndex := strings.Index(projectViewScript, `if (!path.startsWith("/projects/"))`)
	kanbanSuffixIndex := strings.Index(projectViewScript, `if (path.endsWith("/kanban"))`)
	if projectGuardIndex < 0 || kanbanSuffixIndex < 0 || projectGuardIndex > kanbanSuffixIndex {
		t.Fatalf("project view detection must reject fleet paths before matching /kanban suffix:\n%s", projectViewScript)
	}

	assertActiveSidebarLink(t, html, "/kanban")
}

func TestDashboardRendersReadableAgentTimelineForConcurrentSessions(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 31, 15, 0, 0, 0, time.UTC)
	running := make([]telemetry.Running, 0, 5)
	for i := range 5 {
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
		Title:         "Detent",
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
		"min-w-[78rem]",
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
		Title:         "Detent",
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
		"Token throughput",
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

func TestDashboardRendersProjectSmallMultiples(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 1, 15, 0, 0, 0, time.UTC)
	html := renderDashboard(t, templates.DashboardData{
		Title:         "Detent",
		ConnectorName: "github",
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
		},
		Projects: []templates.ProjectSmallMultiple{
			{
				ID:                        "detent",
				Name:                      "Detent",
				URL:                       "https://github.com/digitaldrywood/detent",
				Color:                     "#1192e8",
				Running:                   1,
				QueueCount:                2,
				Completed:                 3,
				ThroughputTokensPerSecond: 4.5,
				CurrentSpendUSD:           6.75,
				Samples: []templates.ProjectSmallMultipleSample{
					{At: now.Add(-time.Minute), ThroughputTokensPerSecond: 2, SpendUSD: 5, QueueDepth: 1},
					{At: now, ThroughputTokensPerSecond: 4.5, SpendUSD: 6.75, QueueDepth: 2},
				},
			},
		},
	})

	for _, want := range []string{
		"Fleet grid",
		"Detent project",
		"Live running, queue, blocked, and token signals across configured projects.",
		"1 running / 2 queued / 0 blocked",
		"4.5 tps",
		"$6.75",
		`data-project-color="#1192e8"`,
		`aria-label="Detent project color"`,
		`aria-label="Detent throughput sparkline"`,
		`aria-label="Detent spend sparkline"`,
		`aria-label="Detent queue depth sparkline"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("dashboard missing %q:\n%s", want, html)
		}
	}
	if strings.Contains(html, "md:grid-cols-2 xl:grid-cols-3") {
		t.Fatalf("single project small multiples should not render multi-column grid:\n%s", html)
	}
}

func TestDashboardRendersProjectColorMarkersOnSidebarAndKanban(t *testing.T) {
	t.Parallel()

	docsColor := projectcolor.ColorForID("docs-site")
	html := renderProjectKanbanPage(t, templates.DashboardData{
		Title:       "Detent",
		ProjectID:   "detent",
		ProjectName: "Detent",
		Projects: []templates.ProjectSmallMultiple{
			{ID: "detent", Name: "Detent", Color: "#1192e8", Running: 1},
			{ID: "docs-site", Name: "Docs Site", QueueCount: 1},
		},
		Kanban: templates.KanbanData{
			Mode:   "read_only",
			States: []string{"Todo"},
		},
		Snapshot: telemetry.Snapshot{
			BoardIssues: []telemetry.Issue{
				{ID: "detent-card", Identifier: "digitaldrywood/detent#540", ProjectID: "detent", Title: "Color Detent card", State: "Todo"},
				{ID: "docs-card", Identifier: "digitaldrywood/docs-site#12", ProjectID: "docs-site", Title: "Color Docs card", State: "Todo"},
			},
		},
	})

	for _, want := range []string{
		`data-project-id="detent"`,
		`data-project-id="docs-site"`,
		`data-project-color="#1192e8"`,
		`data-project-color="` + docsColor + `"`,
		`aria-label="Detent project color"`,
		`aria-label="Docs Site project color"`,
		`aria-label="Project detent color marker"`,
		`aria-label="Project docs-site color marker"`,
		`>detent</span>`,
		`>docs-site</span>`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("dashboard missing project color marker %q:\n%s", want, html)
		}
	}
}

func TestDashboardRendersBudgetHistoryAndDailyCap(t *testing.T) {
	t.Parallel()

	perDay := 100.0
	perIssue := 25.0
	now := time.Date(2026, 5, 31, 15, 0, 0, 0, time.UTC)
	html := renderDashboard(t, templates.DashboardData{
		Title:         "Detent",
		ConnectorName: "github",
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			Budget: telemetry.Budget{
				Enabled:         true,
				PerDayMaxUSD:    &perDay,
				PerIssueMaxUSD:  &perIssue,
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
		`aria-label="Help: Current spend"`,
		`aria-label="Daily budget usage"`,
		`style="width: 13%;"`,
		"Budget history",
		`aria-label="Help: Budget history"`,
		`aria-label="Spend over the last seven days"`,
		`title="2026-05-25: $4.00"`,
		`title="2026-05-30: $15.00"`,
		`style="height: 100%;"`,
		`aria-label="Help: Projected spend"`,
		`aria-label="Help: Daily cap"`,
		`aria-label="Help: Issue cap"`,
		"$25.00",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("dashboard missing %q:\n%s", want, html)
		}
	}
}

func TestDashboardRendersAccessibleHelpAffordances(t *testing.T) {
	t.Parallel()

	html := renderDashboard(t, templates.DashboardData{
		Title:         "Detent",
		ConnectorName: "github",
		Snapshot: telemetry.Snapshot{
			Counts: telemetry.Counts{
				Running:   1,
				Queue:     1,
				Blocked:   1,
				Completed: 1,
			},
			Running: []telemetry.Running{
				{
					Issue: telemetry.Issue{Identifier: "DD-173", State: "In Progress"},
				},
			},
			Queue: []telemetry.Queued{
				{
					Issue:   telemetry.Issue{Identifier: "DD-174"},
					Attempt: 1,
				},
			},
			Blocked: []telemetry.Blocked{
				{
					Issue: telemetry.Issue{Identifier: "DD-175", State: "Blocked"},
				},
			},
			Completed: []telemetry.Completed{
				{
					Issue: telemetry.Issue{Identifier: "DD-176"},
				},
			},
		},
	})

	for _, want := range []string{
		`data-help-tip`,
		`data-help-trigger`,
		`data-help-term="running"`,
		`data-help-title="Running"`,
		`data-help-description="Issues currently assigned to Codex`,
		`id="help-tooltip"`,
		`data-help-tooltip`,
		`aria-label="Help: Running"`,
		`role="tooltip"`,
		`aria-label="Help: Token throughput"`,
		`aria-label="Help: Runtime"`,
		`aria-label="Help: Backoff queue"`,
		`aria-label="Help: Budget"`,
		`aria-label="Help: Rate limits"`,
		`aria-label="Help: Tokens"`,
		`aria-label="Help: Diff"`,
		`aria-label="Help: Session"`,
		`tps`,
		`USD`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("dashboard missing %q:\n%s", want, html)
		}
	}
	for _, forbidden := range []string{
		`onmouseenter=`,
		`onfocus=`,
		`popovertarget=`,
	} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("dashboard contains flicker-prone popover markup %q:\n%s", forbidden, html)
		}
	}
}

func TestDashboardRendersHealthIndicators(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 31, 15, 0, 0, 0, time.UTC)
	html := renderDashboard(t, templates.DashboardData{
		Title:         "Detent",
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
		Title:         "Detent",
		ConnectorName: "github",
	})
	if !strings.Contains(offlineHTML, "Starting") || !strings.Contains(offlineHTML, "Loading tracker state...") {
		t.Fatalf("dashboard missing startup status:\n%s", offlineHTML)
	}
}

func TestDashboardRendersIssueAndSessionControls(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 31, 15, 0, 0, 0, time.UTC)
	html := renderDashboard(t, templates.DashboardData{
		Title:         "Detent",
		ConnectorName: "github",
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			Running: []telemetry.Running{
				{
					Issue: telemetry.Issue{
						ID:         "issue-running-url",
						Identifier: "digitaldrywood/detent#91",
						URL:        "https://github.com/digitaldrywood/detent/issues/91",
						Title:      "Running URL controls",
					},
					SessionID: "thread-running-url",
				},
			},
			Queue: []telemetry.Queued{
				{
					Issue: telemetry.Issue{
						ID:         "issue-retry-url",
						Identifier: "digitaldrywood/detent#92",
						URL:        "https://github.com/digitaldrywood/detent/issues/92",
						Title:      "Retry URL controls",
					},
					Attempt: 1,
				},
			},
			Blocked: []telemetry.Blocked{
				{
					Issue: telemetry.Issue{
						ID:         "issue-blocked-url",
						Identifier: "digitaldrywood/detent#93",
						URL:        "https://github.com/digitaldrywood/detent/issues/93",
						Title:      "Blocked URL controls",
					},
					SessionID: "thread-blocked-url",
				},
			},
			Completed: []telemetry.Completed{
				{
					Issue: telemetry.Issue{
						ID:         "issue-recent-url",
						Identifier: "digitaldrywood/detent#94",
						URL:        "https://github.com/digitaldrywood/detent/issues/94",
						Title:      "Recent URL controls",
					},
					SessionID:   "thread-recent-url",
					CompletedAt: now,
				},
			},
		},
	})

	for _, want := range []string{
		`data-copy="https://github.com/digitaldrywood/detent/issues/91"`,
		`data-copy="https://github.com/digitaldrywood/detent/issues/92"`,
		`data-copy="https://github.com/digitaldrywood/detent/issues/93"`,
		`data-copy="https://github.com/digitaldrywood/detent/issues/94"`,
		`href="https://github.com/digitaldrywood/detent/issues/91"`,
		`href="https://github.com/digitaldrywood/detent/issues/92"`,
		`href="https://github.com/digitaldrywood/detent/issues/93"`,
		`href="https://github.com/digitaldrywood/detent/issues/94"`,
		`href="/api/v1/digitaldrywood%2Fdetent%2391"`,
		`href="/api/v1/digitaldrywood%2Fdetent%2392"`,
		`href="/api/v1/digitaldrywood%2Fdetent%2393"`,
		`data-copy="thread-running-url"`,
		`data-copy="thread-blocked-url"`,
		`data-copy="thread-recent-url"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("dashboard missing %q:\n%s", want, html)
		}
	}

	if strings.Contains(html, `href="/api/v1/digitaldrywood%2Fdetent%2394"`) {
		t.Fatalf("dashboard rendered JSON details for completed session:\n%s", html)
	}
}

func TestDashboardRendersRunningActivityHoverCard(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 31, 15, 0, 0, 0, time.UTC)
	html := renderDashboard(t, templates.DashboardData{
		Title:         "Detent",
		ConnectorName: "github",
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			Running: []telemetry.Running{
				{
					Issue: telemetry.Issue{
						ID:         "issue-running-activity",
						Identifier: "digitaldrywood/detent#182",
						Title:      "Rich running activity hover card",
						State:      "In Progress",
					},
					SessionID:      "thread-running-activity",
					TurnCount:      7,
					LastEventAt:    &now,
					LastEvent:      "agent_message_delta",
					LastMessage:    "full latest codex activity message with enough detail to need a hover card",
					RuntimeSeconds: 150,
					Tokens: telemetry.Tokens{
						Input:  1200,
						Output: 340,
						Total:  1540,
					},
					RecentEvents: []telemetry.ActivityEvent{
						{At: now.Add(-2 * time.Second), Event: "turn_started", Message: "turn started"},
						{At: now.Add(-time.Second), Event: "agent_message_delta", Message: "working on templates"},
						{At: now, Event: "token_usage", Message: "tokens updated"},
					},
				},
			},
		},
	})

	for _, want := range []string{
		"data-running-activity",
		"data-running-activity-trigger",
		"data-running-activity-panel",
		"data-popover",
		"data-popover-trigger",
		"data-popover-panel",
		`data-popover-interactive="true"`,
		`data-popover-width="448"`,
		`aria-describedby="running-activity-0"`,
		`aria-hidden="true"`,
		`role="tooltip"`,
		"Latest message",
		"full latest codex activity message with enough detail to need a hover card",
		"Recent activity",
		"token_usage",
		"tokens updated",
		"Turn / tokens / runtime",
		"Turn 7",
		"In 1,200 / Out 340",
		"2m 30s",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("dashboard missing running activity marker %q:\n%s", want, html)
		}
	}
}

func TestDashboardIncludesMobileResponsiveLayouts(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 31, 15, 0, 0, 0, time.UTC)
	html := renderDashboard(t, templates.DashboardData{
		Title:         "Detent",
		ConnectorName: "github",
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			Running: []telemetry.Running{
				{
					Issue: telemetry.Issue{
						ID:         "responsive-running",
						Identifier: "digitaldrywood/detent#170",
						URL:        "https://github.com/digitaldrywood/detent/issues/170",
						Title:      "Responsive dashboard",
						State:      "In Progress",
					},
					SessionID:      "thread-responsive-running",
					StartedAt:      now.Add(-5 * time.Minute),
					LastEvent:      "turn_completed",
					LastMessage:    "responsive pass",
					RuntimeSeconds: 300,
				},
			},
			Queue: []telemetry.Queued{
				{
					Issue: telemetry.Issue{
						ID:         "responsive-retry",
						Identifier: "digitaldrywood/detent#171",
						Title:      "Retry responsive dashboard",
					},
					Attempt: 1,
				},
			},
			Blocked: []telemetry.Blocked{
				{
					Issue: telemetry.Issue{
						ID:         "responsive-blocked",
						Identifier: "digitaldrywood/detent#172",
						Title:      "Blocked responsive dashboard",
						State:      "Blocked",
					},
					SessionID: "thread-responsive-blocked",
				},
			},
			Completed: []telemetry.Completed{
				{
					Issue: telemetry.Issue{
						ID:         "responsive-recent",
						Identifier: "digitaldrywood/detent#173",
						Title:      "Recent responsive dashboard",
					},
					SessionID:   "thread-responsive-recent",
					CompletedAt: now,
				},
			},
		},
	})

	for _, want := range []string{
		"overflow-x-hidden",
		"px-3 py-3",
		"dashboard-shell",
		"min-h-screen",
		"dashboard-sidebar",
		"text-sidebar-foreground",
		"data-tui-sidebar-mobile-portal",
		"data-tui-sheet",
		"dashboard-topbar",
		"min-h-8",
		"h-7 w-7",
		"md:hidden",
		"hidden md:block",
		"sm:hidden",
		"hidden overflow-hidden rounded-md border border-border sm:block",
		"running-mobile-issue-popover-0",
		"retry-mobile-issue-popover-0",
		"blocked-mobile-issue-popover-0",
		"recent-mobile-issue-popover-0",
		"dashboard-body-grid grid min-w-0 items-start gap-4 lg:grid-cols-[minmax(0,1fr)_20rem] lg:gap-5 xl:grid-cols-[minmax(0,1fr)_22rem]",
		"dashboard-primary-column grid min-w-0 content-start gap-4 lg:gap-5",
		"dashboard-aside-column grid min-w-0 content-start gap-4 lg:gap-5",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("dashboard missing responsive marker %q:\n%s", want, html)
		}
	}
}

func TestDashboardRendersUnknownDiffStatusAsPending(t *testing.T) {
	t.Parallel()

	html := renderDashboard(t, templates.DashboardData{
		Title:         "Detent",
		ConnectorName: "github",
		Snapshot: telemetry.Snapshot{
			Running: []telemetry.Running{
				{
					Issue: telemetry.Issue{
						ID:         "issue-35",
						Identifier: "digitaldrywood/detent#35",
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

	now := time.Date(2026, 6, 22, 10, 0, 0, 0, time.UTC)
	lastRefreshAt := now.Add(-time.Minute)
	html := renderDashboard(t, templates.DashboardData{
		Title:         "Detent",
		ConnectorName: "memory",
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			Refresh: telemetry.Refresh{
				Status:        telemetry.RefreshStatusReady,
				LastRefreshAt: &lastRefreshAt,
			},
		},
	})

	for _, want := range []string{
		"No active issue sessions.",
		"No issues are currently backing off.",
		"No blocked sessions.",
		"No completed sessions recorded.",
		"No completed issues yet.",
		"No board states recorded.",
		"No completed session history yet.",
		"Budget disabled",
		"No rate-limit snapshot.",
		"No token trend yet.",
		"Lifetime totals unavailable.",
		"dashboard-empty-state",
		"items-start gap-3",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("dashboard missing %q:\n%s", want, html)
		}
	}
	for _, forbidden := range []string{
		"min-h-56",
		"py-8",
	} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("dashboard empty states should stay compact, found %q:\n%s", forbidden, html)
		}
	}
}

func TestDashboardIncludesMotionAndThemeHooks(t *testing.T) {
	t.Parallel()

	html := renderDashboard(t, templates.DashboardData{
		Title:         "Detent",
		ConnectorName: "github",
	})

	for _, want := range []string{
		`document.documentElement.classList.toggle("dark"`,
		`id="snapshot"`,
		`sse-swap="snapshot"`,
		`id="dashboard-sidebar-live"`,
		`sse-swap="sidebar"`,
		`hx-swap="morph:innerHTML"`,
		`sse-surface`,
		`sse-tick`,
		`data-detent-sse-warning`,
		`Live updates delayed`,
		`window.__detentSSEMetrics`,
		`[detent] SSE metrics`,
		`htmx:sseBeforeMessage`,
		`htmx.swap`,
		`replayPending`,
		`queued: metrics.queued`,
		`replayed: metrics.replayed`,
		`timeouts: metrics.timeouts`,
		`swapErrors: metrics.swapErrors`,
		`diagnostics: diagnosticsFor`,
		`state.processing`,
		`detentSseKey`,
		`htmx:sseError`,
		`detentSseStatus`,
		`/static/js/templui/dialog.min.js`,
		`/static/js/templui/popover.min.js`,
		`/static/js/templui/sidebar.min.js`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("dashboard missing %q:\n%s", want, html)
		}
	}
}

func TestDashboardRendersTemplUISidebar(t *testing.T) {
	t.Parallel()

	html := renderDashboard(t, templates.DashboardData{
		Title:            "Detent",
		ConnectorName:    "github",
		SidebarCollapsed: true,
		Projects: []templates.ProjectSmallMultiple{
			{
				ID:      "detent",
				Name:    "Detent",
				Running: 2,
			},
		},
	})

	for _, want := range []string{
		`data-tui-sidebar-state="collapsed"`,
		`data-tui-sidebar-collapsible="icon"`,
		`data-tui-sidebar-keyboard-shortcut="b"`,
		`data-tui-sidebar="menu-badge"`,
		`data-tui-popover-root`,
		`data-tui-popover-trigger`,
		`data-tui-dialog`,
		`data-tui-sheet`,
		`Detent - active, 2 running`,
		`>Fleet</span>`,
		`>Reports</span>`,
		`>Settings</span>`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("dashboard sidebar missing %q:\n%s", want, html)
		}
	}

	for _, forbidden := range []string{
		"data-dashboard-sidebar-toggle",
		"data-dashboard-project-drag-handle",
		"detent.dashboard.sidebar.collapsed",
		"grid-rows-[auto_auto_minmax(0,1fr)_auto]",
	} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("dashboard sidebar rendered old marker %q:\n%s", forbidden, html)
		}
	}
}

func TestDashboardRendersProjectOrderControls(t *testing.T) {
	t.Parallel()

	html := renderDashboard(t, templates.DashboardData{
		Title:         "Detent",
		ConnectorName: "github",
		Projects: []templates.ProjectSmallMultiple{
			{ID: "paused", Name: "Paused", Paused: true},
			{ID: "idle", Name: "Idle"},
			{ID: "active", Name: "Active", Running: 1},
			{ID: "blocked", Name: "Blocked", Blocked: 1},
		},
	})

	for _, want := range []string{
		`data-dashboard-project-order-list`,
		`data-dashboard-project-entry`,
		`data-project-id="blocked"`,
		`data-project-default-index="0"`,
		`data-dashboard-project-drag-handle`,
		`data-dashboard-project-order-controls`,
		`data-dashboard-project-move="up"`,
		`data-dashboard-project-move="down"`,
		`Move Blocked up`,
		`Move Blocked down`,
		`data-dashboard-project-order-reset`,
		`Reset order`,
		`detent.ui.projectOrder`,
		`storageVersion = 1`,
		`window.localStorage`,
		`JSON.stringify({ v: storageVersion, order: order })`,
		`document.addEventListener("DOMContentLoaded"`,
		`document.addEventListener("htmx:afterSettle"`,
		`function projectFilterActive()`,
		`!projectFilterActive()`,
		`document.addEventListener("dragstart"`,
		`document.addEventListener("dragover"`,
		`document.addEventListener("drop"`,
		`document.addEventListener("htmx:beforeSwap"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("dashboard project order controls missing %q:\n%s", want, html)
		}
	}

	blockedIndex := strings.Index(html, `data-project-id="blocked"`)
	activeIndex := strings.Index(html, `data-project-id="active"`)
	idleIndex := strings.Index(html, `data-project-id="idle"`)
	pausedIndex := strings.Index(html, `data-project-id="paused"`)
	if blockedIndex < 0 || activeIndex < 0 || idleIndex < 0 || pausedIndex < 0 {
		t.Fatalf("project indexes missing: blocked=%d active=%d idle=%d paused=%d\n%s", blockedIndex, activeIndex, idleIndex, pausedIndex, html)
	}
	if blockedIndex >= activeIndex || activeIndex >= idleIndex || idleIndex >= pausedIndex {
		t.Fatalf("project order should be blocked, active, idle, paused: blocked=%d active=%d idle=%d paused=%d\n%s", blockedIndex, activeIndex, idleIndex, pausedIndex, html)
	}
}

func TestDashboardDistinguishesMissingRunningDetails(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 22, 10, 30, 0, 0, time.UTC)
	lastRefreshAt := now.Add(-time.Minute)
	html := renderDashboard(t, templates.DashboardData{
		Title:         "Detent",
		ConnectorName: "github",
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			Refresh: telemetry.Refresh{
				Status:        telemetry.RefreshStatusReady,
				LastRefreshAt: &lastRefreshAt,
			},
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

	now := time.Date(2026, 6, 22, 10, 35, 0, 0, time.UTC)
	lastRefreshAt := now.Add(-time.Minute)
	html := renderDashboard(t, templates.DashboardData{
		Title:         "Detent",
		ConnectorName: "github",
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			Refresh: telemetry.Refresh{
				Status:        telemetry.RefreshStatusReady,
				LastRefreshAt: &lastRefreshAt,
			},
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

func renderProjectRunsSnapshot(t *testing.T, data templates.DashboardData) string {
	t.Helper()

	var buf bytes.Buffer
	if err := templates.ProjectRunsSnapshot(data).Render(context.Background(), &buf); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	return buf.String()
}

func renderProjectKanbanPage(t *testing.T, data templates.DashboardData) string {
	t.Helper()

	var buf bytes.Buffer
	if err := templates.ProjectKanbanPage(data).Render(context.Background(), &buf); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	return buf.String()
}

func renderProjectDiagnosticsPage(t *testing.T, data templates.DashboardData) string {
	t.Helper()

	var buf bytes.Buffer
	if err := templates.ProjectDiagnosticsPage(data).Render(context.Background(), &buf); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	return buf.String()
}

func renderDashboardShell(t *testing.T, data templates.DashboardShellData) string {
	t.Helper()

	var buf bytes.Buffer
	child := templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		_, err := io.WriteString(w, `<main data-shell-test-child>Shell content</main>`)
		return err
	})
	ctx := templ.WithChildren(context.Background(), child)
	if err := templates.DashboardShell(data).Render(ctx, &buf); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	return buf.String()
}

func assertActiveSidebarLink(t *testing.T, body string, href string) {
	t.Helper()

	if !sidebarLinkActive(body, href) {
		t.Fatalf("body missing active sidebar link %q:\n%s", href, body)
	}
}

func assertInactiveSidebarLink(t *testing.T, body string, href string) {
	t.Helper()

	if sidebarLinkActive(body, href) {
		t.Fatalf("body rendered inactive sidebar link %q as active:\n%s", href, body)
	}
}

func assertTemplateHealthInSidebar(t *testing.T, body string) {
	t.Helper()

	sidebarIndex := strings.Index(body, `id="dashboard-sidebar-live"`)
	healthIndex := strings.Index(body, `id="github-api-health"`)
	mainIndex := strings.Index(body, `data-tui-sidebar="inset"`)
	if sidebarIndex < 0 || healthIndex < 0 || mainIndex < 0 {
		t.Fatalf("body missing sidebar health placement markers: sidebar=%d health=%d main=%d\n%s", sidebarIndex, healthIndex, mainIndex, body)
	}
	if healthIndex < sidebarIndex || healthIndex > mainIndex {
		t.Fatalf("github api health rendered outside sidebar: sidebar=%d health=%d main=%d\n%s", sidebarIndex, healthIndex, mainIndex, body)
	}
}

func gitHubAPIHealthStateTestLabel(state string) string {
	switch state {
	case "healthy":
		return "Healthy"
	case "warning":
		return "Warning"
	case "backoff":
		return "Backoff"
	case "exhausted":
		return "Exhausted"
	default:
		return "Unknown"
	}
}

func sidebarLinkActive(body string, href string) bool {
	pattern := `<a[^>]*href="` + regexp.QuoteMeta(href) + `"[^>]*>`
	for _, link := range regexp.MustCompile(pattern).FindAllString(body, -1) {
		if strings.Contains(link, `data-tui-sidebar-active="true"`) && strings.Contains(link, `aria-current="page"`) {
			return true
		}
	}
	return false
}

func dashboardSection(t *testing.T, html string, start string, end string) string {
	t.Helper()

	startIndex := strings.Index(html, start)
	if startIndex < 0 {
		t.Fatalf("section start %q missing:\n%s", start, html)
	}
	endIndex := strings.Index(html[startIndex:], end)
	if endIndex < 0 {
		t.Fatalf("section end %q after %q missing:\n%s", end, start, html[startIndex:])
	}
	return html[startIndex : startIndex+endIndex]
}

func dashboardTail(t *testing.T, html string, start string) string {
	t.Helper()

	startIndex := strings.Index(html, start)
	if startIndex < 0 {
		t.Fatalf("section start %q missing:\n%s", start, html)
	}
	return html[startIndex:]
}

func projectKanbanSection(t *testing.T, html string) string {
	t.Helper()

	startIndex := strings.Index(html, `aria-label="Project Kanban"`)
	if startIndex < 0 {
		t.Fatalf("project Kanban section missing:\n%s", html)
	}
	endIndex := strings.Index(html[startIndex:], `<script>`)
	if endIndex < 0 {
		return html[startIndex:]
	}
	return html[startIndex : startIndex+endIndex]
}

func compactKanbanCardSection(t *testing.T, html string, title string) string {
	t.Helper()

	titleIndex := strings.Index(html, title)
	if titleIndex < 0 {
		t.Fatalf("card title %q missing:\n%s", title, html)
	}
	startIndex := strings.LastIndex(html[:titleIndex], `<article`)
	if startIndex < 0 {
		t.Fatalf("card title %q missing enclosing article:\n%s", title, html)
	}
	endIndex := strings.Index(html[titleIndex:], `</article>`)
	if endIndex < 0 {
		t.Fatalf("card title %q missing article close:\n%s", title, html[titleIndex:])
	}
	return html[startIndex : titleIndex+endIndex+len(`</article>`)]
}

func projectKanbanVisibilityKeyFromHTML(t *testing.T, html string) string {
	t.Helper()

	const attr = `data-project-kanban-visibility-key="`
	start := strings.Index(html, attr)
	if start < 0 {
		t.Fatalf("project kanban visibility key missing:\n%s", html)
	}
	start += len(attr)
	end := strings.Index(html[start:], `"`)
	if end < 0 {
		t.Fatalf("project kanban visibility key unterminated:\n%s", html[start:])
	}
	return html[start : start+end]
}
