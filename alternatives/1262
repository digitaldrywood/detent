package scenariotest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func (*nativeProgressRerouteAgent) SupportsReadOnlyInvestigation() bool { return true }

type nativeOutageProgressBackend struct{}

func (nativeOutageProgressBackend) Decide(_ context.Context, evidence progresscheck.Request) (progresscheck.Decision, error) {
	last := evidence.Attempts[len(evidence.Attempts)-1]
	return progresscheck.Decision{Outcome: progresscheck.SystemDefect, Diagnosis: last.ID + " records a shared backend protocol outage", Reason: "Shared backend protocol outage", Evidence: []string{last.ID}, DefectKey: "shared-backend-protocol", DefectTitle: "Shared backend protocol outage", DefectBody: "Repair the backend protocol." + issueContractTestSections}, nil
}

func RunNativeRetryProgressSharedOutagePersistsOneDefect(t *testing.T, factories Constructors) {
	newNativeChangeHubTransport := func(t *testing.T, review string, states []tracker.NativeState, memory bool) *nativeChangeHub {
		return newNativeChangeHubConfigured(t, review, states, memory, false, hubserver.Config{DatabasePath: hubDatabasePath(t), InitialAdminToken: []byte(nativeChangeAdminToken)}, factories)
	}

	t.Parallel()
	h := newNativeChangeHubTransport(t, "In Review", []tracker.NativeState{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}},
		{Name: "Backlog", Transitions: []string{"Todo"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"In Review", "Done", "Todo"}},
		{Name: "In Review", Transitions: []string{"In Progress", "Done"}},
		{Name: "Done", Terminal: true},
	}, true)
	var defectID tracker.NativeWorkItemID
	for index := range 6 {
		issue := h.createInProgress(t, fmt.Sprintf("Backend outage issue %d", index))
		h.claim(t, issue.ID)
		failed := h.scheduler.RunExecution(issue.ID)
		if err := failed.Start(t.Context(), tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "test"}); err != nil {
			t.Fatal(err)
		}
		if err := failed.Finish(t.Context(), "failed"); err != nil {
			t.Fatal(err)
		}
		if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "failed"); err != nil {
			t.Fatal(err)
		}
		issue = h.claim(t, issue.ID)
		request, owner, err := h.scheduler.RetryProgress(t.Context(), issue)
		if err != nil {
			t.Fatal(err)
		}
		record, checked, err := progresscheck.Check(t.Context(), request, nativeOutageProgressBackend{}, owner)
		if err != nil || !checked || record.DefectIssueID == "" {
			t.Fatalf("outage progress publication failed: record=%+v checked=%v error=%v", record, checked, err)
		}
		persisted, err := h.admin.Issue(t.Context(), tracker.NativeWorkItemID(issue.ID))
		if err != nil || len(persisted.Dependencies) != 1 || string(persisted.Dependencies[0]) != record.DefectIssueID {
			t.Fatalf("missing persisted defect reference: issue=%+v record=%+v error=%v", persisted, record, err)
		}
		if defectID == "" {
			defectID = persisted.Dependencies[0]
		}
		if persisted.Dependencies[0] != defectID {
			t.Fatalf("outage created another defect: got %s want %s", persisted.Dependencies[0], defectID)
		}
		if err := h.connector.UpdateIssueState(t.Context(), issue.ID, "In Review"); err != nil {
			t.Fatal(err)
		}
		if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "failed"); err != nil {
			t.Fatal(err)
		}
	}
	page, err := h.admin.Issues(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defects := 0
	for _, issue := range page.Items {
		if issue.Title == "Shared backend protocol outage" {
			defects++
		}
	}
	if defects != 1 {
		t.Fatalf("persisted %d shared outage defects, want 1", defects)
	}
}

