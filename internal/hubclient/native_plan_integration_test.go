package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/codex"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workpad"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestNativePlanContinuation(t *testing.T) {
	for _, test := range []struct {
		name        string
		report      string
		outcome     string
		disposition *tracker.NativeDisposition
		candidate   bool
	}{
		{name: "published plan continues immediately", outcome: "succeeded", disposition: &tracker.NativeDisposition{Status: "in_progress"}, candidate: true},
		{name: "human action remains held", outcome: "succeeded", report: "status: blocked\nhuman_action: Approve the plan", disposition: &tracker.NativeDisposition{Status: "blocked", HumanAction: true}},
		{name: "permission remains held", outcome: "succeeded", report: "status: blocked\nreason_code: permission_wait", disposition: &tracker.NativeDisposition{Status: "blocked", ReasonCode: "permission_wait"}},
		{name: "untyped external blocker stays claimable", candidate: true, outcome: "succeeded", report: "status: blocked\nblockers:\n  - reason: Await upstream approval", disposition: &tracker.NativeDisposition{Status: "blocked", Blockers: true, BlockerEvidence: []workpad.Blocker{{Reason: "Await upstream approval", Owner: workpad.BlockerOwnerHuman, Unverifiable: true}}}},
		{name: "instance limitation leaves admission to the runner", candidate: true, outcome: "succeeded", report: "status: blocked\nreason_code: instance_limitation", disposition: &tracker.NativeDisposition{Status: "blocked", ReasonCode: "instance_limitation"}},
		{name: "instance failure is not a successful plan", outcome: "failed", candidate: true},
		{name: "incomplete plan retains its active claim"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := newNativeChangeHub(t, true)
			held := h.createInProgress(t, "Human decision")
			h.claim(t, held.ID)
			owner := h.scheduler.RunExecution(held.ID).(*nativeExecution)
			if err := owner.Start(t.Context(), tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "test"}); err != nil {
				t.Fatal(err)
			}
			owner.SetDiffSource(nativeChangeDiff(nativeChangeFixtureHead("c")))
			if err := owner.PrepareFinish(t.Context(), "succeeded", "```detent-status\nschema: 1\nstatus: blocked\nblockers: []\nhuman_action: Approve the work\n```", nil); err != nil {
				t.Fatal(err)
			}
			if err := h.scheduler.ReleaseClaim(t.Context(), held.ID, "completed"); err != nil {
				t.Fatal(err)
			}
			issue := h.createInProgress(t, "Planning continuation")
			h.claim(t, issue.ID)
			execution := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
			guarded, stop, err := execution.Guard(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer stop()
			if err := execution.Start(guarded, tracker.NativeExecutionIdentity{Role: runner.RolePlan, Backend: "codex", Model: "test"}); err != nil {
				t.Fatal(err)
			}
			execution.SetDiffSource(nativeChangeDiff(nativeChangeFixtureHead("c")))
			if test.outcome != "" {
				output := ""
				if test.report != "" {
					output = "```detent-status\nschema: 1\n" + test.report + "\n```"
				}
				if err := execution.PrepareFinish(guarded, test.outcome, output, nil); err != nil {
					t.Fatal(err)
				}
				if test.outcome == "succeeded" {
					if err := h.connector.CreateComment(guarded, issue.ID, "## Detent Plan\n\nUpdate the README and run the project checks."); err != nil {
						t.Fatal(err)
					}
				}
				if err := h.scheduler.ReleaseClaim(guarded, issue.ID, "completed"); err != nil {
					t.Fatal(err)
				}
			}
			candidates := h.candidates(t)
			if len(candidates) != 0 && (len(candidates) != 1 || candidates[0].ID != issue.ID) {
				t.Fatalf("planning completion unlocked another item: %+v", candidates)
			}
			if (len(candidates) == 1) != test.candidate {
				evidence, err := h.admin.RuntimeEvidence(t.Context(), tracker.NativeWorkItemID(issue.ID), "")
				t.Fatalf("immediate candidate=%v, want %v; scheduling=%+v, error=%v", len(candidates) == 1, test.candidate, evidence.Scheduling, err)
			}
			recovery, err := h.admin.Recovery(t.Context(), tracker.NativeWorkItemID(issue.ID))
			if err != nil || len(recovery.Attempts) != 1 {
				t.Fatalf("planning receipt=%+v, error=%v", recovery.Attempts, err)
			}
			attempt := recovery.Attempts[0]
			if !reflect.DeepEqual(attempt.Disposition, test.disposition) || attempt.WorkItemRevision != recovery.Issue.Revision {
				t.Fatalf("planning receipt=%+v, want disposition=%+v at revision=%d", attempt, test.disposition, recovery.Issue.Revision)
			}
			if test.candidate {
				if _, err := h.scheduler.AdoptClaim(t.Context(), candidates[0], time.Now()); err != nil {
					t.Fatal(err)
				}
				implementation := h.scheduler.RunExecution(issue.ID)
				if err := implementation.Start(t.Context(), tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "test"}); err != nil {
					t.Fatal(err)
				}
				if err := h.connector.CreateComment(guarded, issue.ID, "## Detent Plan\n\nStale plan"); err == nil {
					t.Fatal("stale planning authority published a plan")
				}
				stale := tracker.NativeRunEvent{Mutation: nativeMutationKey(), Type: "run.finished", SchemaVersion: 1, Data: execution.data}
				stale.Data.Sequence++
				stale.Data.Disposition = &tracker.NativeDisposition{Status: "in_progress"}
				if err := h.native.AppendEvent(t.Context(), tracker.NativeWorkItemID(held.ID), stale); err == nil {
					t.Fatal("stale planning authority unlocked a different item")
				}
				if candidates := h.candidates(t); len(candidates) != 0 {
					t.Fatalf("active implementation claim or human decision lost protection: %+v", candidates)
				}
			}
			comments, err := h.connector.FetchIssueComments(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			wantPlans := 0
			if test.outcome == "succeeded" {
				wantPlans = 1
			}
			if len(comments) != wantPlans {
				t.Fatalf("plan publications=%d, want %d", len(comments), wantPlans)
			}
		})
	}
}

