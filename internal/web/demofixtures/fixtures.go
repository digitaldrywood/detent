package demofixtures

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/projectcolor"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web/templates"
)

const (
	demoPrimaryProjectID   = "dogfood"
	demoPrimaryProjectName = "detent-core"
)

var demoBaseTime = time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

func ProjectByID(projects []templates.ProjectSmallMultiple, id string) (templates.ProjectSmallMultiple, bool) {
	for _, project := range projects {
		if strings.TrimSpace(project.ID) == strings.TrimSpace(id) {
			return project, true
		}
	}
	return templates.ProjectSmallMultiple{}, false
}

func SnapshotForScenario(id string, variant string) telemetry.Snapshot {
	snapshot := demoHealthySnapshot()
	switch variant {
	case "empty":
		snapshot = demoEmptySnapshot()
		snapshot.GeneratedAt = time.Time{}
		snapshot.Project = telemetry.Project{}
		snapshot.Projects = nil
	case "project-empty", "reports-empty", "settings-empty", "no-history":
		snapshot = demoEmptySnapshot()
	case "overloaded", "backoff-heavy":
		snapshot = demoOverloadedSnapshot()
	case "draining":
		snapshot = demoDrainingSnapshot()
	case "degraded":
		snapshot = demoDegradedSnapshot()
	case "budget-refusals":
		snapshot = demoBudgetRefusalsSnapshot()
	case "blocked-heavy":
		snapshot = demoBlockedHeavySnapshot()
	case "long-content":
		snapshot = demoLongContentSnapshot()
	case "dense", "dense-kanban":
		snapshot = demoDenseSnapshot()
	case "hot-path", "model-heavy", "filtered-project":
		snapshot = demoHotPathSnapshot()
	case "tracker-refresh-gap":
		snapshot = demoTrackerRefreshGapSnapshot()
	case "external-lane-timer":
		snapshot = demoExternalLaneTimerSnapshot()
	case "unblocker-boost":
		snapshot.BoardIssues[1].UnblockerCount = 2
	case "startup-loading":
		snapshot = demoStartupLoadingSnapshot()
	case "github-api-healthy":
		snapshot = demoGitHubAPIHealthySnapshot()
	case "github-api-warning":
		snapshot = demoGitHubAPIWarningSnapshot()
	case "github-api-secondary-backoff":
		snapshot = demoGitHubAPISecondaryBackoffSnapshot()
	case "github-api-primary-exhausted":
		snapshot = demoGitHubAPIPrimaryExhaustedSnapshot()
	case "backend-capacity-outage":
		snapshot = demoBackendCapacityOutageSnapshot()
	case "board-ramp-active-recoveries":
		snapshot = demoBoardRampActiveRecoveriesSnapshot()
	case "board-scheduled-pacing":
		snapshot = demoBoardScheduledPacingSnapshot()
	case "board-degraded-health-banners":
		snapshot = demoBoardDegradedHealthBannersSnapshot()
	}
	if variant == "terminal" {
		snapshot.BoardIssues = append(snapshot.BoardIssues, demoIssue(demoPrimaryProjectID, "demo-cancelled", "digitaldrywood/detent-core#5259", "Cancelled alternate dashboard theme", "Cancelled", 48))
	}
	if id != "" && id != demoPrimaryProjectID && variant == "project-empty" {
		snapshot.Projects = demoProjectSnapshots(ProjectsForVariant("project-empty"))
	}
	return snapshot
}

