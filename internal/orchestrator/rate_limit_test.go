package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestTickPausesUntilGitHubGraphQLResetWhenRemainingLow(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	resetAt := now.Add(17 * time.Minute)
	cfg := normalizeConfig(Config{
		PollInterval:        30 * time.Second,
		MaxConcurrentAgents: 1,
		ActiveStates:        []string{"Todo", "In Progress"},
		TerminalStates:      []string{"Done", "Cancelled"},
	})
	state := newState(cfg)
	tracker := &rateLimitConnector{
		hasRateLimit: true,
		rateLimit: connector.GraphQLRateLimit{
			Limit:     5000,
			Used:      4975,
			Remaining: 25,
			Cost:      4,
			ResetAt:   resetAt,
			UpdatedAt: now,
		},
	}
	orch := newRateLimitTestOrchestrator(cfg, tracker)

	orch.tick(context.Background(), &state, now)

	if tracker.fetchCandidateCalls != 1 {
		t.Fatalf("FetchCandidateIssues() calls = %d, want 1", tracker.fetchCandidateCalls)
	}
	if state.RateLimits == nil || state.RateLimits.GitHubGraphQL == nil {
		t.Fatalf("RateLimits = %#v, want GitHub GraphQL snapshot", state.RateLimits)
	}
	if state.RateLimits.GitHubGraphQL.Remaining != 25 || state.RateLimits.GitHubGraphQL.Cost != 4 {
		t.Fatalf("GitHubGraphQL = %#v, want remaining 25 cost 4", state.RateLimits.GitHubGraphQL)
	}
	if state.PollInterval != 17*time.Minute {
		t.Fatalf("PollInterval = %s, want reset pause 17m", state.PollInterval)
	}
	if !state.NextRefreshAt.Equal(resetAt) {
		t.Fatalf("NextRefreshAt = %v, want %v", state.NextRefreshAt, resetAt)
	}
}

func TestTickSkipsConnectorPollingDuringGitHubGraphQLPause(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	resetAt := now.Add(10 * time.Minute)
	cfg := normalizeConfig(Config{
		PollInterval:        30 * time.Second,
		MaxConcurrentAgents: 1,
		ActiveStates:        []string{"Todo", "In Progress"},
		TerminalStates:      []string{"Done", "Cancelled"},
	})
	state := newState(cfg)
	state.RateLimits = &telemetry.RateLimits{
		GitHubGraphQL: &telemetry.RateLimitBucket{
			Remaining: 0,
			Limit:     5000,
			Used:      5000,
			ResetAt:   &resetAt,
		},
	}
	tracker := &rateLimitConnector{}
	orch := newRateLimitTestOrchestrator(cfg, tracker)

	orch.tick(context.Background(), &state, now)

	if tracker.fetchCandidateCalls != 0 {
		t.Fatalf("FetchCandidateIssues() calls = %d, want 0 during pause", tracker.fetchCandidateCalls)
	}
	if tracker.fetchByStatesCalls != 0 {
		t.Fatalf("FetchIssuesByStates() calls = %d, want 0 during pause", tracker.fetchByStatesCalls)
	}
	if state.PollInterval != 10*time.Minute {
		t.Fatalf("PollInterval = %s, want reset pause 10m", state.PollInterval)
	}
}

func TestTickPausesForGitHubGraphQLRetryAfterWithPrimaryRemaining(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{
		PollInterval:        30 * time.Second,
		MaxConcurrentAgents: 1,
		ActiveStates:        []string{"Todo", "In Progress"},
		TerminalStates:      []string{"Done", "Cancelled"},
	})
	state := newState(cfg)
	tracker := &rateLimitConnector{
		hasRateLimit: true,
		rateLimit: connector.GraphQLRateLimit{
			Limit:      5000,
			Used:       120,
			Remaining:  4880,
			RetryAfter: 2 * time.Minute,
			UpdatedAt:  now,
		},
	}
	orch := newRateLimitTestOrchestrator(cfg, tracker)

	orch.tick(context.Background(), &state, now)

	if state.PollInterval != 2*time.Minute {
		t.Fatalf("PollInterval = %s, want retry-after pause 2m", state.PollInterval)
	}
	if state.RateLimits == nil || state.RateLimits.GitHubGraphQL == nil {
		t.Fatalf("RateLimits = %#v, want GitHub GraphQL retry-after snapshot", state.RateLimits)
	}
	if state.RateLimits.GitHubGraphQL.Remaining != 4880 {
		t.Fatalf("GitHubGraphQL.Remaining = %d, want preserved primary remaining", state.RateLimits.GitHubGraphQL.Remaining)
	}
	if state.RateLimits.GitHubGraphQL.ResetInSeconds != 120 {
		t.Fatalf("GitHubGraphQL.ResetInSeconds = %d, want 120", state.RateLimits.GitHubGraphQL.ResetInSeconds)
	}
}

