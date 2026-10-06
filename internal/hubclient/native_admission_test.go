package hubclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func TestNativeAdmissionBatch(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	t.Parallel()
	for _, test := range []struct {
		name         string
		slots        int
		budget       int
		waiting      int
		preview      bool
		failAfter    int
		failClaim    int
		want         int
		failCode     string
		failStatus   int
		failPreview  bool
		emptyPreview bool
		wantError    bool
	}{
		{name: "six slots", slots: 6, budget: 14, preview: true, want: 6},
		{name: "project slots", slots: 2, budget: 10, preview: true, want: 2},
		{name: "one evaluation budget", slots: 6, budget: 3, preview: true, want: 3},
		{name: "waiting head", slots: 6, budget: 6, waiting: 4, preview: true, want: 2},
		{name: "direct claims", slots: 6, budget: 14, want: 6},
		{name: "hydration releases whole batch", slots: 6, budget: 14, preview: true, failAfter: 2},
		{name: "claim failure releases partial batch", slots: 6, budget: 14, preview: true, failClaim: 3},
		{name: "direct provider full", slots: 6, budget: 14, failClaim: 1, failCode: "provider_capacity"},
		{name: "direct runner full", slots: 6, budget: 14, failClaim: 1, failCode: "runner_capacity"},
		{name: "direct host full", slots: 6, budget: 14, failClaim: 1, failCode: "host_capacity"},
		{name: "preview provider full", slots: 6, budget: 14, preview: true, failPreview: true, failCode: "provider_capacity"},
		{name: "preview runner full", slots: 6, budget: 14, preview: true, failPreview: true, failCode: "runner_capacity"},
		{name: "preview host full", slots: 6, budget: 14, preview: true, failPreview: true, failCode: "host_capacity"},
		{name: "preview claim provider full", slots: 6, budget: 14, preview: true, failClaim: 1, failCode: "provider_capacity"},
		{name: "preview claim runner full", slots: 6, budget: 14, preview: true, failClaim: 1, failCode: "runner_capacity"},
		{name: "preview claim host full", slots: 6, budget: 14, preview: true, failClaim: 1, failCode: "host_capacity"},
		{name: "empty provider preview strict provider full", slots: 6, budget: 14, preview: true, emptyPreview: true, failClaim: 1, failCode: "provider_capacity"},
		{name: "empty provider preview strict runner full", slots: 6, budget: 14, preview: true, emptyPreview: true, failClaim: 1, failCode: "runner_capacity"},
		{name: "empty provider preview strict host full", slots: 6, budget: 14, preview: true, emptyPreview: true, failClaim: 1, failCode: "host_capacity"},
		{name: "partial provider full", slots: 6, budget: 14, preview: true, failClaim: 3, failCode: "provider_capacity", want: 2},
		{name: "partial runner full", slots: 6, budget: 14, preview: true, failClaim: 3, failCode: "runner_capacity", want: 2},
		{name: "partial host full", slots: 6, budget: 14, failClaim: 3, failCode: "host_capacity", want: 2},
		{name: "unauthorized", slots: 6, budget: 14, failClaim: 1, failCode: "unauthorized", failStatus: http.StatusUnauthorized, wantError: true},
		{name: "policy denied releases partial batch", slots: 6, budget: 14, preview: true, failClaim: 3, failCode: "claim_not_permitted", failStatus: http.StatusForbidden, wantError: true},
		{name: "unrelated conflict releases partial batch", slots: 6, budget: 14, preview: true, failClaim: 3, failCode: "policy_mismatch", wantError: true},
		{name: "draining", slots: 6, budget: 14, preview: true, failPreview: true, failCode: "runner_draining", wantError: true},
		{name: "disabled", slots: 6, budget: 14, failClaim: 1, failCode: "runner_disabled", wantError: true},
		{name: "offline", slots: 6, budget: 14, failClaim: 1, failCode: "runner_offline", wantError: true},
		{name: "malformed preview", slots: 6, budget: 14, preview: true, failPreview: true, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := newNativeChangeHubTransport(t, "In Review", []tracker.NativeState{{Name: "Todo", Dispatchable: true}}, true)
			h.scheduler.machine.Capacity = 6
			for i := range 8 {
				if _, err := h.admin.CreateIssue(t.Context(), tracker.CreateIssue{Mutation: nativeMutationKey(), Title: fmt.Sprintf("work-%d", i), State: "Todo"}); err != nil {
					t.Fatal(err)
				}
			}
			evaluations := 0
			var readyIDs []string
			request := orchestrator.SchedulingRequest{ProjectID: "local", Policy: h.descriptor, WorkflowStates: []string{"Todo"}, AdmissionLimit: test.slots, CandidateLimit: test.budget}
			if test.emptyPreview {
				h.scheduler.providerReports = func() ([]providercapacity.Report, error) { return nil, nil }
				request.ProviderRequirement = func(context.Context, connector.Issue, []providercapacity.Report) (providercapacity.Requirement, error) {
					t.Error("empty preview resolved a provider requirement")
					return providercapacity.Requirement{}, nil
				}
			}
			if test.preview {
				request.CandidateReady = func(_ context.Context, issue connector.Issue) bool {
					evaluations++
					if evaluations <= test.waiting {
						return false
					}
					readyIDs = append(readyIDs, issue.ID)
					return true
				}
			}
			transport := h.failChanges.next
			recoveries := 0
			claims := 0
			h.failChanges.next = executionRoundTrip(func(r *http.Request) (*http.Response, error) {
				if test.emptyPreview && strings.HasSuffix(r.URL.Path, "/claims/preview") {
					response := httptest.NewRecorder()
					response.WriteString(`{"items":[]}`)
					return response.Result(), nil
				}
				if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/claims") {
					claims++
					if claims == test.failClaim && test.failCode == "" {
						return nil, errors.New("injected claim failure")
					}
				}
				if test.failPreview && strings.HasSuffix(r.URL.Path, "/claims/preview") || test.failCode != "" && claims == test.failClaim && r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/claims") {
					response := httptest.NewRecorder()
					if test.failCode == "" {
						response.WriteString(`{"items":`)
					} else {
						status := test.failStatus
						if status == 0 {
							status = http.StatusConflict
						}
						response.WriteHeader(status)
						fmt.Fprintf(response, `{"code":%q,"message":"injected refusal"}`, test.failCode)
					}
					return response.Result(), nil
				}
				if r.Method == http.MethodGet && r.URL.Query().Get("view") == "recovery" {
					recoveries++
					if recoveries == test.failAfter {
						return nil, errors.New("injected hydration failure")
					}
				}
				return transport.RoundTrip(r)
			})
			issues, err := h.scheduler.FetchCandidateIssues(t.Context(), request)
			if test.failAfter != 0 || test.failClaim != 0 && test.failCode == "" || test.wantError {
				if err == nil || len(issues) != 0 {
					t.Fatalf("hydration failure = %d issues, %v", len(issues), err)
				}
				if test.failCode != "" {
					var failure *APIError
					if !errors.As(err, &failure) || failure.Code != test.failCode {
						t.Fatalf("refusal lost its typed owner: %v", err)
					}
				}
				h.failChanges.next = transport
				readyIDs = nil
				issues, err = h.scheduler.FetchCandidateIssues(t.Context(), request)
				if err != nil || len(issues) != 6 {
					t.Fatalf("failed batch retained leases: %d issues, %v", len(issues), err)
				}
			} else if err != nil || len(issues) != test.want || evaluations > test.budget {
				t.Fatalf("admitted=%d evaluated=%d error=%v, want %d", len(issues), evaluations, err, test.want)
			}
			if test.failCode != "" && !test.wantError && (claims != test.failClaim || len(h.scheduler.nativeClaims) != test.want) {
				t.Fatalf("capacity refusal overclaimed: claims=%d retained=%d", claims, len(h.scheduler.nativeClaims))
			}
			h.failChanges.next = transport
			h.scheduler.providerReports = nil
			request.ProviderRequirement = nil
			seen := make(map[string]bool)
			sessions := make(map[string]bool)
			for i, issue := range issues {
				lease := h.scheduler.nativeClaims[issue.ID].lease
				if seen[issue.ID] || sessions[lease.SessionID] || lease.FencingToken == 0 || lease.PolicyID != h.descriptor.ID {
					t.Fatalf("batch lost distinct fenced claims: %+v", lease)
				}
				seen[issue.ID], sessions[lease.SessionID] = true, true
				if test.preview && issue.ID != readyIDs[i] {
					t.Fatalf("queue order = %s at %d, want %s", issue.ID, i, readyIDs[i])
				}
				if _, err := h.scheduler.AdoptClaim(t.Context(), issue, time.Now()); err != nil {
					t.Fatal(err)
				}
				execution := h.scheduler.RunExecution(issue.ID)
				if err := execution.Start(t.Context(), tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "test"}); err != nil {
					t.Fatal(err)
				}
				recovery, err := h.admin.Recovery(t.Context(), tracker.NativeWorkItemID(issue.ID))
				if err != nil || len(recovery.Attempts) != 1 || recovery.Attempts[0].Status != "running" {
					t.Fatalf("batch start missing: %+v, %v", recovery.Attempts, err)
				}
			}
			for _, issue := range issues {
				if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "dispatch_deferred"); err != nil {
					t.Fatal(err)
				}
			}
			request.CandidateReady = nil
			if again, err := h.scheduler.FetchCandidateIssues(t.Context(), request); err != nil || len(again) != min(test.slots, test.budget) {
				t.Fatalf("released batch did not free slots: %d, %v", len(again), err)
			}
		})
	}
}