func demoHealthySnapshot() telemetry.Snapshot {
	now := demoBaseTime
	dayMax := 42.0
	issueMax := 8.0
	leaseRenewed := now.Add(-90 * time.Second)
	leaseExpires := now.Add(9 * time.Minute)
	nextRefresh := now.Add(45 * time.Second)
	lastRefresh := now.Add(-15 * time.Second)
	snapshot := telemetry.Snapshot{
		GeneratedAt:  now,
		Project:      telemetry.Project{DisplayName: "multiple projects"},
		Instance:     telemetry.Instance{Name: "detent-demo-screenshots", GitHubLogin: "detent-bot", AuthorizationScope: "repo, read:project", AuthorizationConfigured: true},
		Projects:     demoProjectSnapshots(ProjectsForVariant("healthy")),
		DashboardURL: "http://localhost:0",
		Shutdown:     telemetry.Shutdown{Status: "running"},
		Refresh:      telemetry.Refresh{PollIntervalSeconds: 60, LastRefreshAt: &lastRefresh, NextRefreshAt: &nextRefresh},
		Counts:       telemetry.Counts{Running: 3, Queue: 3, Blocked: 2, Completed: 4},
		Events: []telemetry.ActivityEvent{
			{
				At:      now.Add(-3 * time.Minute),
				Event:   "workspace_reap_succeeded",
				Message: "workspace cleanup succeeded for digitaldrywood/mobile-client#5243 reason=cancelled worktrees=1 branches=1 processes=0",
			},
		},
		BoardIssues: []telemetry.Issue{
			demoIssue(demoPrimaryProjectID, "demo-backlog", "digitaldrywood/detent-core#5250", "Backlog observability fixture intake", "Backlog", 72),
			demoIssue(demoPrimaryProjectID, "demo-todo", "digitaldrywood/detent-core#5251", "Add screenshot manifest smoke test", "Todo", 9),
			demoIssue("agent-lab", "agent-lab-todo", "digitaldrywood/agent-lab#111", "Try secondary runner routing", "Todo", 11),
		},
		Pipeline: []telemetry.Issue{
			demoPipelineIssue(demoPrimaryProjectID, "demo-review", "digitaldrywood/detent-core#5290", "Review deterministic chart colors", "Human Review", 5290, "success", "clean", 2),
			demoPipelineIssue(demoPrimaryProjectID, "demo-rework", "digitaldrywood/detent-core#5291", "Address visual diff finding", "Rework", 5291, "failure", "dirty", 12),
			demoPipelineIssue("release-train", "demo-merging", "digitaldrywood/release-train#5292", "Merge release readiness bundle", "Merging", 5292, "pending", "clean", 1),
			demoPipelineIssue(demoPrimaryProjectID, "demo-done-pr", "digitaldrywood/detent-core#5293", "Ship completed PR lane fixture", "Done", 5293, "success", "clean", 24),
		},
		Running: []telemetry.Running{
			{
				Issue: demoIssueWithRuntimeIdentity(
					demoIssue(demoPrimaryProjectID, "demo-running-core", "digitaldrywood/detent-core#5260", "Implement page-addressable screenshot scenarios", "In Progress", 1),
					agentidentity.Configured("codex-high", "codex", "high", "code", "gpt-5.5", "", "", "", now.Add(-34*time.Minute)).
						Merge(agentidentity.RuntimeUpdate("gpt-5.6-sol", "openai", "xhigh", "priority", now.Add(-33*time.Minute))),
				),
				DetentSessionID: 5260,
				WorkerHost:      "demo-worker-a",
				ProcessIdentity: "pid-5260",
				WorkspacePath:   "/tmp/detent-screenshots/workspaces/detent-core/5260",
				SessionID:       "thread-demo-core-5260",
				TurnCount:       7,
				StartedAt:       now.Add(-34 * time.Minute),
				LastEventAt:     demoTimePtr(now.Add(-2 * time.Minute)),
				LastEvent:       "agent_message",
				LastMessage:     "Rendered manifest and route smoke checks.",
				RecentEvents: []telemetry.ActivityEvent{
					{At: now.Add(-4 * time.Minute), Event: "tool_call", Message: "go test ./internal/web"},
					{At: now.Add(-2 * time.Minute), Event: "agent_message", Message: "Rendered manifest and route smoke checks."},
				},
				RuntimeSeconds: 2040,
				DiffAdded:      812,
				DiffRemoved:    96,
				DiffFiles:      9,
				DiffStatus:     "ok",
				Tokens:         telemetry.Tokens{Input: 38240, CachedInput: 24700, Output: 12840, ReasoningOutput: 3100, Total: 51080, RuntimeSeconds: 2040, ModelContextWindow: demoInt64Ptr(64000)},
			},
			{
				Issue: demoIssueWithRuntimeIdentity(
					demoIssue("docs-site", "demo-running-docs", "digitaldrywood/docs-site#5261", "Write direct loading documentation examples", "In Progress", 2),
					agentidentity.Configured("claude-local", "claude_code", "local", "code", "fable", "ollama", "", "", now.Add(-18*time.Minute)).
						Merge(agentidentity.RuntimeUpdate("qwen3-coder", "", "", "", now.Add(-17*time.Minute))),
				),
				DetentSessionID: 5261,
				WorkerHost:      "demo-worker-b",
				ProcessIdentity: "pid-5261",
				WorkspacePath:   "/tmp/detent-screenshots/workspaces/docs-site/5261",
				SessionID:       "thread-demo-docs-5261",
				TurnCount:       4,
				StartedAt:       now.Add(-18 * time.Minute),
				LastEventAt:     demoTimePtr(now.Add(-90 * time.Second)),
				LastEvent:       "token_usage",
				LastMessage:     "21,450 total tokens (15,200 in, 6,250 out)",
				RuntimeSeconds:  1080,
				DiffAdded:       210,
				DiffRemoved:     18,
				DiffFiles:       3,
				DiffStatus:      "ok",
				Tokens:          telemetry.Tokens{Input: 15200, CachedInput: 9100, Output: 6250, ReasoningOutput: 1500, Total: 21450, RuntimeSeconds: 1080, ModelContextWindow: demoInt64Ptr(32000)},
			},
			{
				Issue:           demoIssue("infra-platform", "demo-running-infra", "digitaldrywood/infra-platform#5262", "Verify isolated runtime paths on ephemeral ports", "In Progress", 3),
				DetentSessionID: 5262,
				WorkerHost:      "demo-worker-c",
				ProcessIdentity: "pid-5262",
				WorkspacePath:   "/tmp/detent-screenshots/workspaces/infra-platform/5262",
				SessionID:       "thread-demo-infra-5262",
				TurnCount:       5,
				StartedAt:       now.Add(-21 * time.Minute),
				LastEventAt:     demoTimePtr(now.Add(-3 * time.Minute)),
				LastEvent:       "diff_stats",
				LastMessage:     "5 files changed, +164 -22",
				RuntimeSeconds:  1260,
				DiffAdded:       164,
				DiffRemoved:     22,
				DiffFiles:       5,
				DiffStatus:      "ok",
				Tokens:          telemetry.Tokens{Input: 21100, CachedInput: 14300, Output: 8100, ReasoningOutput: 2100, Total: 29200, RuntimeSeconds: 1260, ModelContextWindow: demoInt64Ptr(32768)},
			},
		},
		Queue: []telemetry.Queued{
			demoQueued("docs-site", "demo-queued-docs", "digitaldrywood/docs-site#5270", "Capture reports screenshots", 1, now.Add(6*time.Minute), "waiting for weighted fair share slot"),
			demoQueued("billing-api", "demo-queued-billing", "digitaldrywood/billing-api#5271", "Add budget refusal screenshot fixture", 2, now.Add(14*time.Minute), "previous attempt exceeded budget cap"),
			demoQueued("mobile-client", "demo-queued-mobile", "digitaldrywood/mobile-client#5272", "Exercise compact lane overflow on mobile", 3, now.Add(28*time.Minute), "project paused until release train clears"),
		},
		Blocked: []telemetry.Blocked{
			demoBlocked("billing-api", "demo-blocked-billing", "digitaldrywood/billing-api#5280", "Dependency issue waiting on ledger migration", "Depends on digitaldrywood/billing-api#5200", "Todo", 10, telemetry.BlockedSourceDependency),
			demoBlocked(demoPrimaryProjectID, "demo-blocked-hook", "digitaldrywood/detent-core#5281", "Workspace hook error needs operator input", "after_create hook exited 2", "operator", 5),
		},
		Completed: []telemetry.Completed{
			demoCompleted(demoPrimaryProjectID, "demo-complete-core", "digitaldrywood/detent-core#5240", "Complete dashboard density pass", "Done", 91, "gpt-5-codex", 64000),
			demoCompleted("docs-site", "demo-complete-docs", "digitaldrywood/docs-site#5241", "Publish screenshot capture guide", "Human Review", 57, "gpt-5-codex", 38000),
			demoCompleted("release-train", "demo-complete-release", "digitaldrywood/release-train#5242", "Prepare release note bundle", "Merging", 73, "gpt-5", 52000),
			demoCompleted("mobile-client", "demo-complete-mobile", "digitaldrywood/mobile-client#5243", "Cancel stale mobile board experiment", "Cancelled", 11, "gpt-5-mini", 9000),
		},
		Budget: telemetry.Budget{
			Enabled:           true,
			PerDayMaxUSD:      &dayMax,
			PerIssueMaxUSD:    &issueMax,
			CurrentSpendUSD:   18.72,
			ProjectedCostUSD:  5.44,
			ProjectedSpendUSD: 24.16,
			PeriodStart:       now.Truncate(24 * time.Hour),
			PeriodEnd:         now.Truncate(24 * time.Hour).Add(24 * time.Hour),
			SpendPoints:       demoBudgetSpendPoints(now, 18.72),
			Days:              demoBudgetDays(),
		},
		RateLimits: &telemetry.RateLimits{
			LimitID:       "codex-demo",
			LimitName:     "Codex demo pool",
			Primary:       &telemetry.RateLimitBucket{Remaining: 8200, Used: 1800, Limit: 10000, ResetAt: demoTimePtr(now.Add(48 * time.Minute)), ResetInSeconds: 2880},
			Secondary:     &telemetry.RateLimitBucket{Remaining: 460, Used: 40, Limit: 500, ResetAt: demoTimePtr(now.Add(8 * time.Minute)), ResetInSeconds: 480},
			Credits:       &telemetry.RateLimitBucket{HasCredits: true, Balance: "healthy"},
			GitHubGraphQL: &telemetry.RateLimitBucket{Remaining: 4320, Used: 680, Limit: 5000, ResetAt: demoTimePtr(now.Add(42 * time.Minute)), ResetInSeconds: 2520},
			GitHubREST:    &telemetry.RateLimitBucket{Remaining: 4878, Used: 122, Limit: 5000, ResetAt: demoTimePtr(now.Add(46 * time.Minute)), ResetInSeconds: 2760},
			GraphQLCost:   &telemetry.GraphQLCost{TotalQueries: 88, TotalCost: 680, Contributors: []telemetry.GraphQLCostContributor{{QueryType: "project_items", Count: 30, Cost: 300}, {QueryType: "pull_requests", Count: 18, Cost: 180}}},
			RESTUsage:     &telemetry.RESTUsage{TotalRequests: 122, Contributors: []telemetry.RESTUsageContributor{{EndpointFamily: "issues", Count: 82, Remaining: 4878, Limit: 5000}, {EndpointFamily: "pull requests", Count: 40, Remaining: 4878, Limit: 5000}}},
		},
		Tokens:          telemetry.Tokens{Input: 74540, Output: 27190, Total: 101730, RuntimeSeconds: 4380},
		Throughput:      telemetry.TokenThroughput{TokensPerSecond: 23.5, WindowSeconds: 600, Tokens: 14100},
		LifetimeTotals:  telemetry.LifetimeTotals{Available: true, InputTokens: 4410000, OutputTokens: 1390000, TotalTokens: 5800000, RuntimeSeconds: 242000, Sessions: 182, Runs: 37},
		CycleTime:       demoCycleTime(now),
		WorkflowMetrics: demoWorkflowMetrics(now),
		TokenTrend:      demoTokenTrend(now),
	}
	for i := range snapshot.Running {
		snapshot.Running[i].LeaseRenewedAt = &leaseRenewed
		snapshot.Running[i].LeaseExpiresAt = &leaseExpires
	}
	return snapshot
}

