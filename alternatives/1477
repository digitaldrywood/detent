package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const coordinatorArchiveMaxItems = 50

func validateCoordinatorArchiveIDs(ids []string) error {
	if len(ids) < 1 || len(ids) > coordinatorArchiveMaxItems {
		return nativeInvalid("An archive must contain 1 to 50 issues")
	}
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if !strings.HasPrefix(id, "wi_") || len(id) > 256 || seen[id] {
			return nativeInvalid("Unique native work item IDs are required")
		}
		seen[id] = true
	}
	return nil
}

func readCoordinatorArchiveIssue(ctx context.Context, query nativeQueryer, scope nativeScope, id string, revision tracker.Revision, now time.Time) (tracker.NativeIssue, error) {
	issue, err := readNativeArchiveIssue(ctx, query, scope, id, revision, true, now)
	if errors.Is(err, tracker.ErrLeaseConflict) {
		return tracker.NativeIssue{}, nativeInvalid("Finish or stop the running issue before archiving: " + id)
	}
	if err != nil {
		return tracker.NativeIssue{}, err
	}
	if issue.State == "Merging" {
		return tracker.NativeIssue{}, nativeInvalid("A Merging issue cannot be archived: " + id)
	}
	if issue.Archived {
		return tracker.NativeIssue{}, nativeInvalid("Issue is already archived: " + id)
	}
	return issue, nil
}

func (t *coordinatorToolset) proposeIssueArchive(ctx context.Context, record conversationRecord, call runner.AgentToolCall) (any, error) {
	var args struct {
		WorkItemIDs []string `json:"work_item_ids"`
	}
	if err := decodeCoordinatorArguments(call.Arguments, &args); err != nil {
		return nil, err
	}
	if err := validateCoordinatorArchiveIDs(args.WorkItemIDs); err != nil {
		return nil, err
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
	for _, id := range args.WorkItemIDs {
		if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeWrite, ProjectID: string(record.ProjectID), ResourceKind: "work_item", ResourceID: id}); err != nil {
			return nil, err
		}
	}
	s := t.coordinator.service.server
	tx, err := s.database.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now, err := s.database.currentTime()
	if err != nil {
		return nil, err
	}
	archive := chat.IssueArchive{Items: make([]chat.IssueArchiveItem, 0, len(args.WorkItemIDs))}
	for _, id := range args.WorkItemIDs {
		current, _, err := readNativeIssue(ctx, tx, scope, id)
		if err != nil {
			return nil, err
		}
		issue, err := readCoordinatorArchiveIssue(ctx, tx, scope, id, current.Revision, now)
		if err != nil {
			return nil, err
		}
		archive.Items = append(archive.Items, chat.IssueArchiveItem{WorkItemID: id, Revision: int64(issue.Revision), Number: issue.Number, Title: issue.Title, State: issue.State})
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	arguments, err := json.Marshal(archive)
	if err != nil {
		return nil, err
	}
	if len(arguments) > coordinatorToolArgumentBytes {
		return nil, fmt.Errorf("%w: approved archive exceeds %d bytes", errCoordinatorToolArguments, coordinatorToolArgumentBytes)
	}
	action := chat.Action{ConversationID: record.ID, Kind: chat.ActionArchiveItems, ProjectID: string(record.ProjectID), Title: fmt.Sprintf("Archive %d issues", len(archive.Items)), Arguments: arguments, Material: true}
	return t.submitCoordinatorAction(ctx, record, call, action)
}

func (s *Service) executeCoordinatorIssueArchive(ctx context.Context, action chat.Action) (chat.ActionExecution, error) {
	var archive chat.IssueArchive
	if err := decodeCoordinatorArguments(action.Arguments, &archive); err != nil {
		return chat.ActionExecution{}, err
	}
	ids := make([]string, 0, len(archive.Items))
	for _, item := range archive.Items {
		ids = append(ids, item.WorkItemID)
	}
	if err := validateCoordinatorArchiveIDs(ids); err != nil {
		return chat.ActionExecution{}, err
	}
	resolve, ok := ctx.Value(nativeOperatorScopeKey{}).(func(context.Context) (nativeScope, error))
	if !ok {
		return chat.ActionExecution{}, operatortool.ErrAccessDenied
	}
	scope, err := resolve(ctx)
	if err != nil {
		return chat.ActionExecution{}, err
	}
	scope.project = tracker.ProjectID(action.ProjectID)
	for _, id := range ids {
		if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeWrite, ProjectID: action.ProjectID, ResourceKind: "work_item", ResourceID: id}); err != nil {
			return chat.ActionExecution{}, err
		}
	}
	options := nativeCommandOptions{OperationID: nativeOperation(scope, "POST", "/work-items/archive"), RequireLease: true, Feature: "collaboration", Completion: true}
	raw, err := s.executeNativeMutation(ctx, scope, options, tracker.MutationForContext(ctx, action.RequestID), archive, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		for _, item := range archive.Items {
			if _, err := readCoordinatorArchiveIssue(ctx, tx, scope, item.WorkItemID, tracker.Revision(item.Revision), now); err != nil {
				return nil, err
			}
		}
		issues := make([]tracker.NativeIssue, 0, len(archive.Items))
		for _, item := range archive.Items {
			issue, err := setNativeArchiveTx(ctx, tx, scope, item.WorkItemID, tracker.Revision(item.Revision), true, now)
			if err != nil {
				return nil, err
			}
			issues = append(issues, s.nativeIssueResponse(issue))
		}
		return issues, nil
	})
	if err != nil {
		return chat.ActionExecution{}, coordinatorActionError(err)
	}
	var names []string
	for _, item := range archive.Items {
		names = append(names, fmt.Sprintf("#%d (%s)", item.Number, item.WorkItemID))
	}
	return chat.ActionExecution{Message: "Archived " + strings.Join(names, ", ") + ". These issues can be restored.", Data: raw}, nil
}
