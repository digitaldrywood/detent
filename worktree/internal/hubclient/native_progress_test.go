package hubclient

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

type nativeProgressBackend struct{ calls int }

func (b *nativeProgressBackend) Decide(context.Context, progresscheck.Request) (progresscheck.Decision, error) {
	b.calls++
	return progresscheck.Decision{Outcome: progresscheck.Reroute, Step: "implement", Diagnosis: "failed-attempt" + ": the retained failure identifies missing validation evidence; the failing step has not produced its required receipt", Instruction: "Repair the missing validation evidence in the source finalization path and verify the previously failing check on the pinned source", Reason: "Repair the publication protocol", Evidence: []string{"failed-attempt"}}, nil
}

func TestNativeRetryProgressOwnerAndSuccess(t *testing.T) {
	t.Parallel()
	for _, outcome := range []string{"failed", "succeeded", "empty landing", "different issue", "no change", "no work product", "source recovery", "repeated refusal"} {
		t.Run(outcome, func(t *testing.T) {
			recovery := tracker.NativeRecovery{Issue: tracker.NativeIssue{NativeReference: tracker.NativeReference{ProjectID: "prj_example", WorkItemID: "wi_example", Revision: 1}}, Attempts: []tracker.NativeAttempt{{NativeRunData: tracker.NativeRunData{AttemptID: "failed-attempt", Outcome: outcome}}}}
			switch outcome {
			case "empty landing":
				recovery.Attempts[0].Outcome = "succeeded"
				recovery.Attempts[0].Runtime = &tracker.NativeRuntimeObservation{Landing: &tracker.NativeLandingReceipt{RefusalKind: "nothing_to_land", Refusal: "the published source has no deliverable to verify"}}
			case "no change":
				recovery.Attempts[0].Outcome = "succeeded"
				recovery.Attempts[0].Finalization = &tracker.NativeFinalization{Changed: false}
			case "no work product":
				recovery.Attempts[0].Outcome = "succeeded"
				recovery.Attempts[0].Identity = &tracker.NativeExecutionIdentity{Role: "code"}
			case "source recovery":
				recovery.Attempts[0].Outcome = "succeeded"
				recovery.Attempts[0].Disposition = &tracker.NativeDisposition{Status: "blocked", ReasonCode: "source_recovery_required"}
			case "repeated refusal":
				recovery.Attempts[0].Outcome = "succeeded"
				recovery.Attempts[0].Runtime = &tracker.NativeRuntimeObservation{Landing: &tracker.NativeLandingReceipt{RefusalKind: "source_missing", BaseSHA: "base", HeadSHA: "head"}}
				prior := recovery.Attempts[0]
				prior.AttemptID = "prior-attempt"
				recovery.Attempts = append([]tracker.NativeAttempt{prior}, recovery.Attempts...)
			}
			evidence := progresscheck.NativeRequest(recovery, "policy", "machine")
			if outcome == "different issue" {
				recovery.Attempts[0].Outcome = "failed"
				evidence = progresscheck.NativeRequest(recovery, "policy", "machine")
				evidence.IssueID = "wi_another"
			}
			var retained *progresscheck.Record
			reads, writes := 0, 0
			client, err := New(Config{URL: "https://hub.example.test", TokenSource: func() string { return "worker" }, HTTPClient: &http.Client{Transport: executionRoundTrip(func(request *http.Request) (*http.Response, error) {
				if request.URL.Path == "/api/v2/capabilities" {
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"features":["progress_investigation"]}`)), Request: request}, nil
				}
				if request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/work-items/wi_example") {
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"revision":"7"}`)), Request: request}, nil
				}
				if !strings.HasSuffix(request.URL.Path, "/work-items/wi_example/progress") {
					t.Fatalf("unexpected progress route: %s", request.URL.Path)
				}
				var response any
				if request.Method == http.MethodGet {
					reads++
					if request.URL.Query().Get("policy") != "policy" || request.URL.Query().Get("instance") != "machine" {
						t.Fatal("missing progress input binding")
					}
					response = struct {
						Request progresscheck.Request `json:"request"`
						Record  *progresscheck.Record `json:"record"`
					}{evidence, retained}
				} else {
					writes++
					var input progresscheck.Publication
					if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
						t.Fatal(err)
					}
					if input.LeaseID != "lease" || input.FencingToken != 7 || input.Instance != "machine" || input.PolicyID != "policy" {
						t.Fatalf("unfenced progress publication: %+v", input.Mutation)
					}
					retained = &progresscheck.Record{RequestID: input.RequestID, ProjectID: evidence.ProjectID, IssueID: evidence.IssueID, EvidenceRevision: evidence.EvidenceRevision, Decision: input.Decision, CommentID: "comment"}
					response = retained
				}
				encoded, err := json.Marshal(response)
				if err != nil {
					t.Fatal(err)
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(encoded))), Request: request}, nil
			})}})
			if err != nil {
				t.Fatal(err)
			}
			native, err := client.Native("org_example", "prj_example")
			if err != nil {
				t.Fatal(err)
			}
			source := &Scheduler{nativeClaims: map[string]nativeClaim{"wi_example": {source: &NativeConnector{client: native}, lease: tracker.NativeLease{ID: "lease", MachineID: "machine", WorkItemID: "wi_example", FencingToken: 7, PolicyID: "policy"}, recovery: recovery}}}
			backend := &nativeProgressBackend{}
			request, owner, err := source.RetryProgress(t.Context(), connector.Issue{ID: "wi_example"})
			if outcome == "different issue" {
				if err == nil || writes != 0 || backend.calls != 0 {
					t.Fatalf("another issue's evidence accepted: err=%v writes=%d calls=%d", err, writes, backend.calls)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				_, checked, err := progresscheck.Check(t.Context(), request, backend, owner)
				if err != nil || checked != (outcome != "succeeded") {
					t.Fatalf("checked=%v error=%v", checked, err)
				}
			}
			want := 0
			if outcome != "succeeded" {
				want = 1
			}
			if reads != want || writes != want || backend.calls != want {
				t.Fatalf("reads=%d writes=%d turns=%d", reads, writes, backend.calls)
			}
		})
	}
}

