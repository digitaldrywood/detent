package hubserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (e nativeOperatorExecutor) attachmentAuthority(ctx context.Context, name string, raw json.RawMessage) (context.Context, nativeScope, operatortool.AttachmentOperationArguments, error) {
	r, err := operatortool.DecodeAttachmentOperation(name, raw)
	if err != nil {
		return ctx, nativeScope{}, r, err
	}
	if !e.service.hostedShared() {
		return ctx, nativeScope{}, r, operatortool.ErrServiceUnavailable
	}
	scopeRequired := apikey.ScopeWrite
	if name == operatortool.ReadAttachment || name == operatortool.ReadAttachmentMetadata {
		scopeRequired = apikey.ScopeRead
	}
	ctx, err = operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: scopeRequired, ProjectID: r.ProjectID})
	if err != nil {
		return ctx, nativeScope{}, r, err
	}
	resolve, ok := ctx.Value(nativeOperatorScopeKey{}).(func(context.Context) (nativeScope, error))
	if !ok {
		return ctx, nativeScope{}, r, operatortool.ErrAccessDenied
	}
	scope, err := resolve(ctx)
	if err != nil {
		return ctx, nativeScope{}, r, operatortool.ErrAccessDenied
	}
	scope.project = tracker.ProjectID(r.ProjectID)
	return ctx, scope, r, nil
}

