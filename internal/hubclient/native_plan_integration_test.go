package hubclient

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

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
		{name: "abandon deferred planner and recover", abandon: true},
		{name: "provider failure settles before lease retirement", failure: "provider"},
		{name: "refused start retains fenced accounting", failure: "refused"},
		{name: "owned cleanup failure settles instance outcome", failure: "cleanup"},
		{name: "completed provider with exited process preserves staged finalization", failure: "exited"},
		{name: "permanent pre-provider recovery refusal settles claim", failure: "recovery"},
		{name: "planning-enabled Todo refuses recorded code before provider", failure: "recovery", planRecoveryRole: runner.RoleCode},
		{name: "planning-enabled Todo refuses recorded rework before provider", failure: "recovery", planRecoveryRole: runner.RoleRework},
		{name: "planning-enabled clean code identity mismatch preserves current version", failure: "recovery", planRecoveryRole: runner.RoleCode, resumeCase: "identity"},
		{name: "planning-enabled persisted policy refusal preserves current version", failure: "recovery", planRecoveryRole: runner.RoleCode, resumeCase: "policy"},
	} {
		t.Run(test.name, func(t *testing.T) {
			testNativePlannerHandoff(t, test.abandon, test.failure, test.planRecoveryRole, test.resumeCase)
		})
	}
}

