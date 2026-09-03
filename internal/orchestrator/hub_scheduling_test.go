package orchestrator

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestHubSchedulingCycle(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	issue := connector.NewIssue()
	issue.ID = "I_hub"
	issue.Identifier = "acme/widgets#17"
	issue.Number = 17
	issue.Title = "Hub scheduled"
	issue.State = "Todo"
	issue.URL = "https://github.com/acme/widgets/issues/17"
	issue.Fields["detent_hub_work_item_id"] = "42"

	tests := []struct {
		name        string
		fetchError  error
		githubPause bool
		wantRunning bool
		wantDegrade bool
	}{
		{name: "Hub dispatches without connector reads", githubPause: true, wantRunning: true},
		{name: "Hub outage degrades without spending work budgets", fetchError: errors.Join(ErrSchedulingUnavailable, errors.New("Hub unavailable")), wantDegrade: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			trackerBackend := &hubSchedulingConnector{}
			scheduling := &hubSchedulingSource{issue: issue, fetchError: test.fetchError}
			runner := &hubSchedulingRunner{started: make(chan struct{}, 1)}
			cfg := normalizeConfig(Config{
				PollInterval: 30 * time.Second, MaxConcurrentAgents: 1,
				DispatchPriorityByState: []string{"Todo"}, TerminalStates: []string{"Done"},
				Project: schedulerProjectCandidate("widgets"), SchedulingRepository: "acme/widgets",
			})
			orch, err := New(cfg, Dependencies{Connector: trackerBackend, Scheduling: scheduling, Runner: runner, Now: func() time.Time { return now }})
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			state := newState(cfg)
			if test.githubPause {
				resetAt := now.Add(10 * time.Minute)
				state.RateLimits = &telemetry.RateLimits{GitHubGraphQL: &telemetry.RateLimitBucket{Remaining: 0, Limit: 5000, Used: 5000, ResetAt: &resetAt}}
			}
			state.Retry["preserved"] = Retry{Attempt: 3, DueAt: now.Add(time.Hour)}
			state.InstantFailures["preserved"] = InstantFailure{Count: 2}
			state.RepeatedFailures["preserved"] = RepeatedFailure{Count: 2}
			state.FailureBreaker.Failures["existing"] = []ProjectFailure{{IssueID: "preserved", At: now.Add(-time.Minute)}}
			beforeRetry := state.Retry["preserved"]
			beforeInstant := state.InstantFailures["preserved"]
			beforeRepeated := state.RepeatedFailures["preserved"]
			beforeBreaker := cloneProjectFailureBreaker(state.FailureBreaker)

			orch.tick(t.Context(), &state, now)

			if candidates, ids := trackerBackend.candidateReads.Load(), trackerBackend.idReads.Load(); candidates != 0 || ids != 0 {
				t.Fatalf("scheduling-time connector reads = candidates %d ids %d, want zero", candidates, ids)
			}
			_, running := state.Running[issue.ID]
			if running != test.wantRunning {
				t.Fatalf("running = %t, want %t", running, test.wantRunning)
			}
			if test.wantRunning && (scheduling.fetches != 1 || scheduling.adoptions != 1 || scheduling.releases != 0) {
				t.Fatalf("Hub scheduling calls = fetch %d adopt %d release %d", scheduling.fetches, scheduling.adoptions, scheduling.releases)
			}
			if test.githubPause && state.PollInterval != cfg.PollInterval {
				t.Fatalf("Hub poll interval = %s, want %s despite GitHub pause", state.PollInterval, cfg.PollInterval)
			}
			if test.wantDegrade {
				if !strings.Contains(state.LastRefreshError, "Hub unavailable") || !state.Snapshot(now).Refresh.Degraded() {
					t.Fatalf("refresh = %#v", state.Snapshot(now).Refresh)
				}
				if !reflect.DeepEqual(state.Retry["preserved"], beforeRetry) || !reflect.DeepEqual(state.InstantFailures["preserved"], beforeInstant) || !reflect.DeepEqual(state.RepeatedFailures["preserved"], beforeRepeated) || !reflect.DeepEqual(state.FailureBreaker, beforeBreaker) {
					t.Fatalf("outage changed work budgets: retry=%#v instant=%#v repeated=%#v breaker=%#v", state.Retry, state.InstantFailures, state.RepeatedFailures, state.FailureBreaker)
				}
				if state.PollInterval <= cfg.PollInterval {
					t.Fatalf("outage poll interval = %s, want backoff above %s", state.PollInterval, cfg.PollInterval)
				}
			}
			for id := range state.Running {
				orch.cancelRunning(&state, id)
			}
		})
	}
}