func RunNativeRetryProgressLoadsRepeatedLandingRefusals(t *testing.T, factories Constructors) {
	newNativeChangeHub := func(t *testing.T, memory ...bool) *nativeChangeHub { return newScenarioHub(t, factories, memory...) }

	t.Parallel()
	base, head := strings.Repeat("a", 40), strings.Repeat("b", 40)
	for _, test := range []struct {
		name string
		base string
		head string
		want bool
	}{
		{name: "same source", base: base, head: head, want: true},
		{name: "changed base", base: strings.Repeat("c", 40), head: head},
		{name: "changed head", base: base, head: strings.Repeat("c", 40)},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := newNativeChangeHub(t, true)
			issue := h.createInProgress(t, "Retry landing refusal")
			var attempts []string
			for _, source := range []struct{ base, head string }{{base, head}, {test.base, test.head}} {
				h.claim(t, issue.ID)
				execution := h.scheduler.RunExecution(issue.ID).(*ownedExecution)
				if err := execution.StartLanding(t.Context(), 1, 0); err != nil {
					t.Fatal(err)
				}
				if err := execution.ObserveLanding(t.Context(), runner.NativeLanding{RefusalKind: workspace.LandRefusalBaseMoved, BaseSHA: source.base, HeadSHA: source.head}); err != nil {
					t.Fatal(err)
				}
				if err := execution.Checkpoint(t.Context(), tracker.NativeCheckpoint{Resume: "fresh_checkout", Storage: "local_only", Availability: "available", WorktreeState: "unpushed", HeadSHA: source.head, ExternalEffect: "none", EffectState: "none"}); err != nil {
					t.Fatal(err)
				}
				if err := execution.Finish(t.Context(), "succeeded"); err != nil {
					t.Fatal(err)
				}
				attempts = append(attempts, execution.CompletionData().AttemptID)
				if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "waiting"); err != nil {
					t.Fatal(err)
				}
			}
			h.claim(t, issue.ID)
			claim := h.scheduler.RunExecution(issue.ID).(*ownedExecution).claim
			for _, attempt := range claim.recovery.Attempts {
				if attempt.AttemptID == attempts[0] {
					t.Fatal("sparse claim recovery unexpectedly retained the preceding refusal")
				}
			}
			client := fixtureClient(claim.source.NativeClient().Transport()).requests
			transport := client.Load()
			reads := 0
			client.Store(executionRoundTrip(func(request *http.Request) (*http.Response, error) {
				if request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/progress") {
					reads++
				}
				return transport.RoundTrip(request)
			}))
			request, _, err := h.scheduler.RetryProgress(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			if reads != 1 || len(request.Attempts) != 2 {
				t.Fatalf("progress history missing: reads=%d attempts=%+v", reads, request.Attempts)
			}
			if request.Attempts[0].ID != attempts[0] || request.Attempts[1].ID != attempts[1] {
				t.Fatalf("progress attempts out of order: %+v", request.Attempts)
			}
			if got := request.NeedsCheck(); got != test.want {
				t.Fatalf("NeedsCheck=%v, want %v", got, test.want)
			}
		})
	}
}

type nativeProgressRerouteAgent struct {
	PlanningAgent
	attempt     string
	err         error
	decision    *progresscheck.Decision
	duringCheck func(context.Context) error
}

func (a *nativeProgressRerouteAgent) RunTurn(ctx context.Context, _ runner.AgentTurnRequest, update runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
	if a.duringCheck != nil {
		if err := a.duringCheck(ctx); err != nil {
			return runner.AgentTurnResult{}, err
		}
	}
	if a.err != nil {
		return runner.AgentTurnResult{}, a.err
	}
	if err := update(runner.AgentUpdate{Type: runner.AgentUpdateTokenUsage, Tokens: runner.AgentTokenUsage{InputTokens: 10, OutputTokens: 1}}); err != nil {
		return runner.AgentTurnResult{}, err
	}
	output := progresscheck.Decision{Outcome: progresscheck.Reroute, Step: runner.RunModeImplement, Diagnosis: a.attempt + ": the retained failure identifies missing validation evidence; the failing step has not produced its required receipt", Instruction: "Repair the missing validation evidence in the source finalization path and verify the previously failing check on the pinned source", Reason: "Implement after the failed plan", Evidence: []string{a.attempt}}
	if a.decision != nil {
		output = *a.decision
	}
	decision, err := json.Marshal(output)
	if err != nil {
		return runner.AgentTurnResult{}, err
	}
	return runner.AgentTurnResult{}, update(runner.AgentUpdate{Type: runner.AgentUpdateMessageDelta, ItemID: "decision", Delta: string(decision)})
}

type nativeProgressWorkspace struct {
	workspace.Backend
	path string
}