func TestTickPublishesAndLogsGitHubGraphQLCostSummary(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	resetAt := now.Add(time.Hour)
	cfg := normalizeConfig(Config{
		PollInterval:               30 * time.Second,
		MaxConcurrentAgents:        1,
		ActiveStates:               []string{"Todo", "In Progress"},
		TerminalStates:             []string{"Done", "Cancelled"},
		GitHubGraphQLWarnRemaining: 500,
	})
	state := newState(cfg)
	tracker := &rateLimitConnector{
		usage: connector.GraphQLRateLimitUsage{
			HasRateLimit: true,
			RateLimit: connector.GraphQLRateLimit{
				Limit:     5000,
				Used:      4501,
				Remaining: 499,
				Cost:      3,
				ResetAt:   resetAt,
				UpdatedAt: now,
			},
			QueryCosts: []connector.GraphQLQueryCost{
				{QueryType: "candidate_issues", Count: 1, Cost: 5},
				{QueryType: "running_states", Count: 1, Cost: 3},
			},
			TotalQueries: 2,
			TotalCost:    8,
		},
	}
	var logs bytes.Buffer
	orch := &Orchestrator{
		cfg:       cfg,
		connector: tracker,
		logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		})),
	}

	orch.tick(context.Background(), &state, now)

	if tracker.resetUsageCalls != 0 || tracker.flushUsageCalls != 1 {
		t.Fatalf("usage calls = reset %d flush %d, want reset 0 flush 1", tracker.resetUsageCalls, tracker.flushUsageCalls)
	}
	if state.RateLimits == nil || state.RateLimits.GitHubGraphQL == nil || state.RateLimits.GraphQLCost == nil {
		t.Fatalf("RateLimits = %#v, want GitHub GraphQL bucket and cost summary", state.RateLimits)
	}
	if state.RateLimits.GitHubGraphQL.Cost != 8 {
		t.Fatalf("GitHubGraphQL.Cost = %d, want cycle cost 8", state.RateLimits.GitHubGraphQL.Cost)
	}
	if state.RateLimits.GitHubGraphQL.ObservedAt == nil || !state.RateLimits.GitHubGraphQL.ObservedAt.Equal(now) {
		t.Fatalf("GitHubGraphQL.ObservedAt = %v, want %v", state.RateLimits.GitHubGraphQL.ObservedAt, now)
	}
	if state.RateLimits.GraphQLCost.TotalCost != 8 || state.RateLimits.GraphQLCost.TotalQueries != 2 {
		t.Fatalf("GraphQLCost = %#v, want cost 8 queries 2", state.RateLimits.GraphQLCost)
	}
	if len(state.RateLimits.GraphQLCost.Contributors) != 2 {
		t.Fatalf("GraphQLCost.Contributors = %#v, want 2 contributors", state.RateLimits.GraphQLCost.Contributors)
	}

	logOutput := logs.String()
	for _, want := range []string{
		"github graphql budget summary",
		"cycle_cost=8",
		"query_count=2",
		"candidate_issues",
		"github graphql budget below warning floor",
		"warning_floor=500",
	} {
		if !strings.Contains(logOutput, want) {
			t.Fatalf("log output missing %q:\n%s", want, logOutput)
		}
	}
}

func TestTickPublishesGitHubGraphQLExhaustionWithoutRateLimitSnapshot(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{
		PollInterval:        30 * time.Second,
		MaxConcurrentAgents: 1,
		ActiveStates:        []string{"Todo", "In Progress"},
		TerminalStates:      []string{"Done", "Cancelled"},
	})
	state := newState(cfg)
	tracker := &rateLimitConnector{
		usage: connector.GraphQLRateLimitUsage{
			RateLimitStatus: connector.GraphQLRateLimitStatusExhausted,
		},
	}
	orch := newRateLimitTestOrchestrator(cfg, tracker)

	orch.tick(context.Background(), &state, now)

	if state.RateLimits == nil || state.RateLimits.GitHubGraphQL == nil {
		t.Fatalf("RateLimits = %#v, want GitHub GraphQL status bucket", state.RateLimits)
	}
	if state.RateLimits.GitHubGraphQL.Status != telemetry.RateLimitStatusExhausted {
		t.Fatalf("GitHubGraphQL.Status = %q, want %q", state.RateLimits.GitHubGraphQL.Status, telemetry.RateLimitStatusExhausted)
	}
	if state.RateLimits.GitHubGraphQL.Limit != 0 || state.RateLimits.GitHubGraphQL.Remaining != 0 {
		t.Fatalf("GitHubGraphQL = %#v, want status-only exhausted bucket", state.RateLimits.GitHubGraphQL)
	}
}

func TestTickPublishesGitHubGraphQLUnknownWithoutRateLimitSnapshot(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{
		PollInterval:        30 * time.Second,
		MaxConcurrentAgents: 1,
		ActiveStates:        []string{"Todo", "In Progress"},
		TerminalStates:      []string{"Done", "Cancelled"},
	})
	state := newState(cfg)
	tracker := &rateLimitConnector{
		usage: connector.GraphQLRateLimitUsage{
			RateLimitStatus: connector.GraphQLRateLimitStatusUnknown,
		},
	}
	orch := newRateLimitTestOrchestrator(cfg, tracker)

	orch.tick(context.Background(), &state, now)

	if state.RateLimits == nil || state.RateLimits.GitHubGraphQL == nil {
		t.Fatalf("RateLimits = %#v, want GitHub GraphQL status bucket", state.RateLimits)
	}
	if state.RateLimits.GitHubGraphQL.Status != telemetry.RateLimitStatusUnknown {
		t.Fatalf("GitHubGraphQL.Status = %q, want %q", state.RateLimits.GitHubGraphQL.Status, telemetry.RateLimitStatusUnknown)
	}
	if state.RateLimits.GitHubGraphQL.Limit != 0 || state.RateLimits.GitHubGraphQL.Remaining != 0 {
		t.Fatalf("GitHubGraphQL = %#v, want status-only unknown bucket", state.RateLimits.GitHubGraphQL)
	}
}

func TestTickPublishesGitHubGraphQLFailureStatusWithoutStaleRateLimitSnapshot(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	resetAt := now.Add(time.Hour)
	cfg := normalizeConfig(Config{
		PollInterval:        30 * time.Second,
		MaxConcurrentAgents: 1,
		ActiveStates:        []string{"Todo", "In Progress"},
		TerminalStates:      []string{"Done", "Cancelled"},
	})
	state := newState(cfg)
	tracker := &rateLimitConnector{
		hasRateLimit: true,
		rateLimit: connector.GraphQLRateLimit{
			Limit:     5000,
			Used:      120,
			Remaining: 4880,
			ResetAt:   resetAt,
			UpdatedAt: now.Add(-time.Minute),
		},
		usage: connector.GraphQLRateLimitUsage{
			RateLimitStatus: connector.GraphQLRateLimitStatusExhausted,
		},
	}
	orch := newRateLimitTestOrchestrator(cfg, tracker)

	orch.tick(context.Background(), &state, now)

	if state.RateLimits == nil || state.RateLimits.GitHubGraphQL == nil {
		t.Fatalf("RateLimits = %#v, want GitHub GraphQL status bucket", state.RateLimits)
	}
	if state.RateLimits.GitHubGraphQL.Status != telemetry.RateLimitStatusExhausted {
		t.Fatalf("GitHubGraphQL.Status = %q, want %q", state.RateLimits.GitHubGraphQL.Status, telemetry.RateLimitStatusExhausted)
	}
	if state.RateLimits.GitHubGraphQL.Limit != 0 || state.RateLimits.GitHubGraphQL.Remaining != 0 || state.RateLimits.GitHubGraphQL.Used != 0 {
		t.Fatalf("GitHubGraphQL = %#v, want status-only bucket without stale quota", state.RateLimits.GitHubGraphQL)
	}
}

