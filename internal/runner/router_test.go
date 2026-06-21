package runner

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/selector"
)

func TestRouterRoutesByLabelModelFieldPriorityAndDefault(t *testing.T) {
	t.Parallel()

	router, err := NewRouter([]Route{
		{
			Name:      "label",
			BackendID: "codex-high",
			Model:     "gpt-5-codex-high",
			Selector: selector.Selector{
				Labels: selector.Labels{Include: []string{"tier:high"}},
			},
		},
		{
			Name:       "model-field",
			BackendID:  "codex-standard",
			ModelField: "Model",
		},
		{
			Name:      "priority",
			BackendID: "codex-high",
			Model:     "gpt-5-codex-priority",
			Selector: selector.Selector{
				PriorityIn: []int{1},
			},
		},
		{
			Name:      "default",
			BackendID: "codex-standard",
			Model:     "gpt-5-codex-mini",
			Default:   true,
		},
	})
	if err != nil {
		t.Fatalf("NewRouter() error = %v", err)
	}

	priorityOne := 1
	tests := []struct {
		name        string
		issue       connector.Issue
		wantBackend string
		wantModel   string
		wantRoute   string
	}{
		{
			name: "label route",
			issue: connector.Issue{
				Labels: []string{"tier:high"},
				Fields: map[string]string{},
			},
			wantBackend: "codex-high",
			wantModel:   "gpt-5-codex-high",
			wantRoute:   "label",
		},
		{
			name: "model project field route",
			issue: connector.Issue{
				Labels: []string{},
				Fields: map[string]string{"Model": "gpt-5-codex"},
			},
			wantBackend: "codex-standard",
			wantModel:   "gpt-5-codex",
			wantRoute:   "model-field",
		},
		{
			name: "priority route",
			issue: connector.Issue{
				Priority: &priorityOne,
				Labels:   []string{},
				Fields:   map[string]string{},
			},
			wantBackend: "codex-high",
			wantModel:   "gpt-5-codex-priority",
			wantRoute:   "priority",
		},
		{
			name: "default route",
			issue: connector.Issue{
				Labels: []string{},
				Fields: map[string]string{},
			},
			wantBackend: "codex-standard",
			wantModel:   "gpt-5-codex-mini",
			wantRoute:   "default",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := router.Route(tt.issue, selector.Context{})
			if err != nil {
				t.Fatalf("Route() error = %v", err)
			}
			if got.BackendID != tt.wantBackend || got.Model != tt.wantModel || got.RouteName != tt.wantRoute {
				t.Fatalf("Route() = %#v, want backend %q model %q route %q", got, tt.wantBackend, tt.wantModel, tt.wantRoute)
			}
		})
	}
}

func TestRouterSingleDefaultRouteKeepsIssueModelOverride(t *testing.T) {
	t.Parallel()

	router, err := NewRouter([]Route{{
		Name:      "default",
		BackendID: "codex",
		Default:   true,
	}})
	if err != nil {
		t.Fatalf("NewRouter() error = %v", err)
	}

	got, err := router.Route(connector.Issue{ModelOverride: "gpt-5-codex-high"}, selector.Context{})
	if err != nil {
		t.Fatalf("Route() error = %v", err)
	}
	if got.BackendID != "codex" {
		t.Fatalf("BackendID = %q, want codex", got.BackendID)
	}
	if got.Model != "gpt-5-codex-high" {
		t.Fatalf("Model = %q, want issue override", got.Model)
	}
}

func TestRouterRoutesValidatorRoleWithFallbackDefault(t *testing.T) {
	t.Parallel()

	router, err := NewRouter([]Route{
		{
			Name:      "validator",
			Role:      RoleValidator,
			BackendID: "codex-review",
			Model:     "gpt-5-review",
		},
		{
			Name:      "default",
			BackendID: "codex-code",
			Model:     "gpt-5-code",
			Default:   true,
		},
	})
	if err != nil {
		t.Fatalf("NewRouter() error = %v", err)
	}

	got, err := router.RouteForRole(connector.Issue{}, selector.Context{}, RoleValidator)
	if err != nil {
		t.Fatalf("RouteForRole(validator) error = %v", err)
	}
	if got.BackendID != "codex-review" || got.Model != "gpt-5-review" || got.RouteName != "validator" {
		t.Fatalf("RouteForRole(validator) = %#v, want validator route", got)
	}

	code, err := router.Route(connector.Issue{}, selector.Context{})
	if err != nil {
		t.Fatalf("Route(code) error = %v", err)
	}
	if code.BackendID != "codex-code" || code.Model != "gpt-5-code" || code.RouteName != "default" {
		t.Fatalf("Route(code) = %#v, want code default", code)
	}

	fallback, err := NewRouter([]Route{{
		Name:      "default",
		BackendID: "codex",
		Model:     "gpt-5-code",
		Default:   true,
	}})
	if err != nil {
		t.Fatalf("NewRouter(fallback) error = %v", err)
	}
	got, err = fallback.RouteForRole(connector.Issue{}, selector.Context{}, RoleValidator)
	if err != nil {
		t.Fatalf("fallback RouteForRole(validator) error = %v", err)
	}
	if got.BackendID != "codex" || got.Model != "gpt-5-code" || got.RouteName != "default" {
		t.Fatalf("fallback RouteForRole(validator) = %#v, want default route", got)
	}
}

func TestRouterRejectsInvalidRoutes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		routes []Route
	}{
		{
			name:   "empty",
			routes: nil,
		},
		{
			name: "blank backend",
			routes: []Route{{
				Default: true,
			}},
		},
		{
			name: "multiple code defaults",
			routes: []Route{
				{BackendID: "codex", Default: true},
				{BackendID: "codex-high", Default: true},
			},
		},
		{
			name: "multiple validator defaults",
			routes: []Route{
				{Role: RoleValidator, BackendID: "codex", Default: true},
				{Role: RoleValidator, BackendID: "codex-high", Default: true},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, err := NewRouter(tt.routes); err == nil {
				t.Fatal("NewRouter() error = nil, want error")
			}
		})
	}
}