func TestNativeRetryProgressLoadsRepeatedLandingRefusals(t *testing.T) {
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
				execution := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
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
				attempts = append(attempts, execution.data.AttemptID)
				if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "waiting"); err != nil {
					t.Fatal(err)
				}
			}
			h.claim(t, issue.ID)
			claim := h.scheduler.nativeClaims[issue.ID]
			for _, attempt := range claim.recovery.Attempts {
				if attempt.AttemptID == attempts[0] {
					t.Fatal("sparse claim recovery unexpectedly retained the preceding refusal")
				}
			}
			client := claim.source.client.client.httpClient
			transport := client.Transport
			reads := 0
			client.Transport = executionRoundTrip(func(request *http.Request) (*http.Response, error) {
				if request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/progress") {
					reads++
				}
				return transport.RoundTrip(request)
			})
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
	nativePlanningAgent
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

func TestNativeRetryProgressRerouteExecutionIdentity(t *testing.T) {
	for _, reserved := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy admission", true: "provider reservation"}[reserved], func(t *testing.T) {
			for _, tighten := range []bool{false, true} {
				t.Run(map[bool]string{false: "unchanged isolation", true: "stricter isolation"}[tighten], func(t *testing.T) {
					t.Parallel()
					testNativeRetryProgressReroute(t, reserved, tighten)
				})
			}
		})
	}
}

