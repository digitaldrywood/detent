package hubserver

import (
	"context"
	"encoding/json"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (e nativeOperatorExecutor) workflowAuthority(ctx context.Context, raw json.RawMessage) (context.Context, nativeScope, error) {
	request, err := operatortool.DecodeNativeMoveItem(raw)
	if err != nil {
		return ctx, nativeScope{}, err
	}
	ctx, err = operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeWrite, ProjectID: request.ProjectID, ResourceKind: "work_item", ResourceID: request.Identifier})
	if err != nil {
		return ctx, nativeScope{}, err
	}
	resolve, ok := ctx.Value(nativeOperatorScopeKey{}).(func(context.Context) (nativeScope, error))
	if !ok {
		return ctx, nativeScope{}, operatortool.ErrAccessDenied
	}
	scope, err := resolve(ctx)
	if err != nil {
		return ctx, nativeScope{}, operatortool.ErrAccessDenied
	}
	scope.project = tracker.ProjectID(request.ProjectID)
	issue, err := e.service.resolveOperatorNativeItem(ctx, e.service.database.db, scope, request.Identifier)
	if err != nil {
		return ctx, nativeScope{}, hubSafeChangeError(err)
	}
	if issue.Profile != "native" {
		return ctx, nativeScope{}, operatortool.ErrServiceUnavailable
	}
	return ctx, scope, nil
}

func (e nativeOperatorExecutor) workflowProposal(ctx context.Context, scope nativeScope, request operatortool.MoveItemArguments) (chat.Action, error) {
	issue, err := e.service.resolveOperatorNativeItem(ctx, e.service.database.db, scope, request.Identifier)
	if err != nil {
		return chat.Action{}, hubSafeChangeError(err)
	}
	project, err := readNativeProject(ctx, e.service.database.db, scope)
	if err != nil {
		return chat.Action{}, hubSafeChangeError(err)
	}
	action := chat.Action{Kind: chat.ActionMoveItem, NativeWorkflow: true, ProjectID: request.ProjectID, IssueID: string(issue.WorkItemID), Identifier: request.Identifier, CurrentState: issue.State, TargetState: request.TargetState, Revision: request.ExpectedRevision, Title: issue.Title}
	for _, state := range project.States {
		if state.Name == issue.State || state.Name == request.TargetState {
			action.Material = action.Material || state.Terminal
		}
	}
	return action, nil
}

func (e nativeOperatorExecutor) workflowTransition(ctx context.Context, call operatortool.Call) (operatortool.Result, error) {
	ctx, scope, err := e.workflowAuthority(ctx, call.Arguments)
	if err != nil {
		return operatortool.Result{}, err
	}
	request, err := operatortool.DecodeNativeMoveItem(call.Arguments)
	if err != nil {
		return operatortool.Result{}, err
	}
	action, err := e.workflowProposal(ctx, scope, request)
	if err != nil {
		return operatortool.Result{}, err
	}
	request.Identifier = action.IssueID
	action.Identifier = request.Identifier
	var arguments json.RawMessage
	arguments, err = json.Marshal(request)
	if err != nil {
		return operatortool.Result{}, operatortool.ErrInvalidArguments
	}
	identity := operatortool.ConnectionIdentity(ctx)
	action.RequestID, action.Arguments = request.RequestID, arguments
	action.Mutation = mutation.Metadata{PrincipalID: identity.PrincipalID, OrganizationID: identity.OrganizationID, ProjectID: request.ProjectID, ResourceID: action.IssueID, Action: call.Name, Source: "mcp", Mode: "confirmation", Confirmation: "none", CorrelationID: newNativeID("mcp")}
	action.Mutation, err = action.Mutation.Bind(request.RequestID, arguments)
	if err != nil {
		return operatortool.Result{}, operatortool.ErrInvalidArguments
	}
	outcome := "failed"
	defer func() { e.service.hubChangeAudit(ctx, action.Mutation, outcome) }()
	if e.service.config.Hosted != nil {
		previous, replay, err := e.service.operatorChat.RetryResult(ctx, action.Kind, action.RequestID, action.Arguments)
		if err != nil {
			return operatortool.Result{}, err
		}
		if replay {
			outcome = "replayed"
			return e.service.hubActionResult(previous)
		}
	}
	mutationContext := mutation.WithContext(ctx, action.Mutation)
	transition := tracker.Transition{Mutation: tracker.MutationForContext(mutationContext, request.RequestID), ExpectedRevision: tracker.Revision(request.ExpectedRevision), State: request.TargetState, Reason: "user_requested"}
	replay, found, err := e.service.nativeCommandReplay(mutationContext, scope, nativeOperation(scope, "POST", "/work-items/"+request.Identifier+"/workflow"), transition.IdempotencyKey, transition)
	if err != nil {
		return operatortool.Result{}, hubSafeChangeError(err)
	}
	if found {
		outcome = "replayed"
		return e.workflowReceipt(replay)
	}
	if e.service.config.Hosted == nil {
		if chat.RequiresConfirmation(action) {
			return operatortool.Result{}, operatortool.ErrServiceUnavailable
		}
		execution, err := e.executeWorkflowTransition(ctx, action)
		if err != nil {
			return operatortool.Result{}, err
		}
		outcome = "succeeded"
		return e.workflowReceipt(json.RawMessage(execution.Message))
	}
	action, err = e.service.operatorChat.Submit(ctx, action)
	if err != nil {
		return operatortool.Result{}, hubSafeChangeError(err)
	}
	outcome = string(action.Status)
	return e.service.hubActionResult(action)
}