func (w nativeProgressWorkspace) Head(_ context.Context, _ workspace.Info, issue workspace.Issue) (string, error) {
	return issue.PullRequestHeadSHA, nil
}
func (w nativeProgressWorkspace) VerifyReviewTree(context.Context, workspace.Info, workspace.Issue) error {
	return nil
}
func (w nativeProgressWorkspace) Existing(workspace.Issue) (workspace.Info, error) {
	return workspace.Info{Path: w.path}, nil
}

func (w nativeProgressWorkspace) Create(context.Context, workspace.Issue) (workspace.Info, error) {
	return workspace.Info{Path: w.path}, nil
}

type nativeProgressRerouteRun struct {
	request runner.RunRequest
	err     error
}

type nativeProgressRerouteRunner struct {
	*runner.Runner
	started chan nativeProgressRerouteRun
}

type nativeProgressRerouteScheduling struct {
	*nativeCompletionScheduling
	refused chan error
}

func (s *nativeProgressRerouteScheduling) ReadmitRetryProgress(ctx context.Context, request runner.RunRequest, resolver runner.ProviderCapacityResolver) (runner.Execution, *isolation.Policy, error) {
	execution, policy, err := s.Scheduler.ReadmitRetryProgress(ctx, request, resolver)
	if err != nil {
		s.refused <- err
	}
	return execution, policy, err
}

func (r *nativeProgressRerouteRunner) Run(ctx context.Context, request runner.RunRequest) (runner.RunResult, error) {
	requirement, err := r.DispatchCapacity(ctx, request)
	if err == nil {
		err = request.Execution.Start(ctx, tracker.NativeExecutionIdentity{Role: requirement.Role, Backend: requirement.Backend, Model: requirement.Model})
	}
	r.started <- nativeProgressRerouteRun{request: request, err: err}
	<-ctx.Done()
	return runner.RunResult{}, ctx.Err()
}

func RunNativeRetryProgressRerouteExecutionIdentity(t *testing.T, factories Constructors) {
	for _, reserved := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy admission", true: "provider reservation"}[reserved], func(t *testing.T) {
			for _, tighten := range []bool{false, true} {
				t.Run(map[bool]string{false: "unchanged isolation", true: "stricter isolation"}[tighten], func(t *testing.T) {
					t.Parallel()
					testNativeRetryProgressReroute(t, reserved, tighten, factories)
				})
			}
		})
	}
}