func demoEmptySnapshot() telemetry.Snapshot {
	now := demoBaseTime
	lastRefresh := now.Add(-15 * time.Second)
	nextRefresh := now.Add(time.Minute)
	return telemetry.Snapshot{
		GeneratedAt:     now,
		Project:         telemetry.Project{DisplayName: "multiple projects"},
		Instance:        telemetry.Instance{Name: "detent-demo-screenshots", GitHubLogin: "detent-bot", AuthorizationScope: "repo, read:project", AuthorizationConfigured: true},
		Projects:        demoProjectSnapshots(ProjectsForVariant("project-empty")),
		DashboardURL:    "http://localhost:0",
		Shutdown:        telemetry.Shutdown{Status: "running"},
		Refresh:         telemetry.Refresh{PollIntervalSeconds: 60, Status: telemetry.RefreshStatusReady, LastRefreshAt: &lastRefresh, NextRefreshAt: &nextRefresh},
		LifetimeTotals:  telemetry.LifetimeTotals{Available: true},
		CycleTime:       telemetry.CycleTimeReport{Available: false, DegradedReason: "no completed sessions in the selected window"},
		WorkflowMetrics: demoEmptyWorkflowMetrics(now),
	}
}

func demoStartupLoadingSnapshot() telemetry.Snapshot {
	now := demoBaseTime
	nextRefresh := now.Add(20 * time.Second)
	refresh := telemetry.Refresh{PollIntervalSeconds: 60, Status: telemetry.RefreshStatusInitializing, NextRefreshAt: &nextRefresh}
	snapshot := telemetry.Snapshot{
		GeneratedAt:    now,
		Project:        telemetry.Project{DisplayName: "multiple projects"},
		Instance:       telemetry.Instance{Name: "detent-demo-screenshots", GitHubLogin: "detent-bot", AuthorizationScope: "repo, read:project", AuthorizationConfigured: true},
		Projects:       demoProjectSnapshots(ProjectsForVariant("project-empty")),
		DashboardURL:   "http://localhost:0",
		Shutdown:       telemetry.Shutdown{Status: "running"},
		Refresh:        refresh,
		LifetimeTotals: telemetry.LifetimeTotals{Available: true},
	}
	for i := range snapshot.Projects {
		snapshot.Projects[i].Refresh = refresh
	}
	return snapshot
}

func demoOverloadedSnapshot() telemetry.Snapshot {
	snapshot := demoHealthySnapshot()
	now := snapshot.GeneratedAt
	dayMax := 42.0
	snapshot.Queue = append(snapshot.Queue,
		demoQueued(demoPrimaryProjectID, "demo-queued-overload-1", "digitaldrywood/detent-core#5273", "Retry overloaded visual comparison job", 4, now.Add(38*time.Minute), "secondary rate limit in effect"),
		demoQueued("infra-platform", "demo-queued-overload-2", "digitaldrywood/infra-platform#5274", "Re-run isolated port smoke test", 5, now.Add(50*time.Minute), "rate-limit retry budget is low"),
	)
	snapshot.Counts.Queue = len(snapshot.Queue)
	snapshot.Budget.CurrentSpendUSD = 39.85
	snapshot.Budget.ProjectedCostUSD = 4.9
	snapshot.Budget.ProjectedSpendUSD = 44.75
	snapshot.Budget.PerDayMaxUSD = &dayMax
	snapshot.RateLimits = &telemetry.RateLimits{
		LimitID:       "codex-demo-pressure",
		LimitName:     "Codex demo pool",
		Primary:       &telemetry.RateLimitBucket{Remaining: 260, Used: 9740, Limit: 10000, ResetAt: demoTimePtr(now.Add(21 * time.Minute)), ResetInSeconds: 1260},
		Secondary:     &telemetry.RateLimitBucket{Remaining: 4, Used: 496, Limit: 500, ResetAt: demoTimePtr(now.Add(9 * time.Minute)), ResetInSeconds: 540},
		Credits:       &telemetry.RateLimitBucket{HasCredits: true, Balance: "low"},
		GitHubGraphQL: &telemetry.RateLimitBucket{Remaining: 95, Used: 4905, Limit: 5000, ResetAt: demoTimePtr(now.Add(34 * time.Minute)), ResetInSeconds: 2040},
		GitHubREST:    &telemetry.RateLimitBucket{Remaining: 430, Used: 4570, Limit: 5000, ResetAt: demoTimePtr(now.Add(18 * time.Minute)), ResetInSeconds: 1080},
		GraphQLCost:   &telemetry.GraphQLCost{TotalQueries: 410, TotalCost: 4905, Contributors: []telemetry.GraphQLCostContributor{{QueryType: "project_items", Count: 180, Cost: 2700}, {QueryType: "review_threads", Count: 90, Cost: 1260}, {QueryType: "rate_limit_probe", Count: 40, Cost: 480}}},
		RESTUsage:     &telemetry.RESTUsage{TotalRequests: 4570, Contributors: []telemetry.RESTUsageContributor{{EndpointFamily: "issues", Count: 4100, Remaining: 430, Limit: 5000}, {EndpointFamily: "check runs", Count: 470, Remaining: 430, Limit: 5000}}},
	}
	return snapshot
}

func demoGitHubAPIHealthySnapshot() telemetry.Snapshot {
	return demoHealthySnapshot()
}

func demoGitHubAPIWarningSnapshot() telemetry.Snapshot {
	snapshot := demoHealthySnapshot()
	now := snapshot.GeneratedAt
	snapshot.RateLimits.GitHubREST = &telemetry.RateLimitBucket{Remaining: 240, Used: 4760, Limit: 5000, ResetAt: demoTimePtr(now.Add(24 * time.Minute)), ResetInSeconds: 1440}
	snapshot.RateLimits.GitHubGraphQL = &telemetry.RateLimitBucket{Remaining: 3200, Used: 1800, Limit: 5000, ResetAt: demoTimePtr(now.Add(42 * time.Minute)), ResetInSeconds: 2520}
	snapshot.RateLimits.RESTUsage = &telemetry.RESTUsage{TotalRequests: 4760, Contributors: []telemetry.RESTUsageContributor{{EndpointFamily: "issues", Count: 3900, Remaining: 240, Limit: 5000}, {EndpointFamily: "pull requests", Count: 860, Remaining: 240, Limit: 5000}}}
	return snapshot
}

func demoGitHubAPISecondaryBackoffSnapshot() telemetry.Snapshot {
	snapshot := demoHealthySnapshot()
	now := snapshot.GeneratedAt
	backoffUntil := now.Add(5 * time.Minute)
	snapshot.RateLimits.GitHubREST = &telemetry.RateLimitBucket{Remaining: 4878, Used: 122, Limit: 5000, ResetAt: demoTimePtr(now.Add(46 * time.Minute)), ResetInSeconds: 2760}
	snapshot.RateLimits.GitHubGraphQL = &telemetry.RateLimitBucket{Remaining: 4880, Used: 120, Limit: 5000, ResetAt: demoTimePtr(now.Add(42 * time.Minute)), ResetInSeconds: 2520}
	snapshot.RateLimits.RESTUsage = &telemetry.RESTUsage{
		TotalRequests: 122,
		RateLimited:   true,
		BackoffUntil:  &backoffUntil,
		Contributors: []telemetry.RESTUsageContributor{
			{EndpointFamily: "pull requests", Count: 2, RetryAfterMS: (5 * time.Minute).Milliseconds(), RateLimited: true, LastStatus: 429, Remaining: 4878, Limit: 5000},
			{EndpointFamily: "check runs", Count: 1, RateLimited: true, LastStatus: 429, Remaining: 4878, Limit: 5000},
		},
	}
	return snapshot
}

