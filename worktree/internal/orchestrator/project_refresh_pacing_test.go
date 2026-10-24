package orchestrator

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestProjectRefreshBudgetDoesNotSuppressLookups(t *testing.T) {
	t.Parallel()
	for _, projects := range []int{5, 10} {
		t.Run(strconv.Itoa(projects), func(t *testing.T) {
			now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
			reset := now.Add(time.Hour)
			for i := range projects {
				cfg := normalizeConfig(Config{Project: scheduler.ProjectCandidate{ID: strconv.Itoa(i)}, PollInterval: 30 * time.Second, GitHubRESTMinReserve: 1000, GitHubGraphQLMinReserve: 1000})
				tracker := &rateLimitConnector{restStatus: connector.RESTRateLimitUsage{HasRateLimit: true, RateLimit: connector.RESTRateLimit{Limit: 5000, Remaining: 500, ResetAt: reset}}}
				orch := newRateLimitTestOrchestrator(cfg, tracker)
				state := newState(cfg)
				state.RateLimits = &telemetry.RateLimits{GitHubREST: &telemetry.RateLimitBucket{Limit: 5000, Remaining: 500, ResetAt: &reset}}
				if orch.githubLookupBackoffGate(t.Context(), &state, now) {
					t.Fatalf("project %d suppressed at shared reserve floor", i)
				}
				if !githubLookupBackoffAllowsDispatch(&state, "") {
					t.Fatalf("project %d cannot dispatch", i)
				}
			}
		})
	}
}

