package nativechange

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/hubclient/diagnostics"
	executioncontract "github.com/digitaldrywood/detent/internal/hubclient/execution"
	"github.com/digitaldrywood/detent/internal/hubclient/internal/testsupport"
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
			err := e.validateProviderStart(test.identity, "")
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
	first := config.AgentBackend{ID: report.Backend, Kind: config.AgentBackendCodex, Protocol: "app-server", Command: "test-account-one"}
	second := first
	second.Command = "test-account-two"
	for _, test := range []struct {
		name    string
		actual  config.AgentBackend
		missing bool
		refused bool
	}{
		{name: "landing audit uses the reserved local account", actual: first},
		{name: "landing audit refuses another project's account with the same backend ID", actual: second, refused: true},
		{name: "landing audit refuses an unbound local report", actual: first, missing: true, refused: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			current := report
			if !test.missing {
				current.LocalBackendIdentity = config.ProviderIdentity(first)
			}
			audit := selected
			audit.Role = runner.RoleSecurityAudit
			lease := lease
			lease.ProviderReservation = &providercapacity.Reservation{Requirement: providercapacity.Requirement{Role: audit.Role, Backend: audit.Backend, Model: audit.Model}, Report: report}
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			e := &nativeExecution{scheduler: &executionAuthority{now: func() time.Time { return now }, providerReports: func() ([]providercapacity.Report, error) {
				return []providercapacity.Report{current}, nil
			}}, claim: nativeClaim{source: source, lease: lease}, cancel: cancel}
			err := e.validateProviderStart(audit, config.ProviderIdentity(test.actual))
			if test.refused {
				if !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) || !strings.Contains(err.Error(), "backend identity conflicts") {
					t.Fatalf("audit could start with the wrong local account: %v", err)
				}
				return
			}
			if err != nil || ctx.Err() != nil {
				t.Fatalf("audit could not start with the reserved local account: %v", err)
			}
		})
	}
}

func TestReviewedMergingClaimReservesProvider(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name                     string
		conflict, accountChanged bool
	}{
		{name: "registers and lands"},
		{name: "pending conflict waits for capacity", conflict: true},
		{name: "conflict refuses account changed after registration", conflict: true, accountChanged: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			testReviewedMergingClaimReservesProvider(t, test.conflict, test.accountChanged)
		})
	}
}