func demoGitHubAPIPrimaryExhaustedSnapshot() telemetry.Snapshot {
	snapshot := demoHealthySnapshot()
	now := snapshot.GeneratedAt
	snapshot.RateLimits.GitHubREST = &telemetry.RateLimitBucket{Remaining: 0, Used: 5000, Limit: 5000, ResetAt: demoTimePtr(now.Add(30 * time.Minute)), ResetInSeconds: 1800}
	snapshot.RateLimits.GitHubGraphQL = &telemetry.RateLimitBucket{Remaining: 4880, Used: 120, Limit: 5000, ResetAt: demoTimePtr(now.Add(42 * time.Minute)), ResetInSeconds: 2520}
	snapshot.RateLimits.RESTUsage = &telemetry.RESTUsage{TotalRequests: 5000, Contributors: []telemetry.RESTUsageContributor{{EndpointFamily: "issues", Count: 4400, Remaining: 0, Limit: 5000}, {EndpointFamily: "pull requests", Count: 600, Remaining: 0, Limit: 5000}}}
	return snapshot
}

func demoBackendCapacityOutageSnapshot() telemetry.Snapshot {
	snapshot := demoHealthySnapshot()
	snapshot.GeneratedAt = time.Date(2026, 7, 10, 16, 52, 0, 0, time.UTC)
	resumeAt := time.Date(2026, 7, 10, 17, 44, 0, 0, time.UTC)
	outage := telemetry.BackendOutage{
		BackendID: "openai",
		Provider:  "openai",
		Reason:    "provider usage limit reached",
		ResumeAt:  resumeAt,
	}
	snapshot.BackendOutages = []telemetry.BackendOutage{outage, outage}
	return snapshot
}

func demoBoardRampActiveRecoveriesSnapshot() telemetry.Snapshot {
	snapshot := demoHealthySnapshot()
	now := snapshot.GeneratedAt
	snapshot.DispatchRecoveries = []telemetry.DispatchRecovery{
		{ProjectID: demoPrimaryProjectID, Kind: "github_rest", Status: "ramping", StartedAt: now.Add(-2 * time.Minute), Limit: 1, MaxConcurrent: 6, Admitted: 1},
		{ProjectID: "docs-site", Kind: "backend_capacity", Status: "ramping", StartedAt: now.Add(-time.Minute), Limit: 2, MaxConcurrent: 6, Admitted: 2},
	}
	snapshot.OverloadRetriesLastHour = 4
	return snapshot
}

func demoBoardScheduledPacingSnapshot() telemetry.Snapshot {
	snapshot := demoHealthySnapshot()
	now := snapshot.GeneratedAt
	snapshot.DispatchRecoveries = []telemetry.DispatchRecovery{
		{ProjectID: demoPrimaryProjectID, Kind: "github_rest", Reason: "remaining 288 at or below dispatch floor", Status: "waiting", StartedAt: now.Add(-3 * time.Minute), ResumeAt: now.Add(14 * time.Minute), MaxConcurrent: 6},
		{ProjectID: "docs-site", Kind: "github_rest", Reason: "remaining 296 at or below dispatch floor", Status: "waiting", StartedAt: now.Add(-2 * time.Minute), ResumeAt: now.Add(14 * time.Minute), MaxConcurrent: 6},
		{ProjectID: "billing-api", Kind: "pull_request_hydration", Reason: "rest_budget_reserved", Status: "waiting", StartedAt: now.Add(-time.Minute), ResumeAt: now.Add(14 * time.Minute), MaxConcurrent: 6},
	}
	snapshot.BackendOutages = []telemetry.BackendOutage{{
		BackendID:   "github-rest",
		BackendKind: "tracker",
		Provider:    "github",
		Kind:        "github_rest_rate_limit",
		Reason:      "GitHub REST remaining 288 is at or below dispatch floor 1000",
		DetectedAt:  now.Add(-3 * time.Minute),
		ResumeAt:    now.Add(14 * time.Minute),
	}}
	return snapshot
}

func demoBoardDegradedHealthBannersSnapshot() telemetry.Snapshot {
	snapshot := demoHealthySnapshot()
	now := snapshot.GeneratedAt
	snapshot.FailureBreakers = []telemetry.FailureBreaker{
		{ProjectID: demoPrimaryProjectID, Class: "backend_startup_timeout", Count: 4, WindowSeconds: 3600, ResumeAt: now.Add(12 * time.Minute)},
		{ProjectID: "docs-site", Class: "session_token_ceiling", Count: 3, WindowSeconds: 3600, ResumeAt: now.Add(18 * time.Minute)},
	}
	snapshot.DispatchRecoveries = []telemetry.DispatchRecovery{
		{ProjectID: demoPrimaryProjectID, Kind: "github_rest", Reason: "primary quota exhausted", Status: "waiting", StartedAt: now.Add(-3 * time.Minute), ResumeAt: now.Add(20 * time.Minute), MaxConcurrent: 6},
		{ProjectID: "docs-site", Kind: "github_rest", Reason: "automatic retry did not recover", Status: "waiting", StartedAt: now.Add(-12 * time.Minute), ResumeAt: now.Add(-2 * time.Minute), MaxConcurrent: 6},
		{ProjectID: "billing-api", Kind: "backend_capacity", Status: "ramping", StartedAt: now.Add(-time.Minute), Limit: 1, MaxConcurrent: 6, Admitted: 1},
	}
	resumeAt := now.Add(30 * time.Minute)
	snapshot.BackendOutages = []telemetry.BackendOutage{
		{ProjectID: demoPrimaryProjectID, BackendID: "codex", BackendKind: "codex", Provider: "openai", Reason: "provider usage limit reached", ResumeAt: resumeAt},
		{ProjectID: "docs-site", BackendID: "codex", BackendKind: "codex", Provider: "openai", Reason: "provider usage limit reached", ResumeAt: resumeAt},
	}
	snapshot.OverloadRetriesLastHour = 3
	return snapshot
}

func demoDrainingSnapshot() telemetry.Snapshot {
	snapshot := demoHealthySnapshot()
	now := snapshot.GeneratedAt
	snapshot.Shutdown = telemetry.Shutdown{Status: "draining", Draining: true, SessionsRemaining: 3, RequestedAt: demoTimePtr(now.Add(-7 * time.Minute))}
	return snapshot
}

func demoDegradedSnapshot() telemetry.Snapshot {
	snapshot := demoHealthySnapshot()
	snapshot.LifetimeTotals = telemetry.LifetimeTotals{Available: false, DegradedReason: "usage ledger unavailable in this scenario"}
	snapshot.CycleTime = telemetry.CycleTimeReport{Available: false, DegradedReason: "cycle-time query timed out"}
	snapshot.Budget.DegradedReason = "budget history is partially unavailable"
	snapshot.WorkflowMetrics.DegradedReason = "workflow metrics query failed"
	snapshot.WorkflowMetrics.RuntimeStore.Status = "degraded"
	snapshot.WorkflowMetrics.RuntimeStore.Healthy = false
	return snapshot
}

func demoBudgetRefusalsSnapshot() telemetry.Snapshot {
	snapshot := demoHealthySnapshot()
	now := snapshot.GeneratedAt
	capValue := 42.0
	snapshot.Budget.CurrentSpendUSD = 41.15
	snapshot.Budget.ProjectedCostUSD = 6.2
	snapshot.Budget.ProjectedSpendUSD = 47.35
	snapshot.Budget.PerDayMaxUSD = &capValue
	snapshot.Budget.Refusals = []telemetry.BudgetRefusal{
		{IssueID: "demo-refusal-1", Identifier: "digitaldrywood/billing-api#5310", Code: "daily_cap_exceeded", Message: "Projected spend would exceed the daily cap.", CurrentSpendUSD: 41.15, ProjectedCostUSD: 6.2, MaxUSD: &capValue, RefusedAt: now.Add(-18 * time.Minute), ResetAt: demoTimePtr(now.Truncate(24 * time.Hour).Add(24 * time.Hour))},
		{IssueID: "demo-refusal-2", Identifier: "digitaldrywood/billing-api#5311", Code: "per_issue_max_usd", Message: "Projected spend would exceed the per-issue cap.", CurrentSpendUSD: 41.15, ProjectedCostUSD: 6.2, MaxUSD: &capValue, RefusedAt: now.Add(-35 * time.Minute), HardHold: true},
	}
	return snapshot
}

