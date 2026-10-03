package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/backendcapacity"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/policy"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/selector"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHubSchedulingCycle(t *testing.T) {
	now := time.Date(2026, 10, 2, 1, 28, 19, 0, time.UTC)
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
		native      bool
		restPause   bool
		expired     bool
		gitLanding  bool
		interrupted bool
		mergeOnly   bool
		slots       int
		batchSize   int
		empty       bool
		repeats     int
	}{
		{name: "Hub dispatches without connector reads", githubPause: true, wantRunning: true},
		{name: "Hub outage degrades without spending work budgets", fetchError: errors.Join(ErrSchedulingUnavailable, errors.New("Hub unavailable")), wantDegrade: true},
		{name: "native REST-held 137 leaves one-candidate boundary for 138", native: true, restPause: true, wantRunning: true},
		{name: "native merge-only REST wait makes zero claims", native: true, restPause: true, mergeOnly: true},
		{name: "expired REST wait restores native merging", native: true, restPause: true, expired: true, mergeOnly: true, wantRunning: true},
		{name: "native Git landing stays eligible during REST wait", native: true, restPause: true, gitLanding: true, mergeOnly: true, wantRunning: true},
		{name: "non-native REST safety stays active", restPause: true},
		{name: "one refresh starts six scheduled candidates", slots: 6, batchSize: 6, wantRunning: true},
		{name: "unselected batch claims are released", slots: 2, batchSize: 6, wantRunning: true},
		{name: "full capacity refreshes stay healthy", native: true, empty: true, repeats: 4},
		{name: "partial capacity batch stays healthy", slots: 6, batchSize: 2, wantRunning: true, repeats: 4},
		{name: "native interrupted Code claim without local retry", native: true, interrupted: true, wantRunning: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			trackerBackend := &hubSchedulingConnector{}
			scheduling := &hubSchedulingSource{issue: issue, fetchError: test.fetchError}
			selected := issue
			if test.batchSize > 0 {
				for i := range test.batchSize {
					candidate := cloneIssue(issue)
					candidate.ID = fmt.Sprintf("scheduled-%d", i)
					candidate.Identifier = fmt.Sprintf("acme/widgets#%d", i+20)
					scheduling.issues = append(scheduling.issues, candidate)
				}
				selected = scheduling.issues[0]
			}
			var backend connector.Connector = trackerBackend
			if test.native {
				backend = &nativeHubSchedulingConnector{trackerBackend}
				merge := dispatchTestIssue("wi_137", "Merging")
				merge.Fields["detent_hub_work_item_id"] = "137"
				merge.Priority = new(1)
				coding := cloneIssue(issue)
				coding.ID, coding.Identifier = "wi_138", "native#138"
				coding.Priority = new(2)
				scheduling.issues = []connector.Issue{merge, coding}
				selected = coding
				if test.mergeOnly {
					selected = merge
				}
			}
			runner := &hubSchedulingRunner{started: make(chan struct{}, 1)}
			if test.interrupted {
				selected.State = "In Progress"
				scheduling.issues = []connector.Issue{selected}
				scheduling.execution = &nativeLandingJourneyExecution{}
				runner.requests = make(chan RunRequest, 1)
			}
			cfg := normalizeConfig(Config{
				PollInterval: 30 * time.Second, MaxConcurrentAgents: max(1, test.slots),
				DispatchPriorityByState: []string{"Merging", "Rework", "Todo"}, DispatchPriorityByLabel: []string{"hotfix", "bug"}, PrioritizeUnblockers: true, TerminalStates: []string{"Done"},
				Project: schedulerProjectCandidate("widgets"), SchedulingRepository: "acme/widgets",
			})
			if test.native {
				cfg.Policy = policy.Descriptor{Gates: policy.Gates{GitHubPullRequest: !test.gitLanding}}
			}
			if test.interrupted {
				cfg.ActiveStates = []string{"Todo", "In Progress", "Merging"}
			}
			if test.mergeOnly {
				cfg.ActiveStates = []string{"Merging"}
			}
			if test.empty {
				scheduling.issues = []connector.Issue{}
			}
			orch, err := New(cfg, Dependencies{Connector: backend, Scheduling: scheduling, Runner: runner, Now: func() time.Time { return now }})
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			state := newState(cfg)
			if test.restPause {
				resume := time.Date(2026, 10, 2, 1, 53, 59, 0, time.UTC)
				if test.expired {
					resume = now
				}
				state.BackendOutages["github"] = BackendOutage{Kind: githubRESTCapacityKind, ResumeAt: resume}
			}
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
			if test.interrupted {
				select {
				case admitted := <-runner.requests:
					if admitted.Execution != scheduling.execution || admitted.RetryMode != "" || admitted.ResumeState != (store.AgentResumeState{}) || admitted.Issue.ID != selected.ID {
						t.Fatalf("native claim lost recovery owner or seeded local continuation: %+v", admitted)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("interrupted Code did not reach its native execution owner")
				}
			}
			request := scheduling.request
			if candidates, ids := trackerBackend.candidateReads.Load(), trackerBackend.idReads.Load(); candidates != 0 || ids != 0 {
				t.Fatalf("scheduling-time connector reads = candidates %d ids %d, want zero", candidates, ids)
			}
			if test.repeats > 0 {
				scheduling.issues = []connector.Issue{}
				tickAt := now
				for range test.repeats {
					tickAt = tickAt.Add(cfg.PollInterval)
					orch.tick(t.Context(), &state, tickAt)
					source := state.RefreshSources[telemetry.RefreshSourceCandidates]
					if source.FailureStreak != 0 || source.Condition != "" || source.LastError != "" || state.LastRefreshError != "" || state.Snapshot(tickAt).Refresh.Degraded() || state.PollInterval != cfg.PollInterval {
						t.Fatalf("healthy capacity refresh degraded: source=%+v interval=%s error=%s", source, state.PollInterval, state.LastRefreshError)
					}
				}
			}
			if scheduling.fetches != 0 && (!reflect.DeepEqual(request.DispatchPriorityByState, cfg.DispatchPriorityByState) || !reflect.DeepEqual(request.DispatchPriorityByLabel, cfg.DispatchPriorityByLabel) || !request.PrioritizeUnblockers || request.CandidateLimit != cfg.MaxConcurrentAgents+8 || request.AdmissionLimit != cfg.MaxConcurrentAgents || request.CandidateReady == nil || request.CandidateAdmitted == nil) {
				t.Fatalf("scheduling request lost configured ranking/readiness: %+v", request)
			}
			if test.native && test.mergeOnly && test.restPause && !test.expired && !test.gitLanding && scheduling.fetches != 0 {
				t.Fatal("all states held still requested a native claim")
			}
			if test.wantRunning && request.CandidateReady(t.Context(), selected) {
				t.Fatal("running work remained ready for a new native claim")
			}

			_, running := state.Running[selected.ID]
			if running != test.wantRunning {
				t.Fatalf("running = %t, want %t", running, test.wantRunning)
			}
			wantAdoptions, wantReleases := 1, 0
			if test.batchSize > 0 {
				wantAdoptions = min(test.batchSize, cfg.MaxConcurrentAgents)
				wantReleases = test.batchSize - wantAdoptions
				if len(state.Running) != wantAdoptions {
					t.Fatalf("refresh started %d attempts, want %d", len(state.Running), wantAdoptions)
				}
			}
			if test.wantRunning && (scheduling.fetches != 1+test.repeats || scheduling.adoptions != wantAdoptions || scheduling.releases != wantReleases) {
				t.Fatalf("Hub scheduling calls = fetch %d adopt %d release %d", scheduling.fetches, scheduling.adoptions, scheduling.releases)
			}
			if test.native && test.restPause && !test.expired && !test.gitLanding {
				if stateIn("Merging", request.WorkflowStates) || scheduling.issues[0].State != "Merging" || *scheduling.issues[0].Priority != 1 {
					t.Fatal("REST wait changed native head authority or left it eligible upstream")
				}
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

func TestHubSchedulingPreservesOverloadRetryAcrossAdmissions(t *testing.T) {
	for _, test := range []struct {
		name          string
		empty         bool
		unobserved    bool
		statusError   error
		observedState string
		closed        bool
		unauthorized  bool
		wantReleased  bool
		nonNative     bool
		spillover     bool
	}{
		{name: "unrelated newly leased work"},
		{name: "native operator resume survives host spillover", spillover: true},
		{name: "non-native bounded admission", nonNative: true},
		{name: "healthy empty host capacity batch", empty: true},
		{name: "missing status is not invalidation", empty: true, unobserved: true},
		{name: "failed status read is not invalidation", empty: true, statusError: errors.New("status unavailable")},
		{name: "terminal observation", empty: true, observedState: "Done", wantReleased: true},
		{name: "closed observation", empty: true, closed: true, wantReleased: true},
		{name: "deliberately stopped observation", empty: true, observedState: "Human Review", wantReleased: true},
		{name: "authorization invalidation", empty: true, unauthorized: true, wantReleased: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			now := time.Date(2026, 10, 2, 14, 4, 18, 0, time.UTC)
			issue := dispatchTestIssue("wi_181", "In Progress")
			issue.Identifier = "native#181"
			issue.Priority = new(1)
			issue.Labels = []string{"authorized"}
			issue.Fields["detent_hub_work_item_id"] = issue.ID
			ordinary := dispatchTestIssue("wi_177", "Todo")
			ordinary.Priority = new(3)
			ordinary.Labels = []string{"authorized"}
			ordinary.Fields["detent_hub_work_item_id"] = ordinary.ID
			observed := cloneIssue(issue)
			if test.observedState != "" {
				observed.State = test.observedState
			}
			observed.Closed = test.closed
			if test.unauthorized {
				observed.Labels = nil
			}
			tracker := &hubSchedulingConnector{statuses: []connector.Issue{observed}, statusError: test.statusError}
			if test.unobserved {
				tracker.statuses = nil
			}
			source := &hubSchedulingSource{issues: []connector.Issue{ordinary}}
			if test.empty {
				source.issues = []connector.Issue{}
			}
			runner := &hubSchedulingRunner{requests: make(chan RunRequest, 2)}
			cfg := normalizeConfig(Config{
				MaxConcurrentAgents: 1, PollInterval: 30 * time.Second, OverloadRetryDelay: 45 * time.Second,
				ActiveStates: []string{"Todo", "In Progress"}, TerminalStates: []string{"Done", "Cancelled"},
				ObservedStates: []string{"In Progress", "Done", "Human Review"},
				Authorization:  selector.Selector{Labels: selector.Labels{Include: []string{"authorized"}}},
				Project:        schedulerProjectCandidate("widgets"), SchedulingRepository: "acme/widgets",
				Policy: policy.Descriptor{ID: "original-policy"},
			})
			if test.spillover {
				cfg.WorkerHosts = []string{"local"}
			}
			var backend connector.Connector = &nativeHubSchedulingConnector{tracker}
			if test.nonNative {
				backend = tracker
			}
			o, err := New(cfg, Dependencies{Connector: backend, Scheduling: source, Runner: runner, Now: func() time.Time { return now }, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
			if err != nil {
				t.Fatal(err)
			}
			state := newState(o.cfg)
			t.Cleanup(func() {
				for id := range state.Running {
					o.cancelRunning(&state, id)
				}
			})
			state.Running[issue.ID] = Running{Issue: issue, Mode: runpkg.RunModeImplement, Attempt: 7, TurnCount: 1, StartedAt: now.Add(-time.Minute)}
			state.Claimed[issue.ID] = Claimed{Issue: issue, ClaimedAt: now.Add(-time.Minute)}
			scope := backendcapacity.Scope{BackendID: "codex", BackendKind: "codex", Provider: "openai"}
			o.handleRunResult(t.Context(), &state, runpkg.Completion{
				IssueID: issue.ID, Request: RunRequest{Issue: issue, Attempt: 7}, CompletedAt: now,
				Err:       backendcapacity.NewError(scope, backendcapacity.Details{Type: backendcapacity.ErrorTypeTransientOverload, Kind: "serverOverloaded"}, errors.New("selected model at capacity")),
				Retryable: true, RetryAttempt: 7, RetryDelay: 45 * time.Second,
			})
			retry, exists := state.Retry[issue.ID]
			if !exists || retry.Attempt != 7 || retry.Error != "transient_overload" {
				t.Fatalf("overload retry = %+v, exists=%t", retry, exists)
			}
			retry.RecoveryAttemptID = 294
			retry.RetryMode = runpkg.RetryModeResume
			retry.ResumeState = store.AgentResumeState{DetentSessionID: 181, ProviderSessionID: "original-provider-session", ProviderThreadID: "original-thread", AgentBackendID: "codex", AgentBackendKind: "codex", AgentRole: "code"}
			state.Retry[issue.ID] = retry
			claim, hadClaim := state.Claimed[issue.ID]
			now = retry.DueAt.Add(time.Minute)
			preview := state.clone()
			previewRetry, previewClaim := preview.Retry[issue.ID], preview.Claimed[issue.ID]
			o.dispatchPlanner().plan(&preview, source.issues, now, dispatchPlanHooks{})
			if !reflect.DeepEqual(preview.Retry[issue.ID], previewRetry) || !reflect.DeepEqual(preview.Claimed[issue.ID], previewClaim) {
				t.Error("bounded admission planning discarded retry or continuation ownership")
			}
			o.tick(t.Context(), &state, now)
			_, retained := state.Retry[issue.ID]
			_, claimed := state.Claimed[issue.ID]
			if retained == test.wantReleased || claimed != (hadClaim && !test.wantReleased) {
				t.Fatalf("retry retained=%t claimed=%t, want released=%t", retained, claimed, test.wantReleased)
			}
			if len(state.InstantFailures) != 0 || len(state.RepeatedFailures) != 0 || len(state.BackendOutages) != 0 {
				t.Fatal("provider overload spent issue failure allowance or created a backend outage")
			}
			if test.wantReleased {
				return
			}
			if !reflect.DeepEqual(state.Retry[issue.ID], retry) || !reflect.DeepEqual(state.Claimed[issue.ID], claim) {
				t.Fatal("tick cleanup changed retained retry or exact continuation")
			}
			if sourceHealth := state.RefreshSources[telemetry.RefreshSourceCandidates]; sourceHealth.FailureStreak != 0 || sourceHealth.LastError != "" || state.PollInterval != cfg.PollInterval {
				t.Fatalf("capacity deferral changed candidate health or cadence: %+v interval=%s", sourceHealth, state.PollInterval)
			}
			if !test.empty {
				o.cancelRunning(&state, ordinary.ID)
				delete(state.Running, ordinary.ID)
				delete(state.Claimed, ordinary.ID)
				select {
				case <-runner.requests:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			source.issues = []connector.Issue{ordinary, issue}
			o.refillProjectSlots(t.Context(), &state, now)
			wantAdoptions := 1
			if !test.empty {
				wantAdoptions++
			}
			if running, ok := state.Running[issue.ID]; !ok || running.Attempt != retry.Attempt || source.adoptions != wantAdoptions {
				t.Fatalf("retry did not resume through existing claim owner: running=%t attempt=%d adoptions=%d", ok, running.Attempt, source.adoptions)
			}
			if !reflect.DeepEqual(source.request.Policy, cfg.Policy) {
				t.Fatal("retry admission changed the original policy boundary")
			}
			if _, running := state.Running[ordinary.ID]; running {
				t.Fatal("ordinary new work started ahead of eligible urgent retry")
			}
			select {
			case request := <-runner.requests:
				if request.Issue.ID != issue.ID || request.Attempt != retry.Attempt || request.RecoveryAttemptID != retry.RecoveryAttemptID || request.RetryMode != retry.RetryMode || !reflect.DeepEqual(request.ResumeState, retry.ResumeState) {
					t.Fatalf("resumed request changed continuation: %+v", request)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
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
	admission    tracker.NativeAdmissionContext
	execution    runpkg.Execution
	issues       []connector.Issue
	request      SchedulingRequest
	issue        connector.Issue
	fetchError   error
	releaseError error
	fetches      int
	adoptions    int
	releases     int
}

func (s *hubSchedulingSource) ObserveNativeAdmission(_ string, current tracker.NativeAdmissionContext) {
	s.admission = current
}

func (s *hubSchedulingSource) RunExecution(string) runpkg.Execution { return s.execution }

func (s *hubSchedulingSource) HeartbeatInterval() time.Duration {
	return 30 * time.Second
}

func (s *hubSchedulingSource) FetchCandidateIssues(_ context.Context, request SchedulingRequest) ([]connector.Issue, error) {
	s.fetches++
	s.request = request
	if request.Repository != "acme/widgets" {
		return nil, errors.New("unexpected repository")
	}
	if s.fetchError != nil {
		return nil, s.fetchError
	}
	if s.issues != nil {
		var candidates []connector.Issue
		for _, issue := range s.issues {
			if stateIn(issue.State, request.WorkflowStates) {
				candidates = append(candidates, issue)
			}
		}
		return candidates, nil
	}
	return []connector.Issue{s.issue}, nil
}

type nativeHubSchedulingConnector struct {
	*hubSchedulingConnector
}

func (c *nativeHubSchedulingConnector) WorkflowStates(context.Context) ([]connector.WorkflowState, error) {
	return []connector.WorkflowState{{Name: "Todo"}, {Name: "Merging"}}, nil
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
	return s.releaseError
}

type hubSchedulingConnector struct {
	statuses       []connector.Issue
	statusError    error
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

func (c *hubSchedulingConnector) FetchRefreshIssues(_ context.Context, candidates []string, _ []string, _ connector.IssueFilterHint) connector.RefreshIssueResult {
	c.reads.Add(1)
	if candidates != nil {
		c.candidateReads.Add(1)
	}
	return connector.RefreshIssueResult{Statuses: cloneIssues(c.statuses), StatusError: c.statusError}
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
	started  chan struct{}
	requests chan RunRequest
}

func (r *hubSchedulingRunner) Run(ctx context.Context, request RunRequest) (RunResult, error) {
	if r.requests != nil {
		r.requests <- request
	}
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

func TestHubRefillRetainsNewClaims(t *testing.T) {
	for _, tt := range []struct {
		name                     string
		excluded                 bool
		state                    string
		wantRunning, wantRelease int
	}{
		{name: "new claim", state: "Todo", wantRunning: 1},
		{name: "excluded claim", state: "Todo", excluded: true, wantRelease: 1},
		{name: "ineligible claim", state: "Done", wantRelease: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
			cfg := normalizeConfig(Config{MaxConcurrentAgents: 1, ActiveStates: []string{"Todo"}, TerminalStates: []string{"Done"}, Project: schedulerProjectCandidate("widgets"), SchedulingRepository: "acme/widgets"})
			issue := dispatchTestIssue("new-claim", tt.state)
			issue.Fields["detent_hub_work_item_id"] = "42"
			source := &hubSchedulingSource{issue: issue}
			tracker := &hubSchedulingConnector{}
			o := Orchestrator{cfg: cfg, connector: tracker, scheduling: source, supervisor: newTestSupervisor(t, FakeRunner{}, cfg), runResults: make(chan runpkg.Completion, 1)}
			state := newState(cfg)
			defer o.releaseRunningSlots(&state)
			excluded := ""
			if tt.excluded {
				excluded = issue.ID
			}
			o.refillProjectSlotsExcluding(t.Context(), &state, now, excluded)
			if len(state.Running) != tt.wantRunning || source.releases != tt.wantRelease {
				t.Fatalf("running=%d released=%d, want %d/%d", len(state.Running), source.releases, tt.wantRunning, tt.wantRelease)
			}
			if source.adoptions != tt.wantRunning {
				t.Fatalf("adoptions=%d, want %d", source.adoptions, tt.wantRunning)
			}
			if tracker.candidateReads.Load() != 0 {
				t.Fatal("Hub refill read tracker candidates")
			}
		})
	}
}

func TestHubSchedulingReadinessBeforeClaim(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name     string
		state    string
		setup    func(*State, connector.Issue)
		want     bool
		native   bool
		githubPR bool
	}{
		{name: "ready todo", state: "Todo", want: true},
		{name: "full project lookahead does not claim coding", state: "Todo", native: true, setup: func(s *State, _ connector.Issue) {
			s.Running["first"] = Running{Issue: dispatchTestIssue("first", "Todo")}
			s.Running["second"] = Running{Issue: dispatchTestIssue("second", "Todo")}
		}},
		{name: "merging state full", state: "Merging", native: true, setup: func(s *State, _ connector.Issue) {
			s.Running["other"] = Running{Issue: dispatchTestIssue("other", "Merging")}
		}},
		{name: "future rework retry", state: "Rework", setup: func(s *State, issue connector.Issue) {
			s.Retry[issue.ID] = Retry{Issue: issue, DueAt: now.Add(time.Hour)}
		}},
		{name: "due rework retry remains owned", state: "Rework", setup: func(s *State, issue connector.Issue) {
			s.Retry[issue.ID] = Retry{Issue: issue, DueAt: now.Add(-time.Minute), Attempt: 2}
		}, want: true},
		{name: "fresh native dependency cleared", state: "Todo", setup: func(s *State, issue connector.Issue) {
			s.Blocked[issue.ID] = Blocked{Issue: issue, Source: BlockedSourceDependency, Reason: blockedReasonDependency}
		}, want: true},
		{name: "running work never preempted", state: "Merging", setup: func(s *State, issue connector.Issue) {
			s.Running[issue.ID] = Running{Issue: issue, cancel: func() { t.Error("preview cancelled running work") }}
		}},
		{name: "REST-held native GitHub merging", state: "Merging", native: true, githubPR: true, setup: func(s *State, _ connector.Issue) {
			s.BackendOutages["github"] = BackendOutage{Kind: githubRESTCapacityKind, ResumeAt: now.Add(time.Hour)}
		}},
		{name: "native coding ignores unrelated REST wait", state: "Todo", native: true, githubPR: true, want: true, setup: func(s *State, _ connector.Issue) {
			s.BackendOutages["github"] = BackendOutage{Kind: githubRESTCapacityKind, ResumeAt: now.Add(time.Hour)}
		}},
		{name: "native Git landing ignores REST wait", state: "Merging", native: true, want: true, setup: func(s *State, _ connector.Issue) {
			s.BackendOutages["github"] = BackendOutage{Kind: githubRESTCapacityKind, ResumeAt: now.Add(time.Hour)}
		}},
		{name: "native due coding retry ignores REST wait", state: "Rework", native: true, githubPR: true, want: true, setup: func(s *State, issue connector.Issue) {
			s.BackendOutages["github"] = BackendOutage{Kind: githubRESTCapacityKind, ResumeAt: now.Add(time.Hour)}
			s.Retry[issue.ID] = Retry{Issue: issue, DueAt: now.Add(-time.Minute), Attempt: 2}
		}},
		{name: "native due coding retry ignores historical REST scope", state: "Rework", native: true, githubPR: true, want: true, setup: func(s *State, issue connector.Issue) {
			s.BackendOutages["github"] = BackendOutage{Kind: githubRESTCapacityKind, ResumeAt: now.Add(time.Hour)}
			s.Retry[issue.ID] = Retry{Issue: issue, DueAt: now.Add(-time.Minute), Attempt: 2, CapacityScope: githubRESTCapacityScope}
		}},
		{name: "native due Git landing retry ignores historical REST scope", state: "Merging", native: true, want: true, setup: func(s *State, issue connector.Issue) {
			s.BackendOutages["github"] = BackendOutage{Kind: githubRESTCapacityKind, ResumeAt: now.Add(time.Hour)}
			s.Retry[issue.ID] = Retry{Issue: issue, DueAt: now.Add(-time.Minute), Attempt: 2, CapacityScope: githubRESTCapacityScope}
		}},
		{name: "non-native due coding retry respects REST wait", state: "Rework", setup: func(s *State, issue connector.Issue) {
			s.BackendOutages["github"] = BackendOutage{Kind: githubRESTCapacityKind, ResumeAt: now.Add(time.Hour)}
			s.Retry[issue.ID] = Retry{Issue: issue, DueAt: now.Add(-time.Minute), Attempt: 2, CapacityScope: githubRESTCapacityScope}
		}},
		{name: "native due PR landing retry retains REST wait", state: "Merging", native: true, githubPR: true, setup: func(s *State, issue connector.Issue) {
			s.BackendOutages["github"] = BackendOutage{Kind: githubRESTCapacityKind, ResumeAt: now.Add(time.Hour)}
			s.Retry[issue.ID] = Retry{Issue: issue, DueAt: now.Add(-time.Minute), Attempt: 2}
		}},
		{name: "io pressure hold refuses readiness before a claim", state: "Merging", native: true, setup: func(s *State, issue connector.Issue) {
			s.IOPressure.DispatchHeld = true
		}},
		{name: "memory pressure hold refuses readiness before a claim", state: "Todo", native: true, setup: func(s *State, issue connector.Issue) {
			s.MemoryPressure.DispatchHeld = true
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := normalizeConfig(Config{MaxConcurrentAgents: 2, MaxConcurrentAgentsByState: map[string]int{"Merging": 1}, ActiveStates: []string{"Merging", "Rework", "Todo"}, TerminalStates: []string{"Done"}, Project: schedulerProjectCandidate("widgets"), SchedulingRepository: "acme/widgets"})
			issue := dispatchTestIssue("candidate", test.state)
			issue.DependencySource = connector.BlockedRefSourceNative
			source := &hubSchedulingSource{issue: issue}
			cfg.Policy.Gates.GitHubPullRequest = test.githubPR
			var backend connector.Connector = &hubSchedulingConnector{}
			if test.native {
				backend = &nativeHubSchedulingConnector{&hubSchedulingConnector{}}
			}
			o := Orchestrator{cfg: cfg, connector: backend, scheduling: source, now: func() time.Time { return now }}
			state := newState(cfg)
			if test.setup != nil {
				test.setup(&state, issue)
			}
			beforeRetry, hadRetry := state.Retry[issue.ID]
			beforeBlocked, hadBlocked := state.Blocked[issue.ID]
			beforeRunning := len(state.Running)
			if _, err := o.fetchCandidateIssuesForTick(t.Context(), &state); err != nil {
				t.Fatal(err)
			}
			available := o.dispatchPlanner().hardAvailableSlots(&state)
			if source.request.CandidateLimit != available+dispatchCandidateLookahead || source.request.AdmissionLimit != max(1, available) {
				t.Fatalf("readiness lost bounded lookahead: %+v", source.request)
			}
			if test.native && test.githubPR && test.state == "Merging" && stateIn("Merging", source.request.WorkflowStates) {
				t.Fatal("REST-held native PR landing remained in upstream states")
			}
			if ready := source.request.CandidateReady(t.Context(), issue); ready != test.want {
				t.Errorf("ready = %t, want %t", ready, test.want)
			}
			if test.want && test.native && test.state == "Merging" {
				source.request.CandidateAdmitted(issue)
				next := cloneIssue(issue)
				next.ID = "next-merge"
				if source.request.CandidateReady(t.Context(), next) {
					t.Fatal("batch ignored the occupied Merging slot")
				}
				next.ID, next.State = "independent", "Todo"
				if !source.request.CandidateReady(t.Context(), next) {
					t.Fatal("selected merge starved independent coding")
				}
			}
			afterRetry, hasRetry := state.Retry[issue.ID]
			afterBlocked, hasBlocked := state.Blocked[issue.ID]
			if hadRetry != hasRetry || hadBlocked != hasBlocked || !reflect.DeepEqual(afterRetry, beforeRetry) || !reflect.DeepEqual(afterBlocked, beforeBlocked) || len(state.Running) != beforeRunning {
				t.Fatal("pre-lease readiness mutated live scheduling state")
			}
			outage, paused := activeGitHubRESTCapacityOutage(&state, now)
			if !paused {
				return
			}
			if hadRetry {
				t.Run("retry admission", func(t *testing.T) {
					retryState := newState(cfg)
					test.setup(&retryState, issue)
					action, ready, reason := o.dispatchPlanner().retryAction(&retryState, issue, beforeRetry, now)
					if ready != test.want || !ready && reason != dispatchSkipGitHubRESTCapacity {
						t.Fatalf("retry ready = %t, reason = %q, want ready %t", ready, reason, test.want)
					}
					if ready {
						if action.attempt != beforeRetry.Attempt || action.retryState == nil || action.retryState.CapacityScope != beforeRetry.CapacityScope || !action.retryState.DueAt.Equal(beforeRetry.DueAt) {
							t.Fatalf("retry action lost historical ownership: %+v", action)
						}
					} else if retained := retryState.Retry[issue.ID]; retained.Attempt != beforeRetry.Attempt || retained.CapacityScope != beforeRetry.CapacityScope || !retained.DueAt.Equal(outage.ResumeAt) {
						t.Fatalf("REST-dependent retry lost wait evidence: %+v", retained)
					}
				})
			}
			for _, suppliedRetry := range []bool{false, true} {
				if suppliedRetry && !hadRetry {
					continue
				}
				t.Run("dispatch supplied retry="+strconv.FormatBool(suppliedRetry), func(t *testing.T) {
					dispatchState := newState(cfg)
					test.setup(&dispatchState, issue)
					o.supervisor = newTestSupervisor(t, FakeRunner{}, cfg)
					o.runResults = make(chan runpkg.Completion, 1)
					defer o.releaseRunningSlots(&dispatchState)
					var retry *Retry
					if suppliedRetry {
						retry = &beforeRetry
						delete(dispatchState.Retry, issue.ID)
					}
					outcome := o.dispatchIssueWithAdmission(t.Context(), &dispatchState, issue, beforeRetry.Attempt, now, "", o.dispatchPlanner().modelPermitRequiredAtDispatch(issue), retry)
					if outcome.dispatched != test.want || !outcome.dispatched && outcome.reason != dispatchIssueFailureGitHubRESTPaused {
						t.Fatalf("dispatch = %+v, want dispatched %t", outcome, test.want)
					}
					if !reflect.DeepEqual(dispatchState.BackendOutages, state.BackendOutages) {
						t.Fatal("dispatch changed shared REST outage evidence")
					}
				})
			}
		})
	}
}
