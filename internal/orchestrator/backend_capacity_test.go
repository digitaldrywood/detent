package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/backendcapacity"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestBackendCapacityDispatchAllowsOneResetProbe(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 10, 1, 55, 0, 0, time.UTC)
	resetAt := now.Add(44 * time.Minute)
	scope := backendcapacity.Scope{BackendID: "codex", BackendKind: "codex", Provider: "openai"}
	controller := backendCapacityTestController{scope: scope}
	orch := &Orchestrator{capacityController: controller}
	state := newState(normalizeConfig(Config{}))
	capacityErr, ok := backendcapacity.As(backendcapacity.NewError(
		scope,
		backendcapacity.Details{Kind: "usageLimitExceeded", ResetAt: &resetAt},
		errors.New("usage limit reached"),
	))
	if !ok {
		t.Fatal("capacity error did not unwrap")
	}
	outage := orch.registerBackendOutage(&state, capacityErr, now)
	request := runpkg.RunRequest{Issue: connector.Issue{ID: "issue-1", State: "In Progress"}}

	if _, _, paused := orch.backendCapacityDispatch(&state, request, now); !paused {
		t.Fatal("backendCapacityDispatch() paused = false before reset")
	}
	resolvedScope, probeKey, paused := orch.backendCapacityDispatch(&state, request, outage.ResumeAt)
	if paused || probeKey == "" || !resolvedScope.Matches(scope) {
		t.Fatalf("backendCapacityDispatch() = scope %#v probe %q paused %v, want one reset probe", resolvedScope, probeKey, paused)
	}
	markBackendCapacityProbe(&state, probeKey, request.Issue.ID)
	if _, _, paused := orch.backendCapacityDispatch(&state, request, outage.ResumeAt); !paused {
		t.Fatal("backendCapacityDispatch() paused = false while probe is running")
	}

	orch.recoverBackendCapacity(&state, Running{CapacityScope: scope, CapacityProbe: true}, outage.ResumeAt.Add(time.Second))
	if len(state.BackendOutages) != 0 || len(state.BackendRecoveries) != 1 {
		t.Fatalf("capacity state after successful probe = outages %#v recoveries %#v", state.BackendOutages, state.BackendRecoveries)
	}
}

func TestBackendCapacityDispatchLeavesLocalProviderUnaffected(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 10, 1, 55, 0, 0, time.UTC)
	state := newState(normalizeConfig(Config{}))
	state.BackendOutages["hosted"] = BackendOutage{
		Scope:    backendcapacity.Scope{BackendID: "codex-hosted", BackendKind: "codex", Provider: "openai"},
		ResumeAt: now.Add(time.Hour),
	}
	orch := &Orchestrator{capacityController: backendCapacityTestController{
		scope: backendcapacity.Scope{BackendID: "codex-local", BackendKind: "codex", Provider: "local_ollama"},
	}}

	_, _, paused := orch.backendCapacityDispatch(&state, runpkg.RunRequest{Issue: connector.Issue{ID: "local"}}, now)
	if paused {
		t.Fatal("local provider dispatch paused by hosted-provider outage")
	}
}

func TestBackendCapacityWithoutResetUsesLowFrequencyProbe(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 10, 1, 55, 0, 0, time.UTC)
	if got, want := backendCapacityResumeAt(time.Time{}, now), now.Add(backendCapacityProbeDelay); !got.Equal(want) {
		t.Fatalf("backendCapacityResumeAt() = %s, want %s", got, want)
	}
}

