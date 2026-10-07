package hubserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

type operatorScopeKey struct{}
type workspaceOperatorExecutor struct{ server *Service }

var errWorkspaceOperationUnavailable = errors.New("workspace operation is unavailable")

type workspaceToolRequest = operatortool.WorkspaceArguments

func (e workspaceOperatorExecutor) OpenConnection(ctx context.Context) error {
	if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead}); err != nil {
		return err
	}
	return e.server.operatorChat.AttachConnection(ctx)
}
func (e workspaceOperatorExecutor) ListTools(ctx context.Context) ([]operatortool.Definition, error) {
	if _, err := e.server.authorizeCatalog(ctx, operatortool.Requirement{Scope: apikey.ScopeRead}); err != nil {
		return nil, err
	}
	definitions := []operatortool.Definition{}
	for _, d := range operatortool.WorkspaceCatalog() {
		if e.server.conversations == nil && (strings.Contains(d.Name, "conversation") || d.Name == "list_work_item_references") {
			continue
		}
		if e.server.workspaces == nil && (strings.Contains(d.Name, "workspace") || strings.Contains(d.Name, "action_run")) {
			continue
		}
		if !d.Annotations.ReadOnly {
			if _, err := e.server.authorizeCatalog(ctx, operatortool.Requirement{Scope: apikey.ScopeWrite, ResourceKind: "workspace"}); err != nil {
				continue
			}
		}
		definitions = append(definitions, d)
	}
	for _, d := range operatortool.CommandCatalog() {
		if d.Name == operatortool.ConnectionInfo || d.Name == operatortool.ActionResult {
			definitions = append(definitions, d)
		}
	}
	return definitions, nil
}
func workspaceRequirement(name string, r workspaceToolRequest, write bool) operatortool.Requirement {
	requirement := operatortool.Requirement{Scope: apikey.ScopeRead, ProjectID: r.ProjectID}
	if write {
		requirement.Scope = apikey.ScopeWrite
	}
	switch {
	case r.WorkspaceID != "":
		requirement.ResourceKind, requirement.ResourceID = "workspace", r.WorkspaceID
	case r.ConversationID != "":
		requirement.ResourceKind, requirement.ResourceID = "conversation", r.ConversationID
	case r.ActionID != "":
		requirement.ResourceKind, requirement.ResourceID = "action", r.ActionID
	case r.WorkItemID != "":
		requirement.ResourceKind, requirement.ResourceID = "work_item", r.WorkItemID
	case r.SubjectWorkItemID != "":
		requirement.ResourceKind, requirement.ResourceID = "work_item", r.SubjectWorkItemID
	}
	if name == "create_project_action_run" || name == "workspace_terminal" || name == "workspace_file_read" || name == "workspace_file_list" || name == "create_workspace" || name == "create_project_action" || name == "patch_project_action" {
		requirement.ResourceKind = "runners:" + requirement.ResourceKind
	}
	return requirement
}
func (s *Service) checkOperatorResource(ctx context.Context, query nativeQueryer, scope nativeScope, r operatortool.Requirement) error {
	kind := r.ResourceKind
	if strings.HasPrefix(kind, "runners:") {
		if err := s.requireOperatorRunners(ctx, query, scope); err != nil {
			return err
		}
		kind = strings.TrimPrefix(kind, "runners:")
	}
	if kind == "" && r.ResourceID == "" {
		return nil
	}
	switch kind {
	case "workspace":
		w, err := s.requireWorkspaces()
		if err != nil {
			return err
		}
		_, err = w.readWorkspaceForActor(ctx, query, scope, r.ResourceID)
		return err
	case "conversation":
		if s.conversations == nil {
			return errWorkspaceOperationUnavailable
		}
		_, err := s.conversations.loadConversation(ctx, query, scope, r.ResourceID)
		return err
	case "action":
		_, err := readProjectAction(ctx, query, scope, r.ResourceID)
		return err
	case "work_item":
		_, err := s.resolveOperatorNativeItem(ctx, query, scope, r.ResourceID)
		return err
	default:
		return operatortool.ErrAccessDenied
	}
}
func (e workspaceOperatorExecutor) Execute(ctx context.Context, call operatortool.Call) (operatortool.Result, error) {
	if call.Name == operatortool.ConnectionInfo || call.Name == operatortool.ActionResult {
		return e.connectionResult(ctx, call)
	}
	definition, err := operatortool.WorkspaceDefinition(call.Name)
	if err != nil {
		return operatortool.NewAuthorizedExecutor(nil).Execute(ctx, call)
	}
	identity := operatortool.ConnectionIdentity(ctx)
	m := mutation.Metadata{PrincipalID: identity.PrincipalID, OrganizationID: identity.OrganizationID, Action: call.Name, Source: "mcp", CorrelationID: newNativeID("correlation"), Confirmation: "none"}
	outcome := "failed"
	if !definition.Annotations.ReadOnly {
		defer func() { e.auditMutation(ctx, m, outcome) }()
	}
	if err := operatortool.ValidateWorkspaceArguments(call); err != nil {
		return operatortool.Result{}, err
	}
	var request workspaceToolRequest
	if err := operatortool.DecodeArguments(call.Arguments, &request); err != nil {
		return operatortool.Result{}, err
	}
	write := !definition.Annotations.ReadOnly
	m.ProjectID = request.ProjectID
	requirement := workspaceRequirement(call.Name, request, write)
	if call.Name == "delete_project_action" {
		// A completed deletion has no live definition. Its principal-bound,
		// input-bound receipt proves ownership after current project authority.
		requirement.ResourceKind, requirement.ResourceID = "", ""
	}
	ctx, err = operatortool.AuthorizeCurrent(ctx, requirement)
	if err != nil {
		outcome = "denied"
		return operatortool.Result{}, err
	}
	scope := ctx.Value(operatorScopeKey{}).(nativeScope) //nolint:errcheck // Successful workspace authorization binds the application scope.
	scope.project = tracker.ProjectID(request.ProjectID)
	if !write {
		value, err := e.server.readWorkspaceTool(ctx, scope, call.Name, request)
		if err != nil {
			return operatortool.Result{}, safeWorkspaceError(err)
		}
		return operatortool.WorkspaceResult(struct {
			GeneratedAt time.Time       `json:"generated_at"`
			Freshness   string          `json:"freshness"`
			Data        json.RawMessage `json:"data"`
		}{e.server.config.now(), "live", value})
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(call.Arguments, &fields) != nil {
		return operatortool.Result{}, operatortool.ErrInvalidArguments
	}
	delete(fields, "request_id")
	arguments, err := json.Marshal(fields)
	if err != nil {
		return operatortool.Result{}, errWorkspaceOperationUnavailable
	}
	m, err = m.Bind(request.RequestID, json.RawMessage(arguments))
	if err != nil {
		return operatortool.Result{}, operatortool.ErrInvalidArguments
	}
	ctx = mutation.WithContext(ctx, m)
	command := hostedCommand{actor: identity.PrincipalID, operation: "mcp " + call.Name, key: m.RetryIdentity, input: json.RawMessage(arguments)}
	if raw, found, err := e.server.readHostedOperation(ctx, command); err != nil {
		return operatortool.Result{}, safeWorkspaceError(err)
	} else if found {
		var rejection workspaceRejectedAction
		if json.Unmarshal(raw, &rejection) != nil {
			return operatortool.Result{}, errWorkspaceOperationUnavailable
		}
		if rejection.Rejected {
			outcome = "replayed"
			return e.actionResult(rejection.Action, nil)
		}
		var receipt workspaceCompletedAction
		if json.Unmarshal(raw, &receipt) != nil || receipt.Action.Status != chat.ActionSucceeded {
			return operatortool.Result{}, errWorkspaceOperationUnavailable
		}
		outcome, m = "replayed", receipt.Action.Mutation
		return e.actionResult(receipt.Action, receipt.Execution.Data)
	}
	if call.Name == "delete_project_action" {
		ctx, err = operatortool.AuthorizeCurrent(ctx, workspaceRequirement(call.Name, request, true))
		if err != nil {
			outcome = "denied"
			return operatortool.Result{}, err
		}
	}
	proposal := chat.Action{Kind: chat.ActionKind(call.Name), ProjectID: request.ProjectID, IssueID: resourceSelector(request), RequestID: request.RequestID, Arguments: arguments, Mutation: m}
	if call.Name == "create_project_action_run" {
		configured, err := readProjectAction(ctx, e.server.database.db, scope, request.ActionID)
		if err != nil {
			return operatortool.Result{}, safeWorkspaceError(err)
		}
		if configured.Revision != int64(request.ExpectedRevision) {
			return operatortool.Result{}, errWorkspaceOperationUnavailable
		}
		proposal.Title, proposal.Description = configured.Name, configured.Command
	}
	// Standalone hubs have no authenticated operator browser. Connection
	// setup supplies this authority; tool arguments cannot invent an approval
	// service. Completed receipts above remain readable without confirmation.
	action, err := e.server.operatorChat.Submit(ctx, proposal)
	if err != nil {
		return operatortool.Result{}, safeWorkspaceError(err)
	}
	outcome = string(action.Status)
	m = action.Mutation
	data, err := e.server.operatorChat.ConnectionResult(ctx, action.ID)
	if err != nil {
		return operatortool.Result{}, safeWorkspaceError(err)
	}
	return e.actionResult(action, data)
}
func resourceSelector(r workspaceToolRequest) string {
	for _, id := range []string{r.ConversationID, r.WorkspaceID, r.ActionID, r.WorkItemID} {
		if id != "" {
			return id
		}
	}
	return ""
}
func safeWorkspaceError(err error) error {
	if conflict := operatorNativeConflict(err); conflict != nil {
		return conflict
	}
	for _, safe := range []error{operatortool.ErrAccessDenied, operatortool.ErrInvalidArguments, mutation.ErrConflict, mutation.ErrUncertain} {
		if errors.Is(err, safe) {
			return safe
		}
	}
	return errWorkspaceOperationUnavailable
}
func (e workspaceOperatorExecutor) ExecuteAction(ctx context.Context, action chat.Action) (chat.ActionExecution, error) {
	// Resolve resource authority again after an operator decision, including
	// runner grants. Connection confirmation never supplies capability.
	var request workspaceToolRequest
	if operatortool.DecodeArguments(action.Arguments, &request) != nil {
		return chat.ActionExecution{}, operatortool.ErrInvalidArguments
	}
	request.RequestID = action.RequestID
	// Validate original schema: omit zero selectors absent from the original.
	var fields map[string]json.RawMessage
	if json.Unmarshal(action.Arguments, &fields) != nil || fields == nil {
		return chat.ActionExecution{}, operatortool.ErrInvalidArguments
	}
	requestID, _ := json.Marshal(action.RequestID) //nolint:errcheck // A string cannot fail JSON encoding.
	fields["request_id"] = requestID
	raw, err := json.Marshal(fields)
	if err != nil {
		return chat.ActionExecution{}, operatortool.ErrInvalidArguments
	}
	if err := operatortool.ValidateWorkspaceArguments(operatortool.Call{Name: string(action.Kind), Arguments: raw}); err != nil {
		return chat.ActionExecution{}, err
	}
	ctx, err = operatortool.AuthorizeCurrent(ctx, workspaceRequirement(string(action.Kind), request, true))
	if err != nil {
		return chat.ActionExecution{}, err
	}
	identity := operatortool.ConnectionIdentity(ctx)
	m := action.Mutation
	bound, err := m.Bind(action.RequestID, action.Arguments)
	if err != nil || m.Source != "mcp" || m.PrincipalID != identity.PrincipalID || m.OrganizationID != identity.OrganizationID || m.ProjectID != request.ProjectID || m.Action != string(action.Kind) || bound.RetryIdentity != m.RetryIdentity || bound.InputHash != m.InputHash {
		return chat.ActionExecution{}, operatortool.ErrAccessDenied
	}
	scope := ctx.Value(operatorScopeKey{}).(nativeScope) //nolint:errcheck // Successful workspace authorization binds the application scope.
	scope.project = tracker.ProjectID(request.ProjectID)
	ctx = mutation.WithContext(ctx, m)
	command := hostedCommand{actor: identity.PrincipalID, operation: "mcp " + string(action.Kind), key: m.RetryIdentity, input: action.Arguments}
	claimed, previous, err := e.server.claimHostedOperation(ctx, command)
	if err != nil {
		return chat.ActionExecution{}, safeWorkspaceError(err)
	}
	if !claimed {
		var receipt workspaceCompletedAction
		if json.Unmarshal(previous, &receipt) != nil || receipt.Action.Status != chat.ActionSucceeded {
			return chat.ActionExecution{}, errWorkspaceOperationUnavailable
		}
		return receipt.Execution, nil
	}
	data, err := e.server.commandWorkspaceTool(ctx, scope, string(action.Kind), request)
	if err != nil {
		return chat.ActionExecution{}, safeWorkspaceError(err)
	}
	execution := chat.ActionExecution{Data: data, Message: "Operation completed.", ResourceID: resourceSelector(request)}
	var created struct {
		ID           string `json:"id"`
		RunID        string `json:"run_id"`
		Conversation struct {
			ID string `json:"id"`
		} `json:"conversation"`
	}
	if json.Unmarshal(data, &created) == nil {
		if created.ID != "" {
			execution.ResourceID = created.ID
		}
		if created.RunID != "" {
			execution.ResourceID = created.RunID
		}
		if created.Conversation.ID != "" {
			execution.ResourceID = created.Conversation.ID
		}
	}
	completed := action
	completed.Status, completed.Result = chat.ActionSucceeded, execution.Message
	completed.IssueID = execution.ResourceID
	now := e.server.config.now()
	completed.ResolvedAt = &now
	if _, err := e.server.completeHostedOperation(ctx, command, workspaceCompletedAction{Action: completed, Execution: execution}); err != nil {
		return chat.ActionExecution{}, mutation.ErrUncertain
	}
	return execution, nil
}

type workspaceRejectedAction struct {
	Rejected bool        `json:"rejected"`
	Action   chat.Action `json:"action"`
}

type workspaceCompletedAction struct {
	Action    chat.Action          `json:"action"`
	Execution chat.ActionExecution `json:"execution"`
}

func (e workspaceOperatorExecutor) AuditAction(ctx context.Context, action chat.Action, outcome string) {
	if outcome == "rejected" {
		command := hostedCommand{actor: action.Mutation.PrincipalID, operation: "mcp " + string(action.Kind), key: action.Mutation.RetryIdentity, input: action.Arguments}
		persistCtx, cancel := context.WithTimeout(mutation.WithContext(context.WithoutCancel(ctx), action.Mutation), 2*time.Second)
		defer cancel()
		claimed, _, err := e.server.claimHostedOperation(persistCtx, command)
		if err == nil && claimed {
			action.Status = chat.ActionRejected
			_, err = e.server.completeHostedOperation(persistCtx, command, workspaceRejectedAction{Rejected: true, Action: action})
		}
		if err != nil {
			e.server.config.Logger.WarnContext(ctx, "operator rejection receipt unavailable")
		}
	}

	m := action.Mutation
	m.ResourceID = action.IssueID
	e.auditMutation(ctx, m, outcome)
}
func (e workspaceOperatorExecutor) auditMutation(ctx context.Context, m mutation.Metadata, outcome string) {
	m.RetryIdentity, m.InputHash = "", ""
	raw, err := json.Marshal(struct {
		mutation.Metadata
		Outcome string `json:"outcome"`
	}{m, outcome})
	if err == nil {
		e.server.config.Logger.InfoContext(ctx, "operator mutation", "audit", string(raw))
	}
	if scope, ok := ctx.Value(operatorScopeKey{}).(nativeScope); ok && scope.credential.Hosted != nil {
		auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		if err := e.server.hostedAudit(auditCtx, scope.credential.Hosted, outcome, "mcp "+m.Action, m.ProjectID, 0); err != nil {
			e.server.config.Logger.WarnContext(ctx, "operator audit unavailable")
		}
	}

}
func (e workspaceOperatorExecutor) actionResult(action chat.Action, data json.RawMessage) (operatortool.Result, error) {
	return operatortool.WorkspaceResult(struct {
		Action     chat.Action       `json:"preview"`
		ID         string            `json:"action_id"`
		Status     chat.ActionStatus `json:"status"`
		Data       json.RawMessage   `json:"data,omitempty"`
		ResultTool string            `json:"result_tool"`
	}{action, action.ID, action.Status, data, operatortool.ActionResult})
}
func (e workspaceOperatorExecutor) connectionResult(ctx context.Context, call operatortool.Call) (operatortool.Result, error) {
	var request operatortool.ActionResultArguments
	if call.Name == operatortool.ConnectionInfo {
		if operatortool.DecodeArguments(call.Arguments, &struct{}{}) != nil {
			return operatortool.Result{}, operatortool.ErrInvalidArguments
		}
	} else {
		if operatortool.DecodeArguments(call.Arguments, &request) != nil || request.ActionID == "" || len(request.ActionID) > 256 {
			return operatortool.Result{}, operatortool.ErrInvalidArguments
		}
	}
	if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead}); err != nil {
		return operatortool.Result{}, err
	}
	if err := e.server.operatorChat.CheckConnection(ctx); err != nil {
		return operatortool.Result{}, err
	}
	id := operatortool.CurrentConnection(ctx).ID
	if call.Name == operatortool.ConnectionInfo {
		return operatortool.WorkspaceResult(struct {
			ID string `json:"connection_id"`
		}{id})
	}
	action, ok := e.server.operatorChat.Action(id, request.ActionID)
	if !ok {
		return operatortool.Result{}, errWorkspaceOperationUnavailable
	}
	var r workspaceToolRequest
	if operatortool.DecodeArguments(action.Arguments, &r) != nil {
		return operatortool.Result{}, errWorkspaceOperationUnavailable
	}
	requirement := workspaceRequirement(string(action.Kind), r, true)
	if action.Kind == "delete_project_action" && action.Status == chat.ActionSucceeded {
		requirement.ResourceKind, requirement.ResourceID = "", ""
	}
	if _, err := operatortool.AuthorizeCurrent(ctx, requirement); err != nil {
		return operatortool.Result{}, err
	}
	data, err := e.server.operatorChat.ConnectionResult(ctx, action.ID)
	if err != nil {
		return operatortool.Result{}, safeWorkspaceError(err)
	}
	return e.actionResult(action, data)
}

