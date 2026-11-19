package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"

	"github.com/digitaldrywood/detent/internal/hubserver/conversations"
)

type nativeStopRunReceipt struct {
	LeaseID      tracker.LeaseID          `json:"lease_id"`
	FencingToken tracker.FencingToken     `json:"fencing_token"`
	Destination  string                   `json:"destination"`
	WorkItemID   tracker.NativeWorkItemID `json:"work_item_id"`
	Revision     tracker.Revision         `json:"revision,string"`
	State        string                   `json:"state"`
}

func PreviewStopRun(ctx context.Context, record conversations.ConversationRecord, call runner.AgentToolCall, deps StopRunPreviewDeps) (any, error) {
	var args struct {
		WorkItemID  string `json:"work_item_id"`
		Destination string `json:"destination"`
		Priority    *int   `json:"priority,omitempty"`
		Reason      string `json:"reason,omitempty"`
	}
	if err := DecodeArguments(call.Arguments, &args); err != nil {
		return nil, err
	}
	if !slices.Contains([]string{"Blocked", "Backlog", "Cancelled", "Todo"}, args.Destination) || len(args.Reason) > 280 || args.Priority != nil && (*args.Priority < 1 || *args.Priority > 4) {
		return nil, operatortool.ErrInvalidArguments
	}
	ctx, err := deps.ActionContext(ctx, record)
	if err != nil {
		return nil, err
	}
	ctx, err = operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeWrite, ProjectID: string(record.ProjectID)})
	if err != nil {
		return nil, err
	}
	resolve, ok := ctx.Value(hubscope.NativeOperatorScopeKey{}).(func(context.Context) (hubscope.NativeScope, error))
	if !ok {
		return nil, operatortool.ErrAccessDenied
	}
	scope, err := resolve(ctx)
	if err != nil {
		return nil, err
	}
	scope.Project = record.ProjectID
	issue, err := deps.ResolveIssue(ctx, scope, args.WorkItemID)
	if err != nil {
		return nil, err
	}
	lease, err := deps.ActiveLease(ctx, scope, issue.WorkItemID)
	if errors.Is(err, corestore.ErrNotFound) || err == nil && (lease.ReleasedAt != nil || !lease.ExpiresAt.After(deps.Now())) {
		return nil, hubapi.NativePublicInvalid("This item has no active runner attempt")
	}
	if err != nil {
		return nil, err
	}
	project, err := deps.ReadProject(ctx, scope)
	if err != nil {
		return nil, err
	}
	if err := hubrecords.ValidateNativeWorkflowTransition(project.States, issue.State, args.Destination, scope.Credential.Scope); err != nil {
		return nil, err
	}
	request := operatortool.NativeStopRunArguments{ProjectID: string(record.ProjectID), Identifier: string(issue.WorkItemID), ExpectedRevision: issue.Revision, LeaseID: tracker.LeaseID(lease.LeaseID), FencingToken: tracker.FencingToken(lease.FencingToken), Destination: args.Destination, Priority: args.Priority, Reason: args.Reason}
	raw, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	action := chat.Action{ConversationID: record.ID, Kind: chat.ActionStopRun, ProjectID: request.ProjectID, IssueID: request.Identifier, Identifier: fmt.Sprintf("#%d", issue.Number), CurrentState: issue.State, Destination: args.Destination, Revision: int64(issue.Revision), Title: "Stop runner attempt", Description: fmt.Sprintf("Release attempt %s for %s and move it to %s.", tracker.LeaseID(lease.LeaseID), issue.Title, args.Destination), Arguments: raw, Material: true, RequiresConfirmation: true}
	return deps.Submit(ctx, record, call, action)
}

func ExecuteStopRun(ctx context.Context, scope hubscope.NativeScope, action chat.Action, stop func(context.Context, hubscope.NativeScope, string, operatortool.NativeStopRunArguments) (json.RawMessage, error)) (chat.ActionExecution, error) {
	var request operatortool.NativeStopRunArguments
	if err := operatortool.DecodeArguments(action.Arguments, &request); err != nil {
		return chat.ActionExecution{}, err
	}
	if request.ProjectID != action.ProjectID || request.Identifier != action.IssueID {
		return chat.ActionExecution{}, operatortool.ErrInvalidArguments
	}
	if _, err := stop(ctx, scope, action.RequestID, request); err != nil {
		return chat.ActionExecution{}, ActionError(err)
	}
	return chat.ActionExecution{Message: "Runner attempt released; item moved to " + request.Destination + ".", ResourceID: action.IssueID}, nil
}

func StopRun(ctx context.Context, scope hubscope.NativeScope, requestID string, request operatortool.NativeStopRunArguments, transition StopRunTransition) (json.RawMessage, error) {
	if request.ProjectID != string(scope.Project) || request.LeaseID == "" || !slices.Contains([]string{"Blocked", "Backlog", "Cancelled", "Todo"}, request.Destination) || len(request.Reason) > 280 || request.Priority != nil && (*request.Priority < 1 || *request.Priority > 4) {
		return nil, operatortool.ErrInvalidArguments
	}
	raw, err := transition(ctx, scope, request.Identifier, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: tracker.MutationForContext(ctx, requestID).IdempotencyKey, LeaseID: request.LeaseID, FencingToken: request.FencingToken}, ExpectedRevision: request.ExpectedRevision, State: request.Destination, Reason: "user_requested", ReasonDetail: strings.TrimSpace(request.Reason)}, &request, "")
	if err != nil {
		return nil, err
	}
	var issue tracker.NativeIssue
	if err := json.Unmarshal(raw, &issue); err != nil {
		return nil, err
	}
	return json.Marshal(nativeStopRunReceipt{LeaseID: request.LeaseID, FencingToken: request.FencingToken, Destination: request.Destination, WorkItemID: issue.WorkItemID, Revision: issue.Revision, State: issue.State})
}

func ReadStopRunLease(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, itemID tracker.NativeWorkItemID, authorize func(context.Context, accessstore.Reader, hubscope.NativeScope, bool) error) (leasestore.Lease, error) {
	if err := authorize(ctx, r, scope, true); err != nil {
		return leasestore.Lease{}, err
	}
	item, err := r.WorkItem(ctx, scope.Project, itemID)
	if err != nil {
		return leasestore.Lease{}, err
	}
	return r.ActiveLeaseForIssue(ctx, item.ID)
}
