package runner

import (
	"errors"
	"fmt"
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/selector"
)

var (
	ErrMissingAgentRoutes = errors.New("agent routes are required")
	ErrNoMatchingRoute    = errors.New("no matching agent route")
)

type Route struct {
	Name       string
	BackendID  string
	Model      string
	ModelField string
	Default    bool
	Selector   selector.Selector
}

type RouteSelection struct {
	BackendID string
	Model     string
	RouteName string
}

type Router struct {
	routes       []Route
	defaultIndex int
}

func NewRouter(routes []Route) (*Router, error) {
	if len(routes) == 0 {
		return nil, ErrMissingAgentRoutes
	}

	normalized := make([]Route, len(routes))
	defaultIndex := -1
	for index, route := range routes {
		route.Name = strings.TrimSpace(route.Name)
		route.BackendID = strings.TrimSpace(route.BackendID)
		route.Model = strings.TrimSpace(route.Model)
		route.ModelField = strings.TrimSpace(route.ModelField)
		if route.BackendID == "" {
			return nil, fmt.Errorf("agent route %d backend is required", index)
		}
		if route.Default {
			if defaultIndex >= 0 {
				return nil, errors.New("agent routes must not define multiple defaults")
			}
			defaultIndex = index
		}
		normalized[index] = route
	}

	return &Router{
		routes:       normalized,
		defaultIndex: defaultIndex,
	}, nil
}

func (r *Router) Route(issue connector.Issue, ctx selector.Context) (RouteSelection, error) {
	if r == nil || len(r.routes) == 0 {
		return RouteSelection{}, ErrMissingAgentRoutes
	}

	for _, route := range r.routes {
		if route.Default {
			continue
		}
		if route.matches(issue, ctx) {
			return route.selection(issue), nil
		}
	}

	if r.defaultIndex >= 0 {
		return r.routes[r.defaultIndex].selection(issue), nil
	}

	return RouteSelection{}, ErrNoMatchingRoute
}

func (r Route) matches(issue connector.Issue, ctx selector.Context) bool {
	if r.ModelField != "" {
		value, ok := issueFieldValue(issue.Fields, r.ModelField)
		if !ok || strings.TrimSpace(value) == "" {
			return false
		}
	}
	return selector.Match(issue, r.Selector, ctx)
}

func (r Route) selection(issue connector.Issue) RouteSelection {
	model := strings.TrimSpace(r.Model)
	if model == "" && r.ModelField != "" {
		model, _ = issueFieldValue(issue.Fields, r.ModelField)
		model = strings.TrimSpace(model)
	}
	if model == "" {
		model = strings.TrimSpace(issue.ModelOverride)
	}
	return RouteSelection{
		BackendID: r.BackendID,
		Model:     model,
		RouteName: r.Name,
	}
}

func issueFieldValue(fields map[string]string, name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", false
	}
	if value, ok := fields[name]; ok {
		return value, true
	}
	for fieldName, value := range fields {
		if strings.TrimSpace(fieldName) == name {
			return value, true
		}
	}
	return "", false
}
