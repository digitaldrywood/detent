package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type coordinatorStopRunArguments struct {
	ProjectID        string               `json:"project_id"`
	Identifier       string               `json:"identifier"`
	ExpectedRevision tracker.Revision     `json:"expected_revision,string"`
	LeaseID          tracker.LeaseID      `json:"lease_id"`
	FencingToken     tracker.FencingToken `json:"fencing_token"`
	Destination      string               `json:"destination"`
	Priority         *int                 `json:"priority,omitempty"`
	Reason           string               `json:"reason,omitempty"`
}

func (t *coordinatorToolset) proposeStopRun(ctx context.Context, record conversationRecord, call runner.AgentToolCall) (any, error) {
	var args struct {
		WorkItemID  string `json:"work_item_id"`
		Destination string `json:"destination"`
		Priority    *int   `json:"priority,omitempty"`
		Reason      string `json:"reason,omitempty"`
	}
	if err := decodeCoordinatorArguments(call.Arguments, &args); err != nil {
		return nil, err
	}
	if !slices.Contains([]string{"Blocked", "Backlog", "Cancelled", "Todo"}, args.Destination) || len(args.Reason) > 280 || args.Priority != nil && (*args.Priority < 1 || *args.Priority > 4) {
		return nil, operatortool.ErrInvalidArguments
	}
	ctx, err := t.actionContext(ctx, record)
	if err != nil {
		return nil, err
	}
	ctx, err = operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeWrite, ProjectID: string(record.ProjectID)})
	if err != nil {
		return nil, err
	}
	resolve, ok := ctx.Value(nativeOperatorScopeKey{}).(func(context.Context) (nativeScope, error))
	if !ok {
		return nil, operatortool.ErrAccessDenied
	}
	scope, err := resolve(ctx)
	if err != nil {
		return nil, err
	}
	scope.project = record.ProjectID
	query := t.coordinator.service.store.db
	issue, err := t.coordinator.service.server.resolveOperatorNativeItem(ctx, query, scope, args.WorkItemID)
	if err != nil {
		return nil, err
	}
	_, internalID, err := readNativeIssue(ctx, query, scope, string(issue.WorkItemID))
	if err != nil {
		return nil, err
	}
	lease, found, err := readUnreleasedLease(ctx, query, internalID)
	if err != nil {
		return nil, err
	}
	if !found || requireCurrentLease(lease, lease.session.FencingToken, t.coordinator.service.server.config.now()) != nil {
		return nil, nativeInvalid("This item has no active runner attempt")
	}
	project, err := readNativeProject(ctx, query, scope)
	if err != nil {
		return nil, err
	}
	if err := validateNativeWorkflowTransition(project.States, issue.State, args.Destination, scope.credential.Scope); err != nil {
		return nil, err
	}
	request := coordinatorStopRunArguments{ProjectID: string(record.ProjectID), Identifier: string(issue.WorkItemID), ExpectedRevision: issue.Revision, LeaseID: lease.session.ID, FencingToken: lease.session.FencingToken, Destination: args.Destination, Priority: args.Priority, Reason: args.Reason}
	raw, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	action := chat.Action{ConversationID: record.ID, Kind: chat.ActionStopRun, ProjectID: request.ProjectID, IssueID: request.Identifier, Identifier: fmt.Sprintf("#%d", issue.Number), CurrentState: issue.State, Destination: args.Destination, Revision: int64(issue.Revision), Title: "Stop runner attempt", Description: fmt.Sprintf("Release attempt %s for %s and move it to %s.", lease.session.ID, issue.Title, args.Destination), Arguments: raw, Material: true, RequiresConfirmation: true}
	return t.submitCoordinatorAction(ctx, record, call, action)
}

func (s *Service) executeCoordinatorStopRun(ctx context.Context, scope nativeScope, action chat.Action) (chat.ActionExecution, error) {
	var request coordinatorStopRunArguments
	if err := operatortool.DecodeArguments(action.Arguments, &request); err != nil {
		return chat.ActionExecution{}, err
	}
	if request.ProjectID != action.ProjectID || request.Identifier != action.IssueID || request.LeaseID == "" || !slices.Contains([]string{"Blocked", "Backlog", "Cancelled", "Todo"}, request.Destination) || len(request.Reason) > 280 || request.Priority != nil && (*request.Priority < 1 || *request.Priority > 4) {
		return chat.ActionExecution{}, operatortool.ErrInvalidArguments
	}
	options := nativeCommandOptions{OperationID: nativeOperation(scope, "POST", "/work-items/"+request.Identifier+"/stop-run"), Item: request.Identifier, Feature: "collaboration"}
	_, err := s.executeNativeIssueMutation(ctx, scope, options, tracker.MutationForContext(ctx, action.RequestID), request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		issue, internalID, err := readNativeIssue(ctx, tx, scope, request.Identifier)
		if err != nil {
			return nil, err
		}
		if err := requireNativeEdit(issue, request.ExpectedRevision); err != nil {
			return nil, err
		}
		project, err := readNativeProject(ctx, tx, scope)
		if err != nil {
			return nil, err
		}
		if err := validateNativeWorkflowTransition(project.States, issue.State, request.Destination, scope.credential.Scope); err != nil {
			return nil, err
		}
		lease, found, err := readLeaseByID(ctx, tx, request.LeaseID)
		if err != nil {
			return nil, err
		}
		if !found || lease.issueID != internalID {
			return nil, tracker.ErrLeaseNotFound
		}
		if err := requireCurrentLease(lease, request.FencingToken, now); err != nil {
			return nil, err
		}
		if err := releaseLeaseRow(ctx, tx, lease, "cancelled", now); err != nil {
			return nil, err
		}
		from := issue.State
		issue.State = request.Destination
		if request.Priority != nil {
			issue.Priority = new(*request.Priority - 1)
		}
		return persistNativeIssue(ctx, tx, scope, issue, "workflow.transitioned", tracker.CollaborationData{FromState: from, ToState: issue.State, Reason: "user_requested", ReasonDetail: strings.TrimSpace(request.Reason)}, now)
	})
	if err != nil {
		return chat.ActionExecution{}, coordinatorActionError(err)
	}
	if s.conversations != nil {
		s.conversations.leaseReleased(ctx, request.LeaseID)
	}
	return chat.ActionExecution{Message: "Runner attempt released; item moved to " + request.Destination + ".", ResourceID: action.IssueID}, nil
}
