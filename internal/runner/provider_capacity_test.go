package runner

import (
	"errors"
	"slices"
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

// TestProviderModelDetails covers the projection of a backend's own model
// catalogue onto the per-model detail a provider capacity report carries
// (decisions section 14): only reported models are described, the canonical
// identifier is the one dispatch uses, a model with a named successor is
// legacy, and the runner's configured effort becomes the model's default only
// when the model supports it.
func TestProviderModelDetails(t *testing.T) {
	t.Parallel()
	catalog := []AgentModel{
		{ID: "astra-id", Model: "astra", Default: true, SupportedReasoningEfforts: []string{"Low", " medium ", "high", "medium"}},
		{ID: "sol", SupportedReasoningEfforts: []string{"high", "xhigh"}},
		{ID: "retired", Model: "retired", Upgrade: "astra", SupportedReasoningEfforts: []string{"low"}},
		{ID: "unreported", Model: "unreported"},
		{ID: "", Model: "  "},
		{ID: "astra-id", Model: "astra"},
	}
	details := ProviderModelDetails("openai", []string{"astra", "sol", "retired"}, " MEDIUM ", catalog)
	if len(details) != 3 {
		t.Fatalf("details = %#v, want one per reported model", details)
	}
	for _, test := range []struct {
		name, id, defaultEffort string
		index                   int
		efforts                 []string
		isDefault, legacy       bool
	}{
		{
			name: "catalogue model name wins over its id", index: 0, id: "astra",
			efforts: []string{"low", "medium", "high"}, defaultEffort: "medium", isDefault: true,
		},
		{name: "id stands in when there is no model name", index: 1, id: "sol", efforts: []string{"high", "xhigh"}},
		{name: "a named successor makes a model legacy", index: 2, id: "retired", efforts: []string{"low"}, legacy: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			detail := details[test.index]
			if detail.ID != test.id || detail.Label != test.id || detail.Provider != "openai" {
				t.Fatalf("detail = %#v, want %q from openai", detail, test.id)
			}
			if detail.Default != test.isDefault || detail.Legacy != test.legacy {
				t.Fatalf("detail = %#v, want default %t and legacy %t", detail, test.isDefault, test.legacy)
			}
			if detail.DefaultReasoningEffort != test.defaultEffort || !slices.Equal(detail.ReasoningEfforts, test.efforts) {
				t.Fatalf("detail = %#v, want efforts %v defaulting to %q", detail, test.efforts, test.defaultEffort)
			}
		})
	}
}

// TestProviderModelDetailsWithoutACatalog proves a backend that answers with
// no models leaves the report exactly as the collector wrote it.
func TestProviderModelDetailsWithoutACatalog(t *testing.T) {
	t.Parallel()
	if details := ProviderModelDetails("openai", []string{"sol"}, "high", nil); len(details) != 0 {
		t.Fatalf("details = %#v, want none", details)
	}
}