func TestTickCarriesGitHubGraphQLCostRecordedBetweenRefreshes(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	resetAt := now.Add(time.Hour)
	cfg := normalizeConfig(Config{
		PollInterval:        30 * time.Second,
		MaxConcurrentAgents: 1,
		ActiveStates:        []string{"Todo", "In Progress"},
		TerminalStates:      []string{"Done", "Cancelled"},
	})
	state := newState(cfg)
	tracker := &rateLimitConnector{
		usage: connector.GraphQLRateLimitUsage{
			HasRateLimit: true,
			RateLimit: connector.GraphQLRateLimit{
				Limit:     5000,
				Used:      142,
				Remaining: 4858,
				Cost:      1,
				ResetAt:   resetAt,
				UpdatedAt: now,
			},
			QueryCosts: []connector.GraphQLQueryCost{
				{QueryType: "default_blank_status", Count: 1, Cost: 1},
			},
			TotalQueries: 1,
			TotalCost:    1,
		},
	}
	orch := newRateLimitTestOrchestrator(cfg, tracker)

	orch.tick(context.Background(), &state, now)

	if tracker.resetUsageCalls != 0 {
		t.Fatalf("ResetGraphQLRateLimitUsage() calls = %d, want 0", tracker.resetUsageCalls)
	}
	if state.RateLimits == nil || state.RateLimits.GraphQLCost == nil {
		t.Fatalf("RateLimits = %#v, want carried GraphQL cost summary", state.RateLimits)
	}
	if state.RateLimits.GraphQLCost.TotalCost != 1 || state.RateLimits.GraphQLCost.TotalQueries != 1 {
		t.Fatalf("GraphQLCost = %#v, want carried cost 1 query 1", state.RateLimits.GraphQLCost)
	}
	if got := state.RateLimits.GraphQLCost.Contributors; len(got) != 1 || got[0].QueryType != "default_blank_status" {
		t.Fatalf("GraphQLCost.Contributors = %#v, want default_blank_status", got)
	}
}

func TestTickPublishesGitHubRESTUsageAndBackoff(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{
		PollInterval:        30 * time.Second,
		MaxConcurrentAgents: 1,
		ActiveStates:        []string{"Todo", "In Progress"},
		TerminalStates:      []string{"Done", "Cancelled"},
	})
	state := newState(cfg)
	tracker := &rateLimitConnector{
		restUsage: connector.RESTRateLimitUsage{
			HasRateLimit: true,
			RateLimit: connector.RESTRateLimit{
				Limit:      5000,
				Used:       122,
				Remaining:  4878,
				Resource:   "core",
				RetryAfter: time.Minute,
				UpdatedAt:  now,
			},
			Requests: []connector.RESTEndpointUsage{
				{EndpointFamily: "label issues", Count: 1, Conditional: 1, NotModified: 1, Remaining: 4879, Limit: 5000, Resource: "core"},
				{EndpointFamily: "issue comments", Count: 1, Billable: 1, Remaining: 4878, Limit: 5000, Resource: "core", RateLimited: true},
			},
			TotalRequests:       2,
			ConditionalRequests: 1,
			NotModifiedRequests: 1,
			BillableRequests:    1,
			RateLimited:         true,
			BackoffUntil:        now.Add(time.Minute),
		},
	}
	orch := newRateLimitTestOrchestrator(cfg, tracker)

	orch.tick(context.Background(), &state, now)

	if tracker.flushRESTUsageCalls != 1 {
		t.Fatalf("FlushRESTRateLimitUsage() calls = %d, want 1", tracker.flushRESTUsageCalls)
	}
	if state.RateLimits == nil || state.RateLimits.GitHubREST == nil || state.RateLimits.RESTUsage == nil {
		t.Fatalf("RateLimits = %#v, want GitHub REST bucket and usage summary", state.RateLimits)
	}
	if state.RateLimits.GitHubREST.Remaining != 4878 || state.RateLimits.GitHubREST.ResetInSeconds != 60 {
		t.Fatalf("GitHubREST = %#v, want remaining 4878 reset 60s", state.RateLimits.GitHubREST)
	}
	if state.RateLimits.GitHubREST.Cost != 1 {
		t.Fatalf("GitHubREST.Cost = %d, want one billable request", state.RateLimits.GitHubREST.Cost)
	}
	if state.RateLimits.RESTUsage.TotalRequests != 2 || !state.RateLimits.RESTUsage.RateLimited {
		t.Fatalf("RESTUsage = %#v, want 2 requests and rate limited", state.RateLimits.RESTUsage)
	}
	if state.RateLimits.RESTUsage.ConditionalRequests != 1 || state.RateLimits.RESTUsage.NotModifiedRequests != 1 || state.RateLimits.RESTUsage.BillableRequests != 1 {
		t.Fatalf("RESTUsage conditional breakdown = %#v, want one free 304 and one billable request", state.RateLimits.RESTUsage)
	}
	if len(state.RateLimits.RESTUsage.Contributors) != 2 || state.RateLimits.RESTUsage.Contributors[1].EndpointFamily != "issue comments" {
		t.Fatalf("RESTUsage.Contributors = %#v, want issue comments contributor", state.RateLimits.RESTUsage.Contributors)
	}
	if state.PollInterval != time.Minute {
		t.Fatalf("PollInterval = %s, want REST retry-after pause 1m", state.PollInterval)
	}
}

