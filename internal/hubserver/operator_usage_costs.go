package hubserver

import (
	"context"
	"encoding/json"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (e hubProjectExecutor) monthlyUsageCosts(ctx context.Context, arguments json.RawMessage) (operatortool.Result, error) {
	var request struct {
		ProjectID string `json:"project_id,omitempty"`
		Month     string `json:"month,omitempty"`
		Scope     string `json:"scope,omitempty"`
	}
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
		if !canManageProjectSecrets(scope.credential) || scope.credential.Hosted != nil && !e.service.hostedAllUsageGrants(ctx, scope.credential) {
			return operatortool.Result{}, operatortool.ErrAccessDenied
		}
		projects = nil
	} else {
		scope.project = tracker.ProjectID(request.ProjectID)
		if err := e.service.requireHostedProject(ctx, e.service.database.db, scope, false); err != nil {
			return operatortool.Result{}, operatortool.ErrAccessDenied
		}
		if err := e.service.database.authorizeNativeProject(ctx, scope); err != nil {
			return operatortool.Result{}, operatortool.ErrAccessDenied
		}
	}
	report, err := e.service.database.monthlyCosts(ctx, string(scope.organization), request.Scope, projects, window, e.service.config.now())
	if err != nil {
		return operatortool.Result{}, errProjectServiceUnavailable
	}
	return billingResult(boundedMonthlyCostDetails(report))
}
