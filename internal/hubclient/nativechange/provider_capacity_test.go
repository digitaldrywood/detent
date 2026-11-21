package nativechange

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/hubclient/diagnostics"
	executioncontract "github.com/digitaldrywood/detent/internal/hubclient/execution"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

// Dispatch must not call any workspace operation before acquiring a claim.
type providerPreviewWorkspace struct{ workspace.Backend }

func TestLocalProviderRevalidation(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	requirement := providercapacity.Requirement{Role: "code", Backend: "codex", Model: "sol"}
	selected := tracker.NativeExecutionIdentity{Role: "code", Backend: "codex", Model: "sol"}
	report := providercapacity.Report{Provider: "openai", Backend: "codex", AccountAlias: "work", SharedAccountAlias: "team", Models: []string{"sol"}, MaxConcurrent: 1, Availability: "available", ObservedAt: now}
	source := &NativeConnector{}
	lease := tracker.NativeLease{ID: "lease_test", WorkItemID: "wi_test", ProviderReservation: &providercapacity.Reservation{Requirement: requirement, Report: report}}
	for _, test := range []struct {
		name     string
		change   func(*providercapacity.Report)
		identity tracker.NativeExecutionIdentity
		wantCode string
	}{
		{"same account", func(*providercapacity.Report) {}, selected, ""},
		{"stale quota", func(r *providercapacity.Report) {
			r.Availability = "exhausted"
			r.ObservedAt = now.Add(-providercapacity.MaxAge)
		}, selected, ""},
		{"exhaustion", func(r *providercapacity.Report) { r.Availability = "exhausted" }, selected, ""},
		{"account change", func(r *providercapacity.Report) { r.AccountAlias = "other" }, selected, "unavailable"},
		{"shared pool change", func(r *providercapacity.Report) { r.SharedAccountAlias = "other" }, selected, "unavailable"},
		{"role changed", func(*providercapacity.Report) {}, tracker.NativeExecutionIdentity{Role: "rework", Backend: "codex", Model: "sol"}, "provider_identity_mismatch_role"},
		{"backend changed", func(*providercapacity.Report) {}, tracker.NativeExecutionIdentity{Role: "code", Backend: "other", Model: "sol"}, "provider_identity_mismatch_backend"},
		{"model changed", func(*providercapacity.Report) {}, tracker.NativeExecutionIdentity{Role: "code", Backend: "codex", Model: "astra"}, "provider_identity_mismatch_model"},
		{"all changed", func(*providercapacity.Report) {}, tracker.NativeExecutionIdentity{Role: "rework", Backend: "other", Model: "private model"}, "provider_identity_mismatch_role_backend_model"},
		{"missing report source", nil, selected, "provider_report_source_missing"},
		{"removed account limit", func(r *providercapacity.Report) { r.MaxConcurrent = 0 }, selected, ""},
		{"invalid report", func(r *providercapacity.Report) { r.MaxConcurrent = -1 }, selected, "unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			current := report
			e := &nativeExecution{scheduler: &executionAuthority{now: func() time.Time { return now }}, claim: nativeClaim{source: source, lease: lease}}
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			e.cancel = cancel
			if test.change != nil {
				test.change(&current)
				e.scheduler.providerReports = func() ([]providercapacity.Report, error) { return []providercapacity.Report{current}, nil }
			}
			remote, _ := nativeSSHExecution(t, t.Context(), e, t.TempDir())
			if got := remote.(runner.ProviderCapacityExecution).ProviderCapacity(); !reflect.DeepEqual(got, e.ProviderCapacity()) {
				t.Fatalf("SSH changed the central reservation: %#v", got)
			}
			err := e.validateProviderStart(test.identity)
			if test.wantCode == "" {
				if err != nil || ctx.Err() != nil {
					t.Fatalf("start error = %v, cancellation = %v", err, context.Cause(ctx))
				}
				return
			}
			if !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) || !errors.Is(context.Cause(ctx), err) {
				t.Fatalf("expected authority rejection, got %v, cancellation = %v", err, context.Cause(ctx))
			}
			claims := []executioncontract.ExecutionClaim{{Source: source, Lease: lease, Execution: e}}
			snapshot := runnerauth.ProjectDiagnostics{ObservedAt: now, Records: diagnostics.Claims(claims, source, now, time.Minute)}
			snapshot.Bound()
			page, err := snapshot.Page("project", "runner", string(lease.WorkItemID), "", "", 1, now)
			if err != nil || len(page.Records) != 1 {
				t.Fatalf("provider start diagnostic page: records=%d error=%v", len(page.Records), err)
			}
			record := page.Records[0]
			if record.HostOperation != "native.provider_start" || record.LatestErrorCode != test.wantCode || record.LatestError != "execution authority unavailable [private error text omitted]" {
				t.Fatalf("operator record = %+v, want redacted provider start with code %q", record, test.wantCode)
			}
		})
	}
}