func testNativeRetryProgressReroute(t *testing.T, reserved, tighten bool, factories Constructors) {
	newNativeChangeHubTransport := func(t *testing.T, review string, states []tracker.NativeState, memory bool) *nativeChangeHub {
		return newNativeChangeHubConfigured(t, review, states, memory, false, hubserver.Config{DatabasePath: hubDatabasePath(t), InitialAdminToken: []byte(nativeChangeAdminToken)}, factories)
	}

	t.Helper()
	now := time.Now()
	h := newNativeChangeHubTransport(t, "Human Review", PlanStates(), true)
	if reserved || tighten {
		configureNativeProgressProvider(t, h)
		if !reserved {
			fixtureConfigureScheduler(t, h, func(config *SchedulerConfig) { config.ProviderReports = nil })
		}
	}
	setIsolation := func(ctx context.Context, tier string) error {
		fleet, err := NewFleetClient(h.admin.Transport(), h.organization, nil)
		if err != nil {
			return err
		}
		current, err := fleet.Fleet(ctx)
		if err != nil {
			return err
		}
		if len(current.Runners) != 1 {
			return errors.New("expected one fixture runner")
		}
		r := current.Runners[0]
		routing := r.Routing
		routing.IsolationTier = tier
		return fleet.UpdateRunner(ctx, r.RunnerID, runnerauth.RoutingChange{ExpectedRevision: r.Revision, Routing: routing})
	}
	if tighten {
		if err := setIsolation(t.Context(), isolation.NativeTrusted); err != nil {
			t.Fatal(err)
		}
	}
	issue, err := h.connector.CreateIssue(t.Context(), connector.IssueDraft{Title: "Retry failed planning as implementation", Body: "Update the README." + issueContractTestSections})
	if err != nil {
		t.Fatal(err)
	}
	confirmIssueContract(t, h.admin, issue.ID)
	claimPlan := func() connector.Issue {
		t.Helper()
		candidates, err := h.scheduler.FetchCandidateIssues(t.Context(), orchestrator.SchedulingRequest{Policy: h.descriptor, ProjectID: "local", WorkflowStates: []string{"Todo"}, ProviderRequirement: func(context.Context, connector.Issue, []providercapacity.Report) (providercapacity.Requirement, error) {
			return providercapacity.Requirement{Role: runner.RolePlan, Backend: "codex", Model: "plan-model"}, nil
		}})
		if err != nil || len(candidates) != 1 || candidates[0].ID != issue.ID {
			t.Fatalf("planning admission: candidates=%v error=%v", candidates, err)
		}
		admitted, err := h.scheduler.AdoptClaim(t.Context(), candidates[0], time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return admitted.Issue
	}
	claimPlan()
	failed := h.scheduler.RunExecution(issue.ID).(*ownedExecution)
	if err := failed.Start(t.Context(), tracker.NativeExecutionIdentity{Role: runner.RolePlan, Backend: "codex", Model: "plan-model"}); err != nil {
		t.Fatal(err)
	}
	if err := failed.PrepareFinish(t.Context(), "failed", "", nil); err != nil {
		t.Fatal(err)
	}
	if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "failed"); err != nil {
		t.Fatal(err)
	}
	issue = claimPlan()
	original := h.scheduler.RunExecution(issue.ID)
	if original == nil {
		t.Fatal("claimed planning execution is missing")
	}
	change, err := h.admin.CreateChange(t.Context(), tracker.NativeWorkItemID(issue.ID), tracker.CreateChange{Mutation: nativeMutationKey(), Title: "Retained progress source"})
	if err != nil {
		t.Fatal(err)
	}
	h.publish(t, tracker.NativeWorkItemID(issue.ID), change.ID, nativeChangeFixtureHead("b"))
	provider := &nativeProgressRerouteAgent{attempt: failed.CompletionData().AttemptID}
	if tighten {
		if issue.IsolationPolicy == nil || issue.IsolationPolicy.Tier != isolation.NativeTrusted {
			t.Fatalf("initial claim did not pin trusted isolation: %+v", issue.IsolationPolicy)
		}
		provider.duringCheck = func(ctx context.Context) error { return setIsolation(ctx, isolation.Sandbox) }
	}
	agent, err := runner.NewRunner(runner.Dependencies{ProcessSource: emptyProcessSource, Workflow: config.Workflow{Config: config.Config{Agents: config.Agents{Routes: []config.AgentRoute{
		{Name: "planner", Role: runner.RolePlan, Backend: "codex", Model: "plan-model"},
		{Name: "implementation", Backend: "codex", Model: "code-model", Default: true},
	}}}}, Workspace: nativeProgressWorkspace{path: t.TempDir()}, AgentBackend: provider})
	if err != nil {
		t.Fatal(err)
	}
	backend := &nativeProgressRerouteRunner{Runner: agent, started: make(chan nativeProgressRerouteRun, 1)}
	source := &nativeProgressRerouteScheduling{nativeCompletionScheduling: &nativeCompletionScheduling{Scheduler: h.scheduler, issue: issue, released: make(chan error, 1)}, refused: make(chan error, 1)}
	orch, err := orchestrator.New(orchestrator.Config{Project: scheduler.ProjectCandidate{ID: "local"}, Policy: h.descriptor, Plan: gate.PlanConfig{Enabled: true, Review: gate.PlanReviewAutomated}, PollInterval: time.Hour, MaxConcurrentAgents: 1, ActiveStates: []string{"Todo", "In Progress"}, TerminalStates: []string{"Done"}}, orchestrator.Dependencies{Connector: h.connector, Scheduling: source, Runner: backend, Now: func() time.Time { return now }, Logger: slog.New(slog.NewTextHandler(t.Output(), nil))})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- orch.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
	})
	select {
	case started := <-backend.started:
		if started.err != nil {
			t.Fatalf("rerouted implementation could not start: %v", started.err)
		}
		if started.request.Mode != runner.RunModeImplement || started.request.Execution == original {
			t.Fatalf("reroute did not reconcile the selected execution: mode=%s", started.request.Mode)
		}
		if tighten {
			execution := started.request.Execution.(*ownedExecution)
			if execution.claim.lease.IsolationPolicy == nil || execution.claim.lease.IsolationPolicy.Tier != isolation.Sandbox {
				t.Fatalf("replacement claim did not pin sandbox: %+v", execution.claim.lease.IsolationPolicy)
			}
			if started.request.Issue.IsolationPolicy == nil || started.request.Issue.IsolationPolicy.Tier != isolation.Sandbox {
				t.Fatalf("implementation retained trusted isolation after sandbox readmission: %+v", started.request.Issue.IsolationPolicy)
			}
		}
		check, err := h.admin.NativeAttempt(t.Context(), tracker.NativeWorkItemID(issue.ID), original.(*ownedExecution).CompletionData().AttemptID)
		if err != nil {
			t.Fatal(err)
		}
		implementation, err := h.admin.NativeAttempt(t.Context(), tracker.NativeWorkItemID(issue.ID), started.request.Execution.(*ownedExecution).CompletionData().AttemptID)
		if err != nil {
			t.Fatal(err)
		}
		if check.Identity.Role != runner.RolePlan || check.Identity.Model != "plan-model" || len(check.Usage) != 1 || check.Usage[0].Input != 10 || check.Usage[0].Output != 1 {
			t.Fatalf("progress accounting changed: %+v", check)
		}
		if implementation.Identity.Role != runner.RoleCode || implementation.Identity.Model != "code-model" || implementation.LeaseID == check.LeaseID {
			t.Fatalf("implementation did not receive its own identity and admission: %+v", implementation)
		}
		if err := original.Validate(t.Context()); !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) {
			t.Fatalf("original execution retained authority: %v", err)
		}
		if err := started.request.Execution.Start(t.Context(), *check.Identity); err == nil {
			t.Fatal("implementation allowed its admitted identity to change")
		}
		if reserved {
			reservation := started.request.Execution.(runner.ProviderCapacityExecution).ProviderCapacity()
			if reservation == nil || reservation.Role != runner.RoleCode || reservation.Model != "code-model" {
				t.Fatalf("reroute retained the planning reservation: %+v", reservation)
			}
		}
	case err := <-source.refused:
		t.Fatalf("selected step admission failed: %v", err)
	case <-testenv.WaitContext(t, t.Context()).Done():
		t.Fatal("rerouted implementation did not reach execution start")
	}
}

