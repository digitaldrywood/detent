package runner

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type planLimitsExecution struct {
	stubExecution
	requirement providercapacity.Requirement
}

func (*planLimitsExecution) StartPhased(context.Context, tracker.NativeExecutionIdentity) error {
	return nil
}

func (e *planLimitsExecution) SetPhaseProvider(_ context.Context, requirement providercapacity.Requirement) error {
	e.requirement = requirement
	return nil
}

type publishedChangePlanExecution struct {
	phaseCompletionExecution
	step tracker.NativeStepReceipt
}

func (e *publishedChangePlanExecution) StartPhased(ctx context.Context, identity tracker.NativeExecutionIdentity) error {
	return e.Start(ctx, identity)
}

func (*publishedChangePlanExecution) SetPhaseProvider(context.Context, providercapacity.Requirement) error {
	return errors.New("provider requested before human plan approval")
}

func (e *publishedChangePlanExecution) RecordStep(_ context.Context, step tracker.NativeStepReceipt) error {
	e.step = step
	return nil
}

type publishedChangePlanWorkspace struct {
	*fakeWorkspaceBackend
	itemPhaseWorkspace
	head, tree string
}

func (w *publishedChangePlanWorkspace) ItemPhaseHead(context.Context, workspace.Info, workspace.Issue) (string, string, error) {
	return w.head, w.tree, nil
}

func (w *publishedChangePlanWorkspace) PushItemPhase(ctx context.Context, info workspace.Info, _ workspace.Issue, authorize func(context.Context) error) (string, string, string, error) {
	return info.PublishBranch(), w.head, w.tree, authorize(ctx)
}

func TestPublishedChangeRecoveryRequiresHumanPlanApproval(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		plan                 *tracker.NativePlanState
		uncertainPublication bool
		publishedReceipt     bool
	}{
		{name: "unapproved draft", plan: &tracker.NativePlanState{Draft: "Implement the requested change."}},
		{name: "pending publication cannot skip approval", plan: &tracker.NativePlanState{Draft: "Implement the requested change."}, uncertainPublication: true},
		{name: "missing plan decision fails closed"},
		{name: "published receipt cannot replace plan approval", publishedReceipt: true},
		{name: "approval does not bypass uncertain publication", uncertainPublication: true, plan: &tracker.NativePlanState{
			Draft: "Implement the requested change.", Answer: "Approve plan", AnswerID: "answer_approved", AnswerAttemptID: "attempt_prior",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			head, tree := strings.Repeat("a", 40), strings.Repeat("b", 40)
			change := tracker.NativeChangeReference{VersionID: "version_published", HeadSHA: head}
			recovery := tracker.NativeRecovery{Lease: tracker.NativeLease{ID: "lease_replacement", FencingToken: 2}, Change: &change}
			if tc.uncertainPublication {
				recovery.Attempts = []tracker.NativeAttempt{{
					NativeRunData: tracker.NativeRunData{AttemptID: "attempt_prior"},
					Checkpoint:    &tracker.NativeCheckpoint{ExternalEffect: "pr_create", EffectState: "pending", HeadSHA: head},
				}}
			}
			execution := &publishedChangePlanExecution{phaseCompletionExecution: phaseCompletionExecution{phaseCheckoutExecution: phaseCheckoutExecution{
				testExecution: &testExecution{recovery: recovery},
				continuation:  tracker.NativePhaseContinuation{Plan: tc.plan},
				publishedSource: workspace.ChangeSource{Version: tracker.ChangeVersion{
					ID: change.VersionID, ChangeVersionInput: tracker.ChangeVersionInput{HeadSHA: head},
				}},
			}}}
			if tc.publishedReceipt {
				execution.continuation.PublishedChangeReceipt = &tracker.NativePhaseReceipt{HeadSHA: head, TreeSHA: tree, Phase: "implement"}
			}
			workspaceBackend := &publishedChangePlanWorkspace{
				fakeWorkspaceBackend: &fakeWorkspaceBackend{
					info:           workspace.Info{Path: t.TempDir(), Branch: "detent/example"},
					recoveryStates: []workspace.RecoveryState{{HeadSHA: head}},
				},
				head: head, tree: tree,
			}
			agent := &fakeCodexClient{}
			cfg := config.Default()
			cfg.Plan = gate.PlanConfig{Enabled: true, Review: gate.PlanReviewHuman}
			r, err := NewRunner(Dependencies{
				Workspace: workspaceBackend, ProcessSource: emptyProcessSource, AgentBackend: agent,
				Workflow: config.Workflow{Config: cfg, Prompt: "Complete the issue"},
			})
			if err != nil {
				t.Fatal(err)
			}
			result, err := r.Run(t.Context(), RunRequest{Mode: RunModeImplement, Issue: connector.Issue{ID: "example", Identifier: "EX-1"}, Execution: execution})
			if agent.calls != 0 || result.TurnStarted {
				t.Fatalf("implementation started without approval: calls=%d result=%+v", agent.calls, result)
			}
			if tc.plan == nil {
				if err == nil || !strings.Contains(err.Error(), "human plan approval is unavailable") || workspaceBackend.created {
					t.Fatalf("missing plan decision reached checkout: created=%t error=%v", workspaceBackend.created, err)
				}
				return
			}
			if tc.plan.AnswerID != "" {
				if !errors.Is(err, ErrNativeRecoveryRequired) || execution.publication.Phase != "plan" {
					t.Fatalf("approval skipped source reconciliation: receipt=%s error=%v", execution.publication.Phase, err)
				}
				return
			}
			if err != nil || result.Output != RunOutputPlanApprovalWait || !strings.Contains(result.FinalMessage, tracker.PlanApprovalQuestion) ||
				execution.step.Step != "plan" || execution.publication.Phase != "plan_draft" || execution.publication.HeadSHA != head ||
				workspaceBackend.createIssue.Source == nil || workspaceBackend.createIssue.Source.Version.ID != change.VersionID {
				t.Fatalf("published Change did not wait for approval: output=%s plan=%+v receipt=%+v error=%v", result.Output, execution.step, execution.publication, err)
			}
		})
	}
}

