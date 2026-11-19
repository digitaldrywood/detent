package operatorcommands

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type NativeBoardCounts struct {
	Running         int `json:"running"`
	QueuedInventory int `json:"queued_inventory"`
	Open            int `json:"open"`
	ClosedInventory int `json:"closed_inventory"`
	Total           int `json:"total"`
}

type nativeBoardProject struct {
	Project tracker.NativeProject       `json:"project"`
	Counts  NativeBoardCounts           `json:"counts"`
	Board   operatortool.NativeWorkPage `json:"board"`
}

type NativeBoardResult struct {
	GeneratedAt     time.Time            `json:"generated_at"`
	Freshness       explain.SourceState  `json:"freshness"`
	Counts          NativeBoardCounts    `json:"counts"`
	Projects        []nativeBoardProject `json:"projects"`
	Truncated       bool                 `json:"truncated"`
	Unavailable     []string             `json:"unavailable"`
	EligibilityTool string               `json:"eligibility_tool"`
}

type BoardDeps struct {
	Now           func() time.Time
	ReadProject   func(context.Context, hubscope.NativeScope) (projectstore.Record, error)
	NativeProject func(context.Context, projectstore.Record) (tracker.NativeProject, error)
	ListProjects  func(context.Context, hubscope.NativeScope, int) ([]tracker.NativeProject, bool, error)
	ReadIssues    func(context.Context, hubscope.NativeScope, url.Values) (tracker.NativeIssuePage, error)
}

func (s BoardDeps) Read(ctx context.Context, call operatortool.Call) (operatortool.Result, error) {
	var request struct {
		ProjectID string `json:"project_id"`
		State     string `json:"state"`
		Limit     int    `json:"limit"`
	}
	if err := operatortool.DecodeArguments(call.Arguments, &request); err != nil {
		return operatortool.Result{}, err
	}
	if len(request.ProjectID) > 256 || len(request.State) > 256 || request.Limit < 0 || request.Limit > operatortool.MaxItemLimit || call.Name == operatortool.Dashboard && request.State != "" {
		return operatortool.Result{}, operatortool.ErrInvalidArguments
	}
	if request.Limit == 0 {
		request.Limit = operatortool.DefaultItemLimit
	}
	ctx, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, ProjectID: request.ProjectID})
	if err != nil {
		return operatortool.Result{}, fmt.Errorf("authorize board: %w", err)
	}
	resolve, ok := ctx.Value(hubscope.NativeOperatorScopeKey{}).(func(context.Context) (hubscope.NativeScope, error))
	if !ok {
		return operatortool.Result{}, operatortool.ErrAccessDenied
	}
	scope, err := resolve(ctx)
	if err != nil {
		return operatortool.Result{}, operatortool.ErrAccessDenied
	}
	projects := []tracker.NativeProject{}
	var moreProjects bool
	if request.ProjectID != "" {
		scope.Project = tracker.ProjectID(request.ProjectID)
		record, err := s.ReadProject(ctx, scope)
		if err != nil {
			return operatortool.Result{}, hubapi.SafeWorkReadError(fmt.Errorf("read project record: %w", err))
		}
		project, err := s.NativeProject(ctx, record)
		if err != nil {
			return operatortool.Result{}, hubapi.SafeWorkReadError(fmt.Errorf("load project: %w", err))
		}
		projects = append(projects, project)
	} else {
		projects, moreProjects, err = s.ListProjects(ctx, scope, request.Limit)
		if err != nil {
			return operatortool.Result{}, hubapi.SafeWorkReadError(fmt.Errorf("list projects: %w", err))
		}
	}
	result := NativeBoardResult{GeneratedAt: s.Now(), Freshness: explain.SourceAvailable, Projects: []nativeBoardProject{}, Truncated: moreProjects, Unavailable: []string{"aggregate_dispatch_readiness"}, EligibilityTool: operatortool.ExplainItem}
	remaining := request.Limit
	for _, project := range projects {
		if project.Profile != "native" {
			continue
		}
		if remaining == 0 {
			result.Truncated = true
			break
		}
		projectCtx, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, ProjectID: string(project.ID)})
		if err != nil {
			return operatortool.Result{}, err
		}
		projectResolve, ok := projectCtx.Value(hubscope.NativeOperatorScopeKey{}).(func(context.Context) (hubscope.NativeScope, error))
		if !ok {
			return operatortool.Result{}, operatortool.ErrAccessDenied
		}
		projectScope, err := projectResolve(projectCtx)
		if err != nil {
			return operatortool.Result{}, operatortool.ErrAccessDenied
		}
		projectScope.Project = project.ID
		params := (operatortool.WorkReadRequest{Limit: remaining, State: request.State, Include: []string{"work"}}).NativeWorkQuery()
		page, err := s.ReadIssues(projectCtx, projectScope, params)
		if err != nil {
			return operatortool.Result{}, hubapi.SafeWorkReadError(fmt.Errorf("read board snapshot: %w", err))
		}
		counts := nativeBoardInventory(project, page.Work.Lanes)
		result.Projects = append(result.Projects, nativeBoardProject{Project: project, Counts: counts, Board: operatortool.NativeWorkPageView(string(project.ID), page)})
		result.Counts.Running += counts.Running
		result.Counts.QueuedInventory += counts.QueuedInventory
		result.Counts.Open += counts.Open
		result.Counts.ClosedInventory += counts.ClosedInventory
		result.Counts.Total += counts.Total
		result.Truncated = result.Truncated || page.NextCursor != "" || page.Work.Truncated
		remaining -= max(len(page.Items), len(page.Work.Items))
	}
	return operatortool.EncodeResult(result)
}

func nativeBoardInventory(project tracker.NativeProject, lanes []tracker.NativeWorkLane) NativeBoardCounts {
	counts := NativeBoardCounts{}
	states := make(map[string]tracker.NativeState, len(project.States))
	for _, state := range project.States {
		states[state.Name] = state
	}
	for _, lane := range lanes {
		counts.Total += lane.Total
		if state := states[lane.State]; state.Terminal {
			counts.ClosedInventory += lane.Total
		} else {
			counts.Open += lane.Total
			counts.Running += lane.Running
			if state.Dispatchable {
				counts.QueuedInventory += max(0, lane.Total-lane.Running)
			}
		}
	}
	return counts
}
