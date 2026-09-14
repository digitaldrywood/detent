package runner

import (
	"context"
	"errors"
	"testing"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/selector"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestDispatchCapacityPreservesRouting(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, state, mode, body, modelOverride, routeModel, wantModel, wantRole string
		catalogErr                                                              error
	}{
		{name: "configured model", routeModel: "sol", wantModel: "sol", wantRole: RoleCode},
		{name: "issue field", modelOverride: "astra", wantModel: "astra", wantRole: RoleCode},
		{name: "route precedes field", modelOverride: "astra", routeModel: "sol", wantModel: "sol", wantRole: RoleCode},
		{name: "explicit body model", routeModel: "sol", body: "```detent-agent\nschema: 1\nmodel: astra\neffort: high\n```", wantModel: "astra", wantRole: RoleCode},
		{name: "effort does not upgrade", routeModel: "sol", body: "```detent-agent\nschema: 1\neffort: xhigh\n```", wantModel: "sol", wantRole: RoleCode},
		{name: "retired override retains configured fallback", routeModel: "sol", body: "```detent-agent\nschema: 1\nmodel: retired\n```", wantModel: "sol", wantRole: RoleCode},
		{name: "catalog failure cannot affect dispatch", routeModel: "sol", body: "```detent-agent\nschema: 1\nmodel: astra\n```", catalogErr: errors.New("offline"), wantModel: "astra", wantRole: RoleCode},
		{name: "provider default remains explicit", wantModel: "provider_default", wantRole: RoleCode},
		{name: "plan role", mode: RunModePlan, routeModel: "sol", wantModel: "plan-model", wantRole: RolePlan},
		{name: "merge role fallback", state: "Merging", routeModel: "sol", wantModel: "sol", wantRole: RoleMerge},
		{name: "rework role", state: "Rework", routeModel: "sol", wantModel: "sol", wantRole: RoleRework},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			backend := &catalogAgentBackend{models: []AgentModel{{ID: "sol", Model: "sol", SupportedReasoningEfforts: []string{"high", "xhigh"}}, {ID: "astra", Model: "astra", SupportedReasoningEfforts: []string{"high"}}, {ID: "retired", Model: "retired", Upgrade: "astra"}}, err: test.catalogErr}
			router, err := NewRouter([]Route{{BackendID: "code", Default: true, Model: test.routeModel}, {BackendID: "plan", Role: RolePlan, Default: true, Model: "plan-model"}})
			if err != nil {
				t.Fatal(err)
			}
			r := &Runner{agentRuntime: agentRuntime{router: router, backends: map[string]AgentBackend{"code": backend, "plan": backend}, backendConfigs: map[string]config.AgentBackend{"code": {ID: "code"}, "plan": {ID: "plan"}}}}
			issue := connector.Issue{State: test.state, Description: test.body, ModelOverride: test.modelOverride}
			result, err := r.DispatchCapacity(t.Context(), RunRequest{Issue: issue, Mode: test.mode, SelectorContext: selector.Context{}, ProviderReports: []providercapacity.Report{{Backend: "code", Models: []string{"sol", "astra"}}, {Backend: "plan", Models: []string{"plan-model"}}}})
			if err != nil || result.Role != test.wantRole || result.Model != test.wantModel {
				t.Fatalf("requirement = %+v, %v", result, err)
			}
			if issue.ModelOverride != test.modelOverride || issue.Description != test.body {
				t.Fatal("capacity selection mutated issue policy")
			}
		})
	}
}

func TestDispatchCapacityMissingRoute(t *testing.T) {
	t.Parallel()
	r := &Runner{}
	if _, err := r.DispatchCapacity(t.Context(), RunRequest{}); !errors.Is(err, ErrMissingAgentRoutes) {
		t.Fatal(err)
	}
}

