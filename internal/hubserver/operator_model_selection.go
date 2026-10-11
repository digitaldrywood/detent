package hubserver

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (e hubProjectExecutor) modelSelectionScope(ctx context.Context, name, project string, write bool) (nativeScope, error) {
	permission := apikey.ScopeRead
	if write {
		permission = apikey.ScopeWrite
	}
	ctx, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: permission, ProjectID: project})
	if err != nil {
		return nativeScope{}, err
	}
	scope, err := e.projectScope(ctx)
	if err != nil {
		return scope, err
	}
	scope.project = tracker.ProjectID(project)
	scope.requireHostedAdmin = write
	if operatortool.IsOrganizationModelSelection(name) {
		if project != "" || e.service.config.Hosted == nil || scope.credential.Hosted == nil || scope.credential.SessionHash == "" || scope.credential.HostedKeyScope != "" || scope.credential.HostedRole == "account" || string(scope.organization) != e.service.config.Hosted.OrganizationID {
			return scope, operatortool.ErrAccessDenied
		}
	} else if project == "" {
		return scope, operatortool.ErrInvalidArguments
	}
	if write {
		if scope.credential.Scope != apiScopeOperator && scope.credential.Scope != apiScopeAdmin {
			return scope, operatortool.ErrAccessDenied
		}
		tx, err := e.service.database.db.BeginTx(ctx, nil)
		if err != nil {
			return scope, err
		}
		defer tx.Rollback()
		if e.service.recheckHostedMutation(ctx, tx, scope) != nil {
			return scope, operatortool.ErrAccessDenied
		}
	}
	return scope, nil
}

func (e hubProjectExecutor) readModelSelection(ctx context.Context, call operatortool.Call) (operatortool.Result, error) {
	var request operatortool.ModelSelectionReadRequest
	if operatortool.DecodeModelSelectionArguments(call, &request) != nil {
		return operatortool.Result{}, operatortool.ErrInvalidArguments
	}
	scope, err := e.modelSelectionScope(ctx, call.Name, request.ProjectID, false)
	if err != nil {
		return operatortool.Result{}, err
	}
	selection, err := readCloudModelSelection(ctx, e.service.database.db, scope)
	if err != nil {
		return operatortool.Result{}, projectToolError(err)
	}
	return hubProjectResult(struct {
		OrganizationID tracker.OrganizationID `json:"organization_id"`
		ProjectID      tracker.ProjectID      `json:"project_id,omitempty"`
		ObservedAt     time.Time              `json:"observed_at"`
		Data           cloudModelSelection    `json:"data"`
	}{scope.organization, scope.project, e.service.config.now().UTC(), selection})
}

func (e hubProjectExecutor) modelSelectionCommand(ctx context.Context, call operatortool.Call, mode projectCommandMode) (json.RawMessage, error) {
	var input operatortool.ProjectRequest[operatortool.ModelSelectionInput]
	if operatortool.DecodeModelSelectionArguments(call, &input) != nil {
		return nil, operatortool.ErrInvalidArguments
	}
	scope, err := e.modelSelectionScope(ctx, call.Name, input.ProjectID, true)
	if err != nil {
		return nil, err
	}
	if scope.project == "" && input.Input.Selection == nil {
		return nil, operatortool.ErrInvalidArguments
	}
	if mode == projectCommandPreview {
		return nil, nil
	}
	request := cloudModelSelectionRequest{Mutation: tracker.Mutation{IdempotencyKey: input.RequestID}, ExpectedRevision: input.Input.ExpectedRevision, Selection: input.Input.Selection}
	operation := projectOperationID(scope, http.MethodPut, "/model-selection")
	if scope.project == "" {
		operation = http.MethodPut + " /api/v2/organizations/" + string(scope.organization) + "/model-selection"
	}
	if mode == projectCommandReplay {
		raw, _, err := e.service.nativeCommandReplay(ctx, scope, operation, input.RequestID, request)
		return raw, err
	}
	return e.service.executeNativeMutation(ctx, scope, nativeCommandOptions{OperationID: operation, Feature: "collaboration"}, request.Mutation, request, updateCloudModelSelectionOperation(request))
}

func (e hubProjectExecutor) modelSelectionCatalogAllowed(ctx context.Context, definition operatortool.Definition) bool {
	catalog, err := operatorCatalog(ctx)
	if err != nil {
		return false
	}
	credential := catalog.credential
	if operatortool.IsOrganizationModelSelection(definition.Name) && (e.service.config.Hosted == nil || credential.Hosted == nil || credential.SessionHash == "" || credential.HostedKeyScope != "" || credential.HostedRole == "account") {
		return false
	}
	if !definition.Annotations.ReadOnly && (credential.Scope != apiScopeOperator && credential.Scope != apiScopeAdmin || credential.Hosted != nil && (credential.HostedRole != "owner" && credential.HostedRole != "admin" || credential.HostedKeyScope != "" && !hostedKeyAllows(credential.HostedKeyScope, apikey.ScopeAdmin))) {
		return false
	}
	return true
}