type nativeDependencyRecoveryRunner struct {
	started chan runner.RunRequest
}

func (r nativeDependencyRecoveryRunner) Run(ctx context.Context, request runner.RunRequest) (runner.RunResult, error) {
	r.started <- request
	<-ctx.Done()
	return runner.RunResult{}, ctx.Err()
}

func TestNativeRecordedDependencyAdmission(t *testing.T) {
	for _, test := range []struct {
		name, refusal string
		qualified     bool
		advancePolicy bool
		dirty         bool
	}{
		{name: "reported prerequisite without typed relation"},
		{name: "canonical native reference", qualified: true},
		{name: "dirty1067 canonical prerequisite retains checkpoint and recovery", qualified: true, dirty: true},
		{name: "new approved policy recovers historical report", advancePolicy: true},
		{name: "human action retains hold", refusal: "human"},
		{name: "operator transition retains hold", refusal: "operator"},
		{name: "stale revision cannot recover", refusal: "revision"},
		{name: "stale fence cannot recover", refusal: "fence"},
		{name: "stale report cannot recover", refusal: "report"},
		{name: "unrelated native dependency retains hold", refusal: "relation"},
		{name: "policy change retains hold", refusal: "policy"},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newNativeChangeHubDependencies(t, "Human Review", []tracker.NativeState{
				{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress"}},
				{Name: "In Progress", Dispatchable: true, Transitions: []string{"Blocked", "Human Review"}},
				{Name: "Blocked", Transitions: []string{"In Progress", "Blocked"}},
				{Name: "Human Review"},
				{Name: "Backlog", Transitions: []string{"Done"}},
				{Name: "Done", Terminal: true, Transitions: []string{"Backlog"}},
			}, true, true)
			prerequisite, err := h.admin.CreateIssue(t.Context(), tracker.CreateIssue{Mutation: nativeMutationKey(), Title: "Actual prerequisite", State: "Backlog"})
			if err != nil {
				t.Fatal(err)
			}
			issue := h.createInProgress(t, "Reported dependency")
			var unrelated tracker.NativeIssue
			if test.refusal == "relation" {
				other, err := h.admin.CreateIssue(t.Context(), tracker.CreateIssue{Mutation: nativeMutationKey(), Title: "Unrelated prerequisite", State: "Done"})
				if err != nil {
					t.Fatal(err)
				}
				unrelated = other
				if err := h.connector.AddIssueDependency(t.Context(), issue.ID, string(other.WorkItemID)); err != nil {
					t.Fatal(err)
				}
			}
			h.claim(t, issue.ID)
			if unrelated.WorkItemID != "" {
				if _, err := h.admin.Transition(t.Context(), unrelated.WorkItemID, tracker.Transition{Mutation: nativeMutationKey(), ExpectedRevision: unrelated.Revision, State: "Backlog", Reason: "user_requested"}); err != nil {
					t.Fatal(err)
				}
			}
			execution := h.scheduler.RunExecution(issue.ID)
			if err := execution.Start(t.Context(), tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "test"}); err != nil {
				t.Fatal(err)
			}
			checkpoint := tracker.NativeCheckpoint{Resume: "fresh_checkout", Availability: "available", Storage: "local_only", WorktreeState: "clean", HeadSHA: strings.Repeat("a", 40), ExternalEffect: "none", EffectState: "none"}
			diff := nativeChangeDiff(checkpoint.HeadSHA)
			if test.dirty {
				checkpoint.Resume, checkpoint.WorktreeState = "resume_session", "dirty"
				checkpoint.HeadSHA = "60c199a71dfe408cc26e730b9567c8606985ebfb"
				checkpoint.WorkspaceDigest = strings.Repeat("b", 64)
				diff = nativeChangeDiff(checkpoint.HeadSHA, "inventory.go", "inventory_test.go")
			}
			execution.(runner.DiffExecution).SetDiffSource(diff)
			if err := execution.Checkpoint(t.Context(), checkpoint); err != nil {
				t.Fatal(err)
			}
			reference := fmt.Sprintf("#%d", prerequisite.Number)
			if test.qualified {
				reference = string(h.project) + reference
			}
			human := "null"
			if test.refusal == "human" {
				human = "Approve the release"
			}
			report := fmt.Sprintf("```detent-status\nschema: 1\nstatus: blocked\nblockers:\n  - ref: '%s'\n    reason: prerequisite must finish\n    owner: orchestrator\n    predicate:\n      type: issue_state\n      states: [open]\nhuman_action: %s\n```", reference, human)
			if test.dirty {
				report = fmt.Sprintf("```detent-status\nschema: 1\nstatus: blocked\nblockers:\n  - ref: '%s'\n    reason: prerequisite must finish\nhuman_action: null\n```", reference)
			}
			if err := execution.(runner.CompletionExecution).PrepareFinish(t.Context(), "succeeded", report, nil); err != nil {
				t.Fatal(err)
			}
			if err := execution.Finish(t.Context(), "succeeded"); err != nil {
				t.Fatal(err)
			}
			if test.dirty && (execution.(runner.ChangeExecution).NativeChange() != nil || len(h.changes(t, issue.ID)) != 0) {
				t.Fatal("dirty prerequisite report published unfinished source")
			}
			if err := h.connector.UpdateIssueState(t.Context(), issue.ID, "Blocked"); err != nil {
				t.Fatal(err)
			}
			if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "completed"); err != nil {
				t.Fatal(err)
			}
			blocked, err := h.native.Issue(t.Context(), tracker.NativeWorkItemID(issue.ID))
			if err != nil {
				t.Fatal(err)
			}
			if test.refusal != "relation" && len(blocked.Dependencies) != 0 {
				t.Fatalf("fixture fabricated typed dependencies: %+v", blocked.Dependencies)
			}
			hydrated, err := h.connector.FetchIssueStatesByIDs(t.Context(), []string{issue.ID})
			if err != nil || len(hydrated) != 1 || hydrated[0].WorkpadSignal == nil || len(hydrated[0].WithNativeWorkpadAuthority().WorkpadSignal.Blockers) != 1 {
				t.Fatalf("native disposition lost its prerequisite: %+v, %v", hydrated, err)
			}
			resolved, err := h.connector.FetchIssueStatesByIdentifiers(t.Context(), []string{string(h.project) + fmt.Sprintf("#%d", prerequisite.Number)})
			if err != nil || len(resolved) != 1 || resolved[0].ID != string(prerequisite.WorkItemID) || resolved[0].Closed {
				t.Fatalf("native reference resolved incorrectly: %+v, %v", resolved, err)
			}
			attempts, history, err := h.connector.recordedBlockerContext(t.Context(), blocked.WorkItemID)
			if err != nil {
				t.Fatal(err)
			}
			attempt, prior, valid := tracker.RecordedNativeBlockers(blocked, attempts, history)
			if !valid || prior != "In Progress" {
				t.Fatalf("current report missing: %+v, %s, %t", attempt, prior, valid)
			}
			if test.dirty {
				signal, valid := workpad.SignalFromComment(report, "", "")
				if !valid || signal.Invalid != nil || attempt.Status != "succeeded" || !reflect.DeepEqual(attempt.Checkpoint, &checkpoint) || !reflect.DeepEqual(attempt.Disposition.BlockerEvidence, signal.Blockers) {
					t.Fatalf("dirty terminal authority or checkpoint lost: %+v", attempt)
				}
				page, err := h.admin.History(t.Context(), blocked.WorkItemID, "")
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, event := range page.Items {
					if event.Type == "run.finished" && event.Data.Run != nil && event.Data.Run.AttemptID == attempt.AttemptID {
						found = reflect.DeepEqual(event.Data.Run.Disposition, attempt.Disposition)
					}
				}
				if !found {
					t.Fatal("run.finished omitted dirty terminal disposition")
				}
			}
			request := tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "too-early", LeaseID: attempt.LeaseID, FencingToken: attempt.FencingToken}, PolicyID: h.descriptor.ID, BlockerAttemptID: attempt.AttemptID, ExpectedRevision: blocked.Revision, State: prior, Reason: "dependency_ready", ReasonDetail: "recorded_blocker_recovery"}
			if _, err := h.native.Transition(t.Context(), blocked.WorkItemID, request); err == nil {
				t.Fatal("recovery cleared a genuinely unresolved prerequisite")
			}
			if len(h.candidates(t)) != 0 {
				t.Fatal("blocked prerequisite was admitted")
			}
			if test.refusal != "" {
				if _, err := h.admin.Transition(t.Context(), prerequisite.WorkItemID, tracker.Transition{Mutation: nativeMutationKey(), ExpectedRevision: prerequisite.Revision, State: "Done", Reason: "user_requested"}); err != nil {
					t.Fatal(err)
				}
			}
			request.IdempotencyKey = "ready"
			switch test.refusal {
			case "revision":
				request.ExpectedRevision--
			case "fence":
				request.FencingToken++
			case "report":
				request.BlockerAttemptID = "attempt_unrelated"
			case "operator":
				current, err := h.admin.Transition(t.Context(), blocked.WorkItemID, tracker.Transition{Mutation: nativeMutationKey(), ExpectedRevision: blocked.Revision, State: "Blocked", Reason: "user_requested"})
				if err != nil {
					t.Fatal(err)
				}
				request.ExpectedRevision = current.Revision
			case "policy":
				h.repolicy(t)
			}
			if test.refusal != "" {
				if _, err := h.native.Transition(t.Context(), blocked.WorkItemID, request); err == nil || h.state(t, issue.ID) != "Blocked" {
					t.Fatalf("%s authority was bypassed: %v", test.refusal, err)
				}
				return
			}
			if test.advancePolicy {
				h.repolicy(t)
			}
			runtimeStore, err := store.Open(t.Context(), store.Config{Path: filepath.Join(t.TempDir(), "runtime.db")})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = runtimeStore.Close() })
			started := make(chan runner.RunRequest, 1)
			orch, err := orchestrator.New(orchestrator.Config{Project: scheduler.ProjectCandidate{ID: "local"}, Policy: h.descriptor, PollInterval: 20 * time.Millisecond, MaxConcurrentAgents: 1, ActiveStates: []string{"Todo", "In Progress"}, ObservedStates: []string{"Blocked", "Human Review"}, TerminalStates: []string{"Done"}}, orchestrator.Dependencies{Connector: h.connector, Scheduling: h.scheduler, Runner: nativeDependencyRecoveryRunner{started: started}, WorkAttempts: runtimeStore, LaneLedger: runtimeStore, WorkflowMetrics: runtimeStore, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- orch.Run(ctx) }()
			t.Cleanup(func() { cancel(); <-done })
			deadline := time.After(10 * time.Second)
			for {
				current, err := orch.State(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				entry := current.Blocked[issue.ID]
				if len(entry.BlockerEvidence) == 1 && entry.BlockerEvidence[0].Status == "holds" {
					break
				}
				select {
				case <-started:
					t.Fatal("native recovery dispatched before prerequisite readiness")
				case <-deadline:
					t.Fatalf("native recovery never observed the unmet predicate: %+v", entry)
				case <-time.After(10 * time.Millisecond):
				}
			}
			if _, err := h.admin.Transition(t.Context(), prerequisite.WorkItemID, tracker.Transition{Mutation: nativeMutationKey(), ExpectedRevision: prerequisite.Revision, State: "Done", Reason: "user_requested"}); err != nil {
				t.Fatal(err)
			}
			if _, err := orch.RequestRefresh(t.Context()); err != nil {
				t.Fatal(err)
			}
			select {
			case run := <-started:
				if run.Issue.ID != issue.ID || run.Issue.State != prior {
					t.Fatalf("recovery dispatched wrong lane: %+v", run.Issue)
				}
				if test.dirty {
					fresh, ok := run.Execution.(*nativeExecution)
					if !ok || fresh.claim.lease.FencingToken <= attempt.FencingToken || len(fresh.Recovery().Attempts) != 1 || !reflect.DeepEqual(fresh.Recovery().Attempts[0].Checkpoint, &checkpoint) {
						t.Fatal("prerequisite recovery lost fresh fencing or retained dirty checkpoint")
					}
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("native recorded recovery did not autonomously dispatch: state=%s", h.state(t, issue.ID))
			}
			page, err := h.admin.History(t.Context(), blocked.WorkItemID, "")
			if err != nil {
				t.Fatal(err)
			}
			var recovered, claimed int64
			for _, event := range page.Items {
				if event.Type == "workflow.transitioned" && strings.ReplaceAll(event.Data.ReasonDetail, " ", "_") == "recorded_blocker_recovery" && event.Data.BlockerAttemptID == attempt.AttemptID {
					recovered = event.AggregateSequence
				}
				if event.Type == "scheduler.decision" && event.Data.Decision != nil && event.Data.Decision.Outcome == "claimed" {
					claimed = event.AggregateSequence
				}
			}
			if recovered == 0 || claimed <= recovered {
				t.Fatalf("recovery transition then fresh claim missing: recovered=%d claimed=%d", recovered, claimed)
			}
		})
	}
}

