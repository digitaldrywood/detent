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

	if tracker.resetUsageCalls != 0 || tracker.flushUsageCalls != 1 {
		t.Fatalf("usage calls = reset %d flush %d, want reset 0 flush 1", tracker.resetUsageCalls, tracker.flushUsageCalls)
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
				{EndpointFamily: "label issues", Count: 1, Remaining: 4879, Limit: 5000, Resource: "core"},
				{EndpointFamily: "issue comments", Count: 1, Remaining: 4878, Limit: 5000, Resource: "core", RateLimited: true},
			},
			TotalRequests: 2,
			RateLimited:   true,
			BackoffUntil:  now.Add(time.Minute),
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
	if state.RateLimits.RESTUsage.TotalRequests != 2 || !state.RateLimits.RESTUsage.RateLimited {
		t.Fatalf("RESTUsage = %#v, want 2 requests and rate limited", state.RateLimits.RESTUsage)
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