// Replay Cloud retiring a lease on run.finished. The real runner must leave
// publication and the lane handoff to the orchestrator before that retirement.
func TestNativePlannerAutomaticHandoff(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

	isolateNativeChangeGit(t)
	for _, test := range []struct {
		name             string
		abandon          bool
		failure          string
		planRecoveryRole string
		resumeCase       string
	}{
		{name: "automatic handoff"},
		{name: "workspace hook failure before provider", failure: "workspace"},
		{name: "abandon deferred planner and recover", abandon: true},
		{name: "provider failure settles before lease retirement", failure: "provider"},
		{name: "refused start retains fenced accounting", failure: "refused"},
		{name: "owned cleanup failure after a finished turn keeps finished work", failure: "cleanup"},
		{name: "completed provider with exited process preserves staged finalization", failure: "exited"},
		{name: "historical manual checkpoint resumes implementation", failure: "recovery"},
		{name: "planning-enabled Todo resumes after a historical code checkpoint", failure: "recovery", planRecoveryRole: runner.RoleCode},
		{name: "planning-enabled Todo resumes after a historical rework checkpoint", failure: "recovery", planRecoveryRole: runner.RoleRework},
		{name: "planning-enabled dirty checkpoint role mismatch preserves current version", failure: "recovery", planRecoveryRole: runner.RolePlan, resumeCase: "dirty"},
		{name: "planning-enabled clean checkpoint role mismatch restarts from current version", failure: "recovery", planRecoveryRole: runner.RolePlan, resumeCase: "identity"},
		{name: "planning-enabled changed configuration restarts from current version", failure: "recovery", planRecoveryRole: runner.RoleCode, resumeCase: "policy"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.failure != "workspace" {
				t.Parallel()
			}
			testNativePlannerHandoff(t, test.abandon, test.failure, test.planRecoveryRole, test.resumeCase)
		})
	}
}

