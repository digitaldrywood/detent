package operatorcommands

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type StopRunDeps struct {
	ResolveItem func(context.Context, hubscope.NativeScope, string) (tracker.NativeIssue, error)
	SafeError   func(error) error
	Audit       func(context.Context, mutation.Metadata, string)
	Submit      func(context.Context, chat.Action) (chat.Action, error)
	Result      func(chat.Action) (operatortool.Result, error)
	Stop        func(context.Context, hubscope.NativeScope, string, operatortool.NativeStopRunArguments) (json.RawMessage, error)
}

func (e StopRunDeps) Authorize(ctx context.Context, raw json.RawMessage) (context.Context, hubscope.NativeScope, operatortool.NativeStopRunArguments, error) {
	request, err := operatortool.DecodeNativeStopRun(raw)
	if err != nil {
		return ctx, hubscope.NativeScope{}, request, err
	}
	ctx, err = operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeWrite, Action: operatortool.StopRun, ProjectID: request.ProjectID, ResourceKind: "work_item", ResourceID: request.Identifier})
	if err != nil {
		return ctx, hubscope.NativeScope{}, request, err
	}
	resolve, ok := ctx.Value(hubscope.NativeOperatorScopeKey{}).(func(context.Context) (hubscope.NativeScope, error))
	if !ok {
		return ctx, hubscope.NativeScope{}, request, operatortool.ErrAccessDenied
	}
	scope, err := resolve(ctx)
	if err != nil {
		return ctx, hubscope.NativeScope{}, request, operatortool.ErrAccessDenied
	}
	scope.Project = tracker.ProjectID(request.ProjectID)
	scope.Action, scope.ActionItem, scope.ActionTarget = operatortool.StopRun, request.Identifier, request.Destination
	issue, err := e.ResolveItem(ctx, scope, request.Identifier)
	if err != nil {
		return ctx, hubscope.NativeScope{}, request, e.SafeError(err)
	}
	if issue.Profile != "native" {
		return ctx, hubscope.NativeScope{}, request, operatortool.ErrServiceUnavailable
	}
	request.Identifier = string(issue.WorkItemID)
	return ctx, scope, request, nil
}

func (e StopRunDeps) SubmitCall(ctx context.Context, call operatortool.Call) (operatortool.Result, error) {
	ctx, _, request, err := e.Authorize(ctx, call.Arguments)
	if err != nil {
		return operatortool.Result{}, err
	}
	var arguments json.RawMessage
	arguments, err = json.Marshal(request)
	if err != nil {
		return operatortool.Result{}, operatortool.ErrInvalidArguments
	}
	identity := operatortool.ConnectionIdentity(ctx)
	m := mutation.Metadata{PrincipalID: identity.PrincipalID, OrganizationID: identity.OrganizationID, ProjectID: request.ProjectID, ResourceID: request.Identifier, Action: call.Name, Source: "mcp", Confirmation: "none", CorrelationID: hubapi.NewID("mcp")}
	m, err = m.Bind(request.RequestID, arguments)
	if err != nil {
		return operatortool.Result{}, operatortool.ErrInvalidArguments
	}
	outcome := "failed"
	defer func() { e.Audit(ctx, m, outcome) }()
	action, err := e.Submit(ctx, chat.Action{Kind: chat.ActionStopRun, ProjectID: request.ProjectID, IssueID: request.Identifier, Identifier: request.Identifier, RequestID: request.RequestID, Arguments: arguments, Mutation: m})
	if err != nil {
		var conflict *operatortool.ConflictError
		if errors.As(err, &conflict) {
			return operatortool.Result{}, conflict
		}
		return operatortool.Result{}, e.SafeError(err)
	}
	outcome = string(action.Status)
	return e.Result(action)
}

func (e StopRunDeps) Execute(ctx context.Context, action chat.Action) (chat.ActionExecution, error) {
	ctx, scope, request, err := e.Authorize(ctx, action.Arguments)
	if err != nil {
		return chat.ActionExecution{}, err
	}
	identity := operatortool.ConnectionIdentity(ctx)
	m := action.Mutation
	bound, err := m.Bind(action.RequestID, action.Arguments)
	if err != nil || request.RequestID != action.RequestID || request.ProjectID != action.ProjectID || request.Identifier != action.IssueID || m.Source != "mcp" || m.PrincipalID != identity.PrincipalID || m.OrganizationID != identity.OrganizationID || m.ProjectID != request.ProjectID || m.ResourceID != request.Identifier || m.Action != operatortool.StopRun || m.CorrelationID == "" || bound.RetryIdentity != m.RetryIdentity || bound.InputHash != m.InputHash {
		return chat.ActionExecution{}, operatortool.ErrAccessDenied
	}
	raw, err := e.Stop(mutation.WithContext(ctx, m), scope, request.RequestID, request)
	if err != nil {
		var failure *hubapi.NativeError
		if errors.As(err, &failure) && failure.Code == "revision_conflict" {
			return chat.ActionExecution{}, &operatortool.ConflictError{Code: failure.Code, Message: failure.Message, CurrentRevision: int64(failure.CurrentRevision)}
		}
		if errors.Is(err, tracker.ErrLeaseConflict) || errors.Is(err, tracker.ErrStaleFencingToken) || errors.Is(err, tracker.ErrLeaseNotFound) {
			return chat.ActionExecution{}, &operatortool.ConflictError{Code: "lease_conflict", Message: "The exact runner attempt is no longer current"}
		}
		return chat.ActionExecution{}, e.SafeError(err)
	}
	return chat.ActionExecution{Message: string(raw), ResourceID: request.Identifier, Identifier: request.Identifier}, nil
}