func TestReviewedMergingClaimRegistersWithoutProvider(t *testing.T) {
	t.Parallel()
	h := newNativeChangeHubTransport(t, "Merging", []tracker.NativeState{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Merging"}},
		{Name: "Merging", Dispatchable: true, Transitions: []string{"Done"}},
		{Name: "Done", Terminal: true},
	}, true)
	h.configureCloneURL(nativeChangeRepository)
	if _, err := h.admin.ApproveChangeReviewPolicy(t.Context(), tracker.ApproveChangeReviewPolicy{
		Mutation: nativeMutationKey(),
		Policy:   tracker.ChangeReviewPolicy{PolicyID: h.descriptor.ID, RequireReview: true, RequiredChecks: []tracker.ChangeCheckSpec{}},
	}); err != nil {
		t.Fatal(err)
	}
	issue := h.createInProgress(t, "Reviewed historical landing")
	item := tracker.NativeWorkItemID(issue.ID)
	change, err := h.admin.CreateChange(t.Context(), item, tracker.CreateChange{Mutation: nativeMutationKey(), Title: issue.Title})
	if err != nil {
		t.Fatal(err)
	}
	version := h.publish(t, item, change.ID, nativeChangeFixtureHead("b"))
	if _, err := h.admin.ReviewChange(t.Context(), item, change.ID, version.ID, tracker.ReviewChange{Mutation: nativeMutationKey(), Decision: "approved", Body: "Reviewed the current version"}); err != nil {
		t.Fatal(err)
	}
	current, err := h.admin.Issue(t.Context(), item)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != "Merging" {
		if _, err := h.admin.Transition(t.Context(), item, tracker.Transition{Mutation: nativeMutationKey(), ExpectedRevision: current.Revision, State: "Merging", Reason: "user_requested"}); err != nil {
			t.Fatal(err)
		}
	}
	fixtureConfigureScheduler(t, h, func(cfg *SchedulerConfig) {
		cfg.ProviderReports = func() ([]providercapacity.Report, error) {
			return []providercapacity.Report{{Provider: "openai", Backend: "codex", AccountAlias: "local", SharedAccountAlias: "team",
				Models: []string{"test-model"}, MaxConcurrent: 1, Availability: "available", ObservedAt: h.clock()}}, nil
		}
	})
	candidates, err := h.scheduler.FetchCandidateIssues(t.Context(), orchestrator.SchedulingRequest{
		ProjectID: "local", Policy: h.descriptor, WorkflowStates: []string{"Todo", "In Progress"},
		ProviderRequirement: func(context.Context, connector.Issue, []providercapacity.Report) (providercapacity.Requirement, error) {
			return providercapacity.Requirement{}, errors.New("landing unexpectedly asked for a provider")
		},
	})
	if err != nil || len(candidates) != 1 || candidates[0].ID != issue.ID || candidates[0].State != "Merging" || candidates[0].NativeLandingHeadSHA != version.HeadSHA {
		t.Fatalf("historical landing was not claimed for its reviewed head: %+v, %v", candidates, err)
	}
	if _, err := h.scheduler.AdoptClaim(t.Context(), candidates[0], h.clock()); err != nil {
		t.Fatal(err)
	}
	execution := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
	lease := execution.Recovery().Lease
	if lease.ID == "" || execution.ProviderCapacity() != nil {
		t.Fatalf("expected a fenced Git-only claim without a provider: lease=%q provider=%+v", lease.ID, execution.ProviderCapacity())
	}
	now := h.clock().UTC().Truncate(time.Microsecond)
	worker := tracker.NativeAttemptWorker{PID: 500, PGID: 500, StartedAt: now, StartKey: "fixture-start"}
	observation := tracker.NativeRuntimeObservation{Phase: "merging", HeartbeatAt: now, LocalAttemptID: 1, Generation: 1,
		WorkerLaunch: &tracker.NativeWorkerLaunch{
			PID: int(worker.PID), GroupID: int(worker.PGID), ProcessStart: now,
			PolicyID: lease.PolicyID, ExecutionVersion: "test", ControlVersion: "test",
			ExecutionCommit: strings.Repeat("a", 40), ExecutionSHA256: strings.Repeat("b", 64),
			Protocol: runner.SSHProtocolVersion, SelectedAt: now,
		}}
	primary, err := execution.RegisterAttemptWorker(t.Context(), worker, observation)
	if err != nil || !primary {
		t.Fatalf("register reviewed landing worker: primary=%t error=%v", primary, err)
	}
	if err := execution.StartLanding(t.Context(), observation.LocalAttemptID, observation.Generation); err != nil {
		t.Fatal(err)
	}
	if err := execution.FlushRuntime(t.Context()); err != nil {
		t.Fatal(err)
	}
	recovery, err := h.admin.Recovery(t.Context(), item)
	if err != nil {
		t.Fatal(err)
	}
	identity := tracker.NativeExecutionIdentity{Role: "merge", Backend: "git", Model: "none"}
	if len(recovery.Attempts) != 1 || recovery.Attempts[0].LeaseID != lease.ID || recovery.Attempts[0].FencingToken != lease.FencingToken ||
		recovery.Attempts[0].Identity == nil || *recovery.Attempts[0].Identity != identity {
		t.Fatalf("registered landing did not start as Git on its original lease: %+v", recovery.Attempts)
	}
	if err := execution.Start(t.Context(), tracker.NativeExecutionIdentity{Role: "code", Backend: "codex", Model: "test-model"}); !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) {
		t.Fatalf("landing worker could start a model without a provider reservation: %v", err)
	}
}

