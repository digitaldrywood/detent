package orchestrator

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
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
		logger:    slog.New(slog.NewTextHandler(&logs, nil)),
	}

	orch.tick(context.Background(), &state, now)

	if tracker.resetUsageCalls != 1 || tracker.flushUsageCalls != 1 {
		t.Fatalf("usage calls = reset %d flush %d, want 1 each", tracker.resetUsageCalls, tracker.flushUsageCalls)
	}
	if state.RateLimits == nil || state.RateLimits.GitHubGraphQL == nil || state.RateLimits.GraphQLCost == nil {
		t.Fatalf("RateLimits = %#v, want GitHub GraphQL bucket and cost summary", state.RateLimits)
	}
	if state.RateLimits.GitHubGraphQL.Cost != 8 {
		t.Fatalf("GitHubGraphQL.Cost = %d, want cycle cost 8", state.RateLimits.GitHubGraphQL.Cost)
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

type rateLimitConnector struct {
	candidates          []connector.Issue
	stateIssues         []connector.Issue
	issuesByID          []connector.Issue
	rateLimit           connector.GraphQLRateLimit
	hasRateLimit        bool
	usage               connector.GraphQLRateLimitUsage
	resetUsageCalls     int
	flushUsageCalls     int
	fetchCandidateCalls int
	fetchByStatesCalls  int
	fetchByIDCalls      int
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

func (c *rateLimitConnector) FetchIssueStatesByIDs(context.Context, []string) ([]connector.Issue, error) {
	c.fetchByIDCalls++
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
}

func (c *rateLimitConnector) FlushGraphQLRateLimitUsage() connector.GraphQLRateLimitUsage {
	c.flushUsageCalls++
	return c.usage
}