func TestTickSkipsConnectorPollingDuringGitHubRESTBackoff(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	resetAt := now.Add(2 * time.Minute)
	cfg := normalizeConfig(Config{
		PollInterval:        30 * time.Second,
		MaxConcurrentAgents: 1,
		ActiveStates:        []string{"Todo", "In Progress"},
		TerminalStates:      []string{"Done", "Cancelled"},
	})
	state := newState(cfg)
	state.RateLimits = &telemetry.RateLimits{
		GitHubREST: &telemetry.RateLimitBucket{
			Remaining:      4878,
			Limit:          5000,
			Used:           122,
			ResetAt:        &resetAt,
			ResetInSeconds: 120,
		},
	}
	tracker := &rateLimitConnector{}
	orch := newRateLimitTestOrchestrator(cfg, tracker)

	orch.tick(context.Background(), &state, now)

	if tracker.fetchCandidateCalls != 0 {
		t.Fatalf("FetchCandidateIssues() calls = %d, want 0 during REST backoff", tracker.fetchCandidateCalls)
	}
	if tracker.fetchByStatesCalls != 0 {
		t.Fatalf("FetchIssuesByStates() calls = %d, want 0 during REST backoff", tracker.fetchByStatesCalls)
	}
	if state.PollInterval != 2*time.Minute {
		t.Fatalf("PollInterval = %s, want reset pause 2m", state.PollInterval)
	}
}

func TestGitHubRESTCapacityOutagePausesDispatchAndResumesAtReset(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 10, 22, 0, 0, 0, time.UTC)
	resetAt := now.Add(37 * time.Minute)
	issue := connector.Issue{
		ID:               "issue-1211",
		Identifier:       "digitaldrywood/detent#1211",
		Title:            "Protect REST capacity",
		State:            "In Progress",
		AssignedToWorker: true,
	}
	cfg := normalizeConfig(Config{
		Project:                scheduler.ProjectCandidate{ID: "detent"},
		MaxConcurrentAgents:    1,
		ActiveStates:           []string{"Todo", "In Progress"},
		TerminalStates:         []string{"Done", "Cancelled"},
		GitHubRESTMinReserve:   1000,
		ContinuationRetryDelay: time.Minute,
	})
	state := newState(cfg)
	state.RateLimits = &telemetry.RateLimits{GitHubREST: &telemetry.RateLimitBucket{
		Limit:      5000,
		Used:       4100,
		Remaining:  900,
		ResetAt:    &resetAt,
		ObservedAt: timePointer(now),
	}}
	state.RepeatedFailures[issue.ID] = RepeatedFailure{Issue: issue, Count: 2}
	state.InstantFailures[issue.ID] = InstantFailure{Issue: issue, Count: 2}
	orch := newRateLimitTestOrchestrator(cfg, &rateLimitConnector{})
	orch.syncGitHubRESTCapacityOutage(&state, now)

	dispatches := 0
	hooks := dispatchPlanHooks{dispatch: func(connector.Issue, int, string) bool {
		dispatches++
		return true
	}}
	orch.dispatchPlanner().plan(&state, []connector.Issue{issue}, now, hooks)

	if dispatches != 0 {
		t.Fatalf("dispatches = %d, want 0 during GitHub REST capacity outage", dispatches)
	}
	outage, ok := activeGitHubRESTCapacityOutage(&state, now)
	if !ok {
		t.Fatalf("BackendOutages = %#v, want active GitHub REST outage", state.BackendOutages)
	}
	if outage.Kind != githubRESTCapacityKind || !outage.ResetAt.Equal(resetAt) || !outage.ResumeAt.Equal(resetAt) {
		t.Fatalf("outage = %#v, want reset-aware GitHub REST capacity outage", outage)
	}
	if state.RepeatedFailures[issue.ID].Count != 2 || state.InstantFailures[issue.ID].Count != 2 {
		t.Fatalf("breakers changed during capacity window: repeated=%#v instant=%#v", state.RepeatedFailures, state.InstantFailures)
	}
	snapshot := state.Snapshot(now)
	if len(snapshot.BackendOutages) != 1 || snapshot.BackendOutages[0].Kind != githubRESTCapacityKind {
		t.Fatalf("snapshot BackendOutages = %#v, want GitHub REST banner telemetry", snapshot.BackendOutages)
	}

	orch.syncGitHubRESTCapacityOutage(&state, resetAt)
	orch.dispatchPlanner().plan(&state, []connector.Issue{issue}, resetAt, hooks)

	if dispatches != 1 {
		t.Fatalf("dispatches = %d, want 1 at reset", dispatches)
	}
	if _, ok := activeGitHubRESTCapacityOutage(&state, resetAt); ok {
		t.Fatalf("BackendOutages = %#v, want automatic recovery at reset", state.BackendOutages)
	}
}

func TestTickDetectsGitHubRESTExhaustionBeforeDispatch(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 10, 22, 0, 0, 0, time.UTC)
	resetAt := now.Add(30 * time.Minute)
	issue := connector.Issue{
		ID:               "issue-1211-tick",
		Identifier:       "digitaldrywood/detent#1211",
		Title:            "Stop same-cycle dispatch",
		State:            "In Progress",
		AssignedToWorker: true,
	}
	cfg := normalizeConfig(Config{
		Project:                scheduler.ProjectCandidate{ID: "detent"},
		PollInterval:           30 * time.Second,
		MaxConcurrentAgents:    1,
		ActiveStates:           []string{"Todo", "In Progress"},
		TerminalStates:         []string{"Done", "Cancelled"},
		GitHubRESTMinReserve:   1000,
		ContinuationRetryDelay: time.Minute,
	})
	tracker := &rateLimitConnector{
		candidates: []connector.Issue{issue},
		restUsage: connector.RESTRateLimitUsage{
			HasRateLimit: true,
			RateLimit: connector.RESTRateLimit{
				Limit: 5000, Used: 5000, Remaining: 0, Resource: "core", ResetAt: resetAt, UpdatedAt: now,
			},
		},
	}
	orch := newRateLimitTestOrchestrator(cfg, tracker)
	state := newState(cfg)

	orch.tick(context.Background(), &state, now)

	if len(state.Running) != 0 {
		t.Fatalf("Running = %#v, want no paid dispatch after same-cycle exhaustion", state.Running)
	}
	if _, ok := activeGitHubRESTCapacityOutage(&state, now); !ok {
		t.Fatalf("BackendOutages = %#v, want active GitHub REST outage", state.BackendOutages)
	}
	if state.PollInterval != 30*time.Minute || !state.NextRefreshAt.Equal(resetAt) {
		t.Fatalf("refresh = interval %s next %v, want reset at %v", state.PollInterval, state.NextRefreshAt, resetAt)
	}
}