func demoBlockedHeavySnapshot() telemetry.Snapshot {
	snapshot := demoHealthySnapshot()
	snapshot.Blocked = append(snapshot.Blocked,
		demoBlocked("billing-api", "demo-blocked-human", "digitaldrywood/billing-api#5282", "Human approval required for billing migration", "waiting for operator approval", "human-review", 18),
		demoBlocked("infra-platform", "demo-blocked-stale", "digitaldrywood/infra-platform#5283", "Stale lease after workspace hook timeout", "lease expired before worker heartbeat", "reclaim", 20),
	)
	snapshot.Counts.Blocked = len(snapshot.Blocked)
	return snapshot
}

func demoLongContentSnapshot() telemetry.Snapshot {
	snapshot := demoHealthySnapshot()
	if len(snapshot.Running) > 0 {
		identifier := "digitaldrywood/creswoodcorners-phone#66"
		snapshot.Running[0].Identifier = identifier
		snapshot.Running[0].URL = demoIssueURL(identifier)
		snapshot.Running[0].Title = "Implement page-addressable screenshot scenarios with very long deterministic fixture names, wide workspace paths, detailed token accounting, and browser-friendly waiting selectors"
		snapshot.Running[0].SessionID = "thread-demo-core-5260-very-long-session-id-for-wide-table-verification-0000000001"
		snapshot.Running[0].WorkspacePath = "/tmp/detent-screenshots/workspaces/detent-core/5260/very/deep/generated/worktree/path/that/exercises/wrapping"
		snapshot.Running[0].LastMessage = "Long message: scenario route loaded, manifest matched, Chart.js endpoint agreed with seeded ledger, and visual baseline is ready for capture."
		snapshot.Running[0].PullRequest = &telemetry.PullRequest{
			Number:           75,
			URL:              demoPRURL(identifier, 75),
			BranchName:       "detent/demo-long-content",
			State:            "OPEN",
			MergeableState:   "clean",
			CIStatus:         "pending",
			CodexReviewState: "CLEAN",
		}
		snapshot.Running[0].Tokens = telemetry.Tokens{Input: 980000, Output: 214000, Total: 1194000, RuntimeSeconds: 9180}
	}
	return snapshot
}

func demoDenseSnapshot() telemetry.Snapshot {
	snapshot := demoHealthySnapshot()
	for i := range 12 {
		state := []string{"Backlog", "Todo", "In Progress", "Blocked", "Human Review", "Rework"}[i%6]
		snapshot.BoardIssues = append(snapshot.BoardIssues, demoIssue(demoPrimaryProjectID, fmt.Sprintf("demo-dense-%02d", i), fmt.Sprintf("digitaldrywood/detent-core#54%02d", i), fmt.Sprintf("Dense lane card %02d exercises compact chips and overflow", i+1), state, i+1))
	}
	return snapshot
}

func demoHotPathSnapshot() telemetry.Snapshot {
	snapshot := demoHealthySnapshot()
	snapshot.Projects = demoProjectSnapshots(ProjectsForVariant("hot-path"))
	snapshot.Tokens = telemetry.Tokens{Input: 180000, Output: 52000, Total: 232000, RuntimeSeconds: 6400}
	snapshot.Budget.CurrentSpendUSD = 36.8
	return snapshot
}

func demoTrackerRefreshGapSnapshot() telemetry.Snapshot {
	snapshot := demoHealthySnapshot()
	completed := demoCompleted(demoPrimaryProjectID, "demo-tracker-refresh-gap", "digitaldrywood/detent-core#5294", "Keep completed implementation visible during tracker refresh", "completed", 3, "gpt-5-codex", 41000)
	completed.PullRequest = &telemetry.PullRequest{
		Number:             5294,
		URL:                demoPRURL(completed.Identifier, 5294),
		BranchName:         "detent/demo-tracker-refresh-gap",
		State:              "OPEN",
		MergeableState:     "clean",
		CIStatus:           "success",
		CheckRunCount:      5,
		StatusContextCount: 1,
		CIDurationSeconds:  260,
		CodexReviewState:   "CLEAN",
	}
	snapshot.Completed = append([]telemetry.Completed{completed}, snapshot.Completed...)
	snapshot.Counts.Completed = len(snapshot.Completed)
	for i := range snapshot.Projects {
		if snapshot.Projects[i].Project.ID == demoPrimaryProjectID {
			snapshot.Projects[i].Counts.Completed++
		}
	}
	return snapshot
}

func demoExternalLaneTimerSnapshot() telemetry.Snapshot {
	snapshot := demoHealthySnapshot()
	now := snapshot.GeneratedAt
	enteredAt := now.Add(-111 * time.Minute)
	recentUpdate := now.Add(-2 * time.Minute)
	issue := demoIssue(demoPrimaryProjectID, "demo-external-lane-timer", "digitaldrywood/detent-core#1162", "Externally moved Blocked lane timer", "Blocked", 0)
	issue.CurrentLaneEnteredAt = &enteredAt
	issue.CurrentLaneAgeSeconds = int64(now.Sub(enteredAt) / time.Second)
	issue.StageUpdatedAt = &recentUpdate
	issue.UpdatedAt = &recentUpdate
	snapshot.BoardIssues = append(snapshot.BoardIssues, issue)
	return snapshot
}

func demoIssue(projectID string, id string, identifier string, title string, state string, hoursAgo int) telemetry.Issue {
	at := demoBaseTime.Add(-time.Duration(hoursAgo) * time.Hour)
	return telemetry.Issue{
		ID:             id,
		Identifier:     identifier,
		ProjectID:      projectID,
		URL:            demoIssueURL(identifier),
		Title:          title,
		Description:    title + " for deterministic Detent screenshot scenarios.",
		State:          state,
		Labels:         []string{"enhancement", "demo"},
		Assignees:      []string{"detent-bot"},
		Owner:          "detent-bot",
		UpdatedAt:      &at,
		StageUpdatedAt: &at,
	}
}

func demoIssueWithRuntimeIdentity(issue telemetry.Issue, identity agentidentity.Identity) telemetry.Issue {
	issue.RuntimeIdentity = identity
	return issue
}

func demoPipelineIssue(projectID string, id string, identifier string, title string, state string, pr int, ci string, mergeable string, hoursAgo int) telemetry.Issue {
	issue := demoIssue(projectID, id, identifier, title, state, hoursAgo)
	issue.PullRequest = &telemetry.PullRequest{
		Number:             pr,
		URL:                demoPRURL(identifier, pr),
		BranchName:         "detent/demo-" + id,
		State:              "OPEN",
		MergeableState:     mergeable,
		CIStatus:           ci,
		CheckRunCount:      5,
		StatusContextCount: 1,
		CIDurationSeconds:  int64(240 + hoursAgo*20),
		QuietWaitSeconds:   int64(hoursAgo * 60),
		RunningChecks:      []string{"make check"},
		CodexReviewState:   "CLEAN",
	}
	if ci == "failure" {
		issue.PullRequest.CodexReviewState = "P1"
		issue.PullRequest.SlowChecks = []telemetry.PullRequestCheck{{Name: "go test -race", Status: "completed", Conclusion: "failure", DurationSeconds: 620}}
	}
	if state == "Merging" {
		issue.PullRequest.MergeQueueEntry = &telemetry.PullRequestMergeQueueEntry{
			ID:                          "demo-native-merge-queue-entry",
			State:                       "AWAITING_CHECKS",
			Position:                    2,
			Depth:                       6,
			EstimatedTimeToMergeSeconds: 720,
			URL:                         "https://github.com/digitaldrywood/release-train/queue/main",
		}
	}
	if state == "Done" {
		issue.PullRequest.State = "MERGED"
	}
	return issue
}

