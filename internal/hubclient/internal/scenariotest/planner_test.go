package scenariotest

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/codex"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/hubclient/internal/testsupport"
	"github.com/digitaldrywood/detent/internal/hubclient/nativechange"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestMain(m *testing.M) { os.Exit(testsupport.RunMain(m)) }

func TestNativePlannerAutomaticHandoff(t *testing.T) {
	factories := Constructors{New: nativechange.NewExecution, Recovered: nativechange.NewRecoveredExecution}
	repositoryRootOnce.Do(func() { nativeChangeFixtureRoot = os.Getenv("TMPDIR") })

	isolateNativeChangeGit(t)
	for _, test := range []struct {
		name             string
		planReview       string
		answerHistory    bool
		loseLease        bool
		failure          string
		planRecoveryRole string
		resumeCase       string
		legacy           bool
	}{
		{name: "automatic plan and code share a claim", planReview: gate.PlanReviewAutomated},
		{name: "planning disabled"},
		{name: "legacy plan review starts a fresh plan despite an old approval", planReview: gate.PlanReviewAutomated, legacy: true},
		{name: "human plan approval resumes at code", planReview: gate.PlanReviewHuman},
		{name: "both plan reviews retain the human gate", planReview: gate.PlanReviewBoth},
		{name: "human plan approval survives unrelated transitions", planReview: gate.PlanReviewHuman, answerHistory: true},
		{name: "lost lease resumes after approved plan", planReview: gate.PlanReviewAutomated, loseLease: true},
		{name: "replacement approval binds the published draft", planReview: gate.PlanReviewHuman, loseLease: true, answerHistory: true},
		{name: "workspace hook failure before provider", failure: "workspace"},
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
			testNativePlannerHandoff(t, test.planReview, test.answerHistory, test.loseLease, test.failure, test.planRecoveryRole, test.resumeCase, test.legacy, factories)
		})
	}
}