func testReviewedMergingClaimReservesProvider(t *testing.T, conflict, accountChanged bool) {
	h := newNativeChangeHubTransport(t, "Merging", []tracker.NativeState{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Merging"}},
		{Name: "Merging", Dispatchable: true, Transitions: []string{"Done"}},
		{Name: "Done", Terminal: true},
	}, true)
	requirement := providercapacity.Requirement{Role: runner.RoleMerge, Backend: "codex", Model: "test-model"}
	var changedAccount atomic.Bool
	reports := func() ([]providercapacity.Report, error) {
		account, shared := "local", "team"
		if changedAccount.Load() {
			account, shared = "other", "other-team"
		}
		return []providercapacity.Report{{Provider: "openai", Backend: "codex", AccountAlias: account, SharedAccountAlias: shared,
			Models: []string{"test-model"}, MaxConcurrent: 1, Availability: "available", ObservedAt: h.clock()}}, nil
	}
	fixtureConfigureScheduler(t, h, func(cfg *SchedulerConfig) { cfg.ProviderReports = reports })
	var claimReason string
	fetch := func(issueID string) ([]connector.Issue, error) {
		return h.scheduler.FetchCandidateIssues(t.Context(), orchestrator.SchedulingRequest{
			ProjectID: "local", Policy: h.descriptor, WorkflowStates: []string{"Todo", "In Progress"},
			CandidateReady: func(_ context.Context, issue connector.Issue) bool { return issue.ID == issueID },
			CandidateClaimObserved: func(_ context.Context, _ connector.Issue, _ bool, reason string, _ *providercapacity.Requirement) {
				claimReason = reason
			},
			ProviderRequirement: func(_ context.Context, issue connector.Issue, _ []providercapacity.Report) (providercapacity.Requirement, error) {
				selected := requirement
				if issue.State != "Merging" {
					selected.Role = runner.RoleCode
				}
				return selected, nil
			},
		})
	}
	source := nativeChangeSourceRepo(t)
	remote := filepath.Join(nativeChangeTempDir(t), "origin.git")
	h.configureCloneURL(remote)
	nativeChangeGit(t, source, "init", "--bare", "-b", "main", remote)
	nativeChangeGit(t, source, "remote", "add", "origin", nativeChangeRepository)
	nativeChangeGit(t, source, "config", "url."+remote+".insteadOf", nativeChangeRepository)
	nativeChangeGit(t, source, "push", "-u", "origin", "main")
	headAt := func(dir, ref string) string {
		t.Helper()
		command := exec.CommandContext(t.Context(), "git", "rev-parse", ref)
		command.Dir = dir
		output, err := command.Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(output))
	}
	base := headAt(source, "HEAD")
	nativeChangeGit(t, source, "checkout", "-b", "reviewed")
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("reviewed change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	nativeChangeGit(t, source, "commit", "-am", "prepare reviewed change")
	head := headAt(source, "HEAD")
	capture, err := workspace.CaptureChangeSource(t.Context(), source, base, head)
	if err != nil {
		t.Fatal(err)
	}
	nativeChangeGit(t, source, "checkout", "main")
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
	version := h.publishCaptured(t, item, change.ID, head, &capture, "", base)
	if _, err := h.admin.ReviewChange(t.Context(), item, change.ID, version.ID, tracker.ReviewChange{Mutation: nativeMutationKey(), Decision: "approved", Body: "Reviewed the current version"}); err != nil {
		t.Fatal(err)
	}
	if conflict {
		items, err := fetch(issue.ID)
		if err != nil || len(items) != 1 || items[0].ID != issue.ID {
			t.Fatalf("claim to record prior conflict: %+v, %v", items, err)
		}
		if _, err := h.scheduler.AdoptClaim(t.Context(), items[0], h.clock()); err != nil {
			t.Fatal(err)
		}
		prior := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
		reservation := prior.ProviderCapacity()
		if reservation == nil {
			t.Fatal("prior attempt did not reserve its provider")
		}
		if err := prior.Start(t.Context(), tracker.NativeExecutionIdentity{Role: reservation.Role, Backend: reservation.Backend, Model: reservation.Model}); err != nil {
			t.Fatal(err)
		}
		if err := prior.ObserveLanding(t.Context(), runner.NativeLanding{ChangeID: change.ID, VersionID: version.ID, HeadSHA: head,
			BaseSHA: base, Path: "merge_resolution_pending", RefusalKind: workspace.LandRefusalConflict, ConflictFiles: []string{"README.md"}}); err != nil {
			t.Fatal(err)
		}
		if err := prior.Finish(t.Context(), "failed"); err != nil {
			t.Fatal(err)
		}
		if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "failed"); err != nil && !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) {
			t.Fatal(err)
		}
		detail, err := h.admin.CurrentChange(t.Context(), item)
		if err != nil || len(detail.Versions) == 0 || detail.Versions[0].Landing == nil || detail.Versions[0].Landing.Path != "merge_resolution_pending" {
			t.Fatalf("reviewed version did not retain the conflict: %+v, %v", detail.Versions, err)
		}
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
	if conflict {
		occupied := h.createInProgress(t, "Occupy the provider account")
		other := h.otherWorker(t)
		holder, err := newTestScheduler(other.Transport(), SchedulerConfig{
			OrganizationID: h.organization, NativeProjects: map[string]tracker.ProjectID{"local": h.project},
			Machine:         Machine{ID: testsupport.NativeChangeOtherMachineID, Hostname: "other-host", Capacity: 1, Version: "test"},
			ProviderReports: reports, Now: h.clock, HeartbeatInterval: time.Second, LeaseTTL: 90 * time.Second,
		})
		if err != nil {
			t.Fatalf("configure capacity holder: %v", err)
		}
		items, err := holder.FetchCandidateIssues(t.Context(), orchestrator.SchedulingRequest{
			ProjectID: "local", Policy: h.descriptor, WorkflowStates: []string{"Todo", "In Progress"},
			CandidateReady: func(_ context.Context, issue connector.Issue) bool { return issue.ID == occupied.ID },
			ProviderRequirement: func(context.Context, connector.Issue, []providercapacity.Report) (providercapacity.Requirement, error) {
				return providercapacity.Requirement{Role: runner.RoleCode, Backend: "codex", Model: requirement.Model}, nil
			},
		})
		if err != nil || len(items) != 1 || items[0].ID != occupied.ID {
			t.Fatalf("occupy provider account: %+v, %v", items, err)
		}
		items, err = fetch(issue.ID)
		if err != nil || len(items) != 0 || claimReason != "hub_claim.provider_capacity" {
			t.Fatalf("pending conflict landing did not wait for provider capacity: %+v, %s, %v", items, claimReason, err)
		}
		if err := holder.ReleaseClaim(t.Context(), occupied.ID, "released"); err != nil {
			t.Fatalf("release capacity holder: %v", err)
		}
	}
	candidates, err := fetch(issue.ID)
	if err != nil || len(candidates) != 1 || candidates[0].ID != issue.ID || candidates[0].State != "Merging" || candidates[0].NativeLandingHeadSHA != version.HeadSHA {
		t.Fatalf("historical landing was not claimed for its reviewed head: %+v, %v", candidates, err)
	}
	if _, err := h.scheduler.AdoptClaim(t.Context(), candidates[0], h.clock()); err != nil {
		t.Fatal(err)
	}
	execution := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
	lease := execution.Recovery().Lease
	if reserved := execution.ProviderCapacity(); lease.ID == "" || reserved == nil || reserved.Requirement != requirement {
		t.Fatalf("reviewed landing did not reserve its provider: lease=%q provider=%+v", lease.ID, reserved)
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
	identity := tracker.NativeExecutionIdentity{Role: requirement.Role, Backend: requirement.Backend, Model: requirement.Model}
	if conflict && !accountChanged {
		if err := execution.StartLanding(t.Context(), observation.LocalAttemptID, observation.Generation); err != nil {
			t.Fatalf("start the pending conflict landing: %v", err)
		}
		if err := execution.Start(t.Context(), identity); err != nil {
			t.Fatalf("start merge resolution with the reserved provider: %v", err)
		}
		return
	}
	if accountChanged {
		if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("conflicting upstream change\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		nativeChangeGit(t, source, "commit", "-am", "change the same file on main")
		nativeChangeGit(t, source, "push", "origin", "main")
		target, err := execution.LandingTarget(t.Context())
		if err != nil || !target.MergeResolution {
			t.Fatalf("fixture did not select conflict resolution: %+v, %v", target, err)
		}
		changedAccount.Store(true)
	}
	backend, err := workspace.NewBackend(workspace.KindLocalGit, workspace.LocalGitOptions{
		ProcessSource: emptyProcessSource, Root: filepath.Join(nativeChangeTempDir(t), "workspaces"), SourceRoot: source, AutoBranch: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	provider := &committingAgent{}
	agent, err := runner.NewRunner(runner.Dependencies{
		ProcessSource: emptyProcessSource, ProjectID: "local", Workspace: backend, AgentBackend: provider, Store: api.NewRunnerSessions(h.native),
		Workflow: config.Workflow{Config: config.Config{Gate: gate.Config{Run: "git diff --check"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := agent.Run(t.Context(), runner.RunRequest{
		ProjectID: "local", Issue: candidates[0], Mode: runner.RunModeMerge, Execution: execution,
		AttemptWorkspace: execution.AttemptWorkspace(), WorkAttemptID: observation.LocalAttemptID, Generation: observation.Generation, DeferExecutionFinish: true,
	})
	if accountChanged {
		if !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) || !strings.Contains(err.Error(), "local provider account or model capability changed after dispatch") ||
			result.TurnStarted || provider.calls != 0 || headAt(remote, "refs/heads/main") != headAt(source, "HEAD") {
			t.Fatalf("conflict resolution did not refuse the changed account before its first turn: turns=%d error=%v", provider.calls, err)
		}
		return
	}
	landed := result.NativeLanding
	if err != nil || landed == nil || !landed.Landed || landed.HeadSHA != head || landed.MergeSHA != headAt(remote, "refs/heads/main") || provider.calls != 0 {
		t.Fatalf("registered landing did not publish the reviewed source: %+v, provider turns=%d, %v", landed, provider.calls, err)
	}
	recovery, err := h.admin.Recovery(t.Context(), item)
	if err != nil {
		t.Fatal(err)
	}
	if len(recovery.Attempts) != 1 || recovery.Attempts[0].LeaseID != lease.ID || recovery.Attempts[0].FencingToken != lease.FencingToken ||
		recovery.Attempts[0].Identity == nil || *recovery.Attempts[0].Identity != identity {
		t.Fatalf("landing did not retain its worker and reserved identity on the original lease: %+v", recovery.Attempts)
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
	review string
}

func (h planPolicyHost) PinnedExecutionPolicy(string) policy.Descriptor {
	var pinned policy.Descriptor
	pinned.ID = "policy_plan"
	pinned.Gates.PlanEnabled, pinned.Gates.PlanReview = true, h.review
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
	execution := &nativeExecution{scheduler: &executionAuthority{host: planPolicyHost{review: gate.PlanReviewAutomated}}, claim: nativeClaim{source: source, lease: lease, recovery: recovery}, data: data, reconnecting: true}
	continuation, err := execution.PhaseReceipts(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	if continuation.LastPushed == nil || *continuation.LastPushed != receipt || continuation.Plan == nil || !continuation.Plan.Accepted || continuation.NextPhase != runner.RunModeImplement {
		t.Fatalf("live attempt discarded its approved plan: %+v", continuation)
	}
}

func TestHumanPlanContinuationWaitsForMissingApproval(t *testing.T) {
	t.Parallel()
	for _, review := range []string{gate.PlanReviewHuman, gate.PlanReviewBoth} {
		for _, test := range []struct {
			name       string
			receipt    bool
			checkpoint string
			change     bool
		}{
			{name: "implementation receipt", receipt: true},
			{name: "preserved code checkpoint", checkpoint: runner.RoleCode},
			{name: "preserved rework checkpoint", checkpoint: runner.RoleRework},
			{name: "existing Change", change: true},
		} {
			t.Run(review+"/"+test.name, func(t *testing.T) {
				t.Parallel()
				lease := tracker.NativeLease{ID: "lease_plan", WorkItemID: "wi_plan", FencingToken: 3, PolicyID: "policy_plan"}
				recovery := tracker.NativeRecovery{Lease: lease}
				recovery.Issue.WorkItemID = lease.WorkItemID
				if test.checkpoint != "" {
					recovery.Attempts = []tracker.NativeAttempt{{
						NativeRunData: tracker.NativeRunData{AttemptID: "attempt_prior", Identity: &tracker.NativeExecutionIdentity{Role: test.checkpoint}},
						Checkpoint:    &tracker.NativeCheckpoint{HeadSHA: "head", Resume: "resume_session", WorktreeState: "clean"},
					}}
				}
				if test.change {
					recovery.Change = &tracker.NativeChangeReference{HeadSHA: "head"}
				}
				receipts := []tracker.NativePhaseReceipt{}
				continuation := tracker.NativePhaseContinuation{NextPhase: runner.RunModeImplement}
				if test.receipt {
					receipts = append(receipts, tracker.NativePhaseReceipt{
						Phase: "implement", AttemptID: "attempt_prior", HeadSHA: "head", TreeSHA: "tree", PolicyID: lease.PolicyID,
					})
					continuation.LastPushed = &receipts[0]
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					var payload any = tracker.NativeProject{}
					if strings.HasSuffix(req.URL.Path, "/phases") {
						payload = receipts
						if req.URL.Query().Has("phase") {
							payload = continuation
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
				execution := &nativeExecution{
					scheduler: &executionAuthority{host: planPolicyHost{review: review}},
					claim:     nativeClaim{source: source, lease: lease, recovery: recovery},
				}
				resolved, err := execution.PhaseReceipts(t.Context(), "")
				if err != nil {
					t.Fatal(err)
				}
				if resolved.Plan == nil || !resolved.Plan.NeedsProvider() || resolved.NextPhase != runner.RunModePlan {
					t.Fatalf("recovery did not wait for plan approval: %+v", resolved)
				}
				if test.receipt && (resolved.LastPushed == nil || *resolved.LastPushed != receipts[0]) {
					t.Fatalf("recovery lost the published source: %+v", resolved.LastPushed)
				}
			})
		}
	}
}