func RunNativeRetryProgressFailureReleasesExecutionIdentity(t *testing.T, factories Constructors) {
	newNativeChangeHubTransport := func(t *testing.T, review string, states []tracker.NativeState, memory bool) *nativeChangeHub {
		return newNativeChangeHubConfigured(t, review, states, memory, false, hubserver.Config{DatabasePath: hubDatabasePath(t), InitialAdminToken: []byte(nativeChangeAdminToken)}, factories)
	}

	t.Parallel()
	for _, tt := range []struct {
		name     string
		failure  error
		invalid  bool
		conflict bool
		reason   string
	}{
		{name: "backend", failure: errors.New("provider unavailable"), reason: "provider unavailable"},
		{name: "timeout", failure: context.DeadlineExceeded, reason: "context deadline exceeded"},
		{name: "invalid decision", invalid: true, reason: "progress decision names unavailable step"},
		{name: "old Hub publication conflict", conflict: true, reason: "revision_conflict"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Now()
			states := PlanStates()
			states[1].Transitions = append(states[1].Transitions, "Rework")
			states = append(states, tracker.NativeState{Name: "Rework", Dispatchable: true, Transitions: []string{"Todo", "In Progress"}})
			h := newNativeChangeHubTransport(t, "Human Review", states, true)
			conflicts := 0
			if tt.conflict {
				next := h.failChanges.next
				h.failChanges.next = executionRoundTrip(func(request *http.Request) (*http.Response, error) {
					if request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/progress") {
						conflicts++
						return &http.Response{StatusCode: http.StatusConflict, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":"revision_conflict","message":"evidence changed"}`)), Request: request}, nil
					}
					return next.RoundTrip(request)
				})
			}
			issue, err := h.connector.CreateIssue(t.Context(), connector.IssueDraft{Title: "Retry implementation after progress failure", Body: "Update the README." + issueContractTestSections})
			if err != nil {
				t.Fatal(err)
			}
			confirmIssueContract(t, h.admin, issue.ID)
			claim := func(state string) connector.Issue {
				t.Helper()
				candidates, err := h.scheduler.FetchCandidateIssues(t.Context(), orchestrator.SchedulingRequest{Policy: h.descriptor, ProjectID: "local", WorkflowStates: []string{state}})
				if err != nil || len(candidates) != 1 || candidates[0].ID != issue.ID {
					t.Fatalf("admission: candidates=%v error=%v", candidates, err)
				}
				admitted, err := h.scheduler.AdoptClaim(t.Context(), candidates[0], time.Now())
				if err != nil {
					t.Fatal(err)
				}
				return admitted.Issue
			}
			claim("Todo")
			failed := h.scheduler.RunExecution(issue.ID).(*ownedExecution)
			if err := failed.Start(t.Context(), tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "code-model"}); err != nil {
				t.Fatal(err)
			}
			if err := failed.PrepareFinish(t.Context(), "failed", "", nil); err != nil {
				t.Fatal(err)
			}
			if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "failed"); err != nil {
				t.Fatal(err)
			}
			current, err := h.admin.Issue(t.Context(), tracker.NativeWorkItemID(issue.ID))
			if err != nil {
				t.Fatal(err)
			}
			for _, state := range []string{"In Progress", "Rework"} {
				current, err = h.admin.Transition(t.Context(), current.WorkItemID, tracker.Transition{Mutation: nativeMutationKey(), ExpectedRevision: current.Revision, State: state, Reason: "user_requested"})
				if err != nil {
					t.Fatal(err)
				}
			}
			issue = claim("Rework")
			original := h.scheduler.RunExecution(issue.ID).(*ownedExecution)
			change, err := h.admin.CreateChange(t.Context(), tracker.NativeWorkItemID(issue.ID), tracker.CreateChange{Mutation: nativeMutationKey(), Title: "Retained progress source"})
			if err != nil {
				t.Fatal(err)
			}
			h.publish(t, tracker.NativeWorkItemID(issue.ID), change.ID, nativeChangeFixtureHead("b"))
			provider := &nativeProgressRerouteAgent{err: tt.failure, attempt: failed.CompletionData().AttemptID}
			if tt.invalid {
				provider.decision = &progresscheck.Decision{Outcome: progresscheck.Reroute, Step: "unavailable", Diagnosis: failed.CompletionData().AttemptID + ": the retained failure identifies missing validation evidence; the failing step has not produced its required receipt", Instruction: "Repair the missing validation evidence in the source finalization path and verify the previously failing check on the pinned source", Reason: "Continue", Evidence: []string{failed.CompletionData().AttemptID}}
			}
			agent, err := runner.NewRunner(runner.Dependencies{ProcessSource: emptyProcessSource, Workflow: config.Workflow{Config: config.Config{Agents: config.Agents{Routes: []config.AgentRoute{{Name: "implementation", Backend: "codex", Model: "code-model", Default: true}}}}}, Workspace: nativeProgressWorkspace{path: t.TempDir()}, AgentBackend: provider})
			if err != nil {
				t.Fatal(err)
			}
			backend := &nativeProgressRerouteRunner{Runner: agent, started: make(chan nativeProgressRerouteRun, 1)}
			source := &nativeProgressRerouteScheduling{nativeCompletionScheduling: &nativeCompletionScheduling{Scheduler: h.scheduler, issue: issue, released: make(chan error, 1)}, refused: make(chan error, 1)}
			orch, err := orchestrator.New(orchestrator.Config{Project: scheduler.ProjectCandidate{ID: "local"}, Policy: h.descriptor, PollInterval: time.Hour, MaxConcurrentAgents: 1, ActiveStates: []string{"Todo", "In Progress", "Rework"}, TerminalStates: []string{"Done"}}, orchestrator.Dependencies{Connector: h.connector, Scheduling: source, Runner: backend, Now: func() time.Time { return now }, Logger: slog.New(slog.NewTextHandler(t.Output(), nil))})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- orch.Run(ctx) }()
			t.Cleanup(func() {
				cancel()
				if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
					t.Error(err)
				}
			})
			var started nativeProgressRerouteRun
			var admissionErr error
			select {
			case started = <-backend.started:
				if started.err != nil {
					t.Fatal(started.err)
				}
			case err := <-source.refused:
				admissionErr = err
			case err := <-source.released:
				t.Fatalf("ordinary retry stopped after progress failure: %v", err)
			case <-testenv.WaitContext(t, t.Context()).Done():
				t.Fatal("ordinary retry did not start")
			}
			check, err := h.admin.NativeAttempt(t.Context(), tracker.NativeWorkItemID(issue.ID), original.CompletionData().AttemptID)
			if err != nil {
				t.Fatal(err)
			}
			if check.Identity == nil || check.Identity.Role != runner.RoleRework || check.Outcome != "cancelled" || check.ClaimReleasedAt == nil {
				t.Fatalf("failed progress check retained its rework identity: %+v", check)
			}
			if admissionErr != nil {
				t.Fatalf("ordinary retry admission failed: %v", admissionErr)
			}
			if started.request.Mode != runner.RunModeImplement || started.request.ProgressDecision.RequestID != "" || strings.Count(started.request.PriorAttempt.Reason, tt.reason) != 1 {
				t.Fatalf("ordinary retry lost advisory failure: %+v", started.request)
			}
			if tt.conflict && conflicts != 2 {
				t.Fatalf("old Hub conflict made %d publications, want 2", conflicts)
			}
			if check.Runtime == nil || check.Runtime.Recovery == nil || strings.Count(check.Runtime.Recovery.Reason, tt.reason) != 1 {
				t.Fatalf("progress failure evidence missing: %+v", check.Runtime)
			}
			implementation := started.request.Execution.(*ownedExecution)
			if implementation.CompletionData().AttemptID == check.AttemptID || implementation.claim.lease.ID == check.LeaseID {
				t.Fatal("implementation retry reused progress admission")
			}

			changedIdentity := *check.Identity
			changedIdentity.Role = runner.RolePlan
			if err := implementation.Start(t.Context(), changedIdentity); err == nil {
				t.Fatal("implementation allowed its admitted identity to change")
			}

		})
	}
}