func TestRunCompletionDuringGitHubRESTCapacityOutageDoesNotStrikeBreakers(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 10, 22, 0, 0, 0, time.UTC)
	resetAt := now.Add(20 * time.Minute)
	issue := implementProgressIssueWithoutPR()
	cfg := normalizeConfig(Config{
		Project:                scheduler.ProjectCandidate{ID: "detent"},
		AutoPromote:            AutoPromoteConfig{NoProgressLimit: 3},
		ActiveStates:           []string{"Todo", "In Progress"},
		TerminalStates:         []string{"Done", "Cancelled"},
		GitHubRESTMinReserve:   1000,
		ContinuationRetryDelay: time.Minute,
	})
	attempts := &implementProgressAttemptStore{}
	orch := &Orchestrator{
		cfg:          cfg,
		connector:    &implementProgressConnector{},
		workAttempts: attempts,
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	state := newState(cfg)
	state.RateLimits = &telemetry.RateLimits{GitHubREST: &telemetry.RateLimitBucket{
		Limit: 5000, Used: 5000, Remaining: 0, ResetAt: &resetAt, ObservedAt: timePointer(now),
	}}
	orch.syncGitHubRESTCapacityOutage(&state, now)
	state.Running[issue.ID] = Running{
		Issue:         issue,
		Attempt:       2,
		WorkAttemptID: 42,
		Mode:          runpkg.RunModeImplement,
		StartedAt:     now.Add(-5 * time.Minute),
		DiffStats:     DiffStats{Status: "clean"},
	}
	state.Claimed[issue.ID] = Claimed{Issue: issue, ClaimedAt: now.Add(-5 * time.Minute)}

	orch.handleRunResult(context.Background(), &state, runpkg.Completion{
		IssueID:      issue.ID,
		CompletedAt:  now,
		RetryAttempt: 3,
		Request:      runpkg.RunRequest{Mode: runpkg.RunModeImplement},
		Result:       runpkg.RunResult{FinalState: runpkg.FinalStateFailed, DiffStats: DiffStats{Status: "clean"}},
		Err:          errors.New("gh pr create: HTTP 403 API rate limit exceeded"),
	})

	if len(attempts.completions) != 1 || attempts.completions[0].TerminalState != store.WorkAttemptTerminalCapacity {
		t.Fatalf("completions = %#v, want one capacity terminal", attempts.completions)
	}
	retry, ok := state.Retry[issue.ID]
	if !ok || retry.Attempt != 2 || !retry.DueAt.Equal(resetAt) {
		t.Fatalf("Retry[%q] = %#v, want same-attempt retry at reset", issue.ID, retry)
	}
	if len(state.RepeatedFailures) != 0 || len(state.InstantFailures) != 0 {
		t.Fatalf("breakers = repeated %#v instant %#v, want no strikes", state.RepeatedFailures, state.InstantFailures)
	}
	if _, ok := state.Completed[issue.ID]; ok {
		t.Fatalf("Completed[%q] present during capacity outage", issue.ID)
	}
}

func TestTickSkipsNonessentialGitHubWorkBelowRESTReserve(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	resetAt := now.Add(time.Hour)
	cfg := normalizeConfig(Config{
		PollInterval:                  30 * time.Second,
		MaxConcurrentAgents:           1,
		ActiveStates:                  []string{"Todo", "In Progress"},
		ObservedStates:                []string{"Human Review", "Blocked"},
		TerminalStates:                []string{"Done", "Cancelled"},
		WorkspaceCleanupSweepInterval: time.Minute,
		GitHubRESTMinReserve:          1000,
		GitHubGraphQLMinReserve:       1000,
	})
	state := newState(cfg)
	state.RateLimits = &telemetry.RateLimits{
		GitHubREST: &telemetry.RateLimitBucket{
			Remaining: 900,
			Limit:     5000,
			Used:      4100,
			ResetAt:   &resetAt,
		},
	}
	tracker := &rateLimitConnector{}
	var logs bytes.Buffer
	orch := &Orchestrator{
		cfg:       cfg,
		connector: tracker,
		reaper:    rateLimitWorkspaceReaper{},
		logger:    slog.New(slog.NewTextHandler(&logs, nil)),
	}

	orch.tick(context.Background(), &state, now)

	if tracker.fetchCandidateCalls != 1 {
		t.Fatalf("FetchCandidateIssues() calls = %d, want minimal active candidate fetch", tracker.fetchCandidateCalls)
	}
	if tracker.fetchByStatesCalls != 0 {
		t.Fatalf("FetchIssuesByStates() calls = %d, want no cleanup or observed sweep", tracker.fetchByStatesCalls)
	}
	if tracker.fetchByStatesLimitCalls != 0 {
		t.Fatalf("FetchIssuesByStatesLimit() calls = %d, want no observed probe during reserve", tracker.fetchByStatesLimitCalls)
	}
	if !rateLimitRecentEventContains(state.RecentEvents, "github_budget_reserved", "preserve shared budget") {
		t.Fatalf("RecentEvents = %#v, want github budget reserve event", state.RecentEvents)
	}
	if !rateLimitRecentEventContains(state.RecentEvents, "github_rest_capacity_paused", "resuming at") {
		t.Fatalf("RecentEvents = %#v, want GitHub REST dispatch pause event", state.RecentEvents)
	}
	logOutput := logs.String()
	for _, want := range []string{
		"github polling degraded to preserve shared budget",
		"workspace cleanup skipped to preserve shared github budget",
		"observed status polling skipped to preserve shared github budget",
		"rest_remaining=900",
		"rest_reserve=1000",
	} {
		if !strings.Contains(logOutput, want) {
			t.Fatalf("log output missing %q:\n%s", want, logOutput)
		}
	}
}