func TestValidatorCapacityPausesWithoutFailureBackoff(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 10, 1, 55, 0, 0, time.UTC)
	resetAt := now.Add(44 * time.Minute)
	scope := backendcapacity.Scope{BackendID: "codex", BackendKind: "codex", Provider: "openai"}
	validator := &backendCapacityTestValidator{
		requests: make(chan ValidatorRequest, 2),
		err: backendcapacity.NewError(
			scope,
			backendcapacity.Details{Kind: "usageLimitExceeded", ResetAt: &resetAt},
			errors.New("usage limit reached"),
		),
	}
	controller := backendCapacityTestController{scope: scope}
	orch := &Orchestrator{
		cfg:                     normalizeConfig(Config{}),
		validator:               validator,
		validatorCapacity:       controller,
		validatorRuns:           map[string]struct{}{},
		validatorResults:        map[string]validatorStageResult{},
		validatorFailures:       map[string]validatorStageFailure{},
		now:                     func() time.Time { return now },
		validatorCapacityEvents: make(chan validatorCapacityEvent, 1),
		done:                    make(chan struct{}),
	}
	state := newState(orch.cfg)
	issue := connector.Issue{
		ID:    "issue-validator-capacity",
		State: "In Progress",
		PullRequest: &connector.PullRequest{
			HeadSHA: "capacity-head",
		},
	}

	orch.startValidatorStage(t.Context(), &state, issue, now)
	select {
	case <-validator.requests:
	case <-time.After(time.Second):
		t.Fatal("validator did not run")
	}
	var event validatorCapacityEvent
	select {
	case event = <-orch.validatorCapacityEvents:
	case <-time.After(time.Second):
		t.Fatal("validator capacity event was not published")
	}
	orch.handleValidatorCapacityEvent(&state, event)
	if len(orch.validatorFailures) != 0 {
		t.Fatalf("validator failures = %#v, want none", orch.validatorFailures)
	}
	if len(state.BackendOutages) != 1 {
		t.Fatalf("backend outages = %#v, want one", state.BackendOutages)
	}

	orch.startValidatorStage(t.Context(), &state, issue, now.Add(time.Minute))
	select {
	case <-validator.requests:
		t.Fatal("validator ran while backend capacity was paused")
	case <-time.After(20 * time.Millisecond):
	}
}

func TestRecoverBackendCapacityBlockedIssues(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)
	tests := []struct {
		name        string
		pullRequest *connector.PullRequest
		wantState   string
	}{
		{name: "implementation issue returns to todo", wantState: "Todo"},
		{
			name:        "open pull request returns to rework",
			pullRequest: &connector.PullRequest{Number: 1142, State: "open"},
			wantState:   "Rework",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			issue := connector.Issue{
				ID:          "issue-capacity-blocked",
				Identifier:  "digitaldrywood/detent#1142",
				State:       "Blocked",
				PullRequest: tt.pullRequest,
				Comments: []connector.IssueComment{{
					Body: "Detent stopped retrying this worker after 5 consecutive instant failures with the same backend error. backend_error_body: " +
						`{"error":{"type":"usageLimitExceeded","resetAt":1783666800}}`,
				}},
			}
			tracker := &backendCapacityTestConnector{}
			controller := backendCapacityTestController{scope: backendcapacity.Scope{BackendID: "codex", BackendKind: "codex", Provider: "openai"}}
			orch := &Orchestrator{
				cfg:                normalizeConfig(Config{ActiveStates: []string{"Todo", "In Progress", "Rework"}}),
				connector:          tracker,
				capacityController: controller,
			}
			state := newState(orch.cfg)
			state.Blocked[issue.ID] = Blocked{Issue: issue, Source: BlockedSourceProjectStatus}

			if transitioned := orch.recoverBackendCapacityBlockedIssues(t.Context(), &state, []connector.Issue{issue}, now); len(transitioned) != 0 {
				t.Fatalf("initial transitioned = %#v, want reset jitter wait", transitioned)
			}
			transitioned := orch.recoverBackendCapacityBlockedIssues(t.Context(), &state, []connector.Issue{issue}, now.Add(backendCapacityResetJitter))
			if _, ok := transitioned[issue.ID]; !ok {
				t.Fatalf("transitioned = %#v, want %s recovered", transitioned, issue.ID)
			}
			if len(tracker.updates) != 1 || tracker.updates[0].state != tt.wantState {
				t.Fatalf("updates = %#v, want target %s", tracker.updates, tt.wantState)
			}
			if len(tracker.comments) != 1 || !strings.Contains(tracker.comments[0], "reason: backend_capacity_recovered") {
				t.Fatalf("comments = %#v, want machine-classifiable recovery comment", tracker.comments)
			}
			if _, ok := state.Blocked[issue.ID]; ok {
				t.Fatalf("Blocked[%q] still present after recovery", issue.ID)
			}
		})
	}
}