func TestDispatchCapacityDoesNotLaunchWorkspaceDependentBackend(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ name, body, unavailable string }{
		{"explicit model", "```detent-agent\nschema: 1\nmodel: gpt-5.6-sol\n```", "fallback"},
		{"automatic fail policy", "", "fail"},
		{"automatic fallback policy", "", "fallback"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			backend := &workspaceDependentCatalogBackend{turnErr: errors.New("test turn reached")}
			cfg := config.Default()
			cfg.Agents.ModelSelection.Preset = new("sol_first")
			cfg.Agents.ModelSelection.Unavailable = &test.unavailable
			r, err := NewRunner(Dependencies{Workflow: config.Workflow{Config: cfg}, Workspace: &fakeWorkspaceBackend{info: workspace.Info{Path: t.TempDir()}}, AgentBackend: backend})
			if err != nil {
				t.Fatal(err)
			}
			req := RunRequest{Issue: connector.Issue{ID: "issue-2561", Identifier: "detent#2561", Description: test.body}}
			result, err := r.DispatchCapacity(t.Context(), req)
			if err != nil || result.Model != "gpt-5.6-sol" {
				t.Fatalf("dispatch = %+v, %v", result, err)
			}
			if backend.calls != 0 {
				t.Fatalf("dispatch launched %d workspace-dependent catalog probes", backend.calls)
			}
			if _, err := r.Run(t.Context(), req); !errors.Is(err, backend.turnErr) {
				t.Fatalf("attempt did not reach the turn: %v", err)
			}
			if backend.calls != 1 || backend.process.Workspace == "" || backend.process.Environment.Variables["DETENT_ISSUE_ID"] != req.Issue.ID || backend.process.Environment.Variables["DETENT_ISSUE_IDENTIFIER"] != req.Issue.Identifier {
				t.Fatalf("attempt catalog context = %+v, calls=%d", backend.process, backend.calls)
			}
		})
	}
}

type workspaceDependentCatalogBackend struct {
	catalogAgentBackend
	calls   int
	process AgentProcessRequest
	turnErr error
}

func (b *workspaceDependentCatalogBackend) ListModels(_ context.Context, process AgentProcessRequest) ([]AgentModel, error) {
	b.calls++
	b.process = process
	if process.Workspace == "" || process.Environment.Variables["DETENT_WORKSPACE"] != process.Workspace {
		return nil, errors.New("wrapper requires attempt workspace")
	}
	return selectionCatalog(), nil
}

func TestDispatchCapacityMatchesAttemptModelScope(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name              string
		automatic         bool
		body, base, label string
	}{
		{name: "automatic fallback", automatic: true, label: "complexity:complex"},
		{name: "legacy override outside report", base: "gpt-5.6-sol", body: "```detent-agent\nschema: 1\nmodel: gpt-6-astra\n```"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			cfg := config.Default()
			cfg.Agents.ModelSelection.Enabled = new(test.automatic)
			if test.automatic {
				cfg.Agents.ModelSelection.Preset = new("sol_first")
			}
			backend := &catalogAgentBackend{models: selectionCatalog()}
			router, err := NewRouter([]Route{{BackendID: "codex", Default: true, Model: test.base}})
			if err != nil {
				t.Fatal(err)
			}
			backendConfig := config.AgentBackend{ID: "codex", Kind: config.AgentBackendCodex}
			r := &Runner{workflow: config.Workflow{Config: cfg}, agentRuntime: agentRuntime{router: router, backends: map[string]AgentBackend{"codex": backend}, backendConfigs: map[string]config.AgentBackend{"codex": backendConfig}}}
			report := providercapacity.Report{Backend: "codex", Models: []string{"gpt-5.6-sol"}}
			req := RunRequest{Issue: connector.Issue{Description: test.body, Labels: []string{test.label}}, ProviderReports: []providercapacity.Report{report}}
			capacity, err := r.DispatchCapacity(t.Context(), req)
			if err != nil || capacity.Model != "gpt-5.6-sol" {
				t.Fatalf("dispatch = %+v, %v", capacity, err)
			}
			if backend.calls != 0 {
				t.Fatal("dispatch launched catalog")
			}
			req.Execution = &capacityTestExecution{reservation: &providercapacity.Reservation{Requirement: capacity, Report: report}}
			selected := resolveRequestAgentSelection(t.Context(), req, AgentProcessRequest{Workspace: t.TempDir()}, test.base, RoleCode, cfg, backendConfig, backend)
			if selected.Err != nil || selected.Model != capacity.Model {
				t.Fatalf("attempt = %+v, reservation = %+v", selected, capacity)
			}
			if len(backend.models) != 2 {
				t.Fatal("selection changed shared backend catalog")
			}
		})
	}
}

type capacityTestExecution struct {
	Execution
	reservation *providercapacity.Reservation
}

func (e *capacityTestExecution) ProviderCapacity() *providercapacity.Reservation {
	return e.reservation
}

func (b *workspaceDependentCatalogBackend) RunTurn(context.Context, AgentTurnRequest, AgentUpdateHandler) (AgentTurnResult, error) {
	return AgentTurnResult{}, b.turnErr
}