func TestProjectRefreshCadence(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	reset := now.Add(time.Hour)
	tests := []struct {
		name      string
		remaining int64
		active    string
		expired   bool
		want      time.Duration
	}{
		{name: "healthy", remaining: 5000, want: 30 * time.Second},
		{name: "quiet at reserve", remaining: 1000, want: time.Minute},
		{name: "quiet low", remaining: 100, want: 5 * time.Minute},
		{name: "candidate", remaining: 100, active: "candidate", want: 30 * time.Second},
		{name: "running", remaining: 100, active: "running", want: 30 * time.Second},
		{name: "pending claim", remaining: 100, active: "claim", want: 30 * time.Second},
		{name: "deferred write", remaining: 100, active: "write", want: 30 * time.Second},
		{name: "reset", remaining: 100, expired: true, want: 30 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := normalizeConfig(Config{PollInterval: 30 * time.Second, GitHubRESTMinReserve: 1000})
			orch := newRateLimitTestOrchestrator(cfg, &rateLimitConnector{})
			state := newState(cfg)
			state.RateLimits = &telemetry.RateLimits{GitHubREST: &telemetry.RateLimitBucket{Limit: 5000, Remaining: tt.remaining, ResetAt: &reset}}
			switch tt.active {
			case "candidate":
				state.LaneSignalCandidates = []connector.Issue{{ID: "candidate"}}
			case "running":
				state.Running["running"] = Running{}
			case "claim":
				state.Claimed["claim"] = Claimed{}
			case "write":
				state.deferredCompletions["write"] = deferredCompletion{}
			}
			at := now
			if tt.expired {
				at = reset
			}
			orch.now = func() time.Time { return at }
			if got := orch.adaptivePollInterval(&state, orch.clockNow()); got != tt.want {
				t.Fatalf("interval = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestProjectRefreshColdFleetRequestCounts(t *testing.T) {
	t.Parallel()
	for _, projects := range []int{5, 10} {
		t.Run(strconv.Itoa(projects), func(t *testing.T) {
			now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
			reset := now.Add(time.Hour)
			states := make([]State, projects)
			trackers := make([]*rateLimitConnector, projects)
			fleet := make([]*Orchestrator, projects)
			for i := range fleet {
				cfg := normalizeConfig(Config{Project: scheduler.ProjectCandidate{ID: strconv.Itoa(i)}, PollInterval: 30 * time.Second, GitHubRESTMinReserve: 1000})
				usage := connector.RESTRateLimitUsage{HasRateLimit: true, RateLimit: connector.RESTRateLimit{Limit: 5000, Remaining: 100, ResetAt: reset}}
				trackers[i] = &rateLimitConnector{restStatus: usage, restUsage: usage}
				states[i] = newState(cfg)
				fleet[i] = newRateLimitTestOrchestrator(cfg, trackers[i])
				fleet[i].now = func() time.Time { return now }
			}
			// Drive each project's real tick and next-refresh timer using one injected
			// clock. Cold projects each get a read; no project consumes another's turn.
			for ; now.Before(reset); now = now.Add(30 * time.Second) {
				for i, orch := range fleet {
					state := &states[i]
					if state.NextRefreshAt.After(now) {
						continue
					}
					orch.tick(t.Context(), state, orch.clockNow())
					if state.LastRefreshError != "" {
						t.Fatalf("project %d stale: %s", i, state.LastRefreshError)
					}
					if _, _, ok := githubLookupBackoff(state.BackendOutages); ok {
						t.Fatalf("project %d globally suppressed", i)
					}
				}
			}
			total := 0
			for i, orch := range fleet {
				if got := trackers[i].fetchCandidateCalls; got != 12 {
					t.Fatalf("project %d: %d candidate reads in hour, want 12", i, got)
				}
				total += trackers[i].fetchCandidateCalls
				orch.tick(t.Context(), &states[i], orch.clockNow())
				if states[i].PollInterval != 30*time.Second {
					t.Fatalf("project %d did not recover at reset: %s", i, states[i].PollInterval)
				}
			}
			t.Logf("%d cold projects: %d candidate refreshes/hour, versus %d at base cadence", projects, total, projects*120)
		})
	}
}

// Every project has two permits and one candidate: quiet-project pacing must
// neither starve its fresh candidate nor suppress the dispatch transition write.
func TestProjectRefreshActiveFleetDispatch(t *testing.T) {
	t.Parallel()
	for _, projects := range []int{5, 10} {
		t.Run(strconv.Itoa(projects), func(t *testing.T) {
			now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
			reset := now.Add(time.Hour)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			for i := range projects {
				now = reset.Add(-time.Hour)
				cfg := normalizeConfig(Config{Project: scheduler.ProjectCandidate{ID: strconv.Itoa(i)}, PollInterval: 30 * time.Second, MaxConcurrentAgents: 2, ActiveStates: []string{"Todo", "In Progress"}, GitHubRESTMinReserve: 1000})
				issue := connector.Issue{ID: strconv.Itoa(i), Identifier: fmt.Sprintf("example/repo#%d", i+1), Title: "fresh candidate", State: "Todo", AssignedToWorker: true}
				tracker := &pacingDispatchConnector{rateLimitConnector: &rateLimitConnector{issuesByID: []connector.Issue{issue}, restStatus: connector.RESTRateLimitUsage{HasRateLimit: true, RateLimit: connector.RESTRateLimit{Limit: 5000, Remaining: 100, ResetAt: reset}}}}
				orch := newRateLimitTestOrchestrator(cfg, tracker)
				orch.supervisor = newTestSupervisor(t, parityBlockingRunner{}, cfg)
				orch.runResults = make(chan runpkg.Completion, 1)
				orch.now = func() time.Time { return now }
				state := newState(cfg)
				state.RateLimits = &telemetry.RateLimits{GitHubREST: &telemetry.RateLimitBucket{Limit: 5000, Remaining: 100, ResetAt: &reset}}
				orch.tick(ctx, &state, now)
				if state.NextRefreshAt.Sub(now) != 5*time.Minute {
					t.Fatalf("quiet refresh delay = %s", state.NextRefreshAt.Sub(now))
				}
				tracker.candidates = []connector.Issue{issue}
				now = state.NextRefreshAt
				orch.tick(ctx, &state, now)
				if len(state.Running) != 1 {
					t.Fatalf("project %d running = %d, decisions = %+v", i, len(state.Running), state.SchedulerDecisions)
				}
				if len(tracker.transitions) != 1 || tracker.transitions[0] != "In Progress" {
					t.Fatalf("project %d writes = %v", i, tracker.transitions)
				}
				if state.PollInterval != cfg.PollInterval {
					t.Fatalf("project %d active cadence = %s", i, state.PollInterval)
				}
			}
		})
	}
}

type pacingDispatchConnector struct {
	*rateLimitConnector
	transitions []string
}

func (c *pacingDispatchConnector) UpdateIssueState(_ context.Context, _ string, state string) error {
	c.transitions = append(c.transitions, state)
	return nil
}