func TestTickAllowsConditionalPollingBelowRESTReserve(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	resetAt := now.Add(time.Hour)
	issue := connector.Issue{ID: "I_1133", Identifier: "digitaldrywood/detent#1133", State: "Human Review"}
	cfg := normalizeConfig(Config{
		PollInterval:            time.Minute,
		MaxConcurrentAgents:     1,
		ActiveStates:            []string{"Todo", "In Progress"},
		ObservedStates:          []string{"Human Review", "Blocked"},
		TerminalStates:          []string{"Done"},
		GitHubRESTMinReserve:    1000,
		GitHubGraphQLMinReserve: 1000,
	})
	state := newState(cfg)
	state.Pipeline = []connector.Issue{issue}
	state.LastWorkspaceCleanupAt = now
	state.RateLimits = &telemetry.RateLimits{
		GitHubREST: &telemetry.RateLimitBucket{
			Remaining: 900,
			Limit:     5000,
			Used:      4100,
			ResetAt:   &resetAt,
		},
	}
	base := &rateLimitConnector{stateIssues: []connector.Issue{issue}}
	tracker := &conditionalRateLimitConnector{rateLimitConnector: base}
	orch := newRateLimitTestOrchestrator(cfg, tracker)

	orch.tick(context.Background(), &state, now)

	if base.fetchCandidateCalls != 1 || base.fetchByStatesCalls != 1 {
		t.Fatalf("fetch calls = candidates %d states %d, want one conditional cycle", base.fetchCandidateCalls, base.fetchByStatesCalls)
	}
	for _, event := range state.RecentEvents {
		if event.Event == "github_budget_reserved" {
			t.Fatalf("RecentEvents = %#v, want no reserve skip for conditional polling", state.RecentEvents)
		}
	}
}

func TestTickClearsStaleBlockedStatusWhenCandidateResumesBelowGitHubReserve(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	resetAt := now.Add(time.Hour)
	issue := connector.Issue{
		ID:         "I_98",
		Identifier: "digitaldrywood/detent#98",
		Title:      "Clear stale blocked status",
		State:      "In Progress",
	}
	blockedIssue := issue
	blockedIssue.State = "Blocked"
	blockedIssue.BlockerReason = "waiting on human input"
	cfg := normalizeConfig(Config{
		PollInterval:            30 * time.Second,
		MaxConcurrentAgents:     1,
		ActiveStates:            []string{"Todo", "In Progress"},
		ObservedStates:          []string{"Human Review", "Blocked"},
		TerminalStates:          []string{"Done", "Cancelled"},
		GitHubRESTMinReserve:    1000,
		GitHubGraphQLMinReserve: 1000,
	})
	state := newState(cfg)
	state.RateLimits = &telemetry.RateLimits{
		GitHubREST: &telemetry.RateLimitBucket{
			Remaining: 900,
			Limit:     5000,
			Used:      4100,
			ResetAt:   &resetAt,
		},
	}
	state.Running[issue.ID] = Running{Issue: issue, StartedAt: now.Add(-time.Minute)}
	state.Blocked[issue.ID] = Blocked{
		Issue:     blockedIssue,
		Reason:    blockedStatusReason(blockedIssue),
		BlockedAt: now.Add(-time.Hour),
		Source:    BlockedSourceProjectStatus,
	}
	tracker := &rateLimitConnector{candidates: []connector.Issue{issue}}
	orch := newRateLimitTestOrchestrator(cfg, tracker)

	orch.tick(context.Background(), &state, now)

	if tracker.fetchCandidateCalls != 1 {
		t.Fatalf("FetchCandidateIssues() calls = %d, want minimal active candidate fetch", tracker.fetchCandidateCalls)
	}
	if tracker.fetchByStatesCalls != 0 {
		t.Fatalf("FetchIssuesByStates() calls = %d, want observed polling skipped", tracker.fetchByStatesCalls)
	}
	if tracker.fetchByStatesLimitCalls != 0 {
		t.Fatalf("FetchIssuesByStatesLimit() calls = %d, want observed probe skipped", tracker.fetchByStatesLimitCalls)
	}
	if len(state.BoardIssues) != 1 || state.BoardIssues[0].ID != issue.ID || state.BoardIssues[0].State != "In Progress" {
		t.Fatalf("BoardIssues = %#v, want resumed In Progress candidate", state.BoardIssues)
	}
	if _, ok := state.Blocked[issue.ID]; ok {
		t.Fatalf("Blocked[%q] still tracked after candidate resumed In Progress during budget reserve", issue.ID)
	}
}

func TestTickSkipsObservedPollingBelowGraphQLReserve(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	resetAt := now.Add(time.Hour)
	cfg := normalizeConfig(Config{
		PollInterval:            30 * time.Second,
		MaxConcurrentAgents:     1,
		ActiveStates:            []string{"Todo", "In Progress"},
		ObservedStates:          []string{"Human Review"},
		TerminalStates:          []string{"Done", "Cancelled"},
		GitHubGraphQLMinReserve: 1000,
		GitHubRESTMinReserve:    1000,
	})
	state := newState(cfg)
	state.RateLimits = &telemetry.RateLimits{
		GitHubGraphQL: &telemetry.RateLimitBucket{
			Remaining: 900,
			Limit:     5000,
			Used:      4100,
			ResetAt:   &resetAt,
		},
	}
	tracker := &rateLimitConnector{}
	orch := newRateLimitTestOrchestrator(cfg, tracker)

	orch.tick(context.Background(), &state, now)

	if tracker.fetchCandidateCalls != 1 {
		t.Fatalf("FetchCandidateIssues() calls = %d, want minimal active candidate fetch", tracker.fetchCandidateCalls)
	}
	if tracker.fetchByStatesCalls != 0 {
		t.Fatalf("FetchIssuesByStates() calls = %d, want observed polling skipped", tracker.fetchByStatesCalls)
	}
	if tracker.fetchByStatesLimitCalls != 0 {
		t.Fatalf("FetchIssuesByStatesLimit() calls = %d, want observed probe skipped", tracker.fetchByStatesLimitCalls)
	}
}