func TestHubSchedulingHeartbeatPreservesClaimedIssue(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	issue := connector.NewIssue()
	issue.ID = "I_hub"
	issue.Identifier = "acme/widgets#17"
	issue.Title = "Hub scheduled"
	issue.Labels = []string{"detent:in-progress"}
	issue.Assignees = []string{"worker-a"}
	issue.BlockedBy = []connector.BlockedRef{{ID: "I_blocker"}}
	issue.Fields["effort"] = "high"
	renewedIssue := connector.Issue{ID: issue.ID}
	renewed := Claimed{Issue: renewedIssue, Owner: "machine-a", ClaimedAt: now.Add(-time.Minute), LeaseRenewedAt: now, LeaseExpiresAt: now.Add(90 * time.Second)}
	cfg := normalizeConfig(Config{PollInterval: 30 * time.Second, MaxConcurrentAgents: 1, Project: schedulerProjectCandidate("widgets")})
	manager := newHeartbeatManager(cfg, nil, nil, func() time.Time { return now }, nil, &hubSchedulingSource{})
	manager.upsert(heartbeatTarget{issueID: issue.ID, claimOwner: "machine-a"})
	manager.mu.Lock()
	sequence := manager.targets[issue.ID].sequence
	manager.mu.Unlock()
	state := newState(cfg)
	state.Running[issue.ID] = Running{Issue: issue}
	state.Claimed[issue.ID] = Claimed{Issue: issue, Owner: "machine-a"}
	orch := &Orchestrator{cfg: cfg, heartbeats: manager}

	orch.handleHeartbeatResult(&state, heartbeatResult{issueID: issue.ID, sequence: sequence, claimRenewed: true, claim: renewed, claimIssue: renewedIssue})

	claim := state.Claimed[issue.ID]
	if claim.Issue.Title != issue.Title || !reflect.DeepEqual(claim.Issue.Labels, issue.Labels) ||
		!reflect.DeepEqual(claim.Issue.Assignees, issue.Assignees) || !reflect.DeepEqual(claim.Issue.BlockedBy, issue.BlockedBy) ||
		!reflect.DeepEqual(claim.Issue.Fields, issue.Fields) || claim.Owner != "machine-a" || !claim.LeaseExpiresAt.Equal(renewed.LeaseExpiresAt) {
		t.Fatalf("renewed claim = %#v", claim)
	}
}

type hubSchedulingSource struct {
	issue      connector.Issue
	fetchError error
	fetches    int
	adoptions  int
	releases   int
}

func (s *hubSchedulingSource) HeartbeatInterval() time.Duration {
	return 30 * time.Second
}

func (s *hubSchedulingSource) FetchCandidateIssues(_ context.Context, request SchedulingRequest) ([]connector.Issue, error) {
	s.fetches++
	if request.Repository != "acme/widgets" {
		return nil, errors.New("unexpected repository")
	}
	if s.fetchError != nil {
		return nil, s.fetchError
	}
	return []connector.Issue{s.issue}, nil
}

func (s *hubSchedulingSource) AdoptClaim(_ context.Context, issue connector.Issue, now time.Time) (Claimed, error) {
	s.adoptions++
	return Claimed{Issue: issue, ClaimedAt: now, LeaseRenewedAt: now, LeaseExpiresAt: now.Add(90 * time.Second), Owner: "machine-a"}, nil
}

func (s *hubSchedulingSource) RenewClaim(_ context.Context, _ string, _ time.Time) (Claimed, error) {
	return Claimed{}, nil
}

func (s *hubSchedulingSource) ReleaseClaim(_ context.Context, _ string, _ string) error {
	s.releases++
	return nil
}

type hubSchedulingConnector struct {
	reads          atomic.Int64
	candidateReads atomic.Int64
	stateReads     atomic.Int64
	idReads        atomic.Int64
	writes         atomic.Int64
}

func (c *hubSchedulingConnector) Name() string { return "github" }

func (c *hubSchedulingConnector) FetchCandidateIssues(context.Context) ([]connector.Issue, error) {
	c.reads.Add(1)
	c.candidateReads.Add(1)
	return nil, nil
}

func (c *hubSchedulingConnector) FetchIssuesByStates(context.Context, []string) ([]connector.Issue, error) {
	c.reads.Add(1)
	c.stateReads.Add(1)
	return nil, nil
}

func (c *hubSchedulingConnector) FetchIssueStatesByIDs(context.Context, []string) ([]connector.Issue, error) {
	c.reads.Add(1)
	c.idReads.Add(1)
	return nil, nil
}

func (c *hubSchedulingConnector) CombinedRefreshEnabled() bool { return true }

func (c *hubSchedulingConnector) FetchRefreshIssues(context.Context, []string, []string, connector.IssueFilterHint) connector.RefreshIssueResult {
	c.reads.Add(1)
	c.candidateReads.Add(1)
	return connector.RefreshIssueResult{}
}

func (c *hubSchedulingConnector) CreateComment(context.Context, string, string) error {
	c.writes.Add(1)
	return nil
}

func (c *hubSchedulingConnector) UpdateIssueState(context.Context, string, string) error {
	c.writes.Add(1)
	return nil
}

func (c *hubSchedulingConnector) SetAssignee(context.Context, string, string) error {
	c.writes.Add(1)
	return nil
}

func (c *hubSchedulingConnector) SetField(context.Context, string, string, string) error {
	c.writes.Add(1)
	return nil
}

type hubSchedulingRunner struct {
	started chan struct{}
}

func (r *hubSchedulingRunner) Run(ctx context.Context, _ RunRequest) (RunResult, error) {
	select {
	case r.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return RunResult{}, ctx.Err()
}

func schedulerProjectCandidate(id string) scheduler.ProjectCandidate {
	return scheduler.ProjectCandidate{ID: id, Weight: 1}
}