func testNativePlannerHandoff(t *testing.T, planReview string, answerHistory, loseLease bool, failure, planRecoveryRole, resumeCase string, legacy bool, factories Constructors) {
	t.Helper()
	planningRecovery := planRecoveryRole != ""
	manualCheckpoint := failure == "recovery" && resumeCase == ""
	cleanCheckpoint := resumeCase == "identity" || resumeCase == "policy"
	refusalMode, dispatchState := runner.RunModePlan, "Todo"
	if resumeCase != "" {
		refusalMode, dispatchState = runner.RunModeImplement, "In Progress"
	}
	states := hubserverPlanStates()
	humanPlan := planReview == gate.PlanReviewHuman || planReview == gate.PlanReviewBoth
	const legacyStop, legacyDraft = "Plan Review", "Implement README changes and verify them.\n\nPreserve  the two spaces."
	if legacy {
		states[0].Transitions = append(states[0].Transitions, legacyStop)
		states = append(states, tracker.NativeState{Name: legacyStop, Transitions: []string{"In Progress"}})
	}
	if humanPlan || failure == "recovery" {
		states[0].Transitions = append(states[0].Transitions, "Blocked")
		states[1].Transitions = append(states[1].Transitions, "Blocked")
		states = append(states, tracker.NativeState{Name: "Blocked", Transitions: []string{"Todo"}})
	}
	h := newPlannerHub(t, "Human Review", states, factories)
	checkProvider := planReview == gate.PlanReviewAutomated && !loseLease && !legacy
	if checkProvider {
		fixture, _ := schedulerFixtures.Load(h.scheduler)
		cfg := fixture.(*schedulerFixture).config
		cfg.ProviderReports = func() ([]providercapacity.Report, error) {
			return []providercapacity.Report{{Provider: "openai", Backend: "codex", AccountAlias: "example", Models: []string{"provider_default"}, MaxConcurrent: 1, Availability: "available", ObservedAt: time.Now().UTC()}}, nil
		}
		var err error
		h.scheduler, err = newTestScheduler(h.native.Transport(), cfg)
		if err != nil {
			t.Fatal(err)
		}
	}
	previousPolicyID := h.descriptor.ID
	h.descriptor.Gates.HumanReview = true
	h.descriptor.Gates.PlanEnabled = planReview != "" || planningRecovery
	h.descriptor.Gates.PlanReview = planReview
	if legacy {
		h.descriptor.Gates.PlanStopDigest = policy.Digest([]byte(legacyStop))
	}
	if planReview == "" {
		h.descriptor.Gates.PlanReview = gate.PlanReviewAutomated
	}
	h.descriptor = h.descriptor.WithID()
	if _, err := h.admin.ApproveProjectPolicy(t.Context(), policy.Change{ExpectedID: previousPolicyID, Policy: h.descriptor}); err != nil {
		t.Fatal(err)
	}
	requestedPolicy := h.descriptor
	if resumeCase == "policy" {
		prior := h.descriptor
		prior.ConfigDigest = policy.Digest([]byte("prior persisted session policy"))
		prior = prior.WithID()
		if _, err := h.admin.ApproveProjectPolicy(t.Context(), policy.Change{ExpectedID: h.descriptor.ID, Policy: prior}); err != nil {
			t.Fatal(err)
		}
		h.descriptor = prior
	}
	issue, err := h.connector.CreateIssue(t.Context(), connector.IssueDraft{Title: "Plan then implement", Body: "Update the README." + issueContractTestSections})
	if err != nil {
		t.Fatal(err)
	}
	confirmIssueContract(t, h.admin, issue.ID)
	if legacy {
		item := tracker.NativeWorkItemID(issue.ID)
		current, err := h.admin.Issue(t.Context(), item)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.admin.UpdateIssue(t.Context(), item, tracker.UpdateIssue{Mutation: api.NativeMutationKey(), ExpectedRevision: current.Revision, Labels: new([]string{gate.DefaultPlanApprovalLabel})}); err != nil {
			t.Fatal(err)
		}
		h.claim(t, issue.ID)
		old := h.scheduler.RunExecution(issue.ID)
		if err := old.Start(t.Context(), tracker.NativeExecutionIdentity{Role: runner.RolePlan, Backend: "codex", Model: "provider_default"}); err != nil {
			t.Fatal(err)
		}
		mutation := api.NativeMutationKey()
		mutation.LeaseID, mutation.FencingToken = old.Recovery().Lease.ID, old.Recovery().Lease.FencingToken
		artifact := "## Detent Plan\n\n### Artifact\n\n" + legacyDraft + "\n\n<!-- detent-plan-completion:" + strings.Repeat("a", 64) + " -->"
		if _, err := h.native.CreateComment(t.Context(), item, tracker.CreateComment{Mutation: mutation, Body: artifact}); err != nil {
			t.Fatal(err)
		}
		current, err = h.native.Issue(t.Context(), item)
		if err != nil {
			t.Fatal(err)
		}
		mutation.IdempotencyKey = api.NativeMutationKey().IdempotencyKey
		if _, err := h.native.Transition(t.Context(), item, tracker.Transition{Mutation: mutation, ExpectedRevision: current.Revision, State: legacyStop, Reason: "worker_progress"}); err != nil {
			t.Fatal(err)
		}
		if err := old.(runner.CompletionExecution).PrepareFinish(t.Context(), "succeeded", legacyDraft, nil); err != nil {
			t.Fatal(err)
		}
		if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "orchestrator_release"); err != nil {
			t.Fatal(err)
		}
		fixture, _ := schedulerFixtures.Load(h.scheduler)
		cfg := fixture.(*schedulerFixture).config
		h.scheduler, err = newTestScheduler(h.native.Transport(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		h.native.ApplyHeartbeatChanges(nil)
	}
	source := nativeChangeSourceRepo(t)
	remote := source
	if failure == "" || failure == "cleanup" {
		remote = filepath.Join(t.TempDir(), "remote.git")
		nativeChangeGit(t, source, "clone", "--bare", "--", source, remote)
	}
	nativeChangeGit(t, source, "remote", "add", "origin", nativeChangeRepository)
	h.configureCloneURL(remote)
	nativeChangeGit(t, source, "config", "url."+remote+".insteadOf", nativeChangeRepository)
	hookLog := filepath.Join(t.TempDir(), "worktrees")
	hooks := workspace.Hooks{AfterCreate: fmt.Sprintf("printf '%%s\\n' \"$PWD\" >> %q", hookLog)}
	if failure == "workspace" {
		t.Setenv("WORKSPACE_TEST_PRIVATE_VALUE", "correcthorsebatterystaple")
		hooks.AfterCreate = "printf '%s\\n' \"$WORKSPACE_TEST_PRIVATE_VALUE\"; exit 127"
	}
	backend, err := workspace.NewBackend(workspace.KindLocalGit, workspace.LocalGitOptions{ProcessSource: emptyProcessSource, Hooks: hooks, Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true})
	if err != nil {
		t.Fatal(err)
	}
	sessions := api.NewRunnerSessions(h.native)
	var preserved *tracker.NativeCheckpoint
	var preservedAttemptID string
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
		preservedAttemptID = execution.(*ownedExecution).CompletionData().AttemptID
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
		if err := execution.Start(t.Context(), tracker.NativeExecutionIdentity{Role: recordedRole, Backend: "codex", Model: "provider_default"}); err != nil {
			t.Fatal(err)
		}
		if resumeCase != "" {
			if resumeCase != "dirty" && (len(state.TrackedPaths) != 0 || len(state.UntrackedPaths) != 0 || state.UnpushedCommits != 0) {
				t.Fatalf("resume refusal setup is not clean: tracked=%d untracked=%d unpushed=%d", len(state.TrackedPaths), len(state.UntrackedPaths), state.UnpushedCommits)
			}
			started := h.clock().UTC()
			identity := agentidentity.Configured("codex", "codex", "", recordedRole, "provider_default", "openai", "", "", started)
			localAttempt := int64(issue.Number)
			sessionContext := sessions.BindExecution(t.Context(), execution)
			session, err := sessions.StartSession(sessionContext, store.SessionStart{ProjectID: "local", IssueID: issue.ID, WorkAttemptID: localAttempt, StartedAt: started, RequestedModel: "provider_default", Model: "provider_default", AgentBackendID: "codex", AgentBackendKind: "codex", AgentRole: recordedRole, RuntimeIdentity: identity})
			if err != nil {
				t.Fatal(err)
			}
			if err := sessions.FinishSession(sessionContext, session, store.SessionFinish{Turns: 1, CompletedAt: h.clock().UTC(), FinalState: "failed", ProviderThreadID: "recorded-thread", ProviderSessionID: "recorded-session"}); err != nil {
				t.Fatal(err)
			}
			if err := execution.(runner.RuntimeExecution).ObserveRuntime(t.Context(), tracker.NativeRuntimeObservation{LocalAttemptID: localAttempt, Generation: 1, Phase: "implementation", HeartbeatAt: started, Identity: identity}); err != nil {
				t.Fatal(err)
			}
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
		if err := h.native.Release(t.Context(), execution.Recovery().Lease, "released"); err != nil {
			t.Fatal(err)
		}
		if planningRecovery {
			if resumeCase != "" {
				change, err := h.admin.CreateChange(t.Context(), tracker.NativeWorkItemID(issue.ID), tracker.CreateChange{Mutation: api.NativeMutationKey(), Title: "Preserved current Change"})
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
	if resumeCase == "policy" {
		if _, err := h.admin.ApproveProjectPolicy(t.Context(), policy.Change{ExpectedID: h.descriptor.ID, Policy: requestedPolicy}); err != nil {
			t.Fatal(err)
		}
		h.descriptor = requestedPolicy
	}
	plan := gate.PlanConfig{Enabled: h.descriptor.Gates.PlanEnabled, Review: h.descriptor.Gates.PlanReview}
	provider := &nativePlanningAgent{failure: failure, humanReview: planReview == gate.PlanReviewBoth}
	if legacy {
		plan.Stop = legacyStop
	}

	workflowConfig := config.Config{Policy: h.descriptor, Plan: plan, Gate: gate.Config{Run: "true"}, Tracker: config.Tracker{Kind: config.TrackerHubNative}}
	if failure == "provider" || failure == "refused" {
		workflowConfig.Agents.Routes = []config.AgentRoute{{Name: "default", Backend: config.DefaultAgentBackendID, Model: "example-model", Default: true}}
	}
	agentDeps := runner.Dependencies{ProcessSource: emptyProcessSource,
		ProjectID: "local", Store: api.NewRunnerSessions(h.native),
		Workflow:  config.Workflow{Config: workflowConfig, Prompt: "Complete the issue"},
		Workspace: backend, AgentBackend: provider,
		ReapWorkspaceProcesses: func(context.Context, string, time.Duration) (int, error) {
			if failure == "cleanup" {
				return 0, errors.Join(errors.New("owned child cleanup deadline"), context.DeadlineExceeded)
			}
			return 0, nil
		},
	}
	agent, err := runner.NewRunner(agentDeps)
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan nativePlanFinish, 4)
	transport := &nativePlanTransport{next: h.failChanges, native: h.native, item: tracker.NativeWorkItemID(issue.ID), realRelease: failure == "", finished: finished, lost: make(chan nativePlanFinish, 1)}
	transport.loseLease.Store(loseLease)
	transport.lossPhase = "plan"
	if humanPlan {
		transport.lossPhase = "plan_draft"
	}
	if checkProvider {
		provider.checkProvider = func(_ context.Context, role string) error {
			if active, _ := transport.providerRole.Load().(string); active != role {
				return fmt.Errorf("active provider permit belongs to %s, want %s", active, role)
			}
			return nil
		}
	}
	fixtureClient(h.native.Transport()).requests.Store(transport)
	if failure == "recovery" && !manualCheckpoint && !cleanCheckpoint {
		fixtureClient(h.native.Transport()).requests.Store(executionRoundTrip(func(request *http.Request) (*http.Response, error) {
			if planningRecovery && request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/release") {
				current, readErr := h.native.Issue(request.Context(), tracker.NativeWorkItemID(issue.ID))
				claim, claimed := h.scheduler.ExecutionClaim(issue.ID)
				var authorityErr error
				if claimed {
					_, authorityErr = h.native.ValidateLease(request.Context(), claim.Lease)
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
		}))
	}
	orchCfg := orchestrator.Config{
		Project: scheduler.ProjectCandidate{ID: "local"}, Policy: h.descriptor, Plan: plan,
		PollInterval: time.Hour, MaxConcurrentAgents: 1,
		ActiveStates: []string{"Todo", "In Progress"}, ObservedStates: []string{"Human Review"}, TerminalStates: []string{"Done"},
	}
	observed := make(chan nativePlanRunObservation, 8)
	var runBackend runner.Backend = &nativePlanObservedRunner{Runner: agent, observed: observed}
	if failure == "recovery" {
		orchCfg.ObservedStates = append(orchCfg.ObservedStates, "Blocked")
	}
	if planningRecovery && !manualCheckpoint && !cleanCheckpoint {
		orchCfg.PollInterval = time.Hour
	}
	logs := io.Writer(io.Discard)
	if failure == "" {
		logs = t.Output()
	}
	dependencies := orchestrator.Dependencies{Connector: h.connector, Scheduling: h.scheduler, Runner: runBackend, Logger: slog.New(slog.NewTextHandler(logs, nil))}
	released := make(chan error, 1)
	releaseScheduler := &nativePlanReleaseScheduler{Scheduler: h.scheduler}
	if failure == "workspace" {
		releaseScheduler.released = released
	}
	dependencies.Scheduling = releaseScheduler
	orch, err := orchestrator.New(orchCfg, dependencies)
	if err != nil {
		t.Fatal(err)
	}
	startOrchestrator := func(current *orchestrator.Orchestrator) func() {
		runContext, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- current.Run(runContext) }()
		var once sync.Once
		stop := func() {
			once.Do(func() {
				cancel()
				if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
					t.Error(err)
				}
			})
		}
		t.Cleanup(stop)
		return stop
	}
	stopOrchestrator := startOrchestrator(orch)
	if failure == "workspace" {
		select {
		case releaseErr := <-released:
			current, readErr := h.admin.Issue(t.Context(), tracker.NativeWorkItemID(issue.ID))
			finished <- nativePlanFinish{state: current.State, err: errors.Join(releaseErr, readErr)}
		case <-testenv.WaitContext(t, t.Context()).Done():
			t.Fatal("workspace setup failure did not release its claim")
		}
	}
	wantStates := []string{"Human Review"}
	if failure == "recovery" && !manualCheckpoint && !cleanCheckpoint {
		wantStates = []string{"Todo"}
	}
	restart := func() {
		stopOrchestrator()
		h.native.ApplyHeartbeatChanges(nil)
		replacementSource := filepath.Join(t.TempDir(), "source")
		nativeChangeGit(t, source, "clone", "--", remote, replacementSource)
		nativeChangeGit(t, replacementSource, "remote", "set-url", "origin", nativeChangeRepository)
		nativeChangeGit(t, replacementSource, "config", "url."+remote+".insteadOf", nativeChangeRepository)
		agentDeps.Workspace, err = workspace.NewBackend(workspace.KindLocalGit, workspace.LocalGitOptions{ProcessSource: emptyProcessSource, Hooks: hooks, Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: replacementSource, AutoBranch: true})
		if err != nil {
			t.Fatal(err)
		}
		replacement, err := runner.NewRunner(agentDeps)
		if err != nil {
			t.Fatal(err)
		}
		dependencies.Runner = &nativePlanObservedRunner{Runner: replacement, observed: observed}
		orch, err = orchestrator.New(orchCfg, dependencies)
		if err != nil {
			t.Fatal(err)
		}
		stopOrchestrator = startOrchestrator(orch)
	}
	if loseLease {
		var injected nativePlanFinish
		select {
		case injected = <-transport.lost:
		case returned := <-observed:
			select {
			case injected = <-transport.lost:
			default:
				t.Fatalf("the runner returned before lease loss was injected: %+v", returned)
			}
		case <-testenv.WaitContext(t, t.Context()).Done():
			t.Fatal("the plan receipt was not published")
		}
		if injected.err != nil || injected.state != "In Progress" {
			t.Fatalf("could not lose the lease after the plan receipt: %+v", injected)
		}
		restart()
	}
	if humanPlan {
		wantPlans := int64(1)
		select {
		case got := <-finished:
			if got.err != nil || got.state != "Blocked" {
				t.Fatalf("plan did not release in the human wait lane: %+v", got)
			}
		case <-testenv.WaitContext(t, t.Context()).Done():
			t.Fatal("plan did not release for human approval")
		}
		current, err := h.admin.Recovery(t.Context(), tracker.NativeWorkItemID(issue.ID))
		if err != nil || len(current.Attempts) == 0 || current.Attempts[len(current.Attempts)-1].Disposition == nil || !current.Attempts[len(current.Attempts)-1].Disposition.HumanAction ||
			provider.plans.Load() != wantPlans || provider.implementations.Load() != 0 || len(h.candidates(t)) != 0 {
			t.Fatalf("plan approval did not hold code: attempts=%+v error=%v", current.Attempts, err)
		}
		if loseLease {
			receipts, err := h.admin.ItemPhaseReceipts(t.Context(), current.Issue.WorkItemID)
			if err != nil || len(receipts) < 2 || receipts[0].Phase != "plan_draft" || receipts[0].AttemptID != current.Attempts[len(current.Attempts)-1].AttemptID || receipts[0].InputDigest != receipts[1].InputDigest {
				t.Fatalf("replacement question is not bound to the same draft: %+v, %v", receipts, err)
			}
		}
		stopOrchestrator()
		direction, err := h.admin.Transition(t.Context(), current.Issue.WorkItemID, tracker.Transition{Mutation: api.NativeMutationKey(), ExpectedRevision: current.Issue.Revision, State: "Todo", Reason: "user_requested", ReasonDetail: "Approve plan"})
		if err != nil {
			t.Fatal(err)
		}
		if answerHistory {
			transitions, err := h.admin.RecentWorkflowTransitions(t.Context(), direction.WorkItemID, 1)
			if err != nil || len(transitions) != 1 || transitions[0].Data.BlockerAttemptID != current.Attempts[len(current.Attempts)-1].AttemptID {
				t.Fatalf("answer did not record the draft attempt identity: %v %v", transitions, err)
			}
			path, err := api.NativeItemPath(direction.WorkItemID)
			if err != nil {
				t.Fatal(err)
			}
			var unrelated tracker.Page[tracker.CollaborationEvent]
			if err := h.admin.Transport().Request(t.Context(), http.MethodGet, h.admin.Scope()+path+"/history?blocker_attempt_id=attempt_unrelated", nil, &unrelated); err != nil || len(unrelated.Items) != 0 {
				t.Fatalf("answer lookup returned an unrelated attempt: %v %v", unrelated.Items, err)
			}
			for _, state := range []string{"In Progress", "Todo", "In Progress", "Todo"} {
				direction, err = h.admin.Transition(t.Context(), direction.WorkItemID, tracker.Transition{Mutation: api.NativeMutationKey(), ExpectedRevision: direction.Revision, State: state, Reason: "user_requested"})
				if err != nil {
					t.Fatal(err)
				}
			}
		}
		recovery, err := h.admin.Recovery(t.Context(), direction.WorkItemID)
		if err != nil {
			t.Fatal(err)
		}
		receipts, err := h.admin.ItemPhaseReceipts(t.Context(), direction.WorkItemID)
		if err != nil {
			t.Fatal(err)
		}
		resolved, err := h.admin.ResolvePlan(t.Context(), recovery, h.descriptor.ID, receipts)
		if err != nil || resolved == nil || resolved.Answer != "Approve plan" {
			t.Fatalf("recorded answer did not resolve: %+v, %v", resolved, err)
		}
		restart()
	}
	if failure == "refused" || failure == "workspace" {
		wantStates = []string{"Todo"}
	}
	for _, want := range wantStates {
		select {
		case got := <-finished:
			if got.err != nil {
				t.Fatal(got.err)
			}
			if planningRecovery && !manualCheckpoint && !cleanCheckpoint {
				select {
				case run := <-observed:
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
		if a.Identity != nil || a.Status != "failed" || a.TerminalFailure == nil || a.TerminalFailure.Error != "workspace hook after_create failed (exit status 127)" || a.TerminalFailure.Stage != "pre_provider" || a.TerminalFailure.Attribution != "instance" || a.TerminalFailure.ObservedAt.IsZero() || a.Runtime == nil || a.Runtime.LocalAttemptID != 0 || a.Runtime.Generation == 0 || a.Runtime.Activity != nil {
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
		if current.Issue.State != "Human Review" || provider.calls.Load() != 1 || provider.verdictCalls.Load() != 1 || len(changes) != 1 || changes[0].CurrentVersion == "" {
			t.Fatalf("historical judgment prevented delivery or lost source: state=%s attempts=%d work_turns=%d verdict_turns=%d changes=%d error=%v", current.Issue.State, len(current.Attempts), provider.calls.Load(), provider.verdictCalls.Load(), len(changes), err)
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
		testenv.WaitUntil(t, t.Context(), func() error {
			current, err := h.admin.Recovery(t.Context(), tracker.NativeWorkItemID(issue.ID))
			if err != nil {
				return err
			}
			if len(current.Attempts) < 2 {
				return errors.New("recovery refusal has not recorded its Hub attempt")
			}
			attempt := current.Attempts[len(current.Attempts)-1]
			if attempt.AttemptID == preservedAttemptID || attempt.Status == "running" {
				return errors.New("recovery refusal did not settle Hub attempt")
			}
			return nil
		})
	}
	if planningRecovery {
		current, err := h.admin.Recovery(t.Context(), tracker.NativeWorkItemID(issue.ID))
		if err != nil || current.Issue.State != "Todo" || (len(current.Attempts) != len(preservedRecovery.Attempts)+1 || !reflect.DeepEqual(current.Attempts[:len(current.Attempts)-1], preservedRecovery.Attempts)) || !reflect.DeepEqual(current.Change, preservedRecovery.Change) || !reflect.DeepEqual(current.ChangeDetail, preservedRecovery.ChangeDetail) || provider.calls.Load() != 0 || !reflect.DeepEqual(changes, preservedChanges) {
			t.Errorf("planning refusal changed preserved evidence: state=%s receipt_unchanged=%v change_unchanged=%v calls=%d changes=%d error=%v", current.Issue.State, reflect.DeepEqual(current.Attempts, preservedRecovery.Attempts), reflect.DeepEqual(current.Change, preservedRecovery.Change), provider.calls.Load(), len(changes), err)
		}
		assertSetupRefusalReceipt(t, current.Attempts)
		if (resumeCase == "identity" || resumeCase == "policy") && provider.resumeVerified.Load() != 1 {
			t.Fatalf("resume refusal did not exercise the intended boundary: case=%s verification_calls=%d", resumeCase, provider.resumeVerified.Load())
		}
		assertInstanceRecoveryRetry(t, h, orch, issue.ID)
		select {
		case extra := <-finished:
			t.Fatalf("duplicate release notification: %+v", extra)
		default:
		}
		return
	}

	if failure == "provider" || failure == "refused" {
		wantVerdicts := int64(0)
		if failure == "provider" {
			wantVerdicts = 1
		}
		if len(changes) != 0 || provider.calls.Load() != 1 || provider.verdictCalls.Load() != wantVerdicts {
			t.Fatalf("failed native run created a Change or repeated coding: changes=%v work_turns=%d verdict_turns=%d", changes, provider.calls.Load(), provider.verdictCalls.Load())
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
		spend, err := h.native.IssueTokenSpend(t.Context(), store.IssueIdentity{ProjectID: "local", IssueID: issue.ID})
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
		recovery, err := h.admin.Recovery(t.Context(), tracker.NativeWorkItemID(issue.ID))
		if err != nil || len(recovery.Attempts) == 0 {
			t.Fatalf("implementation did not publish a Change or attempt receipt: changes=%+v error=%v", changes, err)
		}
		last := recovery.Attempts[len(recovery.Attempts)-1]
		t.Fatalf("implementation did not publish Change Request: changes=%+v outcome=%s checkpoint=%+v finalization=%+v terminal=%+v", changes, last.Status, last.Checkpoint, last.Finalization, last.TerminalFailure)
	}
	if failure == "" {
		item := tracker.NativeWorkItemID(issue.ID)
		records, err := h.admin.Attempts(t.Context(), item, "")
		wantAttempts := 1
		if legacy {
			wantAttempts++
		}
		if humanPlan {
			wantAttempts++
		}
		if loseLease {
			wantAttempts++
		}
		if err != nil || len(records.Items) != wantAttempts {
			t.Fatalf("attempts = %d, want %d, error=%v", len(records.Items), wantAttempts, err)
		}
		slices.SortFunc(records.Items, func(left, right tracker.NativeAttempt) int {
			return cmp.Compare(left.FencingToken, right.FencingToken)
		})
		receipts, err := h.admin.ItemPhaseReceipts(t.Context(), item)
		if err != nil {
			t.Fatal(err)
		}
		attempt := records.Items[wantAttempts-1]
		for _, phase := range []string{"plan", "implement"} {
			expected := attempt
			if loseLease && !humanPlan && phase != "implement" {
				expected = records.Items[0]
			}
			want := phase == "implement" || plan.Enabled
			if found := slices.ContainsFunc(receipts, func(receipt tracker.NativePhaseReceipt) bool {
				return receipt.Phase == phase && receipt.AttemptID == expected.AttemptID &&
					receipt.LeaseID == expected.LeaseID && receipt.FencingToken == expected.FencingToken
			}); found != want {
				t.Fatalf("wrong %s receipt for attempt %s: %+v", phase, expected.AttemptID, receipts)
			}
		}
		wantPlans := int64(0)
		if plan.Enabled {
			wantPlans = 1
		}
		if provider.plans.Load() != wantPlans || provider.implementations.Load() != 1 {
			t.Fatalf("planning turns=%d, implementation turns=%d", provider.plans.Load(), provider.implementations.Load())
		}
		wantWorktrees := wantAttempts
		if legacy {
			wantWorktrees--
			accepted, err := h.admin.NativeAttempt(t.Context(), item, attempt.AttemptID)
			if err != nil || !slices.ContainsFunc(accepted.Steps, func(step tracker.NativeStepReceipt) bool {
				return step.Step == "plan" && step.CompletionBody != legacyDraft && step.PlanApproval == "automated:APPROVED"
			}) {
				t.Fatalf("fresh claim did not record an independently approved new plan: %+v, %v", accepted.Steps, err)
			}
		}
		if plan.Enabled && (provider.planWorkspace == provider.workspace) != (wantWorktrees == 1) {
			t.Fatalf("wrong worktree reuse: plan=%q code=%q", provider.planWorkspace, provider.workspace)
		}
		worktrees, err := os.ReadFile(hookLog)
		if err != nil || len(strings.Fields(string(worktrees))) != wantWorktrees {
			t.Fatalf("workspace creation hooks=%q, want %d paths, error=%v", worktrees, wantWorktrees, err)
		}
		if wantAttempts > 1 && (records.Items[0].LeaseID == attempt.LeaseID || records.Items[0].FencingToken >= attempt.FencingToken) {
			t.Fatal("replacement did not take a new fence")
		}
	}
	if failure == "cleanup" {
		receipts, err := h.admin.ItemPhaseReceipts(t.Context(), tracker.NativeWorkItemID(issue.ID))
		if err != nil || len(receipts) == 0 || provider.calls.Load() != 1 {
			t.Fatalf("finished work was not pushed under the original run: work_turns=%d receipts=%v error=%v", provider.calls.Load(), receipts, err)
		}
		receipt := receipts[0]
		if strings.Contains(receipt.Branch, "-finalization-") {
			t.Fatalf("finished work was pushed to a finalization branch: %s", receipt.Branch)
		}
		nativeChangeGit(t, remote, "merge-base", "--is-ancestor", receipt.HeadSHA, "refs/heads/"+receipt.Branch)
		nativeChangeGit(t, remote, "cat-file", "-e", receipt.HeadSHA+":CHANGE.md")
	}
}

type nativePlanningAgent struct {
	checkProvider   func(context.Context, string) error
	failure         string
	humanReview     bool
	calls           atomic.Int64
	verdictCalls    atomic.Int64
	plans           atomic.Int64
	implementations atomic.Int64
	workspace       string
	planWorkspace   string
	resumeVerified  atomic.Int64
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

	if strings.HasPrefix(req.Prompt, "Judge the completed Detent round") {
		if !req.TextOnly || !req.ReadOnly || req.Resume != (runner.AgentResume{}) || req.Workspace == a.workspace {
			return runner.AgentTurnResult{}, errors.New("round verdict was not isolated from the work turn")
		}
		a.verdictCalls.Add(1)
		return runner.AgentTurnResult{}, update(runner.AgentUpdate{Type: runner.AgentUpdateMessageDelta, Delta: `{"action":"continue","next_phase":"implement","reason":"The host recorded the current source and outcome."}`})
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
	review := strings.HasPrefix(req.Prompt, "Independently review the proposed implementation plan")
	if a.checkProvider != nil {
		role := runner.RoleCode
		if plan || review {
			role = runner.RolePlan
		}
		if err := a.checkProvider(ctx, role); err != nil {
			return runner.AgentTurnResult{}, err
		}
	}
	if plan {
		a.plans.Add(1)
		a.planWorkspace = req.Workspace
		return runner.AgentTurnResult{}, update(runner.AgentUpdate{Type: runner.AgentUpdateMessageDelta, Delta: "Implement README changes and verify them."})
	}
	if review {
		if !req.ReadOnly || req.Resume != (runner.AgentResume{}) || req.Workspace != a.planWorkspace {
			return runner.AgentTurnResult{}, errors.New("plan review did not inspect the retained worktree independently")
		}
		if a.humanReview {
			return runner.AgentTurnResult{}, update(runner.AgentUpdate{Type: runner.AgentUpdateMessageDelta, Delta: "HUMAN REVIEW"})
		}
		return runner.AgentTurnResult{}, update(runner.AgentUpdate{Type: runner.AgentUpdateMessageDelta, Delta: "APPROVED"})
	}
	if a.plans.Load() > 0 && !strings.Contains(req.Prompt, "Implement README changes and verify them.") {
		return runner.AgentTurnResult{}, errors.New("approved plan missing from the implementation prompt")
	}
	a.implementations.Add(1)
	result, err := (&committingAgent{staged: true, complete: a.failure == "cleanup" || a.failure == ""}).RunTurn(ctx, req, update)
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
	mode         string
	state        string
	recordedRole string
	err          error
}

type nativePlanReleaseScheduler struct {
	*Scheduler
	released chan<- error
}

func (s *nativePlanReleaseScheduler) ReleaseClaim(ctx context.Context, id, reason string) error {
	err := s.Scheduler.ReleaseClaim(ctx, id, reason)
	if s.released != nil {
		select {
		case s.released <- err:
		default:
		}
	}
	return err
}

type nativePlanObservedRunner struct {
	*runner.Runner
	observed chan<- nativePlanRunObservation
}

func (r *nativePlanObservedRunner) Run(ctx context.Context, request runner.RunRequest) (runner.RunResult, error) {
	observation := nativePlanRunObservation{mode: request.Mode, state: request.Issue.State}
	if request.Execution != nil {
		if source := request.Execution.Recovery().SourceAttempt(); source != nil && source.Identity != nil {
			observation.recordedRole = source.Identity.Role
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
	item         tracker.NativeWorkItemID
	realRelease  bool
	finished     chan<- nativePlanFinish
	lost         chan nativePlanFinish
	loseLease    atomic.Bool
	lostLease    atomic.Value
	lossPhase    string
	providerRole atomic.Value
}

func (t *nativePlanTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.loseLease.Load() && req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/phases") {
		body, err := req.GetBody()
		if err != nil {
			return nil, err
		}
		defer body.Close()
		var receipt tracker.NativePhaseReceipt
		if err := json.NewDecoder(body).Decode(&receipt); err != nil {
			return nil, err
		}
		if receipt.Phase == t.lossPhase {
			response, err := t.next.RoundTrip(req)
			if err == nil && response.StatusCode == http.StatusOK && t.loseLease.CompareAndSwap(true, false) {
				item := tracker.NativeWorkItemID(strings.Split(strings.TrimSuffix(req.URL.Path, "/phases"), "/work-items/")[1])
				issue, readErr := t.native.Issue(req.Context(), item)
				t.lostLease.Store(string(receipt.LeaseID))
				lossErr := t.native.Release(req.Context(), tracker.NativeLease{ID: receipt.LeaseID, WorkItemID: item, FencingToken: receipt.FencingToken}, "released")
				receipt.IdempotencyKey += ":stale"
				if _, staleErr := t.native.PublishPhaseReceipt(req.Context(), item, receipt); staleErr == nil {
					lossErr = errors.Join(lossErr, errors.New("retired planning authority published a phase"))
				}
				t.lost <- nativePlanFinish{state: issue.State, err: errors.Join(readErr, lossErr)}
			}
			return response, err
		}
	}
	response, err := t.next.RoundTrip(req)
	if err == nil && response.StatusCode == http.StatusOK && req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/claims") {
		raw, readErr := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		response.Body = io.NopCloser(strings.NewReader(string(raw)))
		var lease tracker.NativeLease
		if err := json.Unmarshal(raw, &lease); err != nil {
			return nil, err
		}
		if lease.ProviderReservation != nil {
			t.providerRole.Store(lease.ProviderReservation.Role)
		}
	}
	if err == nil && response.StatusCode < 300 && req.Method == http.MethodPost && (strings.HasSuffix(req.URL.Path, "/renew") || t.realRelease && strings.HasSuffix(req.URL.Path, "/release")) {
		body, readErr := req.GetBody()
		if readErr == nil {
			var mutation tracker.NativeLeaseMutation
			readErr = json.NewDecoder(body).Decode(&mutation)
			_ = body.Close()
			if readErr == nil && mutation.ProviderPhase != nil {
				t.providerRole.Store(mutation.ProviderPhase.Requirement.Role)
			}
			if readErr == nil && t.realRelease && mutation.Reason == "released" {
				lost, _ := t.lostLease.Load().(string)
				if lost == "" || !strings.HasSuffix(req.URL.Path, "/"+lost+"/release") {
					issue, readErr := t.native.Issue(req.Context(), t.item)
					t.finished <- nativePlanFinish{state: issue.State, err: readErr}
				}
			}
		}
	}
	if t.realRelease || err != nil || response.StatusCode >= 300 || req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, "/events") {
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
	if event.Data.Outcome == "cancelled" {
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

type phaseResponses []int

func (p *phaseResponses) RoundTrip(*http.Request) (*http.Response, error) {
	status := (*p)[0]
	*p = (*p)[1:]
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(""))}, nil
}

func TestPlanLeaseLossWaitsForPublishedReceipt(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/work-items/wi_plan"):
			if err := json.NewEncoder(w).Encode(tracker.NativeIssue{State: "In Progress"}); err != nil {
				t.Error(err)
			}
		case strings.HasSuffix(req.URL.Path, "/release"):
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(req.URL.Path, "/phases"):
			w.WriteHeader(http.StatusConflict)
		default:
			t.Errorf("unexpected request: %s", req.URL.Path)
			w.WriteHeader(http.StatusNotFound)
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
	responses := phaseResponses{http.StatusServiceUnavailable, http.StatusOK}
	transport := nativePlanTransport{next: &responses, native: native, lost: make(chan nativePlanFinish, 1), lossPhase: "plan"}
	transport.loseLease.Store(true)
	body, err := json.Marshal(tracker.NativePhaseReceipt{Mutation: tracker.Mutation{LeaseID: "lease_plan", FencingToken: 1}, Phase: "plan"})
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []int{http.StatusServiceUnavailable, http.StatusOK} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/work-items/wi_plan/phases", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		response, err := transport.RoundTrip(req)
		if err != nil || response.StatusCode != status {
			t.Fatalf("phase response = %v, %v, want %d", response, err, status)
		}
		if err := response.Body.Close(); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case lost := <-transport.lost:
		if lost.err != nil || lost.state != "In Progress" {
			t.Fatalf("failed to retire published lease: %+v", lost)
		}
	default:
		t.Fatal("the failed POST consumed the lease-loss trigger")
	}
}
