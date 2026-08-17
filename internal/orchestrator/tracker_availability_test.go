package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestTrackerAvailabilityRequiresCorrelatedEvidence(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 17, 17, 20, 0, 0, time.UTC)
	tests := []struct {
		name       string
		errors     []error
		wantActive bool
	}{
		{name: "single transient", errors: []error{trackerAvailabilityTestError("candidate_issues", connector.TrackerAvailabilityClassServer)}},
		{name: "correlated server failures", errors: []error{trackerAvailabilityTestError("candidate_issues", connector.TrackerAvailabilityClassServer), trackerAvailabilityTestError("candidate_issues", connector.TrackerAvailabilityClassServer)}, wantActive: true},
		{name: "correlated timeouts", errors: []error{trackerAvailabilityTestError("candidate_issues", connector.TrackerAvailabilityClassTimeout), trackerAvailabilityTestError("candidate_issues", connector.TrackerAvailabilityClassTimeout)}, wantActive: true},
		{name: "correlated transport failures", errors: []error{trackerAvailabilityTestError("candidate_issues", connector.TrackerAvailabilityClassTransport), trackerAvailabilityTestError("candidate_issues", connector.TrackerAvailabilityClassTransport)}, wantActive: true},
		{name: "different operations", errors: []error{trackerAvailabilityTestError("candidate_issues", connector.TrackerAvailabilityClassServer), trackerAvailabilityTestError("observed_status", connector.TrackerAvailabilityClassServer)}},
		{name: "different credential identities", errors: []error{trackerAvailabilityTestErrorWithCredential("candidate_issues", connector.TrackerAvailabilityClassServer, "github-rest:first"), trackerAvailabilityTestErrorWithCredential("candidate_issues", connector.TrackerAvailabilityClassServer, "github-rest:second")}},
		{name: "reachable authorization failures", errors: []error{errors.New("github status 403"), errors.New("github status 403")}},
		{name: "tenant capacity failures", errors: []error{errors.New("github status 429"), errors.New("github status 429")}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := normalizeConfig(Config{Project: scheduler.ProjectCandidate{ID: "detent"}})
			orch := Orchestrator{cfg: cfg, connector: &backendCapacityTestConnector{}}
			state := newState(cfg)
			for index, err := range tt.errors {
				orch.observeTrackerReadFailure(&state, telemetry.RefreshSourceCandidates, err, now.Add(time.Duration(index)*time.Second))
			}
			if got := state.TrackerUnavailable != nil; got != tt.wantActive {
				t.Fatalf("TrackerUnavailable active = %v, want %v; evidence = %#v", got, tt.wantActive, state.trackerEvidence)
			}
			if tt.wantActive {
				if state.TrackerUnavailable.ConnectorInstance != "detent:backend-capacity-test" || state.TrackerUnavailable.NextProbeAt.IsZero() {
					t.Fatalf("TrackerUnavailable = %#v, want scoped condition with probe", state.TrackerUnavailable)
				}
			}
		})
	}
}

