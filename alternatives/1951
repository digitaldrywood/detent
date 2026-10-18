package hubserver

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (e nativeOperatorExecutor) stopRunAuthority(ctx context.Context, raw json.RawMessage) (context.Context, nativeScope, operatortool.NativeStopRunArguments, error) {
	request, err := operatortool.DecodeNativeStopRun(raw)
	if err != nil {
		return ctx, nativeScope{}, request, err
	}
	ctx, err = operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeWrite, Action: operatortool.StopRun, ProjectID: request.ProjectID, ResourceKind: "work_item", ResourceID: request.Identifier})
	if err != nil {
		return ctx, nativeScope{}, request, err
	}
	resolve, ok := ctx.Value(nativeOperatorScopeKey{}).(func(context.Context) (nativeScope, error))
	if !ok {
		return ctx, nativeScope{}, request, operatortool.ErrAccessDenied
	}
	scope, err := resolve(ctx)
	if err != nil {
		return ctx, nativeScope{}, request, operatortool.ErrAccessDenied
	}
	scope.project = tracker.ProjectID(request.ProjectID)
	scope.action, scope.actionItem, scope.actionTarget = operatortool.StopRun, request.Identifier, request.Destination
	issue, err := e.service.resolveOperatorNativeItem(ctx, e.service.database.reader, scope, request.Identifier)
	if err != nil {
		return ctx, nativeScope{}, request, hubSafeChangeError(err)
	}
	if issue.Profile != "native" {
		return ctx, nativeScope{}, request, operatortool.ErrServiceUnavailable
	}
	request.Identifier = string(issue.WorkItemID)
	return ctx, scope, request, nil
}

func (e nativeOperatorExecutor) stopRun(ctx context.Context, call operatortool.Call) (operatortool.Result, error) {
	ctx, _, request, err := e.stopRunAuthority(ctx, call.Arguments)
	if err != nil {
		return operatortool.Result{}, err
	}
	var arguments json.RawMessage
	arguments, err = json.Marshal(request)
	if err != nil {
		return operatortool.Result{}, operatortool.ErrInvalidArguments
	}
	identity := operatortool.ConnectionIdentity(ctx)
	m := mutation.Metadata{PrincipalID: identity.PrincipalID, OrganizationID: identity.OrganizationID, ProjectID: request.ProjectID, ResourceID: request.Identifier, Action: call.Name, Source: "mcp", Confirmation: "none", CorrelationID: newNativeID("mcp")}
	m, err = m.Bind(request.RequestID, arguments)
	if err != nil {
		return operatortool.Result{}, operatortool.ErrInvalidArguments
	}
	outcome := "failed"
	defer func() { e.service.hubChangeAudit(ctx, m, outcome) }()
	action, err := e.service.operatorChat.Submit(ctx, chat.Action{Kind: chat.ActionStopRun, ProjectID: request.ProjectID, IssueID: request.Identifier, Identifier: request.Identifier, RequestID: request.RequestID, Arguments: arguments, Mutation: m})
	if err != nil {
		var conflict *operatortool.ConflictError
		if errors.As(err, &conflict) {
			return operatortool.Result{}, conflict
		}
		return operatortool.Result{}, hubSafeChangeError(err)
	}
	outcome = string(action.Status)
	return e.service.hubActionResult(action)
}

func (e nativeOperatorExecutor) executeStopRun(ctx context.Context, action chat.Action) (chat.ActionExecution, error) {
	ctx, scope, request, err := e.stopRunAuthority(ctx, action.Arguments)
	if err != nil {
		return chat.ActionExecution{}, err
	}
	identity := operatortool.ConnectionIdentity(ctx)
	m := action.Mutation
	bound, err := m.Bind(action.RequestID, action.Arguments)
	if err != nil || request.RequestID != action.RequestID || request.ProjectID != action.ProjectID || request.Identifier != action.IssueID || m.Source != "mcp" || m.PrincipalID != identity.PrincipalID || m.OrganizationID != identity.OrganizationID || m.ProjectID != request.ProjectID || m.ResourceID != request.Identifier || m.Action != operatortool.StopRun || m.CorrelationID == "" || bound.RetryIdentity != m.RetryIdentity || bound.InputHash != m.InputHash {
		return chat.ActionExecution{}, operatortool.ErrAccessDenied
	}
	raw, err := e.service.stopNativeRunCommand(mutation.WithContext(ctx, m), scope, request.RequestID, request)
	if err != nil {
		var failure *nativeError
		if errors.As(err, &failure) && failure.Code == "revision_conflict" {
			return chat.ActionExecution{}, &operatortool.ConflictError{Code: failure.Code, Message: failure.Message, CurrentRevision: int64(failure.CurrentRevision)}
		}
		if errors.Is(err, tracker.ErrLeaseConflict) || errors.Is(err, tracker.ErrStaleFencingToken) || errors.Is(err, tracker.ErrLeaseNotFound) {
			return chat.ActionExecution{}, &operatortool.ConflictError{Code: "lease_conflict", Message: "The exact runner attempt is no longer current"}
		}
		return chat.ActionExecution{}, hubSafeChangeError(err)
	}
	return chat.ActionExecution{Message: string(raw), ResourceID: request.Identifier, Identifier: request.Identifier}, nil
}