func TestRecordedAttemptChecksProviderBeforeRestart(t *testing.T) {
	t.Parallel()
	identity := tracker.NativeExecutionIdentity{Role: "code", Backend: "codex", Model: "test"}
	for _, role := range []string{"", "plan"} {
		t.Run("reserved "+role, func(t *testing.T) {
			var permit *providercapacity.Reservation
			if role != "" {
				permit = &providercapacity.Reservation{Requirement: providercapacity.Requirement{Role: role, Backend: "codex", Model: "test"}}
			}
			e := &nativeExecution{
				claim: nativeClaim{lease: tracker.NativeLease{ProviderReservation: permit}},
				scheduler: &executionAuthority{
					now:             time.Now,
					providerReports: func() ([]providercapacity.Report, error) { return nil, nil },
				},
				data: tracker.NativeRunData{Identity: &identity, Sequence: 1},
			}
			if err := e.Start(t.Context(), identity); !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) {
				t.Fatalf("stored attempt can start code without its provider permit: %v", err)
			}
		})
	}
}

type planPolicyHost struct {
	executioncontract.ExecutionHost
}

func (planPolicyHost) PinnedExecutionPolicy(string) policy.Descriptor {
	var pinned policy.Descriptor
	pinned.Gates.PlanEnabled = true
	return pinned
}

func TestLivePlanContinuationKeepsSameAttemptReceipt(t *testing.T) {
	t.Parallel()
	lease := tracker.NativeLease{ID: "lease_plan", WorkItemID: "wi_plan", FencingToken: 3, PolicyID: "policy_plan"}
	recovery := tracker.NativeRecovery{Lease: lease}
	recovery.Issue.WorkItemID, recovery.Issue.Title = lease.WorkItemID, "Plan implementation"
	step := tracker.NativeStepReceipt{
		Step: "plan", PolicyID: lease.PolicyID, ReviewState: tracker.PlanIssueDigest(recovery),
		Source:         tracker.NativeCheckpoint{HeadSHA: "head", WorkspaceDigest: "tree"},
		CompletionBody: "Test empty inputs.", PlanApproval: "automated:APPROVED",
	}
	data := tracker.NativeRunData{AttemptID: "attempt_plan", LeaseID: lease.ID, FencingToken: lease.FencingToken, PolicyID: lease.PolicyID, Steps: []tracker.NativeStepReceipt{step}}
	recovery.Attempts = []tracker.NativeAttempt{{NativeRunData: data, Status: "running"}}
	receipt := tracker.NativePhaseReceipt{Mutation: tracker.Mutation{LeaseID: lease.ID, FencingToken: lease.FencingToken}, Phase: "plan", AttemptID: data.AttemptID, PolicyID: lease.PolicyID, HeadSHA: "head", TreeSHA: "tree"}
	receipt.InputDigest = tracker.PlanPhaseDigest(receipt, step)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var payload any = tracker.NativeProject{}
		if strings.HasSuffix(req.URL.Path, "/phases") {
			payload = []tracker.NativePhaseReceipt{receipt}
			if req.URL.Query().Has("phase") {
				payload = tracker.NativePhaseContinuation{LastPushed: &receipt, NextPhase: runner.RunModeImplement}
			}
		}
		if err := json.NewEncoder(w).Encode(payload); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	client, err := api.New(api.Config{URL: server.URL, TokenSource: func() string { return "test-token" }, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	native, err := client.Native("org_test", "prj_test")
	if err != nil {
		t.Fatal(err)
	}
	source, err := api.NewNativeConnector(native)
	if err != nil {
		t.Fatal(err)
	}
	execution := &nativeExecution{scheduler: &executionAuthority{host: planPolicyHost{}}, claim: nativeClaim{source: source, lease: lease, recovery: recovery}, data: data, reconnecting: true}
	continuation, err := execution.PhaseReceipts(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	if continuation.LastPushed == nil || *continuation.LastPushed != receipt || continuation.Plan == nil || !continuation.Plan.Accepted || continuation.NextPhase != runner.RunModeImplement {
		t.Fatalf("live attempt discarded its approved plan: %+v", continuation)
	}
}