func testNativePlannerHandoff(t *testing.T, abandon bool, failure, planRecoveryRole, resumeCase string) {
	t.Helper()
	planningRecovery := planRecoveryRole != ""
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
	backend, err := workspace.NewBackend(workspace.KindLocalGit, workspace.LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true})
	if err != nil {
		t.Fatal(err)
	}
	runtimeStore, err := store.Open(t.Context(), store.Config{Path: filepath.Join(t.TempDir(), "runtime.db")})
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
			if len(state.TrackedPaths) != 0 || len(state.UntrackedPaths) != 0 || state.UnpushedCommits != 0 {
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
	finished := make(chan nativePlanFinish, 4)
	transport := &nativePlanTransport{next: h.failChanges, native: h.native, finished: finished, blocked: make(chan struct{}, 1)}
	transport.failWorkflow.Store(abandon)
	h.native.client.httpClient.Transport = transport
	if failure == "recovery" {
		h.native.client.httpClient.Transport = executionRoundTrip(func(request *http.Request) (*http.Response, error) {
			if planningRecovery && request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/release") {
				current, readErr := h.native.Issue(request.Context(), tracker.NativeWorkItemID(issue.ID))
				h.scheduler.mu.Lock()
				claim, claimed := h.scheduler.nativeClaims[issue.ID]
				h.scheduler.mu.Unlock()
				var authorityErr error
				if claimed {
					_, authorityErr = h.native.ValidateLease(request.Context(), claim.lease)
				}
				response, releaseErr := transport.RoundTrip(request)
				finished <- nativePlanFinish{state: current.State, authorityLive: claimed && authorityErr == nil, err: errors.Join(readErr, authorityErr, releaseErr)}
				return response, releaseErr
			}
			response, err := transport.RoundTrip(request)
			if request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/release") {
				current, readErr := h.native.Issue(request.Context(), tracker.NativeWorkItemID(issue.ID))
				finished <- nativePlanFinish{state: current.State, err: errors.Join(err, readErr)}
			}
			return response, err
		})
	}
	orchCfg := orchestrator.Config{
		Project: scheduler.ProjectCandidate{ID: "local"}, Policy: h.descriptor, Plan: plan,
		PollInterval: 20 * time.Millisecond, MaxConcurrentAgents: 1,
		ActiveStates: []string{"Todo", "In Progress"}, ObservedStates: []string{"Human Review"}, TerminalStates: []string{"Done"},
	}
	if abandon {
		orchCfg.PollInterval = time.Hour
	}
	var runBackend runner.Backend = agent
	observed := make(chan nativePlanRunObservation, 8)
	if failure == "recovery" {
		orchCfg.ObservedStates = append(orchCfg.ObservedStates, "Blocked")
	}
	if planningRecovery {
		orchCfg.PollInterval = time.Hour
		runBackend = &nativePlanObservedRunner{Runner: agent, observed: observed}
	}
	orch, err := orchestrator.New(orchCfg, orchestrator.Dependencies{Connector: h.connector, Scheduling: h.scheduler, Runner: runBackend, WorkAttempts: runtimeStore, LaneLedger: runtimeStore, WorkflowMetrics: runtimeStore, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
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
	if failure == "recovery" {
		wantStates = []string{"Blocked"}
	}
	if abandon {
		select {
		case <-transport.blocked:
		case <-time.After(10 * time.Second):
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
		orchCfg.PollInterval = 20 * time.Millisecond
		if err := orch.UpdateRuntime(t.Context(), orchestrator.RuntimeUpdate{Config: orchCfg}); err != nil {
			t.Fatal(err)
		}
		wantStates = []string{"Human Review"}
	}
	if failure == "refused" || failure == "cleanup" {
		wantStates = []string{"Todo"}
	}
	var refusalAttemptID int64
	for _, want := range wantStates {
		select {
		case got := <-finished:
			if got.err != nil {
				t.Fatal(got.err)
			}
			if planningRecovery {
				select {
				case run := <-observed:
					refusalAttemptID = run.localAttemptID
					wantReason := "checkpoint_requires_recovery"
					switch resumeCase {
					case "identity":
						wantReason = "session_restart_required"
					case "policy":
						wantReason = "policy_mismatch"
					}
					if run.mode != runner.RunModePlan || run.state != "Todo" || run.recordedRole != planRecoveryRole || !errors.Is(run.err, runner.ErrNativeRecoveryRequired) || !strings.Contains(run.err.Error(), wantReason) {
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
				if planningRecovery {
					t.Errorf("native run finished in %s, want handoff to %s before lease retirement", got.state, want)
				} else {
					t.Fatalf("native run finished in %s, want handoff to %s before lease retirement", got.state, want)
				}
			}
		case <-time.After(10 * time.Second):
			state, _ := orch.State(t.Context())
			t.Fatalf("native handoff did not finish: running=%v retry=%v", len(state.Running), state.Retry)
		}
	}
	changes := h.changes(t, issue.ID)
	if planningRecovery {
		current, err := h.admin.Recovery(t.Context(), tracker.NativeWorkItemID(issue.ID))
		if err != nil || current.Issue.State != "Blocked" || !reflect.DeepEqual(current.Attempts, preservedRecovery.Attempts) || !reflect.DeepEqual(current.Change, preservedRecovery.Change) || !reflect.DeepEqual(current.ChangeDetail, preservedRecovery.ChangeDetail) || provider.calls.Load() != 0 || !reflect.DeepEqual(changes, preservedChanges) {
			t.Errorf("planning refusal changed preserved evidence: state=%s receipt_unchanged=%v change_unchanged=%v calls=%d changes=%d error=%v", current.Issue.State, reflect.DeepEqual(current.Attempts, preservedRecovery.Attempts), reflect.DeepEqual(current.Change, preservedRecovery.Change), provider.calls.Load(), len(changes), err)
		}
		if resumeCase == "identity" && provider.resumeVerified.Load() != 1 || resumeCase == "policy" && provider.resumeVerified.Load() != 0 {
			t.Fatalf("resume refusal did not exercise the intended boundary: case=%s verification_calls=%d", resumeCase, provider.resumeVerified.Load())
		}
		for range 2 {
			if candidates := h.candidates(t); len(candidates) != 0 {
				t.Errorf("permanent planning refusal reclaimable: candidates=%d", len(candidates))
			}
		}
		state, err := orch.State(t.Context())
		if err != nil || state.FailureBreaker.Count != 0 || len(state.Retry) != 0 || len(state.InstantFailures) != 0 || len(state.RepeatedFailures) != 0 {
			t.Errorf("planning refusal charged failures or queued retry: breaker=%d retry=%d instant=%d repeated=%d error=%v", state.FailureBreaker.Count, len(state.Retry), len(state.InstantFailures), len(state.RepeatedFailures), err)
		}
		attempt, err := runtimeStore.WorkAttempt(t.Context(), refusalAttemptID)
		var metadata struct {
			RunMode string `json:"run_mode"`
		}
		metadataErr := json.Unmarshal([]byte(attempt.WorkerMetadataJSON), &metadata)
		if err != nil || metadataErr != nil || metadata.RunMode != runner.RunModePlan || attempt.WorkerType != "planner" || attempt.Status != store.WorkAttemptStatusTerminal || attempt.TerminalState != store.WorkAttemptTerminalCancelled || attempt.ErrorClass != "workspace_preparation" {
			t.Fatalf("planning refusal lost instance accounting: mode=%s worker=%s status=%s terminal=%s class=%s error=%v", metadata.RunMode, attempt.WorkerType, attempt.Status, attempt.TerminalState, attempt.ErrorClass, errors.Join(err, metadataErr))
		}
		return
	}
	if failure == "recovery" {
		if preserved == nil {
			t.Fatal("recovery fixture has no preserved checkpoint")
		}
		current, err := h.admin.Recovery(t.Context(), tracker.NativeWorkItemID(issue.ID))
		if err != nil || len(current.Attempts) != 1 || current.Attempts[0].Status != "interrupted" || current.Attempts[0].Checkpoint == nil || *current.Attempts[0].Checkpoint != *preserved || provider.calls.Load() != 0 || len(changes) != 0 {
			t.Fatalf("recovery refusal fabricated or replaced a receipt: attempts=%+v changes=%v calls=%d error=%v", current.Attempts, changes, provider.calls.Load(), err)
		}
		for range 2 {
			if candidates := h.candidates(t); len(candidates) != 0 {
				t.Fatalf("permanent recovery refusal reclaimed: %+v", candidates)
			}
		}
		state, err := orch.State(t.Context())
		if err != nil || state.FailureBreaker.Count != 0 || len(state.Retry) != 0 {
			t.Fatalf("recovery refusal charged failures or queued retry: %+v, %v", state.FailureBreaker, err)
		}
		attempt, err := runtimeStore.WorkAttempt(t.Context(), 1)
		if err != nil || attempt.Status != store.WorkAttemptStatusTerminal || attempt.TerminalState != store.WorkAttemptTerminalCancelled || attempt.ErrorClass != "workspace_preparation" {
			t.Fatalf("recovery refusal lost instance accounting: %+v, %v", attempt, err)
		}
		return
	}
	if failure == "provider" || failure == "cleanup" || failure == "refused" {
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

type nativePlanObservedRunner struct {
	*runner.Runner
	observed chan<- nativePlanRunObservation
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
	next         http.RoundTripper
	native       *NativeClient
	finished     chan<- nativePlanFinish
	blocked      chan struct{}
	failWorkflow atomic.Bool
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
	if event.Type != "run.finished" {
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