func TestTickInactiveProjectUsesBoundedObservedProbe(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{
		PollInterval:        30 * time.Second,
		MaxConcurrentAgents: 1,
		ActiveStates:        []string{"Todo", "In Progress"},
		ObservedStates:      []string{"Blocked", "Human Review", "Merging"},
		TerminalStates:      []string{"Done", "Cancelled"},
	})
	state := newState(cfg)
	tracker := &rateLimitConnector{}
	orch := newRateLimitTestOrchestrator(cfg, tracker)

	orch.tick(context.Background(), &state, now)

	if tracker.fetchCandidateCalls != 1 {
		t.Fatalf("FetchCandidateIssues() calls = %d, want 1", tracker.fetchCandidateCalls)
	}
	if tracker.fetchByStatesLimitCalls != 1 {
		t.Fatalf("FetchIssuesByStatesLimit() calls = %d, want 1", tracker.fetchByStatesLimitCalls)
	}
	if tracker.fetchByStatesCalls != 0 {
		t.Fatalf("FetchIssuesByStates() calls = %d, want 0 for inactive project", tracker.fetchByStatesCalls)
	}
}

func TestReapWorkspacesVerifiesKnownWorkspaceIssueIDsBeforeStateSweep(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	running := connector.Issue{ID: "I_666", Identifier: "digitaldrywood/detent#666", State: "In Progress"}
	done := running
	done.State = "Done"
	cfg := normalizeConfig(Config{
		PollInterval:                  30 * time.Second,
		MaxConcurrentAgents:           1,
		ActiveStates:                  []string{"Todo", "In Progress"},
		ObservedStates:                []string{"Human Review"},
		TerminalStates:                []string{"Done", "Cancelled"},
		WorkspaceCleanupSweepInterval: time.Minute,
	})
	state := newState(cfg)
	state.Running[running.ID] = Running{
		Issue:         running,
		WorkspacePath: "/tmp/detent-workspaces/issue-666",
		StartedAt:     now.Add(-time.Hour),
	}
	tracker := &rateLimitConnector{issuesByID: []connector.Issue{done}}
	orch := newRateLimitTestOrchestrator(cfg, tracker)
	orch.reaper = rateLimitWorkspaceReaper{}

	orch.reapWorkspacesIfDue(context.Background(), &state, now)

	if tracker.fetchByIDCalls != 1 {
		t.Fatalf("FetchIssueStatesByIDs() calls = %d, want 1", tracker.fetchByIDCalls)
	}
	if tracker.fetchByStatesCalls != 0 {
		t.Fatalf("FetchIssuesByStates() calls = %d, want 0 when known workspace IDs are verified", tracker.fetchByStatesCalls)
	}
	if _, ok := state.Completed[running.ID]; !ok {
		t.Fatalf("Completed[%q] missing after terminal known-workspace verification", running.ID)
	}
}

func TestReapWorkspacesFallsBackToStateSweepWhenKnownWorkspaceIssueIDFetchFails(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	running := connector.Issue{ID: "I_666", Identifier: "digitaldrywood/detent#666", State: "In Progress"}
	terminal := connector.Issue{ID: "I_667", Identifier: "digitaldrywood/detent#667", State: "Done"}
	cfg := normalizeConfig(Config{
		PollInterval:                  30 * time.Second,
		MaxConcurrentAgents:           1,
		ActiveStates:                  []string{"Todo", "In Progress"},
		ObservedStates:                []string{"Human Review"},
		TerminalStates:                []string{"Done", "Cancelled"},
		WorkspaceCleanupSweepInterval: time.Minute,
	})
	state := newState(cfg)
	state.Running[running.ID] = Running{
		Issue:         running,
		WorkspacePath: "/tmp/detent-workspaces/issue-666",
		StartedAt:     now.Add(-time.Hour),
	}
	tracker := &rateLimitConnector{
		fetchByIDErr: errors.New("github transient error: status 502"),
		stateIssues:  []connector.Issue{terminal},
	}
	orch := newRateLimitTestOrchestrator(cfg, tracker)
	orch.reaper = rateLimitWorkspaceReaper{}

	orch.reapWorkspacesIfDue(context.Background(), &state, now)

	if tracker.fetchByIDCalls != 1 {
		t.Fatalf("FetchIssueStatesByIDs() calls = %d, want 1", tracker.fetchByIDCalls)
	}
	if tracker.fetchByStatesCalls != 1 {
		t.Fatalf("FetchIssuesByStates() calls = %d, want fallback state sweep", tracker.fetchByStatesCalls)
	}
	if _, ok := state.ReapedWorkspaces[terminal.ID]; !ok {
		t.Fatalf("ReapedWorkspaces[%q] missing after fallback state sweep", terminal.ID)
	}
	if !rateLimitRecentEventContains(state.RecentEvents, "workspace_cleanup_fetch_failed", "status 502") {
		t.Fatalf("RecentEvents = %#v, want cleanup fetch failure event", state.RecentEvents)
	}
	if state.LastRefreshError != "" || !state.LastRefreshErrorAt.IsZero() {
		t.Fatalf("refresh error = %q at %v, want none for cleanup fallback", state.LastRefreshError, state.LastRefreshErrorAt)
	}
}