func testNativePlannerHandoff(t *testing.T, abandon bool, failure, planRecoveryRole, resumeCase string) {
	t.Helper()
	planningRecovery := planRecoveryRole != ""
	manualCheckpoint := failure == "recovery" && resumeCase == ""
	cleanCheckpoint := resumeCase == "identity" || resumeCase == "policy"
	refusalMode, refusalWorker, dispatchState := runner.RunModePlan, "planner", "Todo"
	if resumeCase != "" {
		refusalMode, refusalWorker, dispatchState = runner.RunModeImplement, "agent", "In Progress"
	}
	states := hubserverPlanStates()
	if failure == "recovery" {
		states[0].Transitions = append(states[0].Transitions, "Blocked")
		states[1].Transitions = append(states[1].Transitions, "Blocked")
		states = append(states, tracker.NativeState{Name: "Blocked", Transitions: []string{"Todo"}})
	}
	h := newNativeChangeHubTransport(t, "Human Review", states, true)
	previousPolicyID := h.descriptor.ID
	h.descriptor.Gates.HumanReview = true
	h.descriptor = h.descriptor.WithID()
	if _, err := h.admin.ApproveProjectPolicy(t.Context(), policy.Change{ExpectedID: previousPolicyID, Policy: h.descriptor}); err != nil {
		t.Fatal(err)
	}
	issue, err := h.connector.CreateIssue(t.Context(), connector.IssueDraft{Title: "Plan then implement", Body: "Update the README." + issueContractTestSections})
	if err != nil {
		t.Fatal(err)
	}
	confirmIssueContract(t, h.admin, issue.ID)
	source := nativeChangeSourceRepo(t)
	nativeChangeGit(t, source, "remote", "add", "origin", nativeChangeRepository)
	nativeChangeGit(t, source, "config", "url."+source+".insteadOf", nativeChangeRepository)
	hooks := workspace.Hooks{}
	if failure == "workspace" {
		t.Setenv("WORKSPACE_TEST_PRIVATE_VALUE", "correcthorsebatterystaple")
		hooks.AfterCreate = "printf '%s\\n' \"$WORKSPACE_TEST_PRIVATE_VALUE\"; exit 127"
	}
	backend, err := workspace.NewBackend(workspace.KindLocalGit, workspace.LocalGitOptions{Hooks: hooks, Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true})
	if err != nil {
		t.Fatal(err)
	}
	runtimeStore, err := store.Open(t.Context(), store.Config{Path: nativeRuntimeDatabasePath(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := runtimeStore.Close(); err != nil {
			t.Error(err)
		}
	})
	var preserved *tracker.NativeCheckpoint
	var preservedRecovery tracker.NativeRecovery
	var preservedChanges []tracker.ChangeRequest
	if failure == "recovery" {
		candidate := h.claim(t, issue.ID)
		info, err := backend.Create(t.Context(), workspace.Issue{ProjectID: "local", ID: issue.ID, Identifier: issue.Identifier, BranchName: candidate.BranchName})
		if err != nil {
			t.Fatal(err)
		}
		if resumeCase == "" {
			if err := os.WriteFile(filepath.Join(info.Path, "README.md"), []byte("preserved source\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			nativeChangeGit(t, info.Path, "add", "README.md")
			nativeChangeGit(t, info.Path, "commit", "-m", "preserved source")
		}
		if resumeCase == "dirty" {
			if err := os.WriteFile(filepath.Join(info.Path, "README.md"), []byte("retained dirty source\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		state, err := backend.(workspace.RecoveryStateProvider).RecoveryState(t.Context(), info, workspace.Issue{ProjectID: "local", ID: issue.ID, Identifier: issue.Identifier})
		if err != nil {
			t.Fatal(err)
		}
		execution := h.scheduler.RunExecution(issue.ID)
		if execution == nil {
			t.Fatal("claimed issue has no native execution")
		}
		execution.(runner.DiffExecution).SetDiffSource(func(ctx context.Context) (tracker.AttemptDiffRequest, bool) {
			diff, err := workspace.GitFileDiffs(ctx, info.Path, workspace.AttemptBase(ctx, source), tracker.MaxDiffBytes)
			if err != nil {
				return tracker.AttemptDiffRequest{}, false
			}
			request := tracker.AttemptDiffRequest{BaseSHA: diff.BaseSHA, HeadSHA: diff.HeadSHA}
			for _, file := range diff.Files {
				request.Files = append(request.Files, tracker.AttemptDiffFile{Path: file.Path, OldPath: file.OldPath, Status: file.Status, Additions: file.Additions, Deletions: file.Deletions, Binary: file.Binary, Patch: file.Patch})
			}
			return request, true
		})
		recordedRole := runner.RoleCode
		if planningRecovery {
			recordedRole = planRecoveryRole
		}
		if resumeCase != "" {
			if resumeCase != "dirty" && (len(state.TrackedPaths) != 0 || len(state.UntrackedPaths) != 0 || state.UnpushedCommits != 0) {
				t.Fatalf("resume refusal setup is not clean: tracked=%d untracked=%d unpushed=%d", len(state.TrackedPaths), len(state.UntrackedPaths), state.UnpushedCommits)
			}
			started := time.Now().Add(-time.Minute)
			identity := agentidentity.Configured("codex", "codex", "", recordedRole, "provider_default", "openai", "high", "", started)
			sessionPolicy := h.descriptor
			if resumeCase == "policy" {
				sessionPolicy.ConfigDigest = policy.Digest([]byte("prior persisted session policy"))
				sessionPolicy = sessionPolicy.WithID()
			}
			metadata, err := json.Marshal(map[string]any{"policy": sessionPolicy})
			if err != nil {
				t.Fatal(err)
			}
			localAttempt, err := runtimeStore.StartWorkAttempt(t.Context(), store.WorkAttemptStart{ProjectID: "local", IssueID: issue.ID, WorkerType: "agent", StartedAt: started, WorkerMetadataJSON: string(metadata), RuntimeIdentity: identity})
			if err != nil {
				t.Fatal(err)
			}
			session, err := runtimeStore.StartSession(t.Context(), store.SessionStart{ProjectID: "local", IssueID: issue.ID, WorkAttemptID: localAttempt, StartedAt: started, RequestedModel: "provider_default", Model: "provider_default", AgentBackendID: "codex", AgentBackendKind: "codex", AgentRole: recordedRole, RuntimeIdentity: identity})
			if err != nil {
				t.Fatal(err)
			}
			if err := runtimeStore.FinishSession(t.Context(), session, store.SessionFinish{Turns: 1, CompletedAt: started.Add(time.Second), FinalState: "failed", ProviderThreadID: "recorded-thread", ProviderSessionID: "recorded-session"}); err != nil {
				t.Fatal(err)
			}
			if err := runtimeStore.CompleteWorkAttempt(t.Context(), store.WorkAttemptCompletion{AttemptID: localAttempt, CompletedAt: started.Add(time.Second), Status: store.WorkAttemptStatusTerminal, TerminalState: store.WorkAttemptTerminalCancelled, WorkerMetadataJSON: string(metadata), MetricsJSON: `{"turns":1,"total_tokens":0}`}); err != nil {
				t.Fatal(err)
			}
			if err := execution.(runner.RuntimeExecution).ObserveRuntime(t.Context(), tracker.NativeRuntimeObservation{LocalAttemptID: localAttempt, Generation: 1, Phase: "implementation", HeartbeatAt: started, Identity: identity}); err != nil {
				t.Fatal(err)
			}
		}
		if err := execution.Start(t.Context(), tracker.NativeExecutionIdentity{Role: recordedRole, Backend: "codex", Model: "provider_default"}); err != nil {
			t.Fatal(err)
		}
		checkpoint := tracker.NativeCheckpoint{Resume: "manual_recovery", Availability: "available", Storage: "local_only", WorktreeState: "unpushed", HeadSHA: state.HeadSHA, WorkspaceDigest: state.WorkspaceFingerprint, ExternalEffect: "none", EffectState: "none"}
		if resumeCase != "" {
			checkpoint.Resume, checkpoint.WorktreeState = "resume_session", "clean"
		}
		if resumeCase == "dirty" {
			checkpoint.WorktreeState = "dirty"
		}
		preserved = &checkpoint
		if err := execution.Checkpoint(t.Context(), checkpoint); err != nil {
			t.Fatal(err)
		}
		if err := execution.Finish(t.Context(), "interrupted"); err != nil {
			t.Fatal(err)
		}
		if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "interrupted"); err != nil {
			t.Fatal(err)
		}
		if planningRecovery {
			if resumeCase != "" {
				change, err := h.admin.CreateChange(t.Context(), tracker.NativeWorkItemID(issue.ID), tracker.CreateChange{Mutation: nativeMutationKey(), Title: "Preserved current Change"})
				if err != nil {
					t.Fatal(err)
				}
				h.publish(t, tracker.NativeWorkItemID(issue.ID), change.ID, state.HeadSHA, "", state.HeadSHA)
			}
			preservedRecovery, err = h.admin.Recovery(t.Context(), tracker.NativeWorkItemID(issue.ID))
			if err != nil || len(preservedRecovery.Attempts) != 1 || preservedRecovery.Attempts[0].Identity == nil || preservedRecovery.Attempts[0].Identity.Role != planRecoveryRole || preservedRecovery.Issue.State != "Todo" {
				t.Fatalf("planning refusal setup did not preserve a Todo source receipt: role=%s attempts=%d change=%v state=%s error=%v", planRecoveryRole, len(preservedRecovery.Attempts), preservedRecovery.Change, preservedRecovery.Issue.State, err)
			}
			preservedChanges = h.changes(t, issue.ID)
			if resumeCase != "" && (preservedRecovery.Change == nil || preservedRecovery.Change.ChangeID == "" || preservedRecovery.Change.VersionID == "" || preservedRecovery.ChangeDetail == nil || len(preservedChanges) != 1 || preservedChanges[0].CurrentVersion == "") {
				t.Fatal("resume refusal setup has no genuine current Change version")
			}
		}
	}
	plan := gate.PlanConfig{Enabled: failure == "" || planningRecovery, Review: gate.PlanReviewAutomated}
	provider := &nativePlanningAgent{failure: failure}
	agent, err := runner.NewRunner(runner.Dependencies{
		ProjectID: "local", Store: runtimeStore,
		Workflow:  config.Workflow{Config: config.Config{Policy: h.descriptor, Plan: plan, Gate: gate.Config{Run: "true"}, Tracker: config.Tracker{Kind: config.TrackerHubNative}}, Prompt: "Complete the issue"},
		Workspace: backend, AgentBackend: provider,
		ReapWorkspaceProcesses: func(context.Context, string, time.Duration) (int, error) {
			if failure == "cleanup" {
				return 0, errors.Join(errors.New("owned child cleanup deadline"), context.DeadlineExceeded)
			}
			return 0, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var checkingProgress atomic.Bool
	finished := make(chan nativePlanFinish, 4)
	transport := &nativePlanTransport{next: h.failChanges, native: h.native, finished: finished, terminated: make(chan tracker.NativeRunData, 1), blocked: make(chan struct{}, 1), checkingProgress: &checkingProgress}
	transport.failWorkflow.Store(abandon)
	h.native.client.httpClient.Transport = transport
	if failure == "recovery" && !manualCheckpoint && !cleanCheckpoint {
		h.native.client.httpClient.Transport = executionRoundTrip(func(request *http.Request) (*http.Response, error) {
			if checkingProgress.Load() {
				return transport.RoundTrip(request)
			}
			if planningRecovery && request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/release") {
				current, readErr := h.native.Issue(request.Context(), tracker.NativeWorkItemID(issue.ID))
				h.scheduler.mu.Lock()
				claim, claimed := h.scheduler.nativeClaims[issue.ID]
				h.scheduler.mu.Unlock()
				var authorityErr error
				if claimed {
					_, authorityErr = h.native.ValidateLease(request.Context(), claim.lease)
				}
				response, releaseErr := h.failChanges.RoundTrip(request)
				finished <- nativePlanFinish{state: current.State, authorityLive: claimed && authorityErr == nil, err: errors.Join(readErr, authorityErr, releaseErr)}
				return response, releaseErr
			}
			response, err := h.failChanges.RoundTrip(request)
			if request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/release") {
				current, readErr := h.native.Issue(request.Context(), tracker.NativeWorkItemID(issue.ID))
				finished <- nativePlanFinish{state: current.State, err: errors.Join(err, readErr)}
			}
			return response, err
		})
	}
	orchCfg := orchestrator.Config{
		Project: scheduler.ProjectCandidate{ID: "local"}, Policy: h.descriptor, Plan: plan,
		PollInterval: time.Hour, MaxConcurrentAgents: 1,
		ActiveStates: []string{"Todo", "In Progress"}, ObservedStates: []string{"Human Review"}, TerminalStates: []string{"Done"},
	}
	if abandon || failure == "workspace" {
		orchCfg.PollInterval = time.Hour
	}
	observed := make(chan nativePlanRunObservation, 8)
	var runBackend runner.Backend = &nativePlanObservedRunner{Runner: agent, observed: observed, checkingProgress: &checkingProgress}
	if failure == "recovery" {
		orchCfg.ObservedStates = append(orchCfg.ObservedStates, "Blocked")
	}
	if planningRecovery && !manualCheckpoint && !cleanCheckpoint {
		orchCfg.PollInterval = time.Hour
	}
	dependencies := orchestrator.Dependencies{Connector: h.connector, Scheduling: h.scheduler, Runner: runBackend, WorkAttempts: runtimeStore, LaneLedger: runtimeStore, WorkflowMetrics: runtimeStore, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	dependencies.Scheduling = &nativePlanProgressScheduler{Scheduler: h.scheduler, checkingProgress: &checkingProgress}
	orch, err := orchestrator.New(orchCfg, dependencies)
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
	wantStates := []string{"In Progress", "Human Review"}
	if failure != "" {
		wantStates = []string{"Human Review"}
	}
	if failure == "recovery" && !manualCheckpoint && !cleanCheckpoint {
		wantStates = []string{"Todo"}
	}
	if manualCheckpoint && planningRecovery {
		wantStates = []string{"In Progress", "Human Review"}
	}
	if abandon {
		select {
		case <-transport.blocked:
		case <-testenv.WaitContext(t, t.Context()).Done():
			t.Fatal("planner did not reach the completion lane write")
		}
		attempts, err := runtimeStore.ListActiveWorkAttempts(t.Context(), store.WorkAttemptQuery{ProjectID: "local"})
		if err != nil || len(attempts) != 1 {
			t.Fatalf("active planner = %v, %v", attempts, err)
		}
		receipt, err := orch.WorkAttemptReceipt(t.Context(), "local", attempts[0].ID)
		if err != nil || receipt.Attempt.Phase != "completion_deferred" {
			t.Fatalf("planner deferral = %v, %v", receipt.Attempt.Phase, err)
		}
		// Hold intake through the operator's lane move and explicit retry intent.
		// Otherwise the next tick can start a worker before the retry request.
		heldConfig := orchCfg
		heldConfig.ActiveStates = []string{"Human Review"}
		heldConfig.ObservedStates = []string{"Todo", "In Progress"}
		if err := orch.UpdateRuntime(t.Context(), orchestrator.RuntimeUpdate{Config: heldConfig}); err != nil {
			t.Fatal(err)
		}
		// Replay Cloud's missing lease followed by supported local abandon.
		h.scheduler.mu.Lock()
		lease := h.scheduler.nativeClaims[issue.ID].lease
		h.scheduler.mu.Unlock()
		if err := h.native.Release(t.Context(), lease, "completed"); err != nil {
			t.Fatal(err)
		}
		response, err := orch.RecoverWorkAttempt(t.Context(), orchestrator.WorkAttemptRecoveryRequest{ProjectID: "local", AttemptID: attempts[0].ID, Action: orchestrator.WorkAttemptRecoveryAbandon, Confirm: true, Reason: "native planner succeeded but its lease ended", Operator: "ops"})
		if err != nil || response.Attempt.TerminalState != string(store.WorkAttemptTerminalAbandoned) {
			t.Fatalf("abandon = %v, %v", response.Status, err)
		}
		select {
		case acknowledgment := <-transport.terminated:
			if acknowledgment.LeaseID != lease.ID || acknowledgment.FencingToken != lease.FencingToken || acknowledgment.Identity == nil || acknowledgment.Identity.Role != runner.RolePlan {
				t.Fatalf("termination acknowledgment lost planner authority: %+v", acknowledgment)
			}
			recovery, err := h.admin.Recovery(t.Context(), tracker.NativeWorkItemID(issue.ID))
			if err != nil || len(recovery.Attempts) != 1 {
				t.Fatalf("abandoned planner receipt = %+v, %v", recovery.Attempts, err)
			}
			attempt := recovery.Attempts[0]
			if attempt.AttemptID != acknowledgment.AttemptID || attempt.LeaseID != lease.ID || attempt.FencingToken != lease.FencingToken || attempt.Status != "cancelled" || attempt.ClaimReleasedAt == nil || attempt.Identity == nil || attempt.Identity.Role != runner.RolePlan {
				t.Fatalf("abandoned planner lost acknowledged termination: %+v", attempt)
			}
		default:
			t.Fatal("abandon completed without acknowledging planner termination")
		}
		current, err := h.admin.Issue(t.Context(), tracker.NativeWorkItemID(issue.ID))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.admin.Transition(t.Context(), current.WorkItemID, tracker.Transition{Mutation: nativeMutationKey(), ExpectedRevision: current.Revision, State: "In Progress", Reason: "user_requested"}); err != nil {
			t.Fatal(err)
		}
		transport.failWorkflow.Store(false)
		response, err = orch.RecoverWorkAttempt(t.Context(), orchestrator.WorkAttemptRecoveryRequest{ProjectID: "local", AttemptID: attempts[0].ID, Action: orchestrator.WorkAttemptRecoveryRetryFresh, Confirm: true, Reason: "operator starts implementation from the preserved planner workspace", Operator: "ops"})
		if err != nil || !response.Queued {
			t.Fatalf("fresh implementation handoff = %v, %v", response.Status, err)
		}
		if err := orch.UpdateRuntime(t.Context(), orchestrator.RuntimeUpdate{Config: orchCfg}); err != nil {
			t.Fatal(err)
		}
		wantStates = []string{"Human Review"}
	}
	if failure == "refused" || failure == "workspace" {
		wantStates = []string{"Todo"}
	}
	var refusalAttemptID int64
	for _, want := range wantStates {
		select {
		case got := <-finished:
			if got.err != nil {
				t.Fatal(got.err)
			}
			if planningRecovery && !manualCheckpoint && !cleanCheckpoint {
				select {
				case run := <-observed:
					refusalAttemptID = run.localAttemptID
					wantReason := "session_restart_required"
					if run.mode != refusalMode || run.state != dispatchState || run.recordedRole != planRecoveryRole || !errors.Is(run.err, runner.ErrNativeRecoveryRequired) || !strings.Contains(run.err.Error(), wantReason) {
						t.Fatalf("planning refusal ran the wrong dispatch or failure: mode=%s state=%s recorded_role=%s error=%v", run.mode, run.state, run.recordedRole, run.err)
					}
					t.Logf("verified dispatch: mode=%s state=%s recorded_role=%s refusal=%v authority_live_at_release=%v lane_before_release=%s", run.mode, run.state, run.recordedRole, run.err, got.authorityLive, got.state)
				default:
					t.Fatal("native release preceded the observed planning refusal")
				}
				if !got.authorityLive {
					t.Fatal("planning refusal lost native claim authority before settlement")
				}
			}
			if got.state != want {
				recovery, readErr := h.admin.Recovery(t.Context(), tracker.NativeWorkItemID(issue.ID))
				var last *tracker.NativeTerminalFailure
				if len(recovery.Attempts) > 0 {
					last = recovery.Attempts[len(recovery.Attempts)-1].TerminalFailure
				}
				t.Fatalf("native run finished in %s, want handoff to %s before lease retirement: attempt=%+v read_error=%v", got.state, want, last, readErr)
			}
		case <-testenv.WaitContext(t, t.Context()).Done():
			state, _ := orch.State(t.Context())
			t.Fatalf("native handoff did not finish: running=%v retry=%v", len(state.Running), state.Retry)
		}
	}
	changes := h.changes(t, issue.ID)
	if failure == "workspace" {
		current, err := h.admin.Recovery(t.Context(), tracker.NativeWorkItemID(issue.ID))
		if err != nil || len(current.Attempts) != 1 || provider.calls.Load() != 0 || len(changes) != 0 {
			t.Fatalf("setup failure lost receipt or started provider: attempts=%+v changes=%v calls=%d err=%v", current.Attempts, changes, provider.calls.Load(), err)
		}
		a := current.Attempts[0]
		if a.Identity != nil || a.Status != "failed" || a.TerminalFailure == nil || a.TerminalFailure.Error != "workspace hook after_create failed (exit status 127)" || a.TerminalFailure.Stage != "pre_provider" || a.TerminalFailure.Attribution != "instance" || a.TerminalFailure.ObservedAt.IsZero() || a.Runtime == nil || a.Runtime.LocalAttemptID == 0 || a.Runtime.Generation == 0 || a.Runtime.Activity != nil {
			t.Fatalf("setup failure diagnostic or identity incorrect: %+v", a)
		}
		state, err := orch.State(t.Context())
		if err != nil {
			t.Fatalf("setup failure charged issue failure allowance: %+v err=%v", state, err)
		}
		return
	}
	if manualCheckpoint || cleanCheckpoint {
		current, err := h.admin.Recovery(t.Context(), tracker.NativeWorkItemID(issue.ID))
		if err != nil {
			t.Fatal(err)
		}
		if preserved == nil {
			t.Fatal("checkpoint recovery preserved no source")
		}
		wantCalls := int64(1)
		if planningRecovery && !cleanCheckpoint {
			wantCalls = 2
		}
		if current.Issue.State != "Human Review" || provider.calls.Load() != wantCalls || len(changes) != 1 || changes[0].CurrentVersion == "" {
			t.Fatalf("historical judgment prevented delivery or lost source: state=%s attempts=%d calls=%d changes=%d error=%v", current.Issue.State, len(current.Attempts), provider.calls.Load(), len(changes), err)
		}
		if cleanCheckpoint {
			if len(preservedChanges) != 1 {
				t.Fatalf("clean restart requires one preserved Change, got %d", len(preservedChanges))
			}
			if provider.resumeVerified.Load() != 1 || changes[0].ID != preservedChanges[0].ID || changes[0].CurrentVersion == preservedChanges[0].CurrentVersion {
				t.Fatalf("clean restart did not advance the existing Change: verified=%d changes=%v", provider.resumeVerified.Load(), changes)
			}
		}
		nativeChangeGit(t, provider.workspace, "merge-base", "--is-ancestor", preserved.HeadSHA, "HEAD")
		return
	}
	if failure == "recovery" {
		attemptID := refusalAttemptID
		if !planningRecovery {
			attemptID = 1
		}
		testenv.WaitUntil(t, t.Context(), func() error {
			attempt, err := runtimeStore.WorkAttempt(t.Context(), attemptID)
			if err != nil {
				t.Fatal(err)
			}
			if attempt.Status != store.WorkAttemptStatusTerminal {
				return fmt.Errorf("recovery refusal did not settle local accounting: status=%s", attempt.Status)
			}
			return nil
		})
	}
	if planningRecovery {
		current, err := h.admin.Recovery(t.Context(), tracker.NativeWorkItemID(issue.ID))
		if err != nil || current.Issue.State != "Todo" || (len(current.Attempts) != len(preservedRecovery.Attempts)+1 || !reflect.DeepEqual(current.Attempts[:len(current.Attempts)-1], preservedRecovery.Attempts)) || !reflect.DeepEqual(current.Change, preservedRecovery.Change) || !reflect.DeepEqual(current.ChangeDetail, preservedRecovery.ChangeDetail) || provider.calls.Load() != 0 || !reflect.DeepEqual(changes, preservedChanges) {
			t.Errorf("planning refusal changed preserved evidence: state=%s receipt_unchanged=%v change_unchanged=%v calls=%d changes=%d error=%v", current.Issue.State, reflect.DeepEqual(current.Attempts, preservedRecovery.Attempts), reflect.DeepEqual(current.Change, preservedRecovery.Change), provider.calls.Load(), len(changes), err)
		}
		assertSetupRefusalReceipt(t, current.Attempts, provider.progressCalls.Load() > 0)
		if (resumeCase == "identity" || resumeCase == "policy") && provider.resumeVerified.Load() != 1 {
			t.Fatalf("resume refusal did not exercise the intended boundary: case=%s verification_calls=%d", resumeCase, provider.resumeVerified.Load())
		}
		assertInstanceRecoveryRetry(t, h, orch, issue.ID)
		select {
		case extra := <-finished:
			t.Fatalf("duplicate release notification: %+v", extra)
		default:
		}
		attempt, err := runtimeStore.WorkAttempt(t.Context(), refusalAttemptID)
		var metadata struct {
			RunMode string `json:"run_mode"`
		}
		metadataErr := json.Unmarshal([]byte(attempt.WorkerMetadataJSON), &metadata)
		if err != nil || metadataErr != nil || metadata.RunMode != refusalMode || attempt.WorkerType != refusalWorker || attempt.Status != store.WorkAttemptStatusTerminal || attempt.TerminalState != store.WorkAttemptTerminalFailure || attempt.ErrorClass != "workspace_preparation" {
			t.Fatalf("planning refusal lost instance accounting: mode=%s worker=%s status=%s terminal=%s class=%s error=%v", metadata.RunMode, attempt.WorkerType, attempt.Status, attempt.TerminalState, attempt.ErrorClass, errors.Join(err, metadataErr))
		}
		return
	}

	if failure == "provider" || failure == "refused" {
		if len(changes) != 0 || provider.calls.Load() != 1 {
			t.Fatalf("failed native run created a Change or repeated coding: changes=%v calls=%d", changes, provider.calls.Load())
		}
		if failure != "refused" {
			if _, err := os.Stat(filepath.Join(provider.workspace, "CHANGE.md")); err != nil {
				t.Fatalf("failed native source not preserved: %v", err)
			}
			staged, err := exec.CommandContext(t.Context(), "git", "-C", provider.workspace, "diff", "--cached", "--name-only").Output()
			if err != nil || strings.TrimSpace(string(staged)) != "CHANGE.md" {
				t.Fatalf("failed native staged source = %q, error=%v", staged, err)
			}
		}
		current, err := h.admin.Recovery(t.Context(), tracker.NativeWorkItemID(issue.ID))
		if err != nil || len(current.Attempts) != 1 || current.Attempts[0].Status != "failed" {
			t.Fatalf("native failed outcome = %+v, error=%v", current.Attempts, err)
		}
		attempt := current.Attempts[0]
		spend, err := runtimeStore.IssueTokenSpend(t.Context(), store.IssueIdentity{ProjectID: "local", IssueID: issue.ID})
		wantSessions := int64(1)
		if failure == "refused" {
			wantSessions = 0
		}
		if err != nil || spend.Sessions != wantSessions {
			t.Fatalf("native lifetime accounting=%+v, %v; want sessions=%d", spend, err, wantSessions)
		}
		if failure == "provider" || failure == "refused" {
			terminal := attempt.TerminalFailure
			if terminal == nil || attempt.TerminalFailureAvailability != "available" || terminal.TurnStartRefused != (failure == "refused") || terminal.Provider != "codex" || terminal.Operation != "turn/start" || terminal.RPCCode == nil || *terminal.RPCCode != -32602 || terminal.ProviderCode != "input_too_large" || terminal.MaxChars == nil || *terminal.MaxChars != 1048576 || terminal.ActualChars == nil || *terminal.ActualChars != 2927066 || terminal.ObservedAt.IsZero() || terminal.Source != "host_runner_completion" {
				t.Fatalf("provider failure lost recorded metadata: %+v", terminal)
			}
			if attempt.Finalization != nil || attempt.ClaimReleasedAt == nil || attempt.Runtime.Completion != nil {
				t.Fatalf("provider failure fabricated Change or acceptance evidence: %+v", attempt)
			}
			evidence, err := h.admin.RuntimeEvidence(t.Context(), tracker.NativeWorkItemID(issue.ID), attempt.AttemptID)
			if err != nil || evidence.Attempt == nil || evidence.Attempt.AttemptID != attempt.AttemptID || evidence.Attempt.FencingToken != attempt.FencingToken || evidence.Attempt.TerminalFailure == nil || evidence.Attempt.TerminalFailure.TurnStartRefused != (failure == "refused") {
				t.Fatalf("runtime read lost fenced failure: %+v, %v", evidence.Attempt, err)
			}
			history, err := h.admin.History(t.Context(), tracker.NativeWorkItemID(issue.ID), "")
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, event := range history.Items {
				if event.Type == "run.finished" && event.Data.Run.AttemptID == attempt.AttemptID {
					found = true
					if event.Data.Run.FencingToken != attempt.FencingToken || event.Data.Run.TerminalFailure == nil || event.Data.Run.TerminalFailure.ProviderCode != "input_too_large" || event.Data.Run.TerminalFailure.TurnStartRefused != (failure == "refused") {
						t.Fatalf("immutable terminal event lost fenced failure: %+v", event.Data.Run)
					}
				}
			}
			if !found {
				t.Fatal("provider failure omitted terminal event")
			}
			raw, err := json.Marshal([]any{current, history, evidence})
			if err != nil {
				t.Fatal(err)
			}
			for _, private := range []string{"private-prompt", "private-secret", "/private/source", "private-rpc-data"} {
				if strings.Contains(string(raw), private) {
					t.Fatalf("supported native reads exposed %q", private)
				}
			}
		}
		return
	}
	if len(changes) != 1 || changes[0].CurrentVersion == "" {
		t.Fatalf("implementation did not publish Change Request: %+v", changes)
	}
	comments, err := h.connector.FetchIssueComments(t.Context(), issue)
	if err != nil {
		t.Fatal(err)
	}
	plans := 0
	for _, comment := range comments {
		if strings.HasPrefix(comment.Body, "## Detent Plan\n") {
			plans++
		}
	}
	if want := 1; failure != "" {
		if plans != 0 {
			t.Fatalf("non-planning run published a plan")
		}
	} else if plans != want {
		t.Fatalf("plan publications = %d, want one", plans)
	}
}

func hubserverPlanStates() []tracker.NativeState {
	return []tracker.NativeState{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Human Review", "Done"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Todo", "Human Review", "Done"}},
		{Name: "Human Review", Transitions: []string{"In Progress", "Merging", "Done"}},
		{Name: "Merging", Dispatchable: true, Transitions: []string{"Human Review", "Done"}},
		{Name: "Done", Terminal: true},
	}
}

type nativePlanningAgent struct {
	failure        string
	calls          atomic.Int64
	progressCalls  atomic.Int64
	workspace      string
	resumeVerified atomic.Int64
}

func (a *nativePlanningAgent) VerifyResume(_ context.Context, _ runner.AgentProcessRequest, resume runner.AgentResume) error {
	if resume.ThreadID != "recorded-thread" || resume.SessionID != "recorded-session" {
		return errors.New("recorded provider session unavailable")
	}
	a.resumeVerified.Add(1)
	return nil
}

func (a *nativePlanningAgent) RunTurn(ctx context.Context, req runner.AgentTurnRequest, update runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
	if req.TextOnly && req.ReadOnly && strings.HasPrefix(req.Prompt, progresscheck.FailureInstructions) {
		outcome := "instance"
		if a.failure == "provider" {
			outcome = "unknown"
		}
		return runner.AgentTurnResult{}, update(runner.AgentUpdate{Type: runner.AgentUpdateMessageDelta, Delta: fmt.Sprintf(`{"outcome":%q,"reason":"fake failure decision"}`, outcome)})
	}
	if req.TextOnly && strings.Contains(req.Prompt, progresscheck.Instructions) {
		a.progressCalls.Add(1)
		if err := update(runner.AgentUpdate{Type: runner.AgentUpdateTokenUsage, Tokens: runner.AgentTokenUsage{InputTokens: 10, OutputTokens: 1}}); err != nil {
			return runner.AgentTurnResult{}, err
		}
		_, encoded, found := strings.Cut(req.Prompt, "\n\nEvidence:\n")
		if !found {
			return runner.AgentTurnResult{}, errors.New("progress evidence is missing")
		}
		var evidence progresscheck.Request
		if err := json.Unmarshal([]byte(encoded), &evidence); err != nil {
			return runner.AgentTurnResult{}, err
		}
		if len(evidence.Attempts) == 0 {
			return runner.AgentTurnResult{}, errors.New("progress attempt is missing")
		}
		decision, err := json.Marshal(progresscheck.Decision{Outcome: progresscheck.Progressing, Reason: "Continue the selected recovery step", Evidence: []string{evidence.Attempts[len(evidence.Attempts)-1].ID}})
		if err != nil {
			return runner.AgentTurnResult{}, err
		}
		return runner.AgentTurnResult{}, update(runner.AgentUpdate{Type: runner.AgentUpdateMessageDelta, ItemID: "decision", Delta: string(decision)})
	}

	a.calls.Add(1)
	a.workspace = req.Workspace
	if a.failure == "cleanup" || a.failure == "exited" {
		if err := update(runner.AgentUpdate{Type: runner.AgentUpdateProcessStarted, WorkerProcess: procgroup.Identity{PID: 2081001, GroupID: 2081001, StartedAt: time.Now()}}); err != nil {
			return runner.AgentTurnResult{}, err
		}
	}
	if a.failure == "refused" {
		return runner.AgentTurnResult{}, &codex.ResponseError{Request: "turn/start", Code: -32602, Message: "input_too_large", Body: `{"error":{"data":{"code":"input_too_large","max_chars":1048576,"actual_chars":2927066}}}`}
	}
	plan := strings.Contains(req.Prompt, "This dispatch is plan-only.")
	if plan {
		if err := update(runner.AgentUpdate{Type: runner.AgentUpdateMessageDelta, Delta: "Implement README changes and verify them.\n\n## Detent Plan Review\n\n- state: approved\n\nThe plan covers acceptance, tests and risks."}); err != nil {
			return runner.AgentTurnResult{}, err
		}
	}
	result, err := (&committingAgent{staged: !plan}).RunTurn(ctx, req, update)
	if a.failure == "provider" {
		return result, errors.Join(err, &codex.ResponseError{Request: "turn/start", Code: -32602, Message: "input_too_large", Body: `{"error":{"code":-32602,"message":"private-prompt","data":{"code":"input_too_large","max_chars":1048576,"actual_chars":2927066,"prompt":"private-prompt","token":"private-secret","path":"/private/source","other":"private-rpc-data"}}}`})
	}
	return result, err
}

type nativePlanFinish struct {
	state         string
	authorityLive bool
	err           error
}

type nativePlanRunObservation struct {
	mode           string
	state          string
	recordedRole   string
	err            error
	localAttemptID int64
}

type nativePlanProgressScheduler struct {
	*Scheduler
	checkingProgress *atomic.Bool
}

func (s *nativePlanProgressScheduler) ReadmitRetryProgress(ctx context.Context, request runner.RunRequest, resolver runner.ProviderCapacityResolver) (runner.Execution, *isolation.Policy, error) {
	s.checkingProgress.Store(true)
	defer s.checkingProgress.Store(false)
	return s.Scheduler.ReadmitRetryProgress(ctx, request, resolver)
}

type nativePlanObservedRunner struct {
	*runner.Runner
	observed         chan<- nativePlanRunObservation
	checkingProgress *atomic.Bool
}

func (r *nativePlanObservedRunner) CheckProgress(ctx context.Context, request runner.RunRequest, evidence progresscheck.Request) (progresscheck.Decision, error) {
	r.checkingProgress.Store(true)
	defer r.checkingProgress.Store(false)
	return r.Runner.CheckProgress(ctx, request, evidence)
}

func (r *nativePlanObservedRunner) Run(ctx context.Context, request runner.RunRequest) (runner.RunResult, error) {
	observation := nativePlanRunObservation{mode: request.Mode, state: request.Issue.State, localAttemptID: request.WorkAttemptID}
	if request.Execution != nil {
		attempts := request.Execution.Recovery().Attempts
		if len(attempts) > 0 && attempts[len(attempts)-1].Identity != nil {
			observation.recordedRole = attempts[len(attempts)-1].Identity.Role
		}
	}
	result, err := r.Runner.Run(ctx, request)
	observation.err = err
	select {
	case r.observed <- observation:
	default:
	}
	return result, err
}

type nativePlanTransport struct {
	next             http.RoundTripper
	native           *NativeClient
	checkingProgress *atomic.Bool
	finished         chan<- nativePlanFinish
	terminated       chan tracker.NativeRunData
	blocked          chan struct{}
	failWorkflow     atomic.Bool
}

func (t *nativePlanTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/workflow") && t.failWorkflow.Load() {
		select {
		case t.blocked <- struct{}{}:
		default:
		}
		return nil, errors.New("native workflow publication unavailable")
	}
	response, err := t.next.RoundTrip(req)
	if err != nil || response.StatusCode >= 300 || req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, "/events") {
		return response, err
	}
	body, err := req.GetBody()
	if err != nil {
		return response, err
	}
	defer body.Close()
	var event tracker.NativeRunEvent
	if err := json.NewDecoder(body).Decode(&event); err != nil {
		return response, err
	}
	if t.checkingProgress != nil && t.checkingProgress.Load() {
		return response, nil
	}
	if event.Type != "run.finished" {
		return response, nil
	}
	if event.Data.Outcome == "cancelled" {
		t.terminated <- event.Data
		return response, nil
	}
	item := tracker.NativeWorkItemID(strings.Split(strings.TrimSuffix(req.URL.Path, "/events"), "/work-items/")[1])
	issue, err := t.native.Issue(req.Context(), item)
	if err == nil {
		err = t.native.Release(req.Context(), tracker.NativeLease{ID: event.Data.LeaseID, WorkItemID: item, FencingToken: event.Data.FencingToken}, "completed")
	}
	t.finished <- nativePlanFinish{state: issue.State, err: err}
	return response, nil
}

func assertSetupRefusalReceipt(t *testing.T, attempts []tracker.NativeAttempt, progressPaid bool) {
	t.Helper()
	if len(attempts) == 0 {
		t.Fatal("setup refusal receipt absent")
	}
	a := attempts[len(attempts)-1]
	stage := "pre_provider"
	if progressPaid {
		stage = "workspace_preparation"
	}
	if (a.Identity != nil) != progressPaid || progressPaid && (len(a.Usage) != 1 || a.Usage[0].Input != 10 || a.Usage[0].Output != 1) || a.Status != "interrupted" || a.Checkpoint != nil || a.TerminalFailure == nil || a.TerminalFailure.Stage != stage || a.TerminalFailure.Attribution != "instance" || a.TerminalFailure.ErrorClass != "source_recovery" || !strings.Contains(a.TerminalFailure.Error, "native checkpoint requires recovery:") {
		t.Fatalf("setup refusal replaced source or lost diagnostic: %+v failure=%+v", a, a.TerminalFailure)
	}
}

func assertInstanceRecoveryRetry(t *testing.T, h *nativeChangeHub, orch *orchestrator.Orchestrator, issueID string) {
	t.Helper()
	if candidates := h.candidates(t); len(candidates) != 1 || candidates[0].ID != issueID {
		t.Errorf("instance recovery refusal not reclaimable in its lane: candidates=%d", len(candidates))
	}
	testenv.WaitUntil(t, t.Context(), func() error {
		state, err := orch.State(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if _, retried := state.Retry[issueID]; !retried {
			return errors.New("recovery refusal retry is not published yet")
		}
		return nil
	})
	state, err := orch.State(t.Context())
	retry, retried := state.Retry[issueID]
	if err != nil || len(state.Retry) != 1 || !retried || !strings.Contains(retry.Error, "native checkpoint requires recovery") {
		t.Errorf("instance recovery refusal charged failures or skipped its retry: retry=%d error=%v", len(state.Retry), err)
	}
}
