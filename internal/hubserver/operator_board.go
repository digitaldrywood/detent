package hubserver

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type nativeBoardCounts struct {
	Running         int `json:"running"`
	QueuedInventory int `json:"queued_inventory"`
	Open            int `json:"open"`
	ClosedInventory int `json:"closed_inventory"`
	Total           int `json:"total"`
}

type nativeBoardProject struct {
	Project tracker.NativeProject       `json:"project"`
	Counts  nativeBoardCounts           `json:"counts"`
	Board   operatortool.NativeWorkPage `json:"board"`
}

type nativeBoardResult struct {
	GeneratedAt     time.Time            `json:"generated_at"`
	Freshness       explain.SourceState  `json:"freshness"`
	Counts          nativeBoardCounts    `json:"counts"`
	Projects        []nativeBoardProject `json:"projects"`
	Truncated       bool                 `json:"truncated"`
	Unavailable     []string             `json:"unavailable"`
	EligibilityTool string               `json:"eligibility_tool"`
}

func (s *Service) executeBoardRead(ctx context.Context, call operatortool.Call) (operatortool.Result, error) {
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
		return operatortool.Result{}, err
	}
	resolve, ok := ctx.Value(nativeOperatorScopeKey{}).(func(context.Context) (nativeScope, error))
	if !ok {
		return operatortool.Result{}, operatortool.ErrAccessDenied
	}
	scope, err := resolve(ctx)
	if err != nil {
		return operatortool.Result{}, operatortool.ErrAccessDenied
	}
	projects := operatorProjectPage{Projects: []tracker.NativeProject{}}
	if request.ProjectID != "" {
		scope.project = tracker.ProjectID(request.ProjectID)
		project, err := readNativeProject(ctx, s.database.db, scope)
		if err != nil {
			return operatortool.Result{}, safeWorkReadError(err)
		}
		projects.Projects = append(projects.Projects, project)
	} else {
		projects, err = s.operatorProjectList(ctx, scope, operatortool.ProjectReadRequest{Limit: request.Limit})
		if err != nil {
			return operatortool.Result{}, safeWorkReadError(err)
		}
	}
	result := nativeBoardResult{GeneratedAt: s.config.now(), Freshness: explain.SourceAvailable, Projects: []nativeBoardProject{}, Truncated: projects.NextCursor != "", Unavailable: []string{"aggregate_dispatch_readiness"}, EligibilityTool: operatortool.ExplainItem}
	remaining := request.Limit
	for _, project := range projects.Projects {
		if remaining == 0 {
			result.Truncated = true
			break
		}
		ctx, err = operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, ProjectID: string(project.ID)})
		if err != nil {
			return operatortool.Result{}, err
		}
		resolve = ctx.Value(nativeOperatorScopeKey{}).(func(context.Context) (nativeScope, error))
		scope, err = resolve(ctx)
		if err != nil {
			return operatortool.Result{}, operatortool.ErrAccessDenied
		}
		scope.project = project.ID
		params := (operatortool.WorkReadRequest{Limit: remaining, State: request.State, Include: []string{"work"}}).NativeWorkQuery()
		page, err := s.readIssues(ctx, scope, params)
		if err != nil {
			return operatortool.Result{}, safeWorkReadError(err)
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

func nativeBoardInventory(project tracker.NativeProject, lanes []tracker.NativeWorkLane) nativeBoardCounts {
	counts := nativeBoardCounts{}
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