func attachmentBinding(record attachment.Metadata) (string, error) {
	record.AuthorizedPrincipal = ""
	raw, err := json.Marshal(record)
	if err != nil || len(raw) > operatortool.MaxResultBytes {
		return "", operatortool.ErrServiceUnavailable
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func (e nativeOperatorExecutor) attachmentOperation(ctx context.Context, call operatortool.Call) (result operatortool.Result, resultErr error) {
	i := operatortool.ConnectionIdentity(ctx)
	m := mutation.Metadata{PrincipalID: i.PrincipalID, OrganizationID: i.OrganizationID, Action: call.Name, Source: "mcp", Mode: "confirmation", Confirmation: "none", CorrelationID: newNativeID("mcp")}
	outcome := "failed"
	if call.Name == operatortool.ReferenceAttachment || call.Name == operatortool.DeleteAttachment {
		defer func() {
			if errors.Is(resultErr, operatortool.ErrAccessDenied) {
				outcome = "denied"
			}
			e.service.hubChangeAudit(ctx, m, outcome)
		}()
	}
	ctx, scope, r, err := e.attachmentAuthority(ctx, call.Name, call.Arguments)
	if err != nil {
		return operatortool.Result{}, err
	}
	m.ProjectID, m.ResourceID = r.ProjectID, r.AttachmentID
	if call.Name == operatortool.DeleteAttachment {
		previous, found, err := e.service.operatorChat.RetryResult(ctx, chat.ActionKind(call.Name), r.RequestID, call.Arguments)
		if err != nil {
			return operatortool.Result{}, err
		}
		if found {
			outcome = "replayed"
			return e.attachmentActionResult(ctx, previous)
		}
	}
	record, err := e.service.readCloudAttachment(ctx, scope, r.AttachmentID)
	if err != nil || record.Validate() != nil {
		return operatortool.Result{}, operatortool.ErrAccessDenied
	}
	if call.Name == operatortool.ReadAttachment || call.Name == operatortool.ReadAttachmentMetadata {
		if r.Offset > record.Size {
			return operatortool.Result{}, operatortool.ErrInvalidArguments
		}
		return operatortool.EncodeResult(map[string]string{"entry_attachment": call.Name})
	}
	m, err = m.Bind(r.RequestID, call.Arguments)
	if err != nil {
		return operatortool.Result{}, operatortool.ErrInvalidArguments
	}
	if call.Name == operatortool.ReferenceAttachment {
		err := e.service.referenceCloudAttachmentCommand(mutation.WithContext(ctx, m), scope, r.AttachmentID, attachment.SourceReference{WorkItemID: r.WorkItemID, CommentID: r.CommentID})
		if err != nil {
			return operatortool.Result{}, hubSafeChangeError(err)
		}
		outcome = "succeeded"
		return operatortool.EncodeResult(map[string]string{"attachment_id": r.AttachmentID, "project_id": r.ProjectID, "work_item_id": r.WorkItemID, "comment_id": r.CommentID, "status": "bound"})
	}
	binding, err := attachmentBinding(record)
	if err != nil {
		return operatortool.Result{}, err
	}
	preview, err := json.Marshal(record)
	if err != nil {
		return operatortool.Result{}, operatortool.ErrServiceUnavailable
	}
	action := chat.Action{Kind: chat.ActionKind(call.Name), ProjectID: r.ProjectID, Identifier: r.AttachmentID, Title: record.Name, CurrentState: binding, Description: string(preview), RequestID: r.RequestID, Arguments: call.Arguments, Mutation: m}
	action, err = e.service.operatorChat.Submit(ctx, action)
	if err != nil {
		return operatortool.Result{}, hubSafeChangeError(err)
	}
	outcome = string(action.Status)
	return e.attachmentActionResult(ctx, action)
}

func (e nativeOperatorExecutor) executeAttachmentDeletion(ctx context.Context, action chat.Action) (chat.ActionExecution, error) {
	ctx, scope, r, err := e.attachmentAuthority(ctx, operatortool.DeleteAttachment, action.Arguments)
	if err != nil {
		return chat.ActionExecution{}, err
	}
	i, m := operatortool.ConnectionIdentity(ctx), action.Mutation
	bound, err := m.Bind(action.RequestID, action.Arguments)
	if err != nil || r.RequestID != action.RequestID || m.Source != "mcp" || m.PrincipalID != i.PrincipalID || m.OrganizationID != i.OrganizationID || m.ProjectID != r.ProjectID || m.ResourceID != r.AttachmentID || m.Action != operatortool.DeleteAttachment || m.CorrelationID == "" || bound.InputHash != m.InputHash || bound.RetryIdentity != m.RetryIdentity || m.Confirmation != "approved" && m.Confirmation != "yolo" {
		return chat.ActionExecution{}, operatortool.ErrAccessDenied
	}
	tx, err := e.service.database.db.BeginTx(ctx, nil)
	if err != nil {
		return chat.ActionExecution{}, operatortool.ErrServiceUnavailable
	}
	defer tx.Rollback()
	if err := e.service.recheckHostedMutation(ctx, tx, scope); err != nil {
		return chat.ActionExecution{}, hubSafeChangeError(err)
	}
	record, err := readCloudAttachment(ctx, tx, scope, r.AttachmentID)
	if err != nil {
		return chat.ActionExecution{}, hubSafeChangeError(err)
	}
	binding, err := attachmentBinding(record)
	if err != nil || binding != action.CurrentState {
		return chat.ActionExecution{}, mutation.ErrConflict
	}
	if err := e.service.deleteCloudAttachmentMetadata(mutation.WithContext(ctx, m), tx, scope, r.AttachmentID); err != nil {
		return chat.ActionExecution{}, hubSafeChangeError(err)
	}
	if err := tx.Commit(); err != nil {
		return chat.ActionExecution{}, operatortool.ErrServiceUnavailable
	}
	return chat.ActionExecution{Message: "Attachment hidden; entry object deletion is pending.", ResourceID: r.AttachmentID, Identifier: r.AttachmentID}, nil
}

func (e nativeOperatorExecutor) attachmentActionResult(ctx context.Context, action chat.Action) (operatortool.Result, error) {
	ctx, scope, r, err := e.attachmentAuthority(ctx, operatortool.DeleteAttachment, action.Arguments)
	if err != nil {
		return operatortool.Result{}, err
	}
	state := ""
	if action.Status == chat.ActionSucceeded {
		var confirmed bool
		err := e.service.database.db.QueryRowContext(ctx, "SELECT object_deleted_at IS NOT NULL FROM attachments WHERE organization_id=? AND project_id=? AND id=? AND deleted_at IS NOT NULL", scope.organization, scope.project, r.AttachmentID).Scan(&confirmed)
		if err != nil {
			return operatortool.Result{}, operatortool.ErrAccessDenied
		}
		state = "pending"
		if confirmed {
			state = "confirmed"
			action.Result = "Attachment deleted; entry object deletion is confirmed."
		}
	}
	return hubChangeResult(struct {
		Preview        chat.Action       `json:"preview"`
		ActionID       string            `json:"action_id"`
		Status         chat.ActionStatus `json:"status"`
		ApprovalURL    string            `json:"approval_url,omitempty"`
		ResultTool     string            `json:"result_tool"`
		ObjectDeletion string            `json:"object_deletion,omitempty"`
	}{action, action.ID, action.Status, action.PendingApprovalURL(e.service.billingApprovalURL(action.ConnectionID)), operatortool.ActionResult, state})
}
