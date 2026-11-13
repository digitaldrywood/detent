package hubserver

import (
	"context"
	"encoding/json"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (e hubProjectExecutor) monthlyUsageCosts(ctx context.Context, arguments json.RawMessage) (operatortool.Result, error) {
	var request operatortool.MonthlyUsageArguments
	if operatortool.DecodeArguments(arguments, &request) != nil || len(request.ProjectID) > 256 {
		return operatortool.Result{}, operatortool.ErrInvalidArguments
	}
	if request.Scope == "" {
		request.Scope = "project"
	}
	if request.Scope != "project" && request.Scope != "organization" || request.Scope == "project" && request.ProjectID == "" || request.Scope == "organization" && request.ProjectID != "" {
		return operatortool.Result{}, operatortool.ErrInvalidArguments
	}
	window, err := costMonth(request.Month, e.service.config.Clock())
	if err != nil {
		return operatortool.Result{}, operatortool.ErrInvalidArguments
	}
	if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, ProjectID: request.ProjectID}); err != nil {
		return operatortool.Result{}, err
	}
	scope, err := e.projectScope(ctx)
	if err != nil {
		return operatortool.Result{}, err
	}
	projects := []string{request.ProjectID}
	if request.Scope == "organization" {
		if !hubscope.CanManageProjectSecrets(scope.Credential) {
			return operatortool.Result{}, operatortool.ErrAccessDenied
		}
		if scope.Credential.Hosted != nil {
			if err := e.service.requireHostedUsageGrants(ctx, scope.Credential); err != nil {
				return operatortool.Result{}, hubscope.OperatorAuthorizationError(err)
			}
		}
		projects = nil
	} else {
		scope.Project = tracker.ProjectID(request.ProjectID)
		if err := e.service.requireHostedProject(ctx, nil, scope, false); err != nil {
			return operatortool.Result{}, hubscope.OperatorAuthorizationError(err)
		}
		if err := e.service.database.authorizeNativeProject(ctx, scope); err != nil {
			return operatortool.Result{}, hubscope.OperatorAuthorizationError(err)
		}
	}
	report, err := e.service.database.monthlyCosts(ctx, string(scope.Organization), request.Scope, projects, window, e.service.config.Clock())
	if err != nil {
		return operatortool.Result{}, hubapi.ErrProjectServiceUnavailable
	}
	return billingResult(boundedMonthlyCostDetails(report))
}