func TestRecoverBackendCapacityBlockedIssuesIgnoresUnrelatedQuotaComment(t *testing.T) {
	t.Parallel()

	issue := connector.Issue{
		ID:         "issue-unrelated-comment",
		Identifier: "digitaldrywood/detent#1143",
		State:      "Blocked",
		Comments: []connector.IssueComment{{
			Body: "A user mentioned usageLimitExceeded while documenting an unrelated blocker.",
		}},
	}
	tracker := &backendCapacityTestConnector{}
	orch := &Orchestrator{
		cfg:                normalizeConfig(Config{ActiveStates: []string{"Todo", "In Progress", "Rework"}}),
		connector:          tracker,
		capacityController: backendCapacityTestController{scope: backendcapacity.Scope{BackendID: "codex", BackendKind: "codex", Provider: "openai"}},
	}
	state := newState(orch.cfg)

	transitioned := orch.recoverBackendCapacityBlockedIssues(t.Context(), &state, []connector.Issue{issue}, time.Now())
	if len(transitioned) != 0 || len(tracker.updates) != 0 || len(state.BackendOutages) != 0 {
		t.Fatalf("unrelated quota comment triggered recovery: transitioned %#v updates %#v outages %#v", transitioned, tracker.updates, state.BackendOutages)
	}
}

type backendCapacityTestController struct {
	scope backendcapacity.Scope
}

func (c backendCapacityTestController) CapacityScope(runpkg.RunRequest) (backendcapacity.Scope, bool) {
	return c.scope, true
}

func (c backendCapacityTestController) ValidatorCapacityScope(runpkg.ValidatorRequest) (backendcapacity.Scope, bool) {
	return c.scope, true
}

func (c backendCapacityTestController) ClassifyCapacityError(
	_ runpkg.RunRequest,
	err error,
	limits *telemetry.RateLimits,
	now time.Time,
) (*backendcapacity.Error, bool) {
	var fallback *time.Time
	if limits != nil && limits.Primary != nil {
		fallback = limits.Primary.ResetAt
	}
	details, ok := backendcapacity.Classify(err.Error(), fallback, now, backendcapacity.Rules{Kinds: []string{"usageLimitExceeded"}})
	if !ok || !c.scope.Hosted() {
		return nil, false
	}
	capacityErr, ok := backendcapacity.As(backendcapacity.NewError(c.scope, details, err))
	return capacityErr, ok
}

type backendCapacityTestUpdate struct {
	issueID string
	state   string
}

type backendCapacityTestConnector struct {
	updates  []backendCapacityTestUpdate
	comments []string
}

func (c *backendCapacityTestConnector) Name() string {
	return "backend-capacity-test"
}

func (c *backendCapacityTestConnector) FetchCandidateIssues(context.Context) ([]connector.Issue, error) {
	return nil, nil
}

func (c *backendCapacityTestConnector) FetchIssuesByStates(context.Context, []string) ([]connector.Issue, error) {
	return nil, nil
}

func (c *backendCapacityTestConnector) FetchIssueStatesByIDs(context.Context, []string) ([]connector.Issue, error) {
	return nil, nil
}

func (c *backendCapacityTestConnector) CreateComment(_ context.Context, _ string, body string) error {
	c.comments = append(c.comments, body)
	return nil
}

func (c *backendCapacityTestConnector) UpdateIssueState(_ context.Context, issueID string, state string) error {
	c.updates = append(c.updates, backendCapacityTestUpdate{issueID: issueID, state: state})
	return nil
}

func (c *backendCapacityTestConnector) SetAssignee(context.Context, string, string) error {
	return nil
}

func (c *backendCapacityTestConnector) SetField(context.Context, string, string, string) error {
	return nil
}

type backendCapacityTestValidator struct {
	requests chan ValidatorRequest
	err      error
}

func (v *backendCapacityTestValidator) Validate(_ context.Context, request ValidatorRequest) (gate.ValidatorResult, error) {
	v.requests <- request
	return gate.ValidatorResult{}, v.err
}