func (s *Service) readWorkspaceTool(ctx context.Context, scope nativeScope, name string, r workspaceToolRequest) (json.RawMessage, error) {
	if (strings.Contains(name, "conversation") || name == "list_work_item_references") && s.conversations == nil {
		return nil, errWorkspaceOperationUnavailable
	}
	limit := r.Limit
	if limit == 0 {
		limit = 100
	}
	length := r.Length
	if length == 0 {
		length = 32768
	}
	switch name {
	case "list_workspaces":
		value, err := s.readWorkspaces(ctx, scope, r.WorkItemID, r.State, limit)
		return operatorWorkspaceProjection(value, true, err)
	case "get_workspace":
		value, err := s.readWorkspace(ctx, scope, r.WorkspaceID)
		return operatorWorkspaceProjection(value, false, err)
	case "list_project_conversations", "list_organization_conversations":
		filter := conversationListQuery{Project: scope.project, Limit: limit, Cursor: r.Cursor, Title: r.Query, Settled: r.Settled, SubjectWorkItemID: r.SubjectWorkItemID}
		if name == "list_organization_conversations" && r.ProjectID == "" {
			projects, every, err := s.readableConversationProjects(ctx, scope)
			if err != nil {
				return nil, err
			}
			if !every {
				filter.Projects = projects
			}
		}
		return s.readConversationsPage(ctx, scope, filter)
	case "get_work_item_conversation":
		raw, err := s.readConversationSnapshotFor(ctx, scope, "", r.WorkItemID, true)
		return boundedOperatorHistory(raw, true, err)
	case "get_conversation":
		raw, err := s.readConversationSnapshot(ctx, scope, r.ConversationID)
		return boundedOperatorHistory(raw, true, err)
	case "stream_conversation_events":
		snapshot, err := s.readWorkspaceTool(ctx, scope, "get_conversation", r)
		if err != nil {
			return nil, err
		}
		return json.Marshal(struct {
			Status   string          `json:"status"`
			Snapshot json.RawMessage `json:"snapshot"`
		}{"unsupported_transport", snapshot})
	case "list_conversation_messages":
		raw, err := s.readConversationMessages(ctx, scope, r.ConversationID, r.Before, limit)
		return boundedOperatorHistory(raw, false, err)
	case "get_conversation_attachment":
		attachment, content, err := s.readAttachment(ctx, scope, r.ConversationID, r.AttachmentID)
		if err != nil {
			return nil, err
		}
		return boundedWorkspaceContent(attachment.ID, content, r.Offset, length)
	case "list_work_item_references":
		return s.readWorkItemReferences(ctx, scope, r.WorkItemID)
	case "list_project_actions":
		return s.readActions(ctx, scope)
	case "list_project_action_runs":
		return s.readActionRuns(ctx, scope, r.ActionID, limit)
	case "get_project_action_run", "get_project_action_run_output":
		run, err := s.readActionRun(ctx, scope, r.ActionID, r.RunID)
		if err != nil {
			return nil, err
		}
		if name == "get_project_action_run" {
			return json.Marshal(run.resource())
		}
		return boundedWorkspaceContent(run.ID, []byte(run.Output), r.Offset, length)
	case "list_workspace_terminal_recordings":
		records, err := s.readRecordings(ctx, scope, r.WorkspaceID, "", limit)
		if err != nil {
			return nil, err
		}
		return json.Marshal(records)
	case "get_workspace_terminal_recording":
		recording, err := s.readRecording(ctx, scope, r.WorkspaceID, r.RecordingID)
		if err != nil {
			return nil, err
		}
		return boundedWorkspaceContent(recording.ID, []byte(recording.Cast), r.Offset, length)
	case "workspace_file_list", "workspace_file_read":
		return s.readWorkspaceFilesTool(ctx, scope, name, r)
	case "workspace_terminal":
		workspace, err := s.readWorkspaceTool(ctx, scope, "get_workspace", r)
		if err != nil {
			return nil, err
		}
		return json.Marshal(struct {
			Status      string          `json:"status"`
			Workspace   json.RawMessage `json:"workspace"`
			Alternative string          `json:"alternative"`
		}{"unsupported_transport", workspace, "Use configured actions, action output, attachments, or terminal recordings."})
	}
	return nil, operatortool.ErrUnknownTool
}
func boundedWorkspaceContent(id string, content []byte, offset, length int) (json.RawMessage, error) {
	if offset > len(content) {
		offset = len(content)
	}
	end := min(offset+length, len(content))
	return json.Marshal(struct {
		ID      string `json:"id"`
		Content string `json:"content_base64"`
		Offset  int    `json:"offset"`
		Next    int    `json:"next_offset"`
		Total   int    `json:"total_bytes"`
		More    bool   `json:"has_more"`
	}{id, base64.StdEncoding.EncodeToString(content[offset:end]), offset, end, len(content), end < len(content)})
}
func (s *Service) commandWorkspaceTool(ctx context.Context, scope nativeScope, name string, r workspaceToolRequest) (json.RawMessage, error) {
	if (strings.Contains(name, "conversation")) && s.conversations == nil {
		return nil, errWorkspaceOperationUnavailable
	}
	switch name {
	case "create_workspace":
		var input workspaceRequest
		if operatortool.DecodeArguments(r.Input, &input) != nil {
			return nil, operatortool.ErrInvalidArguments
		}
		input.IdempotencyKey = r.RequestID
		value, err := s.commandCreateWorkspace(ctx, scope, input)
		return operatorWorkspaceProjection(value, false, err)
	case "delete_workspace":
		return s.commandCloseWorkspace(ctx, scope, r.WorkspaceID)
	case "create_conversation":
		var input conversationCreateRequest
		if operatortool.DecodeArguments(r.Input, &input) != nil {
			return nil, operatortool.ErrInvalidArguments
		}
		input.Key = r.RequestID
		if input.FirstMessage != nil {
			input.FirstMessage.Key = r.RequestID
		}
		return s.commandCreateConversation(ctx, scope, input)
	case "post_conversation_command":
		var input conversation.Command
		if operatortool.DecodeArguments(r.Input, &input) != nil {
			return nil, operatortool.ErrInvalidArguments
		}
		input.Key = r.RequestID
		return s.commandPostConversation(ctx, scope, r.ConversationID, input)
	case "link_conversation":
		var input conversationLinkRequest
		if operatortool.DecodeArguments(r.Input, &input) != nil {
			return nil, operatortool.ErrInvalidArguments
		}
		input.Key = r.RequestID
		return s.commandLinkConversation(ctx, scope, r.ConversationID, input)
	case "patch_conversation":
		var input conversationPatchRequest
		if operatortool.DecodeArguments(r.Input, &input) != nil {
			return nil, operatortool.ErrInvalidArguments
		}
		return s.commandPatchConversation(ctx, scope, r.ConversationID, input)
	case "upload_conversation_attachment":
		var input operatortool.ConversationAttachmentArguments
		if operatortool.DecodeArguments(r.Input, &input) != nil {
			return nil, operatortool.ErrInvalidArguments
		}
		content, err := base64.StdEncoding.DecodeString(input.Content)
		if err != nil || len(content) > 32768 {
			return nil, operatortool.ErrInvalidArguments
		}
		return s.commandUploadAttachment(ctx, scope, r.ConversationID, conversationAttachmentUpload{key: r.RequestID, name: input.Name, media: input.MIME, content: content})
	case "delete_conversation_attachment":
		return s.commandDeleteAttachment(ctx, scope, r.ConversationID, r.AttachmentID)
	case "create_project_action":
		var input projectActionRequest
		if operatortool.DecodeArguments(r.Input, &input) != nil {
			return nil, operatortool.ErrInvalidArguments
		}
		input.IdempotencyKey = r.RequestID
		return s.commandCreateAction(ctx, scope, input)
	case "patch_project_action":
		var input projectActionPatch
		if operatortool.DecodeArguments(r.Input, &input) != nil {
			return nil, operatortool.ErrInvalidArguments
		}
		input.IdempotencyKey = r.RequestID
		return s.commandPatchAction(ctx, scope, r.ActionID, input)
	case "delete_project_action":
		return s.commandDeleteAction(ctx, scope, r.ActionID)
	case "create_project_action_run":
		return s.commandRunAction(ctx, scope, r.ActionID, projectActionRunRequest{Mutation: tracker.Mutation{IdempotencyKey: r.RequestID}, WorkspaceID: r.WorkspaceID, ExpectedActionRevision: r.ExpectedRevision})
	}
	return nil, operatortool.ErrUnknownTool
}