func demoQueued(projectID string, id string, identifier string, title string, attempt int, dueAt time.Time, err string) telemetry.Queued {
	dueIn := dueAt.Sub(demoBaseTime).Milliseconds()
	return telemetry.Queued{
		Issue:          demoIssue(projectID, id, identifier, title, "Todo", attempt+2),
		Attempt:        attempt,
		DueAt:          &dueAt,
		DueInMillis:    dueIn,
		Error:          err,
		WorkerHost:     "demo-worker-queue",
		WorkspacePath:  "/tmp/detent-screenshots/workspaces/" + projectID + "/" + id,
		ProjectedSpend: float64(attempt) * 1.35,
	}
}

func demoBlocked(projectID string, id string, identifier string, title string, err string, target string, hoursAgo int, source ...telemetry.BlockedSource) telemetry.Blocked {
	blockedAt := demoBaseTime.Add(-time.Duration(hoursAgo) * time.Hour)
	lastAt := blockedAt.Add(20 * time.Minute)
	var blockedSource telemetry.BlockedSource
	if len(source) > 0 {
		blockedSource = source[0]
	}
	state := "Blocked"
	if blockedSource == telemetry.BlockedSourceDependency && target == "Todo" {
		state = "Todo"
	}
	return telemetry.Blocked{
		Issue:          demoIssue(projectID, id, identifier, title, state, hoursAgo),
		WorkerHost:     "demo-worker-blocked",
		WorkspacePath:  "/tmp/detent-screenshots/workspaces/" + projectID + "/" + id,
		SessionID:      "thread-" + id,
		Error:          err,
		Source:         blockedSource,
		RecoveryReason: err,
		RecoveryTarget: target,
		BlockedAt:      &blockedAt,
		LastEventAt:    &lastAt,
		LastEvent:      "blocked",
		LastMessage:    err,
	}
}

func demoCompleted(projectID string, id string, identifier string, title string, state string, minutesAgo int, model string, totalTokens int64) telemetry.Completed {
	completedAt := demoBaseTime.Add(-time.Duration(minutesAgo) * time.Minute)
	startedAt := completedAt.Add(-42 * time.Minute)
	input := totalTokens * 72 / 100
	output := totalTokens - input
	return telemetry.Completed{
		Issue:          demoIssue(projectID, id, identifier, title, state, minutesAgo/60+1),
		SessionID:      "thread-" + id,
		StartedAt:      startedAt,
		CompletedAt:    completedAt,
		Turns:          6,
		RuntimeSeconds: completedAt.Sub(startedAt).Seconds(),
		FinalState:     state,
		Model:          model,
		Tokens:         telemetry.Tokens{Input: input, CachedInput: input / 3, Output: output, ReasoningOutput: output / 4, Total: totalTokens, RuntimeSeconds: completedAt.Sub(startedAt).Seconds(), ModelContextWindow: demoInt64Ptr(128000)},
	}
}

func demoIssueURL(identifier string) string {
	repo, number, ok := strings.Cut(identifier, "#")
	if !ok {
		return "https://github.test/digitaldrywood/detent/issues/0"
	}
	return "https://github.test/" + repo + "/issues/" + number
}

func demoPRURL(identifier string, number int) string {
	repo, _, ok := strings.Cut(identifier, "#")
	if !ok {
		repo = "digitaldrywood/detent"
	}
	return fmt.Sprintf("https://github.test/%s/pull/%d", repo, number)
}

func demoTimePtr(value time.Time) *time.Time {
	return &value
}

func demoInt64Ptr(value int64) *int64 {
	return &value
}

func ProjectsForVariant(variant string) []templates.ProjectSmallMultiple {
	now := demoBaseTime
	if variant == "empty" || variant == "settings-empty" {
		return nil
	}
	projects := []templates.ProjectSmallMultiple{
		demoProject(demoPrimaryProjectID, demoPrimaryProjectName, "https://github.test/digitaldrywood/detent-core", false, 1, 1, 1, 2, 101730, 18.72, now),
		demoProject("docs-site", "docs-site", "https://github.test/digitaldrywood/docs-site", false, 1, 1, 0, 1, 59450, 6.1, now),
		demoProject("billing-api", "billing-api", "https://github.test/digitaldrywood/billing-api", false, 0, 1, 1, 0, 48600, 9.4, now),
		demoProject("mobile-client", "mobile-client", "https://github.test/digitaldrywood/mobile-client", true, 0, 1, 0, 1, 22000, 2.2, now),
		demoProject("infra-platform", "infra-platform", "https://github.test/digitaldrywood/infra-platform", false, 1, 0, 0, 0, 29200, 4.7, now),
		demoProject("release-train", "release-train", "https://github.test/digitaldrywood/release-train", false, 0, 0, 0, 1, 52000, 5.8, now),
		demoProject("observability-console", "observability-console-with-long-name", "https://github.test/digitaldrywood/observability-console", false, 0, 0, 0, 0, 17000, 1.8, now),
		demoProject("agent-lab", "agent-lab", "https://github.test/digitaldrywood/agent-lab", false, 0, 0, 0, 0, 0, 0, now),
	}
	workloads := map[string]telemetry.BoardWorkloadCounts{
		demoPrimaryProjectID: {Load: 4, Todo: 1, Active: 3, Blocked: 1},
		"docs-site":          {Load: 3, Todo: 1, Active: 2},
		"billing-api":        {Load: 2, Todo: 1, Waiting: 1},
		"mobile-client":      {Load: 1, Todo: 1},
		"infra-platform":     {Load: 1, Active: 1},
		"release-train":      {Load: 2, Active: 2},
		"agent-lab":          {Load: 1, Todo: 1},
	}
	for i := range projects {
		workload := workloads[projects[i].ID]
		projects[i].BoardLoad = workload.Load
		projects[i].BoardTodo = workload.Todo
		projects[i].BoardActive = workload.Active
		projects[i].BoardWaiting = workload.Waiting
		projects[i].BoardBlocked = workload.Blocked
		projects[i].BudgetEnabled = true
		projects[i].PerDayMaxUSD = demoProjectBudgetCap(projects[i].ID)
		projects[i].PerIssueMaxUSD = projects[i].PerDayMaxUSD / 5
		projects[i].BudgetResetAt = now.Add(12 * time.Hour)
		projects[i].BudgetObservedAt = now
		if projects[i].ID == demoPrimaryProjectID {
			overrideCap := 60.0
			projects[i].PerDayMaxUSD = overrideCap
			projects[i].BudgetOverride = &telemetry.BudgetOverride{
				ProjectID:    demoPrimaryProjectID,
				PerDayMaxUSD: &overrideCap,
				CreatedAt:    now.Add(-time.Hour),
				ExpiresAt:    now.Add(4 * time.Hour),
				Reason:       "release readiness",
			}
		}
	}
	switch variant {
	case "project-empty", "reports-empty", "settings-empty", "no-history":
		for i := range projects {
			projects[i].Running = 0
			projects[i].QueueCount = 0
			projects[i].Blocked = 0
			projects[i].BoardLoad = 0
			projects[i].BoardTodo = 0
			projects[i].BoardActive = 0
			projects[i].BoardWaiting = 0
			projects[i].BoardBlocked = 0
			projects[i].Completed = 0
			projects[i].TotalTokens = 0
			projects[i].ThroughputTokensPerSecond = 0
			projects[i].CurrentSpendUSD = 0
			projects[i].Samples = nil
		}
	case "hot-path", "model-heavy", "filtered-project":
		for i := range projects {
			if projects[i].ID == "billing-api" {
				projects[i].Running = 2
				projects[i].QueueCount = 4
				projects[i].Blocked = 2
				projects[i].Completed = 8
				projects[i].TotalTokens = 310000
				projects[i].CurrentSpendUSD = 31.4
				projects[i].Samples = demoSamples(now, 2, 4, 2, 8, 310000, 31.4)
			}
		}
	}
	return projects
}