func TestReapWorkspacesStillSweepsWhenKnownWorkspaceIsActive(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	running := connector.Issue{ID: "I_666", Identifier: "digitaldrywood/detent#666", State: "In Progress"}
	terminal := connector.Issue{ID: "I_667", Identifier: "digitaldrywood/detent#667", State: "Done"}
	cfg := normalizeConfig(Config{
		PollInterval:                  30 * time.Second,
		MaxConcurrentAgents:           1,
		ActiveStates:                  []string{"Todo", "In Progress"},
		ObservedStates:                []string{"Human Review"},
		TerminalStates:                []string{"Done", "Cancelled"},
		WorkspaceCleanupSweepInterval: time.Minute,
	})
	state := newState(cfg)
	state.Running[running.ID] = Running{
		Issue:         running,
		WorkspacePath: "/tmp/detent-workspaces/issue-666",
		StartedAt:     now.Add(-time.Hour),
	}
	tracker := &rateLimitConnector{
		issuesByID:  []connector.Issue{running},
		stateIssues: []connector.Issue{terminal},
	}
	orch := newRateLimitTestOrchestrator(cfg, tracker)
	orch.reaper = rateLimitWorkspaceReaper{}

	orch.reapWorkspacesIfDue(context.Background(), &state, now)

	if tracker.fetchByIDCalls != 1 {
		t.Fatalf("FetchIssueStatesByIDs() calls = %d, want 1", tracker.fetchByIDCalls)
	}
	if tracker.fetchByStatesCalls != 1 {
		t.Fatalf("FetchIssuesByStates() calls = %d, want due sweep after active known workspace", tracker.fetchByStatesCalls)
	}
	if _, ok := state.ReapedWorkspaces[terminal.ID]; !ok {
		t.Fatalf("ReapedWorkspaces[%q] missing after due sweep", terminal.ID)
	}
}

func TestTickReconcilesRunningIssuesOnSlowerCadence(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	issue := connector.Issue{ID: "I_260", Identifier: "digitaldrywood/detent#260", State: "In Progress"}
	cfg := normalizeConfig(Config{
		PollInterval:        30 * time.Second,
		MaxConcurrentAgents: 1,
		ActiveStates:        []string{"Todo", "In Progress"},
		TerminalStates:      []string{"Done", "Cancelled"},
	})
	state := newState(cfg)
	state.Running[issue.ID] = Running{Issue: cloneIssue(issue)}
	tracker := &rateLimitConnector{issuesByID: []connector.Issue{issue}}
	orch := newRateLimitTestOrchestrator(cfg, tracker)

	orch.tick(context.Background(), &state, now)
	orch.tick(context.Background(), &state, now.Add(30*time.Second))
	orch.tick(context.Background(), &state, now.Add(2*time.Minute))

	if tracker.fetchByIDCalls != 2 {
		t.Fatalf("FetchIssueStatesByIDs() calls = %d, want first tick plus slower cadence", tracker.fetchByIDCalls)
	}
}

func newRateLimitTestOrchestrator(cfg Config, tracker connector.Connector) *Orchestrator {
	return &Orchestrator{
		cfg:       cfg,
		connector: tracker,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func rateLimitRecentEventContains(events []telemetry.ActivityEvent, event string, message string) bool {
	for _, candidate := range events {
		if candidate.Event == event && strings.Contains(candidate.Message, message) {
			return true
		}
	}
	return false
}

type rateLimitConnector struct {
	candidates              []connector.Issue
	stateIssues             []connector.Issue
	issuesByID              []connector.Issue
	rateLimit               connector.GraphQLRateLimit
	hasRateLimit            bool
	usage                   connector.GraphQLRateLimitUsage
	restUsage               connector.RESTRateLimitUsage
	resetUsageCalls         int
	flushUsageCalls         int
	flushRESTUsageCalls     int
	fetchCandidateCalls     int
	fetchByStatesCalls      int
	fetchByStatesLimitCalls int
	fetchByIDCalls          int
	fetchByIDErr            error
}

type conditionalRateLimitConnector struct {
	*rateLimitConnector
}

func (*conditionalRateLimitConnector) ConditionalPollingEnabled() bool {
	return true
}

func (c *rateLimitConnector) Name() string {
	return "rate-limit"
}

func (c *rateLimitConnector) FetchCandidateIssues(context.Context) ([]connector.Issue, error) {
	c.fetchCandidateCalls++
	return cloneIssues(c.candidates), nil
}

func (c *rateLimitConnector) FetchIssuesByStates(context.Context, []string) ([]connector.Issue, error) {
	c.fetchByStatesCalls++
	return cloneIssues(c.stateIssues), nil
}

func (c *rateLimitConnector) FetchIssuesByStatesLimit(context.Context, []string, int) ([]connector.Issue, error) {
	c.fetchByStatesLimitCalls++
	return cloneIssues(c.stateIssues), nil
}

func (c *rateLimitConnector) FetchIssueStatesByIDs(context.Context, []string) ([]connector.Issue, error) {
	c.fetchByIDCalls++
	if c.fetchByIDErr != nil {
		return nil, c.fetchByIDErr
	}
	return cloneIssues(c.issuesByID), nil
}

func (c *rateLimitConnector) CreateComment(context.Context, string, string) error {
	return nil
}

func (c *rateLimitConnector) UpdateIssueState(context.Context, string, string) error {
	return nil
}

func (c *rateLimitConnector) SetAssignee(context.Context, string, string) error {
	return nil
}

func (c *rateLimitConnector) SetField(context.Context, string, string, string) error {
	return nil
}

func (c *rateLimitConnector) GraphQLRateLimit() (connector.GraphQLRateLimit, bool) {
	return c.rateLimit, c.hasRateLimit
}

func (c *rateLimitConnector) ResetGraphQLRateLimitUsage() {
	c.resetUsageCalls++
	c.usage = connector.GraphQLRateLimitUsage{}
}

func (c *rateLimitConnector) FlushGraphQLRateLimitUsage() connector.GraphQLRateLimitUsage {
	c.flushUsageCalls++
	return c.usage
}

func (c *rateLimitConnector) FlushRESTRateLimitUsage() connector.RESTRateLimitUsage {
	c.flushRESTUsageCalls++
	return c.restUsage
}

type rateLimitWorkspaceReaper struct{}

func (rateLimitWorkspaceReaper) ReapWorkspace(context.Context, connector.Issue) (WorkspaceReapResult, error) {
	return WorkspaceReapResult{}, nil
}