func operatorWorkspaceProjection(raw json.RawMessage, list bool, err error) (json.RawMessage, error) {
	if err != nil {
		return nil, err
	}
	project := func(w *workspacesession.Session) { w.WorktreePath = ""; w.Worktree = ""; w.RelaySessions = nil }
	if list {
		var page workspaceListResponse
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, err
		}
		for i := range page.Workspaces {
			project(&page.Workspaces[i])
		}
		return json.Marshal(page)
	}
	var w workspacesession.Session
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, err
	}
	project(&w)
	return json.Marshal(w)
}

// Keep newest messages and preserve the cursor for the omitted older rows.
// The budget leaves room for freshness and the transport wrapper.
func boundedOperatorHistory(raw json.RawMessage, snapshot bool, err error) (json.RawMessage, error) {
	if err != nil {
		return nil, err
	}
	const budget = operatortool.MaxResultBytes / 2
	if len(raw) <= budget {
		return raw, nil
	}
	if snapshot {
		var page struct {
			conversationSnapshot
			HistoryOmitted bool `json:"history_omitted,omitempty"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, err
		}
		for len(page.Messages) > 1 && len(raw) > budget {
			page.Messages = page.Messages[1:]
			page.HasMore, _ = conversationOlderCursor(page.Messages)
			raw, err = json.Marshal(page)
			if err != nil {
				return nil, err
			}
		}
		if len(raw) > budget && len(page.Messages) == 1 {
			page.Messages = []conversationMessageResource{}
			page.HasMore, page.HistoryOmitted = true, true
			raw, err = json.Marshal(page)
			if err != nil {
				return nil, err
			}
		}
	} else {
		var page conversationMessagesPage
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, err
		}
		for len(page.Messages) > 1 && len(raw) > budget {
			page.Messages = page.Messages[1:]
			_, page.NextCursor = conversationOlderCursor(page.Messages)
			raw, err = json.Marshal(page)
			if err != nil {
				return nil, err
			}
		}
	}
	if len(raw) > budget {
		return nil, errWorkspaceOperationUnavailable
	}
	return raw, nil
}