func configureNativeProgressProvider(t *testing.T, h *nativeChangeHub) {
	t.Helper()
	fixtureClient(h.admin.Transport()).baseURL.Scheme = "https"
	report := providercapacity.Report{Provider: "openai", Backend: "codex", AccountAlias: "example", SharedAccountAlias: "example", Models: []string{"plan-model", "code-model"}, MaxConcurrent: 1, Availability: "available", ObservedAt: time.Now()}
	identityPath := filepath.Join(t.TempDir(), "private", "identity.json")
	file, err := runnerauth.Initialize(identityPath, "https://native-hub.test")
	if err != nil {
		t.Fatal(err)
	}
	enrollment, err := h.admin.Transport().CreateRunnerEnrollment(t.Context(), h.organization, runnerauth.EnrollmentRequest{Binding: file.Identity.Binding, ProjectIDs: []tracker.ProjectID{h.project}, Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events, runnerauth.Collaborate}, TTLSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	machine := Machine{BackendIsolation: isolation.Report{"codex": {isolation.Sandbox, isolation.NativeTrusted}}, ID: file.Identity.MachineID, Hostname: "example", DisplayName: "Progress runner", Capacity: 1, Version: "test"}
	var identity runnerauth.Identity
	if err := fixtureRunnerRequest(t, h.admin.Transport(), enrollment.Token, http.MethodPost, "/api/v2/organizations/"+string(h.organization)+"/runner-enrollments/redeem", runnerauth.Redemption{BackendIsolation: machine.BackendIsolation, Binding: file.Identity.Binding, Credential: file.Credential, Hostname: machine.Hostname, DisplayName: machine.DisplayName, Capacity: machine.Capacity, Version: machine.Version}, &identity); err != nil {
		t.Fatal(err)
	}
	file.Identity = identity
	if err := runnerauth.Save(identityPath, file); err != nil {
		t.Fatal(err)
	}
	client, err := createTestClient(t, Config{URL: file.HubURL, IdentityFile: identityPath, HTTPClient: &http.Client{Transport: h.failChanges}}, fixtureClient(h.admin.Transport()).constructors)
	if err != nil {
		t.Fatal(err)
	}
	h.scheduler, err = newTestScheduler(client, SchedulerConfig{OrganizationID: h.organization, NativeProjects: map[string]tracker.ProjectID{"local": h.project}, Machine: machine, HeartbeatInterval: time.Second, LeaseTTL: 90 * time.Second, ProviderReports: func() ([]providercapacity.Report, error) { return []providercapacity.Report{report}, nil }, LeaseHold: h.leaseHold})
	if err != nil {
		t.Fatal(err)
	}
}