func TestTrackerAvailabilityCanaryClearsAndResumesDispatch(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 17, 17, 20, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{
		Project:             scheduler.ProjectCandidate{ID: "detent"},
		MaxConcurrentAgents: 2,
		ActiveStates:        []string{"Todo", "In Progress"},
		TerminalStates:      []string{"Done"},
	})
	issue := dispatchTestIssue("issue-tracker", "Todo")
	connectorBackend := &backendCapacityTestConnector{}
	orch := Orchestrator{cfg: cfg, connector: connectorBackend, now: func() time.Time { return now }}
	state := newState(cfg)
	condition := TrackerCondition{
		ProjectID:         "detent",
		Connector:         "fake",
		ConnectorInstance: "detent:fake",
		Endpoint:          "https://tracker.test/graphql",
		Operation:         "candidate_issues",
		ErrorClass:        connector.TrackerAvailabilityClassServer,
		RefreshSource:     telemetry.RefreshSourceCandidates,
		DetectedAt:        now.Add(-time.Minute),
		LastObservedAt:    now.Add(-time.Minute),
		NextProbeAt:       now,
	}
	state.TrackerUnavailable = &condition
	state.Retry[issue.ID] = Retry{Issue: issue, Attempt: 2, DueAt: now, TrackerUnavailable: true}

	decision := newDispatchPlanner(cfg).dispatchableIssueDecision(issue, &state, false, now, "")
	if decision.dispatchable || decision.reason != dispatchSkipTrackerUnavailable {
		t.Fatalf("dispatch during condition = %#v, want tracker wait", decision)
	}
	if !orch.probeTrackerAvailability(t.Context(), &state, now) {
		t.Fatal("probeTrackerAvailability() = false, want successful canary")
	}
	if state.TrackerUnavailable != nil {
		t.Fatalf("TrackerUnavailable = %#v, want cleared", state.TrackerUnavailable)
	}
	retry := state.Retry[issue.ID]
	if retry.TrackerUnavailable || !retry.DueAt.Equal(now) {
		t.Fatalf("Retry[%q] = %#v, want released", issue.ID, retry)
	}
	_, allowed, reason := newDispatchPlanner(cfg).retryAction(&state, issue, retry, now)
	if !allowed || reason != "" {
		t.Fatalf("retry after canary = allowed %v reason %q, want allowed", allowed, reason)
	}
}

func TestTrackerUnavailableCompletionBypassesAllFailureBreakers(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 17, 17, 20, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{
		Project:             scheduler.ProjectCandidate{ID: "detent"},
		PollInterval:        time.Minute,
		ActiveStates:        []string{"In Progress"},
		TerminalStates:      []string{"Done"},
		MaxConcurrentAgents: 1,
		FailureBreaker: FailureBreakerConfig{
			SameClassLimit: 1,
			Window:         time.Hour,
			Cooldown:       time.Hour,
		},
	})
	tracker := &backendCapacityTestConnector{}
	orch := Orchestrator{cfg: cfg, connector: tracker, now: func() time.Time { return now }}
	state := newState(cfg)
	issue := dispatchTestIssue("issue-tracker-wait", "In Progress")
	state.Running[issue.ID] = Running{Issue: issue, Attempt: 4, StartedAt: now.Add(-time.Minute)}
	state.InstantFailures[issue.ID] = InstantFailure{Issue: issue, Count: instantFailureThreshold - 1}
	state.RepeatedFailures[issue.ID] = RepeatedFailure{Issue: issue, Count: repeatedFailureThreshold - 1}
	err := trackerAvailabilityTestError("candidate_issues", connector.TrackerAvailabilityClassServer)

	orch.handleRunResult(context.Background(), &state, runpkg.Completion{
		IssueID:      issue.ID,
		Request:      runpkg.RunRequest{Issue: issue, Attempt: 4},
		Err:          err,
		CompletedAt:  now,
		RetryAttempt: 5,
		RetryDelay:   time.Minute,
	})

	if got := state.InstantFailures[issue.ID].Count; got != instantFailureThreshold-1 {
		t.Fatalf("instant failure count = %d, want unchanged", got)
	}
	if got := state.RepeatedFailures[issue.ID].Count; got != repeatedFailureThreshold-1 {
		t.Fatalf("repeated failure count = %d, want unchanged", got)
	}
	if state.FailureBreaker.Active() || len(state.FailureBreaker.Failures) != 0 {
		t.Fatalf("FailureBreaker = %#v, want no tracker strike", state.FailureBreaker)
	}
	if _, blocked := state.Blocked[issue.ID]; blocked {
		t.Fatalf("Blocked[%q] present after tracker wait", issue.ID)
	}
	retry, ok := state.Retry[issue.ID]
	if !ok || retry.Attempt != 4 || !retry.TrackerUnavailable {
		t.Fatalf("Retry[%q] = %#v, want same-attempt typed tracker wait", issue.ID, retry)
	}
	if len(tracker.updates) != 0 || len(tracker.comments) != 0 {
		t.Fatalf("tracker mutations = states %#v comments %#v, want none", tracker.updates, tracker.comments)
	}
}

