package web

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/web/templates"
)

func (s *Server) dashboardProjectTools(ctx context.Context) []operatortool.Definition {
	out := []operatortool.Definition{}
	for _, d := range operatortool.ProjectCatalog() {
		switch d.Name {
		case "demo_setup_scenarios":
			if s.demo == nil {
				continue
			}
		case "list_projects", "project_settings", "project_setup":
		default:
			continue
		}
		scope := apikey.ScopeRead
		if !d.Annotations.ReadOnly {
			scope = apikey.ScopeWrite
		}
		if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: scope}); err == nil {
			out = append(out, d)
		}
	}
	return out
}
func (s *Server) dashboardProjectRead(ctx context.Context, call operatortool.Call) (operatortool.Result, error) {
	var r operatortool.ProjectReadRequest
	if operatortool.DecodeProjectArguments(call.Arguments, &r) != nil {
		return operatortool.Result{}, operatortool.ErrInvalidArguments
	}
	if call.Name == "project_settings" && r.ProjectID == "" {
		return operatortool.Result{}, operatortool.ErrInvalidArguments
	}
	ctx, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, ProjectID: r.ProjectID})
	if err != nil {
		return operatortool.Result{}, err
	}
	if call.Name == "demo_setup_scenarios" {
		if s.demo == nil {
			return operatortool.Result{}, errOperatorCommandUnavailable
		}
		return operatorResult(s.demoSetupScenarios())
	}
	if call.Name == "project_setup" {
		// Local setup writes workflow files and uses interactive credential/provider
		// forms. Its useful tool result is the existing setup flow, never shell or
		// arbitrary file writes or credential-bearing workflow output.
		data := s.onboardingData(templates.OnboardingForm{}, nil, templates.OnboardingResult{})
		return operatorResult(struct {
			URL   string   `json:"setup_url"`
			Title string   `json:"title"`
			Steps []string `json:"required_steps"`
		}{"/onboarding", data.Title, []string{"Select tracker", "Configure credentials in the browser", "Bind project and repository", "Configure agent and write workflow"}})
	}
	if s.registry == nil {
		return operatortool.Result{}, errOperatorCommandUnavailable
	}
	projects := []dashboardProjectSettings{}
	for _, p := range s.registry.List() {
		id := string(p.ID())
		if r.ProjectID != "" && id != r.ProjectID || id <= r.After {
			continue
		}
		if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, ProjectID: id}); err != nil {
			continue
		}
		cfg := p.Config()
		wf := p.Workflow().Config
		projects = append(projects, dashboardProjectSettings{ID: id, URL: settingsTrackerProjectURL(ctx, p, wf), Tracker: trackerKind(wf), Weight: cfg.Weight, Priority: cfg.Priority, Paused: p.Paused(), BudgetEnabled: wf.Budget.Enabled, PerDayMaxUSD: wf.Budget.PerDayMaxUSD, PerIssueMaxUSD: wf.Budget.PerIssueMaxUSD, SetupURL: "/onboarding"})
	}
	// Project settings are read-only in this dashboard. Library/report filters
	// remain read parameters owned by the reporting/read children.
	slices.SortFunc(projects, func(a, b dashboardProjectSettings) int { return strings.Compare(a.ID, b.ID) })
	limit := r.Limit
	if limit == 0 {
		limit = 100
	}
	next := ""
	if len(projects) > limit {
		projects = projects[:limit]
		next = projects[len(projects)-1].ID
	}
	if r.ProjectID != "" && len(projects) == 0 {
		return operatortool.Result{}, errOperatorCommandUnavailable
	}
	return operatorResult(struct {
		ObservedAt time.Time                  `json:"observed_at"`
		Projects   []dashboardProjectSettings `json:"projects"`
		NextCursor string                     `json:"next_cursor,omitempty"`
	}{s.now().UTC(), projects, next})
}

type dashboardProjectSettings struct {
	ID             string  `json:"project_id"`
	URL            string  `json:"url,omitempty"`
	Tracker        string  `json:"tracker"`
	Weight         int     `json:"weight"`
	Priority       int     `json:"priority"`
	Paused         bool    `json:"paused"`
	BudgetEnabled  bool    `json:"budget_enabled"`
	PerDayMaxUSD   float64 `json:"per_day_max_usd"`
	PerIssueMaxUSD float64 `json:"per_issue_max_usd"`
	SetupURL       string  `json:"setup_url"`
}