func demoProjectBudgetCap(projectID string) float64 {
	switch projectID {
	case demoPrimaryProjectID:
		return 42
	case "docs-site", "release-train":
		return 20
	case "mobile-client":
		return 15
	case "infra-platform":
		return 25
	default:
		return 10
	}
}

func demoProject(id string, name string, url string, paused bool, running int, queue int, blocked int, completed int, tokens int64, spend float64, now time.Time) templates.ProjectSmallMultiple {
	return templates.ProjectSmallMultiple{
		ID:                        id,
		Name:                      name,
		URL:                       url,
		Color:                     projectcolor.ColorForID(id),
		Paused:                    paused,
		Running:                   running,
		QueueCount:                queue,
		Blocked:                   blocked,
		BoardLoad:                 running + queue,
		BoardTodo:                 queue,
		BoardActive:               running,
		BoardBlocked:              blocked,
		Completed:                 completed,
		TotalTokens:               tokens,
		ThroughputTokensPerSecond: float64(tokens%50000) / 600,
		CurrentSpendUSD:           spend,
		Samples:                   demoSamples(now, running, queue, blocked, completed, tokens, spend),
	}
}

func demoSamples(now time.Time, running int, queue int, blocked int, completed int, tokens int64, spend float64) []templates.ProjectSmallMultipleSample {
	samples := make([]templates.ProjectSmallMultipleSample, 0, 12)
	for i := 11; i >= 0; i-- {
		scale := int64(12 - i)
		samples = append(samples, templates.ProjectSmallMultipleSample{
			At:                        now.Add(-time.Duration(i) * time.Minute),
			Running:                   max(0, running-(i%2)),
			TotalTokens:               tokens - int64(i)*max(100, tokens/30),
			ThroughputTokensPerSecond: float64(scale) * 1.8,
			SpendUSD:                  spend * float64(scale) / 12,
			QueueDepth:                max(0, queue-(i%3)),
			Blocked:                   blocked,
			Completed:                 max(0, completed-(i/4)),
		})
	}
	return samples
}

func demoProjectSnapshots(projects []templates.ProjectSmallMultiple) []telemetry.ProjectSnapshot {
	out := make([]telemetry.ProjectSnapshot, 0, len(projects))
	for _, project := range projects {
		out = append(out, telemetry.ProjectSnapshot{
			Project: telemetry.Project{ID: project.ID, DisplayName: project.Name, URL: project.URL, Color: project.Color},
			Counts:  telemetry.Counts{Running: project.Running, Queue: project.QueueCount, Blocked: project.Blocked, Completed: project.Completed},
			Tokens:  telemetry.Tokens{Total: project.TotalTokens},
			Throughput: telemetry.TokenThroughput{
				TokensPerSecond: project.ThroughputTokensPerSecond,
				WindowSeconds:   600,
				Tokens:          int64(project.ThroughputTokensPerSecond * 600),
			},
		})
	}
	return out
}

func demoBudgetSpendPoints(now time.Time, total float64) []telemetry.BudgetSpendPoint {
	points := make([]telemetry.BudgetSpendPoint, 0, 8)
	for i := 7; i >= 0; i-- {
		scale := float64(8 - i)
		points = append(points, telemetry.BudgetSpendPoint{At: now.Add(-time.Duration(i) * time.Hour), SpendUSD: total * scale / 8})
	}
	return points
}

func demoBudgetDays() []telemetry.BudgetDay {
	return []telemetry.BudgetDay{
		{Date: "2026-06-09", SpendUSD: 8.4},
		{Date: "2026-06-10", SpendUSD: 12.8},
		{Date: "2026-06-11", SpendUSD: 15.2},
		{Date: "2026-06-12", SpendUSD: 10.9},
		{Date: "2026-06-13", SpendUSD: 22.4},
		{Date: "2026-06-14", SpendUSD: 18.1},
		{Date: "2026-06-15", SpendUSD: 18.72},
	}
}

func demoCycleTime(now time.Time) telemetry.CycleTimeReport {
	return telemetry.CycleTimeReport{
		Available:      true,
		AverageSeconds: int64(7 * time.Hour / time.Second),
		Buckets: []telemetry.CycleTimeBucket{
			{Label: "< 2h", MinSeconds: 0, MaxSeconds: int64(2 * time.Hour / time.Second), Count: 3},
			{Label: "2h-8h", MinSeconds: int64(2 * time.Hour / time.Second), MaxSeconds: int64(8 * time.Hour / time.Second), Count: 7},
			{Label: "8h-1d", MinSeconds: int64(8 * time.Hour / time.Second), MaxSeconds: int64(24 * time.Hour / time.Second), Count: 4},
		},
		Issues: []telemetry.CycleTimeIssue{
			{Key: "digitaldrywood/detent-core#5240", StartedAt: now.Add(-9 * time.Hour), CompletedAt: now.Add(-2 * time.Hour), DurationSeconds: int64(7 * time.Hour / time.Second), Sessions: 1},
			{Key: "digitaldrywood/docs-site#5241", StartedAt: now.Add(-13 * time.Hour), CompletedAt: now.Add(-5 * time.Hour), DurationSeconds: int64(8 * time.Hour / time.Second), Sessions: 2},
		},
	}
}

func demoWorkflowMetrics(now time.Time) telemetry.WorkflowMetrics {
	return telemetry.WorkflowMetrics{
		Available:    true,
		RuntimeStore: demoRuntimeStoreEvidence(now, 36),
		Windows: []telemetry.WorkflowMetricsWindow{
			demoWorkflowMetricsWindow("24h", now.Add(-24*time.Hour), now, 6*time.Minute, 18*time.Minute, 11*time.Minute),
			demoWorkflowMetricsWindow("7d", now.Add(-7*24*time.Hour), now, 8*time.Minute, 14*time.Minute, 9*time.Minute),
			demoWorkflowMetricsWindow("30d", now.Add(-30*24*time.Hour), now, 10*time.Minute, 12*time.Minute, 8*time.Minute),
		},
		ActiveBottleneck: telemetry.WorkflowBottleneck{
			Kind:       "lane_age",
			Label:      "Human Review is slowest",
			Detail:     "digitaldrywood/detent-core#5281 has waited longest in Human Review.",
			ProjectID:  demoPrimaryProjectID,
			IssueID:    "demo-blocked-hook",
			Identifier: "digitaldrywood/detent-core#5281",
			Seconds:    int64(5 * time.Hour / time.Second),
			Count:      1,
		},
	}
}

func demoEmptyWorkflowMetrics(now time.Time) telemetry.WorkflowMetrics {
	return telemetry.WorkflowMetrics{
		Available:    true,
		RuntimeStore: demoRuntimeStoreEvidence(now, 0),
		Windows: []telemetry.WorkflowMetricsWindow{
			{Label: "24h", From: now.Add(-24 * time.Hour), To: now},
			{Label: "7d", From: now.Add(-7 * 24 * time.Hour), To: now},
			{Label: "30d", From: now.Add(-30 * 24 * time.Hour), To: now},
		},
	}
}

