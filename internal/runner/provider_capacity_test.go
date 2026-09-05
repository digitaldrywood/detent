package runner

import (
	"errors"
	"testing"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/selector"
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
		{name: "catalog failure retains configured fallback", routeModel: "sol", body: "```detent-agent\nschema: 1\nmodel: astra\n```", catalogErr: errors.New("offline"), wantModel: "sol", wantRole: RoleCode},
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
			result, err := r.DispatchCapacity(t.Context(), RunRequest{Issue: issue, Mode: test.mode, SelectorContext: selector.Context{}})
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