func (e nativeOperatorExecutor) workflowReceipt(raw json.RawMessage) (operatortool.Result, error) {
	var issue tracker.NativeIssue
	if err := json.Unmarshal(raw, &issue); err != nil {
		return operatortool.Result{}, operatortool.ErrServiceUnavailable
	}
	issue = e.service.nativeIssueResponse(issue)
	return hubChangeResult(map[string]any{"data": issue, "resource_id": issue.WorkItemID, "revision": issue.Revision, "status": chat.ActionSucceeded})
}

func (e nativeOperatorExecutor) executeWorkflowTransition(ctx context.Context, action chat.Action) (chat.ActionExecution, error) {
	ctx, scope, err := e.workflowAuthority(ctx, action.Arguments)
	if err != nil {
		return chat.ActionExecution{}, err
	}
	request, err := operatortool.DecodeNativeMoveItem(action.Arguments)
	if err != nil {
		return chat.ActionExecution{}, err
	}
	identity := operatortool.ConnectionIdentity(ctx)
	m := action.Mutation
	bound, err := m.Bind(action.RequestID, action.Arguments)
	if err != nil || request.RequestID != action.RequestID || m.Source != "mcp" || m.PrincipalID != identity.PrincipalID || m.OrganizationID != identity.OrganizationID || m.ProjectID != request.ProjectID || m.Action != operatortool.MoveItem || m.CorrelationID == "" || bound.RetryIdentity != m.RetryIdentity || bound.InputHash != m.InputHash {
		return chat.ActionExecution{}, operatortool.ErrAccessDenied
	}
	current, err := e.workflowProposal(ctx, scope, request)
	if err != nil {
		return chat.ActionExecution{}, err
	}
	if current.Material && m.Confirmation != "approved" && m.Confirmation != "yolo" {
		return chat.ActionExecution{}, operatortool.ErrAccessDenied
	}
	ctx = mutation.WithContext(ctx, m)
	raw, err := e.service.transitionNativeIssueCommand(ctx, scope, request.Identifier, tracker.Transition{Mutation: tracker.MutationForContext(ctx, request.RequestID), ExpectedRevision: tracker.Revision(request.ExpectedRevision), State: request.TargetState, Reason: "user_requested"})
	if err != nil {
		return chat.ActionExecution{}, hubSafeChangeError(err)
	}
	var issue tracker.NativeIssue
	if err := json.Unmarshal(raw, &issue); err != nil {
		return chat.ActionExecution{}, operatortool.ErrServiceUnavailable
	}
	return chat.ActionExecution{Message: string(raw), ResourceID: string(issue.WorkItemID), Identifier: request.Identifier, Revision: int64(issue.Revision)}, nil
}