func demoWorkflowMetricsWindow(label string, from time.Time, to time.Time, inProgress time.Duration, review time.Duration, merging time.Duration) telemetry.WorkflowMetricsWindow {
	rework := merging + 3*time.Minute
	return telemetry.WorkflowMetricsWindow{
		Label: label,
		From:  from,
		To:    to,
		Lanes: []telemetry.WorkflowPhaseMetric{
			demoWorkflowLaneMetric("In Progress", 9, inProgress, false, 46),
			demoWorkflowLaneMetric("Human Review", 5, review, true, 8),
			demoWorkflowLaneMetric("Merging", 4, merging, false, 22),
			demoWorkflowLaneMetric("Rework", 3, rework, false, 58),
		},
		SubPhases: []telemetry.WorkflowPhaseMetric{
			{ProjectID: demoPrimaryProjectID, PhaseType: "agent_session", PhaseName: "agent_active", Count: 9, TotalSeconds: int64((42 * time.Minute) / time.Second), AverageSeconds: int64((5 * time.Minute) / time.Second), Turns: 38, TotalTokens: 284000, EndpointFamily: "codex"},
			{ProjectID: demoPrimaryProjectID, PhaseType: "ci", PhaseName: "ci_wait", Count: 4, TotalSeconds: int64((19 * time.Minute) / time.Second), AverageSeconds: int64((5 * time.Minute) / time.Second), EndpointFamily: "checks"},
		},
		LaneTrends: []telemetry.WorkflowLaneTrend{
			demoWorkflowLaneTrend("In Progress", inProgress),
			demoWorkflowLaneTrend("Human Review", review),
			demoWorkflowLaneTrend("Merging", merging),
			demoWorkflowLaneTrend("Rework", rework),
		},
	}
}

func demoWorkflowLaneMetric(name string, count int64, average time.Duration, bottleneck bool, activePercent int64) telemetry.WorkflowPhaseMetric {
	seconds := int64(average / time.Second)
	totalSeconds := seconds * count
	activeSeconds := totalSeconds * activePercent / 100
	return telemetry.WorkflowPhaseMetric{
		ProjectID:      demoPrimaryProjectID,
		PhaseType:      "lane",
		PhaseName:      name,
		Count:          count,
		TotalSeconds:   totalSeconds,
		AverageSeconds: seconds,
		P50Seconds:     seconds,
		P90Seconds:     int64((average + average/3) / time.Second),
		P95Seconds:     int64((average + average/2) / time.Second),
		ActiveSeconds:  activeSeconds,
		WaitSeconds:    totalSeconds - activeSeconds,
		ActivePercent:  float64(activePercent),
		Bottleneck:     bottleneck,
		Comparison:     &telemetry.WorkflowMetricComparison{Label: "demo comparison", Direction: "unchanged"},
	}
}

func demoWorkflowLaneTrend(name string, average time.Duration) telemetry.WorkflowLaneTrend {
	points := make([]telemetry.WorkflowLaneTrendPoint, 0, 8)
	baseSeconds := int64(average / time.Second)
	for i := range 8 {
		offset := int64(i - 4)
		value := baseSeconds + offset*20
		if value < 0 {
			value = 0
		}
		points = append(points, telemetry.WorkflowLaneTrendPoint{
			Label:          strconv.Itoa(i + 1),
			Count:          1,
			AverageSeconds: value,
		})
	}
	return telemetry.WorkflowLaneTrend{
		ProjectID:  demoPrimaryProjectID,
		PhaseName:  name,
		Points:     points,
		TotalCount: int64(len(points)),
	}
}

func demoRuntimeStoreEvidence(now time.Time, workflowRows int64) telemetry.RuntimeStoreEvidence {
	var oldest *time.Time
	var newest *time.Time
	if workflowRows > 0 {
		oldestValue := now.Add(-27 * 24 * time.Hour)
		newestValue := now.Add(-11 * time.Minute)
		oldest = &oldestValue
		newest = &newestValue
	}
	return telemetry.RuntimeStoreEvidence{
		Backend:          "sqlite",
		Status:           "healthy",
		Healthy:          true,
		Path:             "/tmp/detent-screenshots/detent.db",
		MigrationStatus:  "applied through 6",
		MigrationVersion: 6,
		Tables: []telemetry.RuntimeStoreTableEvidence{
			{Name: "detent_runs", Scope: "fleet", RowCount: 7},
			{Name: "codex_sessions", Scope: "fleet", RowCount: 22},
			{Name: "fair_share_usage", Scope: "project", RowCount: 1},
			{Name: "usage_events", Scope: "project", RowCount: 18},
			{Name: "workflow_phase_events", Scope: "project", RowCount: workflowRows},
			{Name: "work_attempts", Scope: "project", RowCount: 3},
			{Name: "scheduler_decisions", Scope: "project", RowCount: 12},
		},
		WorkflowPhaseEvents: telemetry.RuntimeStoreWorkflowPhaseEvents{
			RowCount:         workflowRows,
			OldestFinishedAt: oldest,
			NewestFinishedAt: newest,
		},
	}
}

func demoTokenTrend(now time.Time) []telemetry.TokenTrendPoint {
	points := make([]telemetry.TokenTrendPoint, 0, 10)
	for i := 9; i >= 0; i-- {
		input := int64(8000 + (9-i)*1400)
		output := int64(2500 + (9-i)*550)
		points = append(points, telemetry.TokenTrendPoint{At: now.Add(-time.Duration(i) * time.Minute), Input: input, Output: output, Total: input + output})
	}
	return points
}

func SeedUsageEvents(ctx context.Context, backend store.Store) error {
	if backend == nil {
		return nil
	}
	for _, event := range UsageEvents() {
		if _, err := backend.RecordUsageEvent(ctx, event); err != nil {
			return fmt.Errorf("seed demo usage event: %w", err)
		}
		sessionID, err := backend.StartSession(ctx, store.SessionStart{
			IssueID:    event.IssueID,
			Identifier: event.Identifier,
			StartedAt:  event.StartedAt,
			Model:      event.Model,
		})
		if err != nil {
			return fmt.Errorf("seed demo session: %w", err)
		}
		if err := backend.FinishSession(ctx, sessionID, store.SessionFinish{
			CompletedAt:       event.FinishedAt,
			InputTokens:       event.InputTokens,
			CachedInputTokens: event.CachedInputTokens,
			OutputTokens:      event.OutputTokens,
			TotalTokens:       event.TotalTokens,
			RuntimeSeconds:    event.RuntimeSeconds,
			FinalState:        event.Outcome,
			Model:             event.Model,
		}); err != nil {
			return fmt.Errorf("finish demo session: %w", err)
		}
	}
	return nil
}

func UsageEvents() []store.UsageEvent {
	events := make([]store.UsageEvent, 0, 24)
	projects := []string{demoPrimaryProjectID, "docs-site", "billing-api", "mobile-client", "infra-platform", "release-train"}
	models := []string{"gpt-5-codex", "gpt-5", "gpt-5-mini"}
	for day := 13; day >= 0; day-- {
		for i, projectID := range projects {
			finished := demoBaseTime.AddDate(0, 0, -day).Add(time.Duration(i-6) * time.Hour)
			tokens := int64(12000 + (14-day)*900 + i*1400)
			if projectID == "billing-api" && day == 2 {
				tokens *= 5
			}
			pr := int64(5200 + day*10 + i)
			events = append(events, store.UsageEvent{
				ProjectID:         projectID,
				IssueID:           fmt.Sprintf("usage-%s-%02d", projectID, day),
				Identifier:        fmt.Sprintf("digitaldrywood/%s#%d", demoUsageRepo(projectID), 5200+day*10+i),
				PRNumber:          &pr,
				Model:             models[(day+i)%len(models)],
				InputTokens:       tokens * 7 / 10,
				CachedInputTokens: tokens * 6 / 10,
				OutputTokens:      tokens * 3 / 10,
				TotalTokens:       tokens,
				CostUSD:           float64(tokens) / 100000,
				RuntimeSeconds:    int64(600 + i*90),
				StartedAt:         finished.Add(-40 * time.Minute),
				FinishedAt:        finished,
				Outcome:           "completed",
			})
		}
	}
	return events
}

func demoUsageRepo(projectID string) string {
	if projectID == demoPrimaryProjectID {
		return demoPrimaryProjectName
	}
	return projectID
}