func TestNativeAdmissionCompetingRunners(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	t.Parallel()
	h := newNativeChangeHubTransport(t, "In Review", []tracker.NativeState{{Name: "Todo", Dispatchable: true}}, true)
	h.scheduler.machine.Capacity = 6
	other, err := NewScheduler(h.native.client, SchedulerConfig{OrganizationID: h.organization, NativeProjects: map[string]tracker.ProjectID{"local": h.project}, Machine: Machine{ID: "other-machine", Hostname: "other-host", Version: "test", Capacity: 6}, HeartbeatInterval: time.Second, LeaseTTL: 90 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 8 {
		if _, err := h.admin.CreateIssue(t.Context(), tracker.CreateIssue{Mutation: nativeMutationKey(), Title: fmt.Sprintf("work-%d", i), State: "Todo"}); err != nil {
			t.Fatal(err)
		}
	}
	ready := make(chan struct{})
	var previews atomic.Int32
	transport := h.failChanges.next
	h.failChanges.next = executionRoundTrip(func(r *http.Request) (*http.Response, error) {
		response, err := transport.RoundTrip(r)
		if strings.HasSuffix(r.URL.Path, "/claims/preview") {
			if previews.Add(1) == 2 {
				close(ready)
			}
			select {
			case <-ready:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		}
		return response, err
	})
	type result struct {
		issues []connector.Issue
		err    error
	}
	results := make(chan result, 2)
	for _, scheduler := range []*Scheduler{h.scheduler, other} {
		go func() {
			issues, err := scheduler.FetchCandidateIssues(t.Context(), orchestrator.SchedulingRequest{ProjectID: "local", Policy: h.descriptor, WorkflowStates: []string{"Todo"}, AdmissionLimit: 6, CandidateLimit: 14, CandidateReady: func(context.Context, connector.Issue) bool { return true }})
			results <- result{issues, err}
		}()
	}
	seen := make(map[string]bool)
	for range 2 {
		result := <-results
		if result.err != nil && !errors.Is(result.err, ErrNoClaimableWork) {
			t.Fatal(result.err)
		}
		for _, issue := range result.issues {
			if seen[issue.ID] {
				t.Fatalf("competing runners both admitted %s", issue.ID)
			}
			seen[issue.ID] = true
		}
	}
	if len(seen) != 8 {
		t.Fatalf("competing batches admitted %d distinct issues, want 8", len(seen))
	}
}