func TestClearTrackerAvailabilityReleasesWaiters(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 17, 17, 20, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{MaxConcurrentAgents: 1})
	orch := Orchestrator{cfg: cfg, now: func() time.Time { return now }}
	state := newState(cfg)
	issue := connector.Issue{ID: "issue-clear", State: "Todo"}
	state.TrackerUnavailable = &TrackerCondition{Connector: "github", DetectedAt: now.Add(-time.Hour)}
	state.Retry[issue.ID] = Retry{Issue: issue, Attempt: 1, DueAt: now.Add(time.Hour), TrackerUnavailable: true}

	cleared := orch.clearTrackerAvailability(&state, now)
	if len(cleared) != 1 || state.TrackerUnavailable != nil {
		t.Fatalf("clearTrackerAvailability() = %#v, state = %#v", cleared, state.TrackerUnavailable)
	}
	retry := state.Retry[issue.ID]
	if retry.TrackerUnavailable || !retry.DueAt.Equal(now) {
		t.Fatalf("Retry[%q] = %#v, want released", issue.ID, retry)
	}
}

func TestTrackerUnavailableCompletionFenceBecomesTypedWait(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 17, 17, 20, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{
		Project:        scheduler.ProjectCandidate{ID: "detent"},
		PollInterval:   time.Minute,
		ActiveStates:   []string{"In Progress"},
		TerminalStates: []string{"Done"},
	})
	err := trackerAvailabilityTestError("issue_lookup", connector.TrackerAvailabilityClassTimeout)
	tracker := &trackerFenceAvailabilityConnector{err: err}
	orch := Orchestrator{cfg: cfg, connector: tracker, now: func() time.Time { return now }}
	state := newState(cfg)
	issue := dispatchTestIssue("issue-completion-fence", "In Progress")
	state.Running[issue.ID] = Running{Issue: issue, Attempt: 2, Generation: 7, StartedAt: now.Add(-time.Minute)}

	orch.handleRunResult(t.Context(), &state, runpkg.Completion{
		IssueID:     issue.ID,
		Request:     runpkg.RunRequest{Issue: issue, Attempt: 2, Generation: 7},
		Result:      runpkg.RunResult{FinalState: FinalStateCompleted},
		CompletedAt: now,
	})

	if len(orch.pendingLaneRevocations) != 0 {
		t.Fatalf("pending lane revocations = %#v, want none", orch.pendingLaneRevocations)
	}
	retry, ok := state.Retry[issue.ID]
	if !ok || !retry.TrackerUnavailable || retry.Attempt != 2 {
		t.Fatalf("Retry[%q] = %#v, want typed tracker wait", issue.ID, retry)
	}
	if _, blocked := state.Blocked[issue.ID]; blocked {
		t.Fatalf("Blocked[%q] present after completion-fence outage", issue.ID)
	}
}

type trackerFenceAvailabilityConnector struct {
	backendCapacityTestConnector
	err error
}

func (c *trackerFenceAvailabilityConnector) FetchIssueStatesByIDs(context.Context, []string) ([]connector.Issue, error) {
	return nil, c.err
}

func trackerAvailabilityTestError(operation string, class string) error {
	return trackerAvailabilityTestErrorWithCredential(operation, class, "github-rest:test")
}

func trackerAvailabilityTestErrorWithCredential(operation string, class string, credentialIdentity string) error {
	return connector.NewTrackerAvailabilityError(connector.TrackerAvailabilityScope{
		Connector:          "github",
		Endpoint:           "https://api.github.test/graphql",
		Operation:          operation,
		CredentialIdentity: credentialIdentity,
	}, class, errors.New("upstream unavailable"))
}