func TestPlanPhaseAgentEnforcesSessionLimits(t *testing.T) {
	for _, test := range []struct {
		name    string
		updates []AgentUpdate
		want    error
	}{
		{name: "draft token ceiling", updates: []AgentUpdate{{Type: AgentUpdateTokenUsage, Tokens: AgentTokenUsage{TotalTokens: 101}}}, want: ErrSessionTokenCeilingExceeded},
		{name: "draft memory ceiling", updates: []AgentUpdate{{Type: AgentUpdateProcessStarted, WorkerProcess: procgroup.Identity{PID: 1899}}}, want: ErrSessionMemoryCeilingExceeded},
		{name: "review accumulates draft tokens", updates: []AgentUpdate{
			{Type: AgentUpdateTokenUsage, Tokens: AgentTokenUsage{ThreadTotal: &AgentTokenCounts{TotalTokens: 60}}},
			{Type: AgentUpdateTokenUsage, Tokens: AgentTokenUsage{ThreadTotal: &AgentTokenCounts{TotalTokens: 41}}},
		}, want: ErrSessionTokenCeilingExceeded},
		{name: "review memory ceiling", updates: []AgentUpdate{
			{Type: AgentUpdateTokenUsage, Tokens: AgentTokenUsage{TotalTokens: 10}},
			{Type: AgentUpdateProcessStarted, WorkerProcess: procgroup.Identity{PID: 1899}},
		}, want: ErrSessionMemoryCeilingExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			turn := 0
			backend := &sessionLimitsBackend{duringTurn: func(_ AgentTurnRequest, emit AgentUpdateHandler) error {
				update := test.updates[turn]
				turn++
				return emit(update)
			}}
			workflow := config.Workflow{Config: config.Config{Agent: config.Agent{MaxSessionTokens: 100}}}
			r, err := NewRunner(Dependencies{ProcessSource: emptyProcessSource, Workflow: workflow,
				Workspace: &fakeWorkspaceBackend{info: workspace.Info{Path: dir}}, AgentBackend: backend})
			if err != nil {
				t.Fatal(err)
			}
			r.maxAgentRSSBytes, r.rssPollInterval = 512, time.Second
			r.processRSS = func(context.Context, procgroup.Identity) (uint64, error) { return 513, nil }
			execution := &planLimitsExecution{}
			_, runtime, _, _ := r.runtimeSnapshot()
			generate, _, err := r.planPhaseAgent(t.Context(), RunRequest{Execution: execution}, workflow, runtime, workspace.Info{Path: dir}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(test.updates) == 2 {
				if _, err := generate(t.Context(), "Draft the plan."); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := generate(t.Context(), "Finish the plan."); !errors.Is(err, test.want) {
				t.Fatalf("plan turn error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestPlanPhaseAgentAdmitsTheSelectedPlanner(t *testing.T) {
	dir := t.TempDir()
	checker := &fakeBudgetChecker{refusal: budget.Refusal{Code: budget.ReasonPerDayMaxUSD}}
	estimator := &fakeDispatchEstimator{}
	backend := &sessionLimitsBackend{}
	workflow := config.Workflow{Config: config.Config{Agent: config.Agent{}, Budget: config.Budget{BillingMode: config.BillingModeMetered}, Agents: config.Agents{Routes: []config.AgentRoute{
		{Name: "code", Role: RoleCode, Backend: "codex", Model: "implementation-model", Default: true},
		{Name: "plan", Role: RolePlan, Backend: "codex", Model: "planner-model", Default: true},
	}}}}
	r, err := NewRunner(Dependencies{ProcessSource: emptyProcessSource, Workflow: workflow,
		Workspace: &fakeWorkspaceBackend{info: workspace.Info{Path: dir}}, AgentBackend: backend,
		BudgetChecker: checker, DispatchEstimator: estimator})
	if err != nil {
		t.Fatal(err)
	}
	execution := &planLimitsExecution{}
	_, runtime, _, _ := r.runtimeSnapshot()
	generate, admission, err := r.planPhaseAgent(t.Context(), RunRequest{Execution: execution}, workflow, runtime, workspace.Info{Path: dir}, checker, estimator)
	if err == nil || admission.BudgetRefusal == nil || generate != nil || checker.model != "planner-model" || estimator.model != "planner-model" || execution.requirement.Role != "" {
		t.Fatalf("planner admission = %v, checked model %q, estimated model %q, provider %v", err, checker.model, estimator.model, execution.requirement)
	}
}
