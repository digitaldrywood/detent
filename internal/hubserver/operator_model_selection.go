package hubserver

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (e hubProjectExecutor) modelSelectionScope(ctx context.Context, name, project string, write bool) (hubscope.NativeScope, error) {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	permission := apikey.ScopeRead
	if write {
		permission = apikey.ScopeWrite
	}
	ctx, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: permission, ProjectID: project, OrganizationWide: operatortool.IsOrganizationModelSelection(name)})
	if err != nil {
		return hubscope.NativeScope{}, err
	}
	scope, err := e.projectScope(ctx)
	if err != nil {
		return scope, err
	}
	scope.Project = tracker.ProjectID(project)
	scope.RequireHostedAdmin = write
	if operatortool.IsOrganizationModelSelection(name) {
		if project != "" || e.service.config.Hosted != nil && (scope.Credential.Hosted == nil || scope.Credential.SessionHash == "" || scope.Credential.HostedKeyScope != "" || scope.Credential.HostedRole == "account" || string(scope.Organization) != e.service.config.Hosted.OrganizationID) {
			return scope, operatortool.ErrAccessDenied
		}
	} else if project == "" {
		return scope, operatortool.ErrInvalidArguments
	}
	if e.service.config.TenantRepository == nil {
		return scope, hubapi.NativeWorkItemStorageUnavailable()
	}
	if e.service.config.AccessRepository == nil {
		return scope, operatortool.ErrAccessDenied
	}
	if write {
		return scope, e.service.recheckProjectRecordMutation(ctx, scope)
	}
	return scope, e.service.accessRead(ctx, scope.Organization, func(r tenantstore.Reader) error { return e.service.authorizeAccessScope(ctx, r, scope, false) })
}

func (e hubProjectExecutor) readModelSelection(ctx context.Context, call operatortool.Call) (operatortool.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	var request operatortool.ModelSelectionReadRequest
	if operatortool.DecodeModelSelectionArguments(call, &request) != nil {
		return operatortool.Result{}, operatortool.ErrInvalidArguments
	}
	scope, err := e.modelSelectionScope(ctx, call.Name, request.ProjectID, false)
	if err != nil {
		return operatortool.Result{}, err
	}
	var selection cloudModelSelection
	if scope.Project == "" {
		selection, err = e.service.readOrganizationModels(ctx, scope.Organization)
	} else {
		record, readErr := e.service.readProjectRecord(ctx, scope)
		if readErr != nil {
			return operatortool.Result{}, projectToolError(readErr)
		}
		selection, err = e.service.projectRecordModel(ctx, record)
	}
	if err != nil {
		return operatortool.Result{}, projectToolError(err)
	}
	return hubProjectResult(struct {
		OrganizationID tracker.OrganizationID `json:"organization_id"`
		ProjectID      tracker.ProjectID      `json:"project_id,omitempty"`
		ObservedAt     time.Time              `json:"observed_at"`
		Data           cloudModelSelection    `json:"data"`
	}{scope.Organization, scope.Project, e.service.config.now().UTC(), selection})
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
	if scope.Project == "" && input.Input.Selection == nil {
		return nil, operatortool.ErrInvalidArguments
	}
	if mode == projectCommandPreview {
		return nil, nil
	}
	request := cloudModelSelectionRequest{Mutation: tracker.Mutation{IdempotencyKey: input.RequestID}, ExpectedRevision: input.Input.ExpectedRevision, Selection: input.Input.Selection}
	operation := projectOperationID(scope, http.MethodPut, "/model-selection")
	if scope.Project == "" {
		operation = http.MethodPut + " /api/v2/organizations/" + string(scope.Organization) + "/model-selection"
	}
	if mode == projectCommandReplay {
		hash, err := hubapi.CommandHash(request)
		if err != nil {
			return nil, err
		}
		raw, _, err := e.service.changeRecordReceipt(ctx, scope, operation, input.RequestID, hash)
		return raw, err
	}
	return e.service.executeNativeMutation(ctx, scope, hubapi.NativeCommandOptions{OperationID: operation, Feature: "collaboration"}, request.Mutation, request, nil)
}

func (e hubProjectExecutor) modelSelectionCatalogAllowed(ctx context.Context, definition operatortool.Definition) bool {
	catalog, err := operatorCatalog(ctx)
	if err != nil {
		return false
	}
	credential := catalog.credential
	if operatortool.IsOrganizationModelSelection(definition.Name) {
		if e.service.config.Hosted == nil {
			if credential.NativeOnly {
				return false
			}
		} else if credential.Hosted == nil || credential.SessionHash == "" || credential.HostedKeyScope != "" || credential.HostedRole == "account" {
			return false
		}
	}
	if !definition.Annotations.ReadOnly && (credential.Scope != hubapi.APIScopeOperator && credential.Scope != hubapi.APIScopeAdmin || credential.Hosted != nil && (credential.HostedRole != "owner" && credential.HostedRole != "admin" || credential.HostedKeyScope != "" && !hubscope.HostedKeyAllows(credential.HostedKeyScope, apikey.ScopeAdmin))) {
		return false
	}
	return true
}
