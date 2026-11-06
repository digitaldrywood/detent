package hubserver

import (
	"context"
	"encoding/json"

	"github.com/digitaldrywood/detent/internal/apikey"
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
	window, err := costMonth(request.Month, e.service.config.now())
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
		if !canManageProjectSecrets(scope.credential) {
			return operatortool.Result{}, operatortool.ErrAccessDenied
		}
		if scope.credential.Hosted != nil {
			if err := e.service.requireHostedUsageGrants(ctx, scope.credential); err != nil {
				return operatortool.Result{}, operatorAuthorizationError(err)
			}
		}
		projects = nil
	} else {
		scope.project = tracker.ProjectID(request.ProjectID)
		if err := e.service.requireHostedProject(ctx, nil, scope, false); err != nil {
			return operatortool.Result{}, operatorAuthorizationError(err)
		}
		if err := e.service.database.authorizeNativeProject(ctx, scope); err != nil {
			return operatortool.Result{}, operatorAuthorizationError(err)
		}
	}
	report, err := e.service.database.monthlyCosts(ctx, string(scope.organization), request.Scope, projects, window, e.service.config.now())
	if err != nil {
		return operatortool.Result{}, errProjectServiceUnavailable
	}
	return billingResult(boundedMonthlyCostDetails(report))
}
