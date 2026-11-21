package runner

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/config"
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
