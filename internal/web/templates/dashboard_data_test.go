package templates

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/projectcolor"
	"github.com/digitaldrywood/detent/internal/runtimeoutput"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web/ui/primitives"
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

func TestQueuedStateLabelDistinguishesCIWaitFromRetry(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		row  telemetry.Queued
		want string
	}{
		{name: "current-head CI wait", row: telemetry.Queued{QueueState: telemetry.QueueStateWaitingOnCI}, want: "Waiting on CI"},
		{name: "failure retry", row: telemetry.Queued{QueueState: telemetry.QueueStateRetrying}, want: "Retrying"},
		{name: "legacy retry", row: telemetry.Queued{}, want: "Retrying"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := queuedStateLabel(tt.row); got != tt.want {
				t.Fatalf("queuedStateLabel() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLifetimeOrphanRecoveryLabels(t *testing.T) {
	t.Parallel()

	totals := telemetry.LifetimeTotals{
		OrphanResumed:       4,
		OrphanFresh:         1,
		ResumedInputTokens:  1000,
		ResumedCachedTokens: 850,
	}
	if got := lifetimeOrphanContinuations(totals); got != "4 / 1" {
		t.Fatalf("lifetimeOrphanContinuations() = %q, want 4 / 1", got)
	}
	if got := lifetimeResumedCacheShare(totals); got != "85%" {
		t.Fatalf("lifetimeResumedCacheShare() = %q, want 85%%", got)
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
	if got := runtimeStatusClass(snapshot); got != "border-warn/15 bg-warn/15 text-warn" {
		t.Fatalf("runtimeStatusClass() = %q, want warning class", got)
	}
}

func TestWorkflowDiagnosticPromptIncludesRequiredLaneSections(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	window := telemetry.WorkflowMetricsWindow{
		Label: "24h",
		From:  now.Add(-24 * time.Hour),
		To:    now,
		SubPhases: []telemetry.WorkflowPhaseMetric{
			{PhaseType: "agent_session", PhaseName: "agent_active", Count: 2, TotalSeconds: 240, TotalTokens: 1500, Turns: 3},
			{PhaseType: "local_check", PhaseName: "make check", Count: 1, TotalSeconds: 120},
			{PhaseType: "ci", PhaseName: "ci", Count: 3, TotalSeconds: 600},
			{PhaseType: "github_backoff", PhaseName: "github_backoff", Count: 1, TotalSeconds: 90},
			{PhaseType: "merge_queue", PhaseName: "merge_queue", Count: 2, TotalSeconds: 300},
		},
	}
	metric := telemetry.WorkflowPhaseMetric{
		ProjectID:      "detent",
		PhaseType:      "lane",
		PhaseName:      "Merging",
		Count:          4,
		TotalSeconds:   1200,
		AverageSeconds: 300,
		P50Seconds:     240,
		P90Seconds:     600,
		P95Seconds:     900,
		ActiveSeconds:  300,
		WaitSeconds:    900,
		Comparison: &telemetry.WorkflowMetricComparison{
			Label:        "24h vs previous 24h",
			DeltaSeconds: 180,
			Direction:    "slower",
		},
		Representatives: []telemetry.WorkflowRepresentativeRun{
			{RunID: 42, SessionID: 84, Identifier: "digitaldrywood/detent#759", IssueURL: "https://github.com/digitaldrywood/detent/issues/759", FinishedAt: now.Add(-time.Hour)},
			{RunID: 43, SessionID: 85, IssueID: "issue-760", FinishedAt: now.Add(-2 * time.Hour)},
		},
	}
	report := telemetry.WorkflowMetrics{
		Windows: []telemetry.WorkflowMetricsWindow{window},
		OldestCards: []telemetry.WorkflowLaneAge{
			{ProjectID: "detent", IssueID: "issue-759", Identifier: "digitaldrywood/detent#759", URL: "https://github.com/digitaldrywood/detent/issues/759", State: "Merging", AgeSeconds: 7200},
			{ProjectID: "detent", IssueID: "issue-760", Identifier: "digitaldrywood/detent#760", URL: "https://github.com/digitaldrywood/detent/issues/760", State: "In Progress", AgeSeconds: 5400},
		},
	}

	prompt := workflowDiagnosticPrompt(telemetry.Project{ID: "detent", DisplayName: "Detent"}, report, window, metric)
	for _, want := range []string{
		"Detent workflow lane diagnostic request",
		"Project: Detent (detent)",
		"Lane: Merging",
		"Selected window: 24h (2026-06-27T12:00:00Z to 2026-06-28T12:00:00Z)",
		"Timing",
		"Count: 4 lane exits",
		"Average: 5m 0s",
		"P50: 4m 0s",
		"P90: 10m 0s",
		"P95: 15m 0s",
		"Trend delta: Slower +3m 0s (24h vs previous 24h)",
		"Wait vs active: 15m 0s wait / 5m 0s active / 20m 0s total (25% active)",
		"Sub-phase breakdown",
		"AI active time/tokens: 2 events, 4m 0s total, 2m 0s avg, 1,500 tokens, 3 turns",
		"Local checks: 1 events, 2m 0s total, 2m 0s avg",
		"CI wait: 3 events, 10m 0s total, 3m 20s avg",
		"GitHub backoff: 1 events, 1m 30s total, 1m 30s avg",
		"Merge-queue wait: 2 events, 5m 0s total, 2m 30s avg",
		"Oldest/currently stuck cards in Merging",
		"digitaldrywood/detent#759 / 2h 0m old / https://github.com/digitaldrywood/detent/issues/759",
		"Representative run identifiers",
		"run_id=42 / session_id=84 / identifier=digitaldrywood/detent#759 / url=https://github.com/digitaldrywood/detent/issues/759 / finished_at=2026-06-28T11:00:00Z",
		"run_id=43 / session_id=85 / issue_id=issue-760 / finished_at=2026-06-28T10:00:00Z",
		"Diagnose why this lane is slow.",
		"Propose concrete prioritized fixes",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("workflow diagnostic prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "digitaldrywood/detent#760 / 1h 30m old") {
		t.Fatalf("workflow diagnostic prompt included oldest card from another lane:\n%s", prompt)
	}
}

func TestGitHubAPIHealthDerivesStatus(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 25, 14, 30, 0, 0, time.UTC)
	lastRefresh := now.Add(-30 * time.Second)
	nextRefresh := now.Add(90 * time.Second)
	resetAt := now.Add(30 * time.Minute)
	observedAt := now.Add(-3 * time.Hour)
	backoffUntil := now.Add(5 * time.Minute)
	expiredBackoffUntil := now.Add(-5 * time.Minute)

	tests := []struct {
		name             string
		snapshot         telemetry.Snapshot
		wantState        gitHubAPIHealthState
		wantLabel        string
		wantSummaryPart  string
		wantDetailParts  []string
		rejectDetailPart string
	}{
		{
			name:            "unknown without GitHub snapshot",
			snapshot:        telemetry.Snapshot{GeneratedAt: now},
			wantState:       gitHubAPIHealthStateUnknown,
			wantLabel:       "GitHub API unknown",
			wantSummaryPart: "No GitHub rate-limit snapshot",
		},
		{
			name: "tracker degraded without GitHub snapshot",
			snapshot: telemetry.Snapshot{
				GeneratedAt: now,
				Refresh: telemetry.Refresh{
					Status:        telemetry.RefreshStatusDegraded,
					LastRefreshAt: &lastRefresh,
					LastError:     "fetch candidate issues failed: fetch github issues: github transient error: status 502",
					LastErrorAt:   &expiredBackoffUntil,
				},
			},
			wantState:       gitHubAPIHealthStateWarning,
			wantLabel:       "GitHub tracker degraded",
			wantSummaryPart: "No GitHub rate-limit snapshot",
			wantDetailParts: []string{
				"Tracker refresh degraded.",
				"GitHub returned a transient 502",
				"Last successful refresh:",
			},
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
			wantSummaryPart: "REST primary: 4,878 remaining / 5,000 total (122 used)",
		},
		{
			name: "never observed graphql stays at rest",
			snapshot: telemetry.Snapshot{
				GeneratedAt: now,
				RateLimits: &telemetry.RateLimits{
					GitHubREST: &telemetry.RateLimitBucket{Remaining: 4878, Used: 122, Limit: 5000, ResetAt: &resetAt},
				},
			},
			wantState:       gitHubAPIHealthStateAtRest,
			wantLabel:       "GitHub API at rest",
			wantSummaryPart: "REST primary: 4,878/5,000 remaining. No GraphQL usage this session.",
			wantDetailParts: []string{
				"GraphQL quota is reported from live GraphQL traffic",
				"none has occurred in this session",
				"This is expected while boards are idle or when the status source is label-backed",
			},
			rejectDetailPart: "GraphQL unknown",
		},
		{
			name: "observed then idle shows freshness",
			snapshot: telemetry.Snapshot{
				GeneratedAt: now,
				RateLimits: &telemetry.RateLimits{
					GitHubREST: &telemetry.RateLimitBucket{Remaining: 4878, Used: 122, Limit: 5000, ResetAt: &resetAt},
					GitHubGraphQL: &telemetry.RateLimitBucket{
						Remaining:  4880,
						Used:       120,
						Limit:      5000,
						ResetAt:    &resetAt,
						ObservedAt: &observedAt,
					},
				},
			},
			wantState:       gitHubAPIHealthStateHealthy,
			wantLabel:       "GitHub API healthy",
			wantSummaryPart: "GraphQL primary: 4,880 remaining / 5,000 total (120 used)",
			wantDetailParts: []string{"GraphQL quota observed 3h 0m ago"},
		},
		{
			name: "probe failed keeps graphql unknown",
			snapshot: telemetry.Snapshot{
				GeneratedAt: now,
				RateLimits: &telemetry.RateLimits{
					GitHubREST:    &telemetry.RateLimitBucket{Remaining: 4878, Used: 122, Limit: 5000, ResetAt: &resetAt},
					GitHubGraphQL: &telemetry.RateLimitBucket{Status: telemetry.RateLimitStatusUnknown},
				},
			},
			wantState:       gitHubAPIHealthStateUnknown,
			wantLabel:       "GitHub API GraphQL unknown",
			wantSummaryPart: "GraphQL primary quota unavailable",
			wantDetailParts: []string{
				"REST primary quota is visible",
				"could not be determined after an observation attempt",
			},
		},
		{
			name: "graphql exhausted status without numeric snapshot",
			snapshot: telemetry.Snapshot{
				GeneratedAt: now,
				RateLimits: &telemetry.RateLimits{
					GitHubREST:    &telemetry.RateLimitBucket{Remaining: 4878, Used: 122, Limit: 5000, ResetAt: &resetAt},
					GitHubGraphQL: &telemetry.RateLimitBucket{Status: telemetry.RateLimitStatusExhausted},
				},
			},
			wantState:       gitHubAPIHealthStateExhausted,
			wantLabel:       "GitHub primary quota exhausted",
			wantSummaryPart: "GraphQL primary: exhausted",
			wantDetailParts: []string{"reset time n/a"},
		},
		{
			name: "graphql backoff status without numeric snapshot",
			snapshot: telemetry.Snapshot{
				GeneratedAt: now,
				RateLimits: &telemetry.RateLimits{
					GitHubREST:    &telemetry.RateLimitBucket{Remaining: 4878, Used: 122, Limit: 5000, ResetAt: &resetAt},
					GitHubGraphQL: &telemetry.RateLimitBucket{Status: telemetry.RateLimitStatusBackoff},
				},
			},
			wantState:       gitHubAPIHealthStateBackoff,
			wantLabel:       "GitHub GraphQL backoff active",
			wantSummaryPart: "GraphQL primary: backoff",
			wantDetailParts: []string{"GitHub GraphQL requests are in backoff", "retry time n/a"},
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
			wantLabel:       "GitHub primary quota low",
			wantSummaryPart: "REST primary: 240 remaining / 5,000 total (4,760 used)",
		},
		{
			name: "tracker refresh failure degrades healthy github api",
			snapshot: telemetry.Snapshot{
				GeneratedAt: now,
				Refresh: telemetry.Refresh{
					Status:        telemetry.RefreshStatusDegraded,
					LastRefreshAt: &lastRefresh,
					LastError:     "fetch candidate issues failed: fetch github issues: github transient error: status 401: Bad credentials",
					LastErrorAt:   &expiredBackoffUntil,
				},
				RateLimits: &telemetry.RateLimits{
					GitHubREST:    &telemetry.RateLimitBucket{Remaining: 4878, Used: 122, Limit: 5000, ResetAt: &resetAt},
					GitHubGraphQL: &telemetry.RateLimitBucket{Remaining: 4880, Used: 120, Limit: 5000, ResetAt: &resetAt},
				},
			},
			wantState:       gitHubAPIHealthStateWarning,
			wantLabel:       "GitHub tracker degraded",
			wantSummaryPart: "REST primary: 4,878 remaining / 5,000 total (122 used)",
			wantDetailParts: []string{
				"Tracker refresh degraded.",
				"Bad credentials",
				"Last successful refresh:",
			},
		},
		{
			name: "secondary throttle preserves healthy primary context",
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
			wantLabel:       "GitHub secondary throttle active for pull requests/check runs",
			wantSummaryPart: "Primary REST quota is healthy: 4,878/5,000 remaining",
			wantDetailParts: []string{
				"GitHub secondary endpoint throttle is active for pull requests/check runs.",
				"Primary REST quota is healthy: 4,878/5,000 remaining.",
				"Retrying at " + localTimeToken(backoffUntil, LocalTimeOnly) + ".",
			},
			rejectDetailPart: "REST primary:",
		},
		{
			name: "secondary throttle with low primary quota names both conditions",
			snapshot: telemetry.Snapshot{
				GeneratedAt: now,
				RateLimits: &telemetry.RateLimits{
					GitHubREST:    &telemetry.RateLimitBucket{Remaining: 240, Used: 4760, Limit: 5000, ResetAt: &resetAt},
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
			wantState:       gitHubAPIHealthStateBackoff,
			wantLabel:       "GitHub secondary throttle active for pull requests",
			wantSummaryPart: "Primary REST quota is low: 240/5,000 remaining",
			wantDetailParts: []string{
				"GitHub secondary endpoint throttle is active for pull requests.",
				"Primary REST quota is low: 240/5,000 remaining.",
				"Retrying at " + localTimeToken(backoffUntil, LocalTimeOnly) + ".",
			},
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
			wantSummaryPart: "REST primary: 4,878 remaining / 5,000 total (122 used)",
		},
		{
			name: "cleared secondary throttle with stale future backoff is healthy",
			snapshot: telemetry.Snapshot{
				GeneratedAt: now,
				RateLimits: &telemetry.RateLimits{
					GitHubREST:    &telemetry.RateLimitBucket{Remaining: 4878, Used: 122, Limit: 5000, ResetAt: &resetAt},
					GitHubGraphQL: &telemetry.RateLimitBucket{Remaining: 4880, Used: 120, Limit: 5000, ResetAt: &resetAt},
					RESTUsage: &telemetry.RESTUsage{
						BackoffUntil: &backoffUntil,
						Contributors: []telemetry.RESTUsageContributor{
							{EndpointFamily: "pull requests", Count: 2, LastStatus: 200},
						},
					},
				},
			},
			wantState:       gitHubAPIHealthStateHealthy,
			wantLabel:       "GitHub API healthy",
			wantSummaryPart: "REST primary: 4,878 remaining / 5,000 total (122 used)",
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
			wantLabel:       "GitHub primary quota exhausted",
			wantSummaryPart: "REST primary: 0 remaining / 5,000 total (5,000 used)",
			wantDetailParts: []string{"reset " + localTimeToken(resetAt, LocalTimeOnly)},
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
			for _, want := range tt.wantDetailParts {
				if !strings.Contains(got.Detail, want) {
					t.Fatalf("gitHubAPIHealth().Detail = %q, want substring %q; view = %#v", got.Detail, want, got)
				}
			}
			if tt.rejectDetailPart != "" && strings.Contains(got.Detail, tt.rejectDetailPart) {
				t.Fatalf("gitHubAPIHealth().Detail = %q, want no substring %q; view = %#v", got.Detail, tt.rejectDetailPart, got)
			}
			if (tt.name == "expired secondary backoff does not mask healthy primary" ||
				tt.name == "cleared secondary throttle with stale future backoff is healthy") &&
				len(got.Endpoints) != 0 {
				t.Fatalf("gitHubAPIHealth().Endpoints = %#v, want no active endpoint backoff rows", got.Endpoints)
			}
		})
	}
}

func TestRESTBudgetContributorRowsPreserveCredentialAndEndpointWindows(t *testing.T) {
	t.Parallel()

	coreReset := time.Date(2026, 8, 8, 19, 0, 0, 0, time.UTC)
	searchReset := coreReset.Add(-45 * time.Minute)
	limits := &telemetry.RateLimits{
		GitHubRESTBudgets: []telemetry.RESTBudget{
			{Consumer: telemetry.RESTConsumerOrchestrator, CredentialIdentity: "github-rest:user", EndpointFamily: "issues", Resource: "core", Remaining: 300, Limit: 5000, ResetAt: &coreReset},
			{Consumer: telemetry.RESTConsumerWorker, CredentialIdentity: "github-rest:app", EndpointFamily: "worker credential", Resource: "core", Used: 4980, Remaining: 20, Limit: 5000, MinRemainingReserve: 1000, ResetAt: &searchReset},
			{Consumer: telemetry.RESTConsumerSharedPool, CredentialIdentity: "github-rest:user", EndpointFamily: "shared credential pool", Resource: "core", Used: 2000, Remaining: 3000, Limit: 5000, MinRemainingReserve: 1250, ResetAt: &coreReset},
		},
		RESTUsage: &telemetry.RESTUsage{Contributors: []telemetry.RESTUsageContributor{
			{Consumer: telemetry.RESTConsumerOrchestrator, CredentialIdentity: "github-rest:user", EndpointFamily: "issues", Resource: "core", Count: 4, LastStatus: 200},
		}},
	}

	rows := restBudgetContributorRows(limits)
	if len(rows) != 3 {
		t.Fatalf("rows len = %d, want 3: %#v", len(rows), rows)
	}
	tests := []struct {
		index      int
		consumer   string
		credential string
		family     string
		resource   string
		count      string
		status     string
		remaining  string
		resetAt    time.Time
	}{
		{index: 0, consumer: telemetry.RESTConsumerOrchestrator, credential: "github-rest:user", family: "issues", resource: "core", count: "4 requests", status: "200", remaining: "300 / 5,000", resetAt: coreReset},
		{index: 1, consumer: telemetry.RESTConsumerWorker, credential: "github-rest:app", family: "worker credential", resource: "core", count: "4,980 used", status: "reserved", remaining: "20 / 5,000", resetAt: searchReset},
		{index: 2, consumer: telemetry.RESTConsumerSharedPool, credential: "github-rest:user", family: "shared credential pool", resource: "core", count: "usage indeterminate", status: "governed shared", remaining: "3,000 / 5,000", resetAt: coreReset},
	}
	for _, test := range tests {
		t.Run(test.family, func(t *testing.T) {
			row := rows[test.index]
			if row.Consumer != test.consumer || row.CredentialIdentity != test.credential || row.EndpointFamily != test.family || row.Resource != test.resource || row.Count != test.count || row.Status != test.status || row.Remaining != test.remaining || row.Reset != localTimeToken(test.resetAt, LocalTimeOnly) {
				t.Fatalf("row = %#v, want credential %q family %q resource %q remaining %q reset %v", row, test.credential, test.family, test.resource, test.remaining, test.resetAt)
			}
		})
	}
	if got := restBudgetCredentialCount(limits); got != 2 {
		t.Fatalf("restBudgetCredentialCount() = %d, want 2", got)
	}
}

func TestGraphQLUnknownStatusFormatsBudgetLabels(t *testing.T) {
	t.Parallel()

	limits := &telemetry.RateLimits{
		GitHubGraphQL: &telemetry.RateLimitBucket{Status: telemetry.RateLimitStatusUnknown},
	}

	if got := graphQLBudgetRemaining(limits); got != "unknown" {
		t.Fatalf("graphQLBudgetRemaining() = %q, want unknown", got)
	}

	rows := rateLimitRows(limits)
	if len(rows) != 1 {
		t.Fatalf("rateLimitRows() len = %d, want 1: %#v", len(rows), rows)
	}
	row := rows[0]
	if row.Name != "GitHub GraphQL" ||
		row.Remaining != "unknown" ||
		row.Used != "usage unknown" ||
		row.Limit != "limit unknown" {
		t.Fatalf("rateLimitRows()[0] = %#v, want unknown GraphQL labels", row)
	}
}

func TestProviderRateLimitRowsShowRemainingPercentages(t *testing.T) {
	t.Parallel()

	rows := rateLimitRows(&telemetry.RateLimits{
		Primary:   &telemetry.RateLimitBucket{Remaining: 72, Used: 28, Limit: 100},
		Secondary: &telemetry.RateLimitBucket{Remaining: 41, Used: 59, Limit: 100},
	})
	if len(rows) != 2 {
		t.Fatalf("rateLimitRows() len = %d, want 2: %#v", len(rows), rows)
	}
	if rows[0].Remaining != "72% left" || rows[0].Used != "28% used" || rows[0].Limit != "rolling window" {
		t.Fatalf("primary row = %#v, want percentage labels", rows[0])
	}
	if rows[1].Remaining != "41% left" || rows[1].Used != "59% used" || rows[1].Limit != "rolling window" {
		t.Fatalf("secondary row = %#v, want percentage labels", rows[1])
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
		localTimeToken(now.Add(-2*time.Minute), LocalTimeWithSeconds): 1,
		localTimeToken(now.Add(-30*time.Second), LocalTimeOnly):       1,
		localTimeToken(now.Add(10*time.Second), LocalTimeWithSeconds): 3,
	}
	for _, point := range points {
		want := wantValues[point.Label]
		if point.Value != want {
			t.Fatalf("point %s = %v, want %v; points = %#v", point.Label, point.Value, want, points)
		}
	}
}

func TestBoardProgressChartUsesLocalTimeTokens(t *testing.T) {
	t.Parallel()

	completedAt := time.Date(2026, 7, 10, 17, 44, 0, 0, time.UTC)
	chart := boardProgressChart(telemetry.Snapshot{
		GeneratedAt: completedAt.Add(time.Minute),
		Counts:      telemetry.Counts{Completed: 1},
		Completed: []telemetry.Completed{
			{Issue: telemetry.Issue{ID: "issue-1"}, CompletedAt: completedAt},
		},
	})

	if len(chart.Points) != 1 {
		t.Fatalf("boardProgressChart() points = %d, want 1", len(chart.Points))
	}
	if got, want := chart.Points[0].Label, localTimeToken(completedAt, LocalTimeOnly); got != want {
		t.Fatalf("boardProgressChart() label = %q, want %q", got, want)
	}
}

func TestWorkflowLaneTrendPointLabel(t *testing.T) {
	t.Parallel()

	bucketEnd := time.Date(2026, 7, 10, 17, 0, 0, 0, time.UTC)
	windowFor := func(span time.Duration) telemetry.WorkflowMetricsWindow {
		return telemetry.WorkflowMetricsWindow{From: bucketEnd.Add(-span), To: bucketEnd}
	}

	tests := []struct {
		name   string
		point  telemetry.WorkflowLaneTrendPoint
		window telemetry.WorkflowMetricsWindow
		want   string
	}{
		{
			name:   "short window uses local time",
			point:  telemetry.WorkflowLaneTrendPoint{BucketEnd: bucketEnd},
			window: windowFor(24 * time.Hour),
			want:   localTimeToken(bucketEnd, LocalTimeOnly),
		},
		{
			name:   "week window uses local date time",
			point:  telemetry.WorkflowLaneTrendPoint{BucketEnd: bucketEnd},
			window: windowFor(7 * 24 * time.Hour),
			want:   localTimeToken(bucketEnd, LocalDateTime),
		},
		{
			name:   "month window uses local date",
			point:  telemetry.WorkflowLaneTrendPoint{BucketEnd: bucketEnd},
			window: windowFor(30 * 24 * time.Hour),
			want:   localTimeToken(bucketEnd, LocalDateOnly),
		},
		{
			name:   "zero bucket end keeps store label",
			point:  telemetry.WorkflowLaneTrendPoint{Label: "17:00"},
			window: windowFor(24 * time.Hour),
			want:   "17:00",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := workflowLaneTrendPointLabel(tt.point, tt.window); got != tt.want {
				t.Fatalf("workflowLaneTrendPointLabel() = %q, want %q", got, tt.want)
			}
		})
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
				Href:            "/projects/busy/kanban",
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
				Href:            "/projects/detent/kanban",
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
		{ID: "paused", Name: "Paused", Paused: true, BoardLoad: 2, BoardBlocked: 1},
		{ID: "idle", Name: "Idle"},
		{ID: "queued", Name: "Queued", QueueCount: 1, BoardLoad: 1, BoardTodo: 1},
		{ID: "active", Name: "Active", Running: 1, BoardLoad: 1, BoardActive: 1},
		{ID: "blocked", Name: "Blocked", Blocked: 1, BoardBlocked: 1},
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
		if got[i].Href != "/projects/"+wantID+"/kanban" {
			t.Fatalf("sidebarProjectItems()[%d].Href = %q, want kanban project opener; got %#v", i, got[i].Href, got)
		}
	}
	idle := got[3]
	paused := got[4]
	if paused.StatusLabel != "paused" {
		t.Fatalf("paused StatusLabel = %q, want paused", paused.StatusLabel)
	}
	if paused.DotClass == idle.DotClass {
		t.Fatalf("paused DotClass = idle DotClass = %q, want distinct classes", paused.DotClass)
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
			name:       "global health",
			data:       DashboardShellData{ActiveNav: "health"},
			id:         "health",
			wantActive: true,
		},
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

func TestSidebarFleetActiveExcludesHealthNav(t *testing.T) {
	t.Parallel()

	if sidebarFleetActive(DashboardShellData{ActiveNav: "health"}) {
		t.Fatalf("sidebarFleetActive() must be false for health nav")
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

func TestRunningActivityIDUsesSessionID(t *testing.T) {
	t.Parallel()

	row := telemetry.Running{
		SessionID: "thread/running activity #1",
		Issue: telemetry.Issue{
			ID:         "issue-running-activity",
			Identifier: "digitaldrywood/detent#795",
		},
	}

	if got, want := runningActivityID("running", row), "running-activity-thread-running-activity-1"; got != want {
		t.Fatalf("runningActivityID() = %q, want %q", got, want)
	}
	if got, want := runningActivityDetailsID("running", row), "running-activity-thread-running-activity-1-details"; got != want {
		t.Fatalf("runningActivityDetailsID() = %q, want %q", got, want)
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
		{event: "turn_completed", message: "turn completed", at: localTimeToken(now, LocalTimeWithSeconds)},
		{event: "rate_limits", message: "rate snapshot", at: localTimeToken(now.Add(-time.Second), LocalTimeWithSeconds)},
		{event: "token_usage", message: "tokens updated", at: localTimeToken(now.Add(-2*time.Second), LocalTimeWithSeconds)},
		{event: "agent_message_delta", message: "editing dashboard", at: localTimeToken(now.Add(-3*time.Second), LocalTimeWithSeconds)},
		{event: "turn_started", message: "turn started", at: localTimeToken(now.Add(-4*time.Second), LocalTimeWithSeconds)},
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
	if rows[0].At != localTimeToken(at, LocalTimeWithSeconds) || rows[0].Event != "agent_message_delta" || rows[0].Message != "working through review feedback" {
		t.Fatalf("runningActivityRows()[0] = %#v", rows[0])
	}
}

func TestRunningActivityRowsMarkTruncatedOutput(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 5, 31, 15, 3, 4, 0, time.UTC)
	truncation := &runtimeoutput.Truncation{Truncated: true}
	row := telemetry.Running{
		LastEventAt:           &at,
		LastEvent:             "agent_message_delta",
		LastMessage:           "01234" + runtimeoutput.Marker + "vwxyz",
		LastMessageTruncation: truncation,
		RecentEvents: []telemetry.ActivityEvent{{
			At:         at,
			Event:      "agent_message_delta",
			Message:    "01234" + runtimeoutput.Marker + "vwxyz",
			Truncation: truncation,
		}},
	}

	if got, want := lastCodexUpdate(row), "01234"+runtimeoutput.Marker+"vwxyz [truncated]"; got != want {
		t.Fatalf("lastCodexUpdate() = %q, want %q", got, want)
	}
	rows := runningActivityRows(row)
	if len(rows) != 1 {
		t.Fatalf("runningActivityRows() len = %d, want 1", len(rows))
	}
	if got, want := rows[0].Message, "01234"+runtimeoutput.Marker+"vwxyz [truncated]"; got != want {
		t.Fatalf("runningActivityRows()[0].Message = %q, want %q", got, want)
	}
}

func TestIssueIdentityKeepsRepositoryIssueAndPullRequest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		issue telemetry.Issue
		want  issueIdentityView
	}{
		{
			name: "repository issue and pull request",
			issue: telemetry.Issue{
				Identifier: "digitaldrywood/creswoodcorners-phone#66",
				PullRequest: &telemetry.PullRequest{
					Number: 75,
					URL:    "https://github.com/digitaldrywood/creswoodcorners-phone/pull/75",
				},
			},
			want: issueIdentityView{
				Repository:        "digitaldrywood/creswoodcorners-phone",
				IssueNumber:       "#66",
				PullRequestNumber: 75,
				PullRequestLabel:  "PR #75",
				PullRequestURL:    "https://github.com/digitaldrywood/creswoodcorners-phone/pull/75",
				Label:             "digitaldrywood/creswoodcorners-phone #66 · PR #75",
			},
		},
		{
			name: "repository issue without pull request",
			issue: telemetry.Issue{
				Identifier: "digitaldrywood/detent#728",
			},
			want: issueIdentityView{
				Repository:  "digitaldrywood/detent",
				IssueNumber: "#728",
				Label:       "digitaldrywood/detent #728",
			},
		},
		{
			name: "repository from issue URL",
			issue: telemetry.Issue{
				Identifier: "ISSUE-728",
				URL:        "https://github.com/digitaldrywood/detent/issues/728",
			},
			want: issueIdentityView{
				Repository:  "digitaldrywood/detent",
				IssueNumber: "ISSUE-728",
				IssueURL:    "https://github.com/digitaldrywood/detent/issues/728",
				Label:       "digitaldrywood/detent ISSUE-728",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := issueIdentity(tt.issue); got != tt.want {
				t.Fatalf("issueIdentity() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestIssueReferenceUsesLocalNumberWithGitHubPrecedence(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name               string
		issue              telemetry.Issue
		wantIssueReference string
		wantKanbanNumber   string
		wantIssueNumber    string
	}{
		{
			name: "local number",
			issue: telemetry.Issue{
				Identifier: "wi-011cd179bc7ecf36b7197e4b",
				Number:     7,
			},
			wantIssueReference: "#7",
			wantKanbanNumber:   "#7",
			wantIssueNumber:    "#7",
		},
		{
			name: "github identifier",
			issue: telemetry.Issue{
				Identifier: "digitaldrywood/detent#779",
				Number:     7,
				Metadata:   map[string]string{githubIssueNumberMetadataKey: "881"},
			},
			wantIssueReference: "#779",
			wantKanbanNumber:   "#779",
			wantIssueNumber:    "#779",
		},
		{
			name: "github metadata",
			issue: telemetry.Issue{
				Identifier: "wi-linked",
				Number:     7,
				Metadata:   map[string]string{githubIssueNumberMetadataKey: "881"},
			},
			wantIssueReference: "#881",
			wantKanbanNumber:   "#881",
			wantIssueNumber:    "#881",
		},
		{
			name: "pull request issue number",
			issue: telemetry.Issue{
				Identifier:  "wi-review",
				Number:      7,
				PullRequest: &telemetry.PullRequest{Number: 32},
			},
			wantIssueReference: "#7",
			wantKanbanNumber:   "#7",
			wantIssueNumber:    "#32",
		},
		{
			name: "fallback identifier",
			issue: telemetry.Issue{
				Identifier: "wi-without-number",
			},
			wantIssueReference: "wi-without-number",
			wantKanbanNumber:   "wi-without-number",
			wantIssueNumber:    "wi-without-number",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := issueReference(tt.issue); got != tt.wantIssueReference {
				t.Fatalf("issueReference() = %q, want %q", got, tt.wantIssueReference)
			}
			if got := projectKanbanIssueNumber(tt.issue); got != tt.wantKanbanNumber {
				t.Fatalf("projectKanbanIssueNumber() = %q, want %q", got, tt.wantKanbanNumber)
			}
			if got := issueNumber(tt.issue); got != tt.wantIssueNumber {
				t.Fatalf("issueNumber() = %q, want %q", got, tt.wantIssueNumber)
			}
		})
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
	if rows[3].Identity.Label != "digitaldrywood/detent DD-2 · PR #22" {
		t.Fatalf("running timeline identity = %q, want full issue and PR identity", rows[3].Identity.Label)
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
			wantClass: "bg-err",
		},
		{
			name:      "completed row",
			row:       rows[1],
			wantState: "completed",
			wantStart: "16.67%",
			wantEnd:   "83.33%",
			wantWidth: "66.67%",
			wantClass: "bg-ok",
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
		{Lane: "Human Review", IssueNumber: "#12", Title: "Review lane PR", URL: "https://github.com/digitaldrywood/detent/issues/12", CIStatus: "pass", CodexReviewState: "clean", TimeInStage: "3m 0s", WaitDetail: "waiting for auto-promote", Labels: "enhancement, stage:s6", Assignees: "alice", ClearedBlockers: "digitaldrywood/detent#10 Done", Metadata: "PR #142"},
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

func TestProjectKanbanBoardPrefersConfiguredStateOverRawGitHubRuntimeState(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 7, 13, 23, 54, 0, time.UTC)
	reviewAt := now.Add(-9 * time.Minute)
	board := projectKanbanBoardView(DashboardData{
		Kanban: KanbanData{
			States: []string{"Backlog", "Todo", "In Progress", "Human Review", "Merging", "Done"},
		},
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			BoardIssues: []telemetry.Issue{
				{
					ID:             "issue-987",
					Identifier:     "digitaldrywood/detent#987",
					ProjectID:      "detent",
					Title:          "Recover workspace reaping when cached files return permission denied",
					State:          "Human Review",
					Labels:         []string{"detent:human-review"},
					StageUpdatedAt: &reviewAt,
				},
			},
			Pipeline: []telemetry.Issue{
				{
					ID:         "issue-987",
					Identifier: "digitaldrywood/detent#987",
					ProjectID:  "detent",
					Title:      "Recover workspace reaping when cached files return permission denied",
					State:      "Open",
				},
			},
			Running: []telemetry.Running{
				{
					Issue: telemetry.Issue{
						ID:         "issue-987",
						Identifier: "digitaldrywood/detent#987",
						ProjectID:  "detent",
						Title:      "Recover workspace reaping when cached files return permission denied",
						State:      "OPEN",
					},
					StartedAt: now.Add(-2 * time.Minute),
				},
			},
			Queue: []telemetry.Queued{
				{
					Issue: telemetry.Issue{
						ID:         "issue-987",
						Identifier: "digitaldrywood/detent#987",
						ProjectID:  "detent",
						Title:      "Recover workspace reaping when cached files return permission denied",
						State:      "OPEN",
					},
					Attempt: 1,
				},
			},
		},
	})

	if got := collectKanbanLaneTitles(board.AllLanes); containsString(got, "Open") {
		t.Fatalf("all lanes = %#v, want no raw Open lane", got)
	}
	got := collectKanbanCards(board.AllLanes)
	want := []kanbanCardSnapshot{
		{Lane: "Human Review", IssueNumber: "#987", Title: "Recover workspace reaping when cached files return permission denied", TimeInStage: "9m 0s", WaitDetail: "waiting for linked PR", Labels: "detent:human-review", Metadata: "No linked PR"},
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

func TestProjectKanbanBoardMapsRawClosedBoardIssueToConfiguredDoneLane(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 7, 14, 5, 0, 0, time.UTC)
	closedAt := now.Add(-3 * time.Minute)
	board := projectKanbanBoardView(DashboardData{
		Kanban: KanbanData{
			States: []string{"Todo", "In Progress", "Done"},
		},
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			BoardIssues: []telemetry.Issue{
				{
					ID:             "issue-991",
					Identifier:     "digitaldrywood/detent#991",
					ProjectID:      "detent",
					Title:          "Closed board issue",
					State:          "CLOSED",
					StageUpdatedAt: &closedAt,
				},
			},
		},
	})

	if got := collectKanbanLaneTitles(board.AllLanes); containsString(got, "Closed") {
		t.Fatalf("all lanes = %#v, want no raw Closed lane", got)
	}
	got := collectKanbanCards(board.AllLanes)
	want := []kanbanCardSnapshot{
		{Lane: "Done", IssueNumber: "#991", Title: "Closed board issue", TimeInStage: "3m 0s", Metadata: "No linked PR"},
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
		{Lane: "Merging", IssueNumber: "#144", Title: "Queued merge", CIStatus: "pending", CodexReviewState: "clean", TimeInStage: "2m 0s", Metadata: "PR #144", MergeLaneStatus: "Queued #2", MergeLaneDetail: "Waiting: project_state_capacity_full; 2nd in merge queue; waiting for repo merge lane behind digitaldrywood/detent#143 / PR #143; phase running checks"},
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

func TestPipelineIssueStageTimePrefersCurrentLaneEntry(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 10, 16, 27, 52, 0, time.UTC)
	createdAt := now.Add(-24 * time.Hour)
	enteredAt := now.Add(-111 * time.Minute)
	stageUpdatedAt := now.Add(-2 * time.Minute)
	updatedAt := now.Add(-time.Minute)
	tests := []struct {
		name  string
		issue telemetry.Issue
		want  time.Time
	}{
		{
			name: "current lane entry",
			issue: telemetry.Issue{
				CurrentLaneEnteredAt: &enteredAt,
				StageUpdatedAt:       &stageUpdatedAt,
				UpdatedAt:            &updatedAt,
				CreatedAt:            &createdAt,
			},
			want: enteredAt,
		},
		{
			name:  "stage fallback",
			issue: telemetry.Issue{StageUpdatedAt: &stageUpdatedAt, UpdatedAt: &updatedAt, CreatedAt: &createdAt},
			want:  stageUpdatedAt,
		},
		{
			name:  "updated fallback",
			issue: telemetry.Issue{UpdatedAt: &updatedAt, CreatedAt: &createdAt},
			want:  updatedAt,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := pipelineIssueStageTime(tt.issue); !got.Equal(tt.want) {
				t.Fatalf("pipelineIssueStageTime() = %v, want %v", got, tt.want)
			}
		})
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
		{
			name:  "unresolved dependency is not elevated outside blocked lane",
			state: "Merging",
			blockedBy: []telemetry.BlockedRef{
				{Identifier: "digitaldrywood/detent#432"},
			},
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

func TestProjectKanbanCardMoveDisabledTextCapabilities(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	card := projectKanbanCard{
		IssueID:   "I_kw1024",
		ProjectID: "detent",
		Stage:     "Todo",
		Movable:   true,
	}
	projectKanban := KanbanData{
		Mode:               "integration",
		States:             []string{"Todo", "In Progress"},
		AllowedTransitions: map[string][]string{"Todo": {"In Progress"}},
		CanMoveCards:       true,
	}
	fleetKanban := KanbanData{
		Projects: map[string]KanbanProjectData{
			"detent": {
				Mode:               "integration",
				ProjectID:          "detent",
				States:             []string{"Todo", "In Progress"},
				AllowedTransitions: map[string][]string{"Todo": {"In Progress"}},
			},
		},
	}
	tests := []struct {
		name string
		data DashboardData
		want string
	}{
		{
			name: "integration with move capability",
			data: DashboardData{
				ProjectID: "detent",
				Snapshot:  telemetry.Snapshot{GeneratedAt: now},
				Kanban:    projectKanban,
			},
		},
		{
			name: "integration without move capability",
			data: DashboardData{
				ProjectID: "detent",
				Snapshot:  telemetry.Snapshot{GeneratedAt: now},
				Kanban: func() KanbanData {
					kanban := projectKanban
					kanban.CanMoveCards = false
					return kanban
				}(),
			},
			want: "This project's tracker does not support moving cards.",
		},
		{
			name: "fleet project without move capability",
			data: DashboardData{
				Snapshot: telemetry.Snapshot{GeneratedAt: now},
				Kanban:   fleetKanban,
			},
			want: "This project's tracker does not support moving cards.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := projectKanbanCardMoveDisabledText(tt.data, card); got != tt.want {
				t.Fatalf("projectKanbanCardMoveDisabledText() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestProjectKanbanCardCanRemoveRespectsCapability(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	card := projectKanbanCard{IssueID: "I_kw1024"}
	data := DashboardData{
		ProjectID: "detent",
		Snapshot:  telemetry.Snapshot{GeneratedAt: now},
		Kanban: KanbanData{
			Mode: "integration",
		},
	}

	if projectKanbanCardCanRemove(data, card) {
		t.Fatalf("projectKanbanCardCanRemove() = true without capability, want false")
	}
	data.Kanban.CanRemoveCards = true
	if !projectKanbanCardCanRemove(data, card) {
		t.Fatalf("projectKanbanCardCanRemove() = false with capability, want true")
	}
}

func TestProjectKanbanBoardShowsRecentTerminalCompletion(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 13, 15, 0, 0, 0, time.UTC)
	data := DashboardData{
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
						State:      "Done",
					},
					CompletedAt: now,
					FinalState:  "Done",
				},
			},
			WorkAttempts: []telemetry.WorkAttempt{{
				IssueID:       "issue-396",
				Identifier:    "digitaldrywood/detent#396",
				Status:        "terminal",
				CompletedAt:   &now,
				TerminalState: "success",
				Phase:         "completed",
				StatusMessage: "worker reached terminal state",
			}},
		},
	}
	board := projectKanbanBoardView(data)

	if len(board.AllLanes) != 2 || len(board.AllLanes[1].Cards) != 1 {
		t.Fatalf("lanes = %#v, want one recent Done card", board.AllLanes)
	}
	if got := board.AllLanes[1].Cards[0].Title; got != "Completed session only" {
		t.Fatalf("Done card title = %q, want completed issue title", got)
	}
	if board.AllLanes[1].DefaultVisible {
		t.Fatalf("populated Done lane should be hidden by default")
	}
	if got := boardFiguresFromDashboard(data)[4].Value; got != "1" {
		t.Fatalf("recent completion count = %q, want 1", got)
	}
}

func TestProjectKanbanBoardRestoresRecentTerminalCompletionFromWorkAttempt(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 17, 15, 0, 0, 0, time.UTC)
	completedAt := now.Add(-25 * time.Hour)
	board := projectKanbanBoardView(DashboardData{
		Kanban: KanbanData{
			States:         []string{"Todo", "Done"},
			TerminalStates: []string{"Done"},
		},
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			WorkAttempts: []telemetry.WorkAttempt{{
				AttemptID:     1385,
				ProjectID:     "detent",
				IssueID:       "issue-1385",
				Identifier:    "digitaldrywood/detent#1385",
				IssueURL:      "https://github.com/digitaldrywood/detent/issues/1385",
				Status:        "terminal",
				CompletedAt:   &completedAt,
				TerminalState: "success",
				Phase:         "completed",
				StatusMessage: "worker reached terminal state",
			}},
		},
	})

	if len(board.AllLanes) != 2 || len(board.AllLanes[1].Cards) != 1 {
		t.Fatalf("lanes = %#v, want durable Done card", board.AllLanes)
	}
	if got := board.AllLanes[1].Cards[0].Identifier; got != "digitaldrywood/detent#1385" {
		t.Fatalf("Done card identifier = %q", got)
	}
	if got := board.AllLanes[1].Cards[0].IssueNumber; got != "#1385" {
		t.Fatalf("Done card issue number = %q, want #1385", got)
	}
	if !board.AllLanes[1].Cards[0].RecentCompletion {
		t.Fatalf("Done card should be marked as a recent completion")
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
		{Lane: "Human Review", IssueNumber: "#550", Title: "Keep completed implementation visible", CIStatus: "pass", CodexReviewState: "clean", TimeInStage: "30s", WaitDetail: "waiting for auto-promote", Metadata: "PR #552"},
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

	if got := projectKanbanIssues(DashboardData{Snapshot: snapshot}); len(got) != 0 {
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

func TestProjectKanbanBoardAccountsForHiddenPopulatedLanes(t *testing.T) {
	t.Parallel()

	issues := make([]telemetry.Issue, 0, 8)
	for i := 1; i <= 3; i++ {
		issues = append(issues, telemetry.Issue{
			ID:         "backlog-" + strconv.Itoa(i),
			Identifier: "digitaldrywood/detent#" + strconv.Itoa(520+i),
			Title:      "Backlog work " + strconv.Itoa(i),
			State:      "Backlog",
		})
	}
	for i := 1; i <= 5; i++ {
		issues = append(issues, telemetry.Issue{
			ID:         "done-" + strconv.Itoa(i),
			Identifier: "digitaldrywood/detent#" + strconv.Itoa(780+i),
			Title:      "Done work " + strconv.Itoa(i),
			State:      "Done",
		})
	}

	board := projectKanbanBoardView(DashboardData{
		Kanban: KanbanData{
			States:         []string{"Backlog", "Done"},
			TerminalStates: []string{"Done"},
		},
		Snapshot: telemetry.Snapshot{BoardIssues: issues},
	})

	if board.TotalCardCount != 8 || board.VisibleCardCount != 3 || board.HiddenCardCount != 5 {
		t.Fatalf("card counts = total %d visible %d hidden %d, want 8/3/5", board.TotalCardCount, board.VisibleCardCount, board.HiddenCardCount)
	}
	if len(board.HiddenPopulatedLanes) != 1 || board.HiddenPopulatedLanes[0].Title != "Done" {
		t.Fatalf("HiddenPopulatedLanes = %#v, want Done", board.HiddenPopulatedLanes)
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
							RunningChecks: []string{"Test Coverage"},
							UnstartedChecks: []telemetry.PullRequestCheck{
								{Name: "Portability Verify", Status: "queued", QueueSeconds: 47 * 60},
							},
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
				{Lane: "Human Review", IssueNumber: "#142", Title: "Review lane PR", CIStatus: "pass", CodexReviewState: "clean", TimeInStage: "2h 0m", WaitDetail: "PR hydration using stale cached data until " + localTimeToken(retryAt, LocalTimeOnly)},
				{Lane: "Merging", IssueNumber: "#143", Title: "Merge lane PR", CIStatus: "pending", CodexReviewState: "P2", TimeInStage: "15m 0s", WaitDetail: "quiet 10m 0s / queued 2m 0s / CI 8m 30s / slow GoReleaser Snapshot 4m 7s (queued 1m 0s) / unstarted Portability Verify queued 47m 0s, never started / running Test Coverage", MergeLaneStatus: "Queued #1", MergeLaneDetail: "1st in merge queue; waiting for repo merge lane"},
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

func TestPRPipelineWaitDetailExplainsHumanReviewReasons(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		issue telemetry.Issue
		want  string
	}{
		{
			name: "missing pull request",
			issue: telemetry.Issue{
				State: "Human Review",
			},
			want: "waiting for linked PR",
		},
		{
			name: "pending ci",
			issue: telemetry.Issue{
				State: "Human Review",
				PullRequest: &telemetry.PullRequest{
					CIStatus: "pending",
				},
			},
			want: "waiting for CI",
		},
		{
			name: "failed ci",
			issue: telemetry.Issue{
				State: "Human Review",
				PullRequest: &telemetry.PullRequest{
					CIStatus: "failure",
				},
			},
			want: "CI failed; Rework routing pending",
		},
		{
			name: "missing automated review",
			issue: telemetry.Issue{
				State: "Human Review",
				PullRequest: &telemetry.PullRequest{
					CIStatus: "success",
				},
			},
			want: "waiting for automated review",
		},
		{
			name: "blocking review finding",
			issue: telemetry.Issue{
				State: "Human Review",
				PullRequest: &telemetry.PullRequest{
					CIStatus:         "success",
					CodexReviewState: "P1",
				},
			},
			want: "P1 review finding blocks promotion",
		},
		{
			name: "ready for auto promote",
			issue: telemetry.Issue{
				State: "Human Review",
				PullRequest: &telemetry.PullRequest{
					CIStatus:         "success",
					CodexReviewState: "COMMENTED",
				},
			},
			want: "waiting for auto-promote",
		},
		{
			name: "auto promote decision reason",
			issue: telemetry.Issue{
				State: "Human Review",
				Metadata: map[string]string{
					projectKanbanAutoPromoteActionMetadataKey: "await_review",
					projectKanbanAutoPromoteReasonMetadataKey: "workpad_status_invalid",
				},
				PullRequest: &telemetry.PullRequest{
					CIStatus:         "success",
					CodexReviewState: "COMMENTED",
				},
			},
			want: "auto-promote await_review: workpad_status_invalid",
		},
		{
			name: "hydration explains stale data instead",
			issue: telemetry.Issue{
				State: "Human Review",
				PullRequest: &telemetry.PullRequest{
					CIStatus:                "success",
					HydrationDegradedReason: "stale_cached_pull_request",
				},
			},
			want: "PR hydration using stale cached data",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := prPipelineWaitDetail(tt.issue); got != tt.want {
				t.Fatalf("prPipelineWaitDetail() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPRPipelineWaitDetailDistinguishesOptionalReviewFromHumanHold(t *testing.T) {
	t.Parallel()

	deadline := time.Date(2026, 7, 13, 19, 30, 0, 0, time.UTC)
	tests := []struct {
		name  string
		issue telemetry.Issue
		want  string
	}{
		{
			name: "optional review shows merge deadline",
			issue: telemetry.Issue{
				State: "In Progress",
				Metadata: map[string]string{
					projectKanbanAutoPromoteActionMetadataKey:            "await_review",
					projectKanbanAutoPromoteReasonMetadataKey:            "automated_review_missing",
					projectKanbanAutomatedReviewModeMetadataKey:          "optional",
					projectKanbanAutomatedReviewTimeoutActionMetadataKey: "merge",
					projectKanbanAutomatedReviewDeadlineMetadataKey:      deadline.Format(time.RFC3339),
				},
			},
			want: "waiting for optional automated review; will merge at " + localTimeToken(deadline, LocalTimeOnly),
		},
		{
			name: "human review shows explicit hold",
			issue: telemetry.Issue{
				State: "Human Review",
				Metadata: map[string]string{
					projectKanbanAutoPromoteActionMetadataKey:            "await_review",
					projectKanbanAutoPromoteReasonMetadataKey:            "automated_review_missing",
					projectKanbanAutomatedReviewModeMetadataKey:          "required",
					projectKanbanAutomatedReviewTimeoutActionMetadataKey: "human_review",
				},
			},
			want: "held for human review after required automated review timed out",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := prPipelineWaitDetail(tt.issue); got != tt.want {
				t.Fatalf("prPipelineWaitDetail() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPRPipelineWaitDetailIncludesDispatchSkipReason(t *testing.T) {
	t.Parallel()

	issue := telemetry.Issue{
		State: "Todo",
		Metadata: map[string]string{
			"detent.dispatch_skip_reason": "artifact_gate_wait_status",
			"detent.artifact_gate_status": "queued",
		},
	}

	if got := prPipelineWaitDetail(issue); got != "waiting on artifact gate status ('queued')" {
		t.Fatalf("prPipelineWaitDetail() = %q, want artifact gate status", got)
	}
}

func TestPRPipelineWaitDetailShowsCurrentMergeSubstate(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 7, 22, 13, 0, 0, time.UTC)
	enteredAt := now.Add(-10 * time.Minute)
	acquiredAt := now.Add(-9 * time.Minute)
	ciWaitStartedAt := now.Add(-8 * time.Minute)
	completedAt := now.Add(-time.Minute)
	tests := []struct {
		name  string
		issue telemetry.Issue
		want  string
	}{
		{
			name: "waiting for merge worker slot",
			issue: telemetry.Issue{
				State:       "Merging",
				MergeTiming: &telemetry.MergeTiming{EnteredMergingAt: &enteredAt},
			},
			want: "waiting for merge worker slot since " + localTimeToken(enteredAt, LocalTimeOnly),
		},
		{
			name: "waiting on named current-head checks",
			issue: telemetry.Issue{
				State: "Merging",
				PullRequest: &telemetry.PullRequest{
					RunningChecks: []string{"Test Coverage", "Release"},
					RequiredCheckFailures: []telemetry.PullRequestCheck{
						{Name: "Test Coverage", Status: "in_progress"},
						{Name: "Release", Status: "queued"},
					},
				},
				MergeTiming: &telemetry.MergeTiming{
					EnteredMergingAt:          &enteredAt,
					MergeWorkerSlotAcquiredAt: &acquiredAt,
					CIWaitStartedAt:           &ciWaitStartedAt,
				},
			},
			want: "waiting on current-head CI since " + localTimeToken(ciWaitStartedAt, LocalTimeOnly) + ": Test Coverage, Release",
		},
		{
			name: "waiting on unstarted current-head check",
			issue: telemetry.Issue{
				State: "Merging",
				PullRequest: &telemetry.PullRequest{
					CIStatus: "pending",
					UnstartedChecks: []telemetry.PullRequestCheck{
						{Name: "Portability Verify", Status: "queued", QueueSeconds: 47 * 60},
					},
					RequiredCheckFailures: []telemetry.PullRequestCheck{
						{Name: "Portability Verify", Status: "queued"},
					},
				},
				MergeTiming: &telemetry.MergeTiming{
					EnteredMergingAt:          &enteredAt,
					MergeWorkerSlotAcquiredAt: &acquiredAt,
					CIWaitStartedAt:           &ciWaitStartedAt,
				},
			},
			want: "waiting on current-head CI since " + localTimeToken(ciWaitStartedAt, LocalTimeOnly) + ": Portability Verify queued 47m 0s, never started",
		},
		{
			name: "active merge without a blocking check",
			issue: telemetry.Issue{
				State: "Merging",
				PullRequest: &telemetry.PullRequest{
					CIStatus:       "success",
					MergeableState: "clean",
				},
				MergeTiming: &telemetry.MergeTiming{
					EnteredMergingAt:          &enteredAt,
					MergeWorkerSlotAcquiredAt: &acquiredAt,
				},
			},
			want: "active merge since " + localTimeToken(acquiredAt, LocalTimeOnly),
		},
		{
			name: "pending ci without a named check",
			issue: telemetry.Issue{
				State: "Merging",
				PullRequest: &telemetry.PullRequest{
					CIStatus:       "pending",
					MergeableState: "clean",
				},
				MergeTiming: &telemetry.MergeTiming{
					EnteredMergingAt:          &enteredAt,
					MergeWorkerSlotAcquiredAt: &acquiredAt,
					CIWaitStartedAt:           &ciWaitStartedAt,
				},
			},
			want: "waiting on current-head CI since " + localTimeToken(ciWaitStartedAt, LocalTimeOnly) + ": check name unavailable",
		},
		{
			name: "waiting for mergeability computation",
			issue: telemetry.Issue{
				State: "Merging",
				PullRequest: &telemetry.PullRequest{
					CIStatus:       "success",
					MergeableState: "unknown",
				},
				MergeTiming: &telemetry.MergeTiming{
					EnteredMergingAt:          &enteredAt,
					MergeWorkerSlotAcquiredAt: &acquiredAt,
					MergeStartedAt:            &ciWaitStartedAt,
				},
			},
			want: "waiting for GitHub mergeability since " + localTimeToken(ciWaitStartedAt, LocalTimeOnly),
		},
		{
			name: "terminal merge retains duration detail",
			issue: telemetry.Issue{
				State: "Done",
				MergeTiming: &telemetry.MergeTiming{
					MergedAt:                   &completedAt,
					QueueWaitSeconds:           60,
					ActiveMergeDurationSeconds: 480,
					TotalMergingSeconds:        540,
				},
			},
			want: "merge queue 1m 0s / active merge 8m 0s / total Merging 9m 0s",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := prPipelineWaitDetail(tt.issue); got != tt.want {
				t.Fatalf("prPipelineWaitDetail() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPRPipelineCardIncludesIssueAndPullRequestIdentity(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 24, 19, 0, 0, 0, time.UTC)
	card := prPipelineCardForIssue(telemetry.Issue{
		ID:         "review",
		Identifier: "digitaldrywood/detent#12",
		Title:      "Review lane PR",
		State:      "Human Review",
		PullRequest: &telemetry.PullRequest{
			Number: 142,
			URL:    "https://github.com/digitaldrywood/detent/pull/142",
		},
	}, "Human Review", "human-review", now.Add(-2*time.Minute), now)

	if card.Identity.Label != "digitaldrywood/detent #12 · PR #142" {
		t.Fatalf("card.Identity.Label = %q, want issue and PR identity", card.Identity.Label)
	}
	if card.IssueNumber != "#142" {
		t.Fatalf("card.IssueNumber = %q, want existing PR summary badge", card.IssueNumber)
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
		{Lane: "Merging", IssueNumber: "#144", Title: "Queued merge", CIStatus: "pending", CodexReviewState: "clean", TimeInStage: "2m 0s", MergeLaneStatus: "Queued #2", MergeLaneDetail: "2nd in merge queue; waiting for repo merge lane behind digitaldrywood/detent#143 / PR #143; phase squash merging"},
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

func TestMergeLaneStatusesDescribeCapacityQueueProgress(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 15, 18, 0, 0, 0, time.UTC)
	activeAt := now.Add(-5 * time.Minute)
	heartbeatAt := now.Add(-5 * time.Second)
	prNumber := int64(113)
	activeIssue := telemetry.Issue{
		ID:         "holder",
		Identifier: "digitaldrywood/video-studio#106",
		ProjectID:  "video-studio",
		Title:      "Active merge",
		State:      "Merging",
		URL:        "https://github.com/digitaldrywood/video-studio/issues/106",
		PullRequest: &telemetry.PullRequest{
			Number: 113,
			URL:    "https://github.com/digitaldrywood/video-studio/pull/113",
		},
	}
	queuedIssue := func(id string, number int, enteredAt time.Time) telemetry.Issue {
		return telemetry.Issue{
			ID:             id,
			Identifier:     "digitaldrywood/video-studio#" + strconv.Itoa(number),
			ProjectID:      "video-studio",
			Title:          "Queued merge",
			State:          "Merging",
			StageUpdatedAt: &enteredAt,
			PullRequest: &telemetry.PullRequest{
				Number: number + 1,
				URL:    "https://github.com/digitaldrywood/video-studio/pull/" + strconv.Itoa(number+1),
			},
		}
	}

	tests := []struct {
		name       string
		attempt    telemetry.WorkAttempt
		warnings   []telemetry.StalenessWarning
		wantLabels map[string]string
		wantKind   primitives.Kind
		wantDetail string
	}{
		{
			name: "fresh holder heartbeat marks every capacity waiter as draining",
			attempt: telemetry.WorkAttempt{
				AttemptID:   41,
				ProjectID:   "video-studio",
				IssueID:     "holder",
				Identifier:  activeIssue.Identifier,
				PRNumber:    &prNumber,
				Lane:        "Merging",
				Status:      "active",
				HeartbeatAt: &heartbeatAt,
				Phase:       "watching current-head CI",
			},
			wantLabels: map[string]string{"queued-1": "Draining #2", "queued-2": "Draining #3"},
			wantKind:   primitives.KindOK,
			wantDetail: "lane draining behind digitaldrywood/video-studio#106 / PR #113; phase watching current-head CI",
		},
		{
			name: "stale holder remains queued without inventing a stall",
			attempt: telemetry.WorkAttempt{
				AttemptID:   41,
				ProjectID:   "video-studio",
				IssueID:     "holder",
				Identifier:  activeIssue.Identifier,
				PRNumber:    &prNumber,
				Lane:        "Merging",
				Status:      "active",
				HeartbeatAt: &heartbeatAt,
				Phase:       "watching current-head CI",
				Stale:       true,
			},
			wantLabels: map[string]string{"queued-1": "Queued #2", "queued-2": "Queued #3"},
			wantKind:   primitives.KindWarn,
			wantDetail: "waiting for repo merge lane behind digitaldrywood/video-studio#106 / PR #113; phase watching current-head CI",
		},
		{
			name: "merge liveness warning is authoritative for not draining",
			attempt: telemetry.WorkAttempt{
				AttemptID:   41,
				ProjectID:   "video-studio",
				IssueID:     "holder",
				Identifier:  activeIssue.Identifier,
				PRNumber:    &prNumber,
				Lane:        "Merging",
				Status:      "active",
				HeartbeatAt: &heartbeatAt,
				Phase:       "watching current-head CI",
			},
			warnings:   []telemetry.StalenessWarning{{Kind: "merge_liveness", ProjectID: "video-studio", Lane: "Merging"}},
			wantLabels: map[string]string{"queued-1": "Not draining #2", "queued-2": "Not draining #3"},
			wantKind:   primitives.KindErr,
			wantDetail: "merge queue is not advancing behind digitaldrywood/video-studio#106 / PR #113; phase watching current-head CI",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			firstAt := now.Add(-4 * time.Minute)
			secondAt := now.Add(-3 * time.Minute)
			first := queuedIssue("queued-1", 114, firstAt)
			second := queuedIssue("queued-2", 116, secondAt)
			snapshot := telemetry.Snapshot{
				GeneratedAt:       now,
				Pipeline:          []telemetry.Issue{activeIssue, first, second},
				Running:           []telemetry.Running{{Issue: activeIssue, WorkAttemptID: 41, StartedAt: activeAt}},
				Queue:             []telemetry.Queued{{Issue: first, Error: "lane_capacity_full"}, {Issue: second, Error: "lane_capacity_full"}},
				WorkAttempts:      []telemetry.WorkAttempt{tt.attempt},
				StalenessWarnings: tt.warnings,
			}

			statuses := mergeLaneStatuses(snapshot)
			for issueID, wantLabel := range tt.wantLabels {
				status := statuses["project:video-studio:id:"+issueID]
				if status.Label != wantLabel || status.Kind != tt.wantKind || !strings.Contains(status.Detail, tt.wantDetail) {
					t.Fatalf("status for %s = %#v, want label %q, kind %q, detail containing %q", issueID, status, wantLabel, tt.wantKind, tt.wantDetail)
				}
			}
		})
	}
}

func TestPRPipelineLanesShowNativeMergeQueueDepthAndETA(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 13, 18, 0, 0, 0, time.UTC)
	queuedAt := now.Add(-2 * time.Minute)
	lanes := prPipelineLanes(telemetry.Snapshot{
		GeneratedAt: now,
		Pipeline: []telemetry.Issue{{
			ID:             "native-queued",
			Identifier:     "digitaldrywood/detent#144",
			Title:          "Native queued merge",
			State:          "Merging",
			StageUpdatedAt: &queuedAt,
			PullRequest: &telemetry.PullRequest{
				Number: 144,
				URL:    "https://github.com/digitaldrywood/detent/pull/144",
				MergeQueueEntry: &telemetry.PullRequestMergeQueueEntry{
					ID:                          "MQE_144",
					State:                       "AWAITING_CHECKS",
					Position:                    2,
					Depth:                       6,
					EstimatedTimeToMergeSeconds: 720,
					URL:                         "https://github.com/digitaldrywood/detent/queue/main",
				},
			},
		}},
	})

	cards := collectPipelineCards(lanes)
	if len(cards) != 1 {
		t.Fatalf("cards len = %d, want 1; cards = %#v", len(cards), cards)
	}
	if cards[0].MergeLaneStatus != "Native #2 of 6 · ~12m 0s" {
		t.Fatalf("MergeLaneStatus = %q, want native position, depth, and ETA", cards[0].MergeLaneStatus)
	}
	wantDetail := "GitHub native merge queue; state awaiting_checks; position 2 of 6; estimated drain 12m 0s"
	if cards[0].MergeLaneDetail != wantDetail {
		t.Fatalf("MergeLaneDetail = %q, want %q", cards[0].MergeLaneDetail, wantDetail)
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
				{Lane: "Human Review", IssueNumber: "#552", Title: "Keep tracker row visible", CIStatus: "pass", CodexReviewState: "clean", TimeInStage: "2m 0s", WaitDetail: "waiting for auto-promote"},
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
	if summary.Depth != "2" || summary.DrainETA != "4m 0s" {
		t.Fatalf("queue forecast = depth %q ETA %q, want 2 and 4m", summary.Depth, summary.DrainETA)
	}
}

func TestPRPipelineMergeSummaryPrefersNativeQueueETA(t *testing.T) {
	t.Parallel()

	summary := prPipelineMergeSummary(telemetry.Snapshot{
		GeneratedAt: time.Date(2026, 7, 13, 18, 0, 0, 0, time.UTC),
		Pipeline: []telemetry.Issue{
			{
				ID:         "native-1",
				Identifier: "digitaldrywood/detent#501",
				State:      "Merging",
				PullRequest: &telemetry.PullRequest{MergeQueueEntry: &telemetry.PullRequestMergeQueueEntry{
					ID:                          "MQE_501",
					EstimatedTimeToMergeSeconds: 300,
				}},
			},
			{
				ID:         "native-2",
				Identifier: "digitaldrywood/detent#502",
				State:      "Merging",
				PullRequest: &telemetry.PullRequest{MergeQueueEntry: &telemetry.PullRequestMergeQueueEntry{
					ID:                          "MQE_502",
					EstimatedTimeToMergeSeconds: 900,
				}},
			},
		},
	})

	if summary.Depth != "2" || summary.DrainETA != "15m 0s" {
		t.Fatalf("queue forecast = depth %q ETA %q, want 2 and 15m", summary.Depth, summary.DrainETA)
	}
}

func TestPRPipelineMergeSummaryUsesDistinctNativeQueueDepths(t *testing.T) {
	t.Parallel()

	summary := prPipelineMergeSummary(telemetry.Snapshot{
		GeneratedAt: time.Date(2026, 7, 13, 18, 0, 0, 0, time.UTC),
		Pipeline: []telemetry.Issue{
			nativeQueueSummaryIssue("native-1", "https://github.test/one/queue/main", 6),
			nativeQueueSummaryIssue("native-2", "https://github.test/one/queue/main", 6),
			nativeQueueSummaryIssue("native-3", "https://github.test/two/queue/main", 3),
		},
	})

	if summary.Depth != "9" {
		t.Fatalf("queue depth = %q, want distinct native queue depths totaling 9", summary.Depth)
	}
}

func nativeQueueSummaryIssue(id string, queueURL string, depth int) telemetry.Issue {
	return telemetry.Issue{
		ID:         id,
		Identifier: "digitaldrywood/detent#" + id,
		State:      "Merging",
		PullRequest: &telemetry.PullRequest{MergeQueueEntry: &telemetry.PullRequestMergeQueueEntry{
			ID:    "MQE_" + id,
			Depth: depth,
			URL:   queueURL,
		}},
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