func testNativeRetryProgressReroute(t *testing.T, reserved, tighten bool) {
	t.Helper()
	now := time.Now()
	h := newNativeChangeHubTransport(t, "Human Review", hubserverPlanStates(), true)
	if reserved || tighten {
		configureNativeProgressProvider(t, h)
		if !reserved {
			h.scheduler.providerReports = nil
		}
	}
	setIsolation := func(ctx context.Context, tier string) error {
		fleet, err := NewFleetClient(h.admin.client, h.organization, nil)
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
	failed := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
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
	provider := &nativeProgressRerouteAgent{attempt: failed.data.AttemptID}
	if tighten {
		if issue.IsolationPolicy == nil || issue.IsolationPolicy.Tier != isolation.NativeTrusted {
			t.Fatalf("initial claim did not pin trusted isolation: %+v", issue.IsolationPolicy)
		}
		provider.duringCheck = func(ctx context.Context) error { return setIsolation(ctx, isolation.Sandbox) }
	}
	agent, err := runner.NewRunner(runner.Dependencies{Workflow: config.Workflow{Config: config.Config{Agents: config.Agents{Routes: []config.AgentRoute{
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
			execution := started.request.Execution.(*nativeExecution)
			if execution.claim.lease.IsolationPolicy == nil || execution.claim.lease.IsolationPolicy.Tier != isolation.Sandbox {
				t.Fatalf("replacement claim did not pin sandbox: %+v", execution.claim.lease.IsolationPolicy)
			}
			if started.request.Issue.IsolationPolicy == nil || started.request.Issue.IsolationPolicy.Tier != isolation.Sandbox {
				t.Fatalf("implementation retained trusted isolation after sandbox readmission: %+v", started.request.Issue.IsolationPolicy)
			}
		}
		check, err := h.admin.NativeAttempt(t.Context(), tracker.NativeWorkItemID(issue.ID), original.(*nativeExecution).data.AttemptID)
		if err != nil {
			t.Fatal(err)
		}
		implementation, err := h.admin.NativeAttempt(t.Context(), tracker.NativeWorkItemID(issue.ID), started.request.Execution.(*nativeExecution).data.AttemptID)
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

func TestNativeRetryProgressFailureReleasesExecutionIdentity(t *testing.T) {
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
			states := hubserverPlanStates()
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
			failed := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
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
			original := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
			change, err := h.admin.CreateChange(t.Context(), tracker.NativeWorkItemID(issue.ID), tracker.CreateChange{Mutation: nativeMutationKey(), Title: "Retained progress source"})
			if err != nil {
				t.Fatal(err)
			}
			h.publish(t, tracker.NativeWorkItemID(issue.ID), change.ID, nativeChangeFixtureHead("b"))
			provider := &nativeProgressRerouteAgent{err: tt.failure, attempt: failed.data.AttemptID}
			if tt.invalid {
				provider.decision = &progresscheck.Decision{Outcome: progresscheck.Reroute, Step: "unavailable", Diagnosis: failed.data.AttemptID + ": the retained failure identifies missing validation evidence; the failing step has not produced its required receipt", Instruction: "Repair the missing validation evidence in the source finalization path and verify the previously failing check on the pinned source", Reason: "Continue", Evidence: []string{failed.data.AttemptID}}
			}
			agent, err := runner.NewRunner(runner.Dependencies{Workflow: config.Workflow{Config: config.Config{Agents: config.Agents{Routes: []config.AgentRoute{{Name: "implementation", Backend: "codex", Model: "code-model", Default: true}}}}}, Workspace: nativeProgressWorkspace{path: t.TempDir()}, AgentBackend: provider})
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
			select {
			case started = <-backend.started:
				if started.err != nil {
					t.Fatal(started.err)
				}
			case err := <-source.refused:
				t.Fatalf("ordinary retry admission failed: %v", err)
			case err := <-source.released:
				t.Fatalf("ordinary retry stopped after progress failure: %v", err)
			case <-testenv.WaitContext(t, t.Context()).Done():
				t.Fatal("ordinary retry did not start")
			}
			if started.request.Mode != runner.RunModeImplement || started.request.ProgressDecision.RequestID != "" || strings.Count(started.request.PriorAttempt.Reason, tt.reason) != 1 {
				t.Fatalf("ordinary retry lost advisory failure: %+v", started.request)
			}
			if tt.conflict && conflicts != 2 {
				t.Fatalf("old Hub conflict made %d publications, want 2", conflicts)
			}

			check, err := h.admin.NativeAttempt(t.Context(), tracker.NativeWorkItemID(issue.ID), original.data.AttemptID)
			if err != nil {
				t.Fatal(err)
			}
			if check.Identity == nil || check.Identity.Role != runner.RoleRework || check.Outcome != "cancelled" || check.ClaimReleasedAt == nil {
				t.Fatalf("failed progress check retained its rework identity: %+v", check)
			}
			if check.Runtime == nil || check.Runtime.Recovery == nil || strings.Count(check.Runtime.Recovery.Reason, tt.reason) != 1 {
				t.Fatalf("progress failure evidence missing: %+v", check.Runtime)
			}
			implementation := started.request.Execution.(*nativeExecution)
			if implementation.data.AttemptID == check.AttemptID || implementation.claim.lease.ID == check.LeaseID {
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
	h.admin.client.baseURL.Scheme = "https"
	report := providercapacity.Report{Provider: "openai", Backend: "codex", AccountAlias: "example", SharedAccountAlias: "example", Models: []string{"plan-model", "code-model"}, MaxConcurrent: 1, Availability: "available", ObservedAt: time.Now()}
	identityPath := filepath.Join(t.TempDir(), "private", "identity.json")
	file, err := runnerauth.Initialize(identityPath, "https://native-hub.test")
	if err != nil {
		t.Fatal(err)
	}
	enrollment, err := h.admin.client.CreateRunnerEnrollment(t.Context(), h.organization, runnerauth.EnrollmentRequest{Binding: file.Identity.Binding, ProjectIDs: []tracker.ProjectID{h.project}, Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events, runnerauth.Collaborate}, TTLSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	machine := Machine{BackendIsolation: isolation.Report{"codex": {isolation.Sandbox, isolation.NativeTrusted}}, ID: file.Identity.MachineID, Hostname: "example", DisplayName: "Progress runner", Capacity: 1, Version: "test"}
	var identity runnerauth.Identity
	if err := h.admin.client.runnerRequest(t.Context(), enrollment.Token, http.MethodPost, "/api/v2/organizations/"+string(h.organization)+"/runner-enrollments/redeem", runnerauth.Redemption{BackendIsolation: machine.BackendIsolation, Binding: file.Identity.Binding, Credential: file.Credential, Hostname: machine.Hostname, DisplayName: machine.DisplayName, Capacity: machine.Capacity, Version: machine.Version}, &identity); err != nil {
		t.Fatal(err)
	}
	file.Identity = identity
	if err := runnerauth.Save(identityPath, file); err != nil {
		t.Fatal(err)
	}
	client, err := New(Config{URL: file.HubURL, IdentityFile: identityPath, HTTPClient: &http.Client{Transport: h.failChanges}})
	if err != nil {
		t.Fatal(err)
	}
	h.scheduler, err = NewScheduler(client, SchedulerConfig{OrganizationID: h.organization, NativeProjects: map[string]tracker.ProjectID{"local": h.project}, Machine: machine, HeartbeatInterval: time.Second, LeaseTTL: 90 * time.Second, ProviderReports: func() ([]providercapacity.Report, error) { return []providercapacity.Report{report}, nil }, LeaseHold: h.leaseHold})
	if err != nil {
		t.Fatal(err)
	}
}

func TestProgressPublicationRefreshesRevisionOnce(t *testing.T) {
	for _, test := range []struct {
		name       string
		status     int
		code       string
		wantWrites int
		wantReads  int
		wantError  bool
	}{
		{"conflict then published", http.StatusConflict, "revision_conflict", 2, 2, false},
		{"repeated conflict stops", http.StatusConflict, "revision_conflict", 2, 2, true},
		{"other error is not retried", http.StatusForbidden, "forbidden", 1, 1, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			evidence := progresscheck.NativeRequest(tracker.NativeRecovery{Issue: tracker.NativeIssue{NativeReference: tracker.NativeReference{ProjectID: "prj_example", WorkItemID: "wi_example", Revision: 1}}, Attempts: []tracker.NativeAttempt{{NativeRunData: tracker.NativeRunData{AttemptID: "failed-attempt", Outcome: "failed"}}}}, "policy", "machine")
			reads, writes := 0, 0
			requestID := ""
			client, err := New(Config{URL: "https://hub.example.test", TokenSource: func() string { return "worker" }, HTTPClient: &http.Client{Transport: executionRoundTrip(func(req *http.Request) (*http.Response, error) {
				if req.URL.Path == "/api/v2/capabilities" {
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"features":["progress_investigation"]}`)), Request: req}, nil
				}
				status := http.StatusOK
				var body any
				if req.Method == http.MethodGet {
					reads++
					if strings.HasSuffix(req.URL.Path, "/progress") {
						t.Fatal("publication must refresh item revision only")
					}
					body = tracker.NativeIssue{NativeReference: tracker.NativeReference{ProjectID: "prj_example", WorkItemID: "wi_example", Revision: tracker.Revision(6 + reads)}}
				} else {
					writes++
					var input map[string]json.RawMessage
					if err := json.NewDecoder(req.Body).Decode(&input); err != nil {
						t.Fatal(err)
					}
					var id string
					if err := json.Unmarshal(input["request_id"], &id); err != nil {
						t.Fatal(err)
					}
					if requestID == "" {
						requestID = id
					}
					if id != requestID {
						t.Fatal("retry changed request key")
					}
					if string(input["expected_revision"]) != fmt.Sprintf("\"%d\"", 6+reads) {
						t.Errorf("expected current item revision, got %s after %d reads", input["expected_revision"], reads)
					}
					if writes == 1 || test.wantError {
						status = test.status
						body = map[string]string{"code": test.code, "error": "publication refused"}
					} else {
						body = progresscheck.Record{RequestID: id, ProjectID: evidence.ProjectID, IssueID: evidence.IssueID, EvidenceRevision: evidence.EvidenceRevision, Decision: progresscheck.Decision{Outcome: progresscheck.Reroute, Step: "implement", Diagnosis: "failed-attempt" + ": the retained failure identifies missing validation evidence; the failing step has not produced its required receipt", Instruction: "Repair the missing validation evidence in the source finalization path and verify the previously failing check on the pinned source", Reason: "Repair the publication protocol", Evidence: []string{"failed-attempt"}}, CommentID: "comment"}
					}
				}
				encoded, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(encoded))), Request: req}, nil
			})}})
			if err != nil {
				t.Fatal(err)
			}
			native, err := client.Native("org_example", "prj_example")
			if err != nil {
				t.Fatal(err)
			}
			owner := &progressOwner{client: native, request: evidence, lease: tracker.NativeLease{ID: "lease", WorkItemID: "wi_example", FencingToken: 7}}
			backend := &nativeProgressBackend{}
			_, checked, err := progresscheck.Check(t.Context(), evidence, backend, owner)
			if (err != nil) != test.wantError || checked == test.wantError || reads != test.wantReads || writes != test.wantWrites || backend.calls != 1 {
				t.Fatalf("checked=%v err=%v reads=%d writes=%d turns=%d", checked, err, reads, writes, backend.calls)
			}
		})
	}
}

func TestProgressPublicationNegotiatesDecisionFields(t *testing.T) {
	for _, modern := range []bool{false, true} {
		t.Run(fmt.Sprintf("investigation capability=%t", modern), func(t *testing.T) {
			request := progresscheck.Request{ProjectID: "prj_example", IssueID: "wi_example", PolicyID: "policy", EvidenceRevision: "revision", Instance: "machine", Attempts: []progresscheck.Attempt{{ID: "failed", Outcome: "failed"}}, AvailableSteps: []string{"implement"}}
			id, err := request.ID()
			if err != nil {
				t.Fatal(err)
			}
			capabilityReads, writes := 0, 0
			client, err := New(Config{URL: "https://hub.example.test", TokenSource: func() string { return "worker" }, HTTPClient: &http.Client{Transport: executionRoundTrip(func(req *http.Request) (*http.Response, error) {
				var body any
				switch {
				case req.URL.Path == "/api/v2/capabilities":
					capabilityReads++
					features := []string{"native_issues", "scoped_collaboration", "repository_policy"}
					if modern {
						features = append(features, progresscheck.InvestigationCapability)
					}
					body = nativeCapabilities{ProtocolMajors: []int{2}, EventSchemas: []int{1}, Features: features}
				case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/prj_example"):
					body = tracker.NativeProject{Profile: "native"}
				case req.Method == http.MethodGet:
					body = tracker.NativeIssue{NativeReference: tracker.NativeReference{Revision: 7}}
				default:
					writes++
					var input progresscheck.Publication
					decoder := json.NewDecoder(req.Body)
					if err := decoder.Decode(&input); err != nil {
						t.Fatal(err)
					}
					if input.Decision.Legacy() == modern {
						t.Fatalf("incorrect negotiated shape: %+v", input.Decision)
					}
					if !modern {
						encoded, err := json.Marshal(input.Decision)
						if err != nil {
							t.Fatal(err)
						}
						var fields map[string]json.RawMessage
						if err := json.Unmarshal(encoded, &fields); err != nil {
							t.Fatal(err)
						}
						for _, name := range []string{"diagnosis", "instruction", "convergence"} {
							if _, found := fields[name]; found {
								t.Fatalf("legacy Hub received new field %s", name)
							}
						}
					}
					body = progresscheck.Record{RequestID: id, ProjectID: request.ProjectID, IssueID: request.IssueID, EvidenceRevision: request.EvidenceRevision, Decision: input.Decision, CommentID: "comment"}
				}
				encoded, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(encoded))), Request: req}, nil
			})}})
			if err != nil {
				t.Fatal(err)
			}
			native, err := client.Native("org_example", "prj_example")
			if err != nil {
				t.Fatal(err)
			}
			if err := native.Negotiate(t.Context()); err != nil {
				t.Fatal(err)
			}
			owner := &progressOwner{client: native, request: request, lease: tracker.NativeLease{WorkItemID: "wi_example", ID: "lease", FencingToken: 7}}
			decision := progresscheck.Decision{Outcome: progresscheck.Reroute, Step: "implement", Reason: "Repair the assertion", Diagnosis: "failed: assertion differs", Instruction: "Repair assertion", Evidence: []string{"failed"}}
			record, err := owner.Publish(t.Context(), progresscheck.Record{RequestID: id, Decision: decision})
			if err != nil || record.Validate(request) != nil || capabilityReads != 1 || writes != 1 {
				t.Fatalf("publication err=%v capability reads=%d writes=%d record=%+v", err, capabilityReads, writes, record)
			}
		})
	}
}
