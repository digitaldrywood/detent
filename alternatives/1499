package hubserver

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/chat"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) bindCoordinatorConnection(ctx context.Context, key string) (context.Context, error) {
	connection := operatortool.CurrentConnection(ctx)
	if s.config.Hosted == nil || !connection.Identity.Valid() || key == "" {
		return ctx, nil
	}
	origin := cmp.Or(connection.Identity.SessionID, connection.Identity.CredentialID)
	if entryOrigin, ok := ctx.Value(operatorEntryOriginKey{}).(string); ok {
		if entryOrigin == "" {
			return ctx, operatortool.ErrAccessDenied
		}
		connection.Origin = entryOrigin
		origin += ":" + entryOrigin
	}
	digest := sha256.Sum256([]byte(origin + ":" + connection.Identity.PrincipalID + ":" + key))
	connection.ID = "luna_" + hex.EncodeToString(digest[:])
	connection.Client = "Luna"
	ctx = operatortool.WithConnection(ctx, connection)
	return ctx, s.operatorChat.AttachConnection(ctx)
}

func coordinatorConnectionData(ctx context.Context) json.RawMessage {
	connection := operatortool.CurrentConnection(ctx)
	if connection.Client != "Luna" {
		return nil
	}
	raw, err := json.Marshal(map[string]string{"operator_connection": connection.ID, "operator_principal": connection.Identity.PrincipalID})
	if err != nil {
		return nil
	}
	return raw
}

func coordinatorActionTools() []runner.AgentTool {
	return []runner.AgentTool{
		coordinatorTool("get_project_integration", "Read this project's integration settings and GitHub transport availability. Runner PR landing is selected by the approved repository policy.", `{"type":"object","properties":{},"additionalProperties":false}`),
		coordinatorTool("update_project_integration", "Preview Hub integration or Cloud-owned workflow Markdown changes (lanes, transitions and dispatch). Repository-owned definitions require a reviewed source change and policy approval. This does not change the runner's approved PR landing policy. The client controls confirmation and submits the proposed call.", `{"type":"object","properties":{"workflow_markdown":{"type":"string","maxLength":131072},"repository_enabled":{"type":"boolean"},"intake":{"type":"string","enum":["disabled","manual"]},"projection":{"type":"string","enum":["disabled","summary"]}},"additionalProperties":false}`),
		coordinatorTool("move_item", "Preview moving a native issue to a workflow state. Retry a blocked issue by moving it to Todo. When leaving Blocked after a human_action question, supply reason_detail quoting the human's chosen answer; it is recorded as an item comment and transition reason. Ask for the answer if missing. Current workflow rules and revision apply; the client controls confirmation and submission.", `{"type":"object","required":["work_item_id","state"],"properties":{"work_item_id":{"type":"string"},"state":{"type":"string"},"reason_detail":{"type":"string"}},"additionalProperties":false}`),
		coordinatorTool("edit_item", "Preview editing an issue's title, body, labels or priority (0 urgent through 3 low). The client controls confirmation and submission.", `{"type":"object","required":["work_item_id"],"properties":{"work_item_id":{"type":"string"},"title":{"type":"string"},"body":{"type":"string"},"labels":{"type":"array","items":{"type":"string"}},"priority":{"type":"integer","minimum":0,"maximum":3}},"additionalProperties":false}`),
		coordinatorTool(string(chat.ActionArchiveItems), "Preview archiving a set of native issues in this project. The client controls confirmation and submission. Running and Merging issues are refused. Archived issues leave the board and dispatch and can be restored.", `{"type":"object","required":["work_item_ids"],"properties":{"work_item_ids":{"type":"array","minItems":1,"maxItems":50,"uniqueItems":true,"items":{"type":"string"}}},"additionalProperties":false}`),
		coordinatorTool("add_comment", "Preview adding an issue comment. The client controls confirmation and submits the exact comment.", `{"type":"object","required":["work_item_id","body"],"properties":{"work_item_id":{"type":"string"},"body":{"type":"string"}},"additionalProperties":false}`),
	}
}

type coordinatorActionArguments struct {
	WorkflowMarkdown  *string   `json:"workflow_markdown"`
	WorkItemID        string    `json:"work_item_id"`
	State             string    `json:"state"`
	ReasonDetail      string    `json:"reason_detail"`
	Title             *string   `json:"title"`
	Body              *string   `json:"body"`
	Labels            *[]string `json:"labels"`
	Priority          *int      `json:"priority"`
	RepositoryEnabled *bool     `json:"repository_enabled"`
	Intake            *string   `json:"intake"`
	Projection        *string   `json:"projection"`
}

type coordinatorWorkArguments struct {
	operatortool.WorkArguments
	ReasonDetail string `json:"reason_detail,omitempty"`
}

func (t *coordinatorToolset) actionContext(ctx context.Context, record conversationRecord) (context.Context, error) {
	if len(t.state.users) == 0 {
		return nil, operatortool.ErrAccessDenied
	}
	message := t.state.users[len(t.state.users)-1]
	var data struct {
		ConnectionID string `json:"operator_connection"`
		PrincipalID  string `json:"operator_principal"`
	}
	if json.Unmarshal(message.Data, &data) != nil || data.ConnectionID == "" {
		return nil, operatortool.ErrAccessDenied
	}
	if age := t.coordinator.service.server.config.now().Sub(message.CreatedAt); age >= 0 && age <= coordinatorTurnTimeout {
		ctx = context.WithValue(ctx, operatorAccessTimeKey{}, message.CreatedAt)
	}
	service := t.coordinator.service.server.operatorChat
	if err := service.RestoreConnection(ctx, data.ConnectionID); err != nil {
		return nil, err
	}
	return service.OriginatingContext(ctx, data.ConnectionID, data.PrincipalID, string(record.OrganizationID))
}

func (t *coordinatorToolset) projectAction(ctx context.Context, record conversationRecord, call runner.AgentToolCall, requireConfirmation ...bool) (any, error) {
	s := t.coordinator.service.server
	ctx, err := t.actionContext(ctx, record)
	if err != nil {
		return nil, err
	}
	requirement := operatortool.Requirement{Scope: apikey.ScopeWrite, ProjectID: string(record.ProjectID)}
	if call.Name == "get_project_integration" || call.Name == "update_project_integration" {
		requirement.Scope = apikey.ScopeAdmin
		if call.Name == "get_project_integration" {
			requirement.Scope = apikey.ScopeRead
		}
	}
	ctx, err = operatortool.AuthorizeCurrent(ctx, requirement)
	if err != nil {
		return nil, err
	}
	resolve, ok := ctx.Value(nativeOperatorScopeKey{}).(func(context.Context) (nativeScope, error))
	if !ok {
		return nil, operatortool.ErrAccessDenied
	}
	scope, err := resolve(ctx)
	if err != nil {
		return nil, operatortool.ErrAccessDenied
	}
	scope.project = record.ProjectID
	var args coordinatorActionArguments
	limit := coordinatorToolArgumentBytes
	if call.Name == "update_project_integration" {
		limit = coordinatorWorkflowArgumentBytes
	}
	if err := decodeCoordinatorArgumentsWithLimit(call.Arguments, &args, limit); err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(call.Arguments, &fields) != nil {
		return nil, operatortool.ErrInvalidArguments
	}
	var allowed []string
	switch call.Name {
	case "get_project_integration":
		allowed = []string{}
	case "update_project_integration":
		allowed = []string{"repository_enabled", "intake", "projection", "workflow_markdown"}
	case operatortool.MoveItem:
		allowed = []string{"work_item_id", "state", "reason_detail"}
	case operatortool.EditItem:
		allowed = []string{"work_item_id", "title", "body", "labels", "priority"}
	case operatortool.AddComment:
		allowed = []string{"work_item_id", "body"}
	default:
		return nil, operatortool.ErrUnknownTool
	}
	for key, value := range fields {
		if !slices.Contains(allowed, key) || string(value) == "null" {
			return nil, operatortool.ErrInvalidArguments
		}
	}
	requestID := coordinatorActionRequestID(t.state.users[len(t.state.users)-1].ID, call)
	action := chat.Action{RequiresConfirmation: len(requireConfirmation) > 0 && requireConfirmation[0], ConversationID: record.ID, Kind: chat.ActionKind(call.Name), ProjectID: string(record.ProjectID), RequestID: requestID, Title: strings.ReplaceAll(call.Name, "_", " "), Material: true}
	if call.Name == "get_project_integration" || call.Name == "update_project_integration" {
		current, err := s.projectIntegration(ctx, s.database.reader, scope)
		if err != nil {
			return nil, err
		}
		if call.Name == "get_project_integration" {
			return current, nil
		}
		if len(fields) == 0 {
			return nil, operatortool.ErrInvalidArguments
		}
		if (args.RepositoryEnabled != nil || args.Intake != nil || args.Projection != nil) && !*current.GitHubTransportAvailable {
			return nil, nativeInvalid("Hub GitHub integration is unavailable. Associate the runner checkout in project setup and approve the runner's repository policy with GitHub PR landing enabled.")
		}
		input := operatortool.IntegrationInput{WorkflowMarkdown: args.WorkflowMarkdown, ExpectedRevision: current.Revision, Intake: current.Intake, Projection: current.Projection, RepositoryEnabled: current.RepositoryEnabled}
		if args.WorkflowMarkdown != nil {
			if current.Authority["workflow"] == "repository" {
				return nil, nativeWorkflowInvalid("Workflow is controlled by the repository; propose a reviewed definition change, then approve its trusted policy revision and verify runner adoption")
			}
			workflow, err := workflowconfig.ParseProjectDefinition(workflowconfig.ProjectDefinitionSources{WorkflowPath: "Cloud WORKFLOW.md", Workflow: []byte(*args.WorkflowMarkdown)})
			if err != nil {
				return nil, nativeWorkflowInvalid(err.Error())
			}
			if workflow.Config.Tracker.Kind != workflowconfig.TrackerHubNative {
				return nil, nativeWorkflowInvalid("Cloud workflow must use tracker.kind hub_native")
			}
			if err := workflow.Config.Validate(); err != nil {
				return nil, nativeWorkflowInvalid(err.Error())
			}
			if err := workflow.Config.ValidateNativeWorkflow(); err != nil {
				return nil, nativeWorkflowInvalid(err.Error())
			}
		}
		if current.Intake == "automatic" {
			input.Intake = ""
		}
		if args.RepositoryEnabled != nil {
			input.RepositoryEnabled = *args.RepositoryEnabled
		}
		if args.Intake != nil {
			input.Intake = *args.Intake
		}
		if args.Projection != nil {
			input.Projection = *args.Projection
		}
		action.Arguments, err = json.Marshal(map[string]any{"project_id": action.ProjectID, "request_id": requestID, "input": input})
		if err != nil {
			return nil, err
		}
		if _, err := (hubProjectExecutor{s}).command(ctx, operatortool.Call{Name: call.Name, Arguments: action.Arguments}, projectCommandPreview); err != nil {
			return nil, err
		}
		proposedIntake := cmp.Or(input.Intake, current.Intake)
		action.Description = fmt.Sprintf("Hub repository integration: %t → %t; intake: %s → %s; projection: %s → %s (revision %d).", current.RepositoryEnabled, input.RepositoryEnabled, current.Intake, proposedIntake, current.Projection, input.Projection, current.Revision)
		if args.WorkflowMarkdown != nil {
			diff, err := coordinatorSettingsDiff(map[string]any{"workflow_markdown": current.WorkflowMarkdown}, map[string]any{"workflow_markdown": *args.WorkflowMarkdown})
			if err != nil {
				return nil, err
			}
			action.Description += "\n" + diff
		}
	} else {
		if args.WorkItemID == "" || len(args.WorkItemID) > 256 {
			return nil, operatortool.ErrInvalidArguments
		}
		current, err := s.nativeWorkObservation(ctx, scope, args.WorkItemID)
		if err != nil {
			return nil, err
		}
		if current.Profile != "native" {
			return nil, nativeInvalid("Chat changes require a native issue")
		}
		action.IssueID, action.Identifier, action.CurrentState, action.Revision = args.WorkItemID, args.WorkItemID, current.State, int64(current.Revision)
		request := coordinatorWorkArguments{WorkArguments: operatortool.WorkArguments{ProjectID: action.ProjectID, Identifier: args.WorkItemID, ExpectedRevision: int64(current.Revision), Title: args.Title, Body: args.Body, Labels: args.Labels, Priority: args.Priority}}
		if call.Name == operatortool.MoveItem {
			if strings.TrimSpace(args.State) == "" || len(args.State) > 256 {
				return nil, operatortool.ErrInvalidArguments
			}
			action.TargetState = args.State
			request.State = args.State
			request.ReasonDetail = strings.TrimSpace(args.ReasonDetail)
			if err := s.validateCoordinatorMoveAnswer(ctx, scope, current, request.State, request.ReasonDetail); err != nil {
				return nil, err
			}
		} else {
			raw, err := json.Marshal(request)
			if err != nil {
				return nil, err
			}
			if call.Name == operatortool.AddComment {
				request.ExpectedRevision = 0
				raw, err = json.Marshal(request)
				if err != nil {
					return nil, err
				}
			}
			if _, err := operatortool.DecodeWorkArguments(call.Name, raw); err != nil {
				return nil, err
			}
		}
		action.Arguments, err = json.Marshal(request)
		if err != nil {
			return nil, err
		}
		action.Description = fmt.Sprintf("Issue %s: %s (revision %d).", current.Title, current.State, current.Revision)
		if call.Name == operatortool.MoveItem {
			action.Description = fmt.Sprintf("Move %s from %s to %s.", current.Title, current.State, args.State)
			if request.ReasonDetail != "" {
				action.Description += "\nRecord the human's answer on the item: " + request.ReasonDetail
			}
		}
	}
	return t.submitCoordinatorAction(ctx, record, call, action)
}

func coordinatorActionRequestID(messageID string, call runner.AgentToolCall) string {
	digest := sha256.Sum256([]byte(messageID + ":" + call.Name + ":" + string(call.Arguments)))
	return "luna_" + hex.EncodeToString(digest[:])
}

func (s *Service) validateCoordinatorMoveAnswer(ctx context.Context, scope nativeScope, issue tracker.NativeIssue, target, answer string) error {
	if !strings.EqualFold(issue.State, "Blocked") || strings.EqualFold(target, issue.State) || strings.TrimSpace(answer) != "" {
		return nil
	}
	var humanAction bool
	err := s.database.reader.QueryRowContext(ctx, `SELECT COALESCE(json_extract(data_json, '$.disposition.human_action'), 0)
FROM native_attempts WHERE organization_id = ? AND project_id = ? AND work_item_id = ?
AND json_extract(data_json, '$.disposition.status') = 'blocked' ORDER BY fencing_token DESC LIMIT 1`, scope.organization, scope.project, issue.WorkItemID).Scan(&humanAction)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if humanAction {
		return &operatortool.RequestError{Code: "invalid_request", Message: "Provide the human's answer to the recorded human_action question in reason_detail before moving this item out of Blocked"}
	}
	return nil
}

func (t *coordinatorToolset) submitCoordinatorAction(ctx context.Context, record conversationRecord, call runner.AgentToolCall, action chat.Action) (any, error) {
	if action.RequestID == "" {
		action.RequestID = coordinatorActionRequestID(t.state.users[len(t.state.users)-1].ID, call)
	}
	identity := operatortool.ConnectionIdentity(ctx)
	action.Mutation = mutation.Metadata{PrincipalID: identity.PrincipalID, OrganizationID: identity.OrganizationID, ProjectID: action.ProjectID, ResourceID: action.IssueID, Action: call.Name, Source: "chat", CorrelationID: newNativeID("luna")}
	var err error
	action.Mutation, err = action.Mutation.Bind(action.RequestID, action.Arguments)
	if err != nil {
		return nil, err
	}
	if action.Description == "" {
		action.Description = action.Title
	}
	action.OrganizationID = string(record.OrganizationID)
	action.ConversationID = record.ID
	action.ID = action.RequestID
	action.Agent = coordinatorAgent(t.state.model)
	if err := t.coordinator.write(ctx, record.ID, func(ctx context.Context, tx *sql.Tx, record *conversationRecord, now time.Time) error {
		data, err := json.Marshal(map[string]any{"operator_action": map[string]any{"action": action, "conversation_id": record.ID}})
		if err != nil {
			return err
		}
		message := conversationMessageRecord{Role: conversation.RoleSystem, Kind: conversation.MessageStatus, Text: action.Description, Data: data, Delivery: conversation.DeliveryCompleted, Actor: conversation.Actor{Kind: conversation.ActorCoordinator}}
		return t.coordinator.service.appendMessage(ctx, tx, record, &message, now)
	}); err != nil {
		return nil, err
	}
	return map[string]any{"action_id": action.ID, "preview": action.Arguments, "status": "proposed", "message": "The client received the proposed call. It controls confirmation and submits the call; do not claim execution succeeded."}, nil
}

func (s *Service) executeCoordinatorAction(ctx context.Context, action chat.Action) (chat.ActionExecution, error) {
	if action.ConversationID == "" {
		return chat.ActionExecution{}, operatortool.ErrAccessDenied
	}
	ctx, err := s.authorizeCoordinatorAction(ctx, action)
	if err != nil {
		return chat.ActionExecution{}, err
	}
	agent, err := s.coordinatorProposalAgent(ctx, action)
	if err != nil {
		return chat.ActionExecution{}, err
	}
	ctx = withCoordinatorAgent(ctx, agent)
	if coordinatorCatalogWrite(string(action.Kind)) {
		return s.executeCoordinatorCatalogCall(ctx, action)
	}
	ctx = mutation.WithContext(ctx, action.Mutation)
	if coordinatorSettingsTool(string(action.Kind)) {
		return s.executeCoordinatorSettingsAction(ctx, action)
	}
	if action.Kind == chat.ActionArchiveItems {
		return s.executeCoordinatorIssueArchive(ctx, action)
	}
	if action.Kind == chat.ActionIssueSplit {
		return s.executeCoordinatorIssueSplit(ctx, action)
	}
	if string(action.Kind) == "set_runner_tier" || string(action.Kind) == "convert_runner_scope" {
		return s.executeCoordinatorRunnerAction(ctx, action)
	}
	if string(action.Kind) == "promote_sprites_token" {
		return s.executePromoteSpritesToken(ctx, action)
	}
	if coordinatorSpriteMutation(string(action.Kind)) {
		return s.executeCoordinatorSpriteAction(ctx, action)
	}
	if coordinatorWorkflowMutation(string(action.Kind)) {
		return s.executeCoordinatorWorkflowAction(ctx, action)
	}
	if string(action.Kind) == "update_project_integration" {
		_, err := (hubProjectExecutor{s}).command(ctx, operatortool.Call{Name: string(action.Kind), Arguments: action.Arguments}, projectCommandExecute)
		if err != nil {
			return chat.ActionExecution{}, err
		}
		return chat.ActionExecution{Message: "Integration updated."}, nil
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
	if action.Kind == chat.ActionStopRun {
		return s.executeCoordinatorStopRun(ctx, scope, action)
	}
	if action.Kind == chat.ActionFileIssue {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(action.Arguments, &fields); err != nil {
			return chat.ActionExecution{}, err
		}
		delete(fields, "request_id")
		raw, err := json.Marshal(fields)
		if err != nil {
			return chat.ActionExecution{}, err
		}
		request, err := operatortool.DecodeFileIssue(raw)
		if err != nil || request.ProjectID != action.ProjectID {
			return chat.ActionExecution{}, operatortool.ErrInvalidArguments
		}
		var priority *int
		if request.Priority != nil {
			priority = new(*request.Priority - 1)
		}
		result, err := s.operatorCreateNativeWork(ctx, scope, tracker.CreateIssue{Mutation: tracker.MutationForContext(ctx, action.RequestID), Title: request.Title, Body: request.Description, State: request.State, Labels: request.Labels, Priority: priority})
		if err != nil {
			return chat.ActionExecution{}, coordinatorActionError(err)
		}
		return chat.ActionExecution{Message: "Issue filed.", Data: result}, nil
	}
	var request coordinatorWorkArguments
	if err := operatortool.DecodeArguments(action.Arguments, &request); err != nil {
		return chat.ActionExecution{}, err
	}
	if request.ProjectID != action.ProjectID || request.Identifier != action.IssueID {
		return chat.ActionExecution{}, operatortool.ErrAccessDenied
	}
	if action.Kind == chat.ActionMoveItem {
		_, err = s.transitionNativeIssueCommandWithChat(ctx, scope, request.Identifier, tracker.Transition{Mutation: tracker.MutationForContext(ctx, action.RequestID), ExpectedRevision: tracker.Revision(request.ExpectedRevision), State: request.State, Reason: "user_requested", ReasonDetail: request.ReasonDetail}, action.ConversationID)
	} else {
		if _, err := operatortool.DecodeWorkArguments(string(action.Kind), action.Arguments); err != nil {
			return chat.ActionExecution{}, err
		}
		_, err = s.operatorNativeWork(ctx, scope, string(action.Kind), request.WorkArguments)
	}
	if err != nil {
		return chat.ActionExecution{}, coordinatorActionError(err)
	}
	message := "Issue updated."
	if action.Kind == chat.ActionMoveItem {
		message = "Moved issue to " + request.State + "."
		if strings.TrimSpace(request.ReasonDetail) != "" {
			message += " Recorded the human's answer on the item as a comment and transition reason."
		}
	} else if string(action.Kind) == operatortool.AddComment {
		message = "Comment added."
	}
	return chat.ActionExecution{Message: message, ResourceID: action.IssueID}, nil
}

func (s *Service) authorizeCoordinatorAction(ctx context.Context, action chat.Action) (context.Context, error) {
	requirement := operatortool.Requirement{Scope: apikey.ScopeWrite, ProjectID: action.ProjectID, ResourceKind: "work_item", ResourceID: action.IssueID}
	if coordinatorWorkflowMutation(string(action.Kind)) || string(action.Kind) == "set_runner_tier" || string(action.Kind) == "convert_runner_scope" || string(action.Kind) == "promote_sprites_token" || string(action.Kind) == "update_project_integration" || coordinatorSpriteMutation(string(action.Kind)) {
		requirement.Scope, requirement.ResourceKind, requirement.ResourceID = apikey.ScopeAdmin, "", ""
	}
	if coordinatorSettingsTool(string(action.Kind)) {
		requirement = coordinatorSettingsRequirement(string(action.Kind), action.ProjectID)
	}
	if coordinatorCatalogWrite(string(action.Kind)) {
		requirement = coordinatorCatalogRequirement(action)
	}
	if action.Kind == chat.ActionArchiveItems || action.Kind == chat.ActionFileIssue {
		requirement.ResourceKind = ""
	}
	ctx, err := operatortool.AuthorizeCurrent(ctx, requirement)
	if err != nil {
		return nil, err
	}
	record, err := s.conversations.store.readConversation(ctx, s.database.reader, tracker.OrganizationID(action.OrganizationID), tracker.ProjectID(action.ProjectID), action.ConversationID)
	if err != nil {
		return nil, operatortool.ErrAccessDenied
	}
	if !coordinatorHandles(record) {
		return nil, nativeInvalid("This chat is linked; request the change in a project chat")
	}
	return ctx, nil
}

func (t *coordinatorToolset) postActionRefusal(ctx context.Context, name string, failure error) error {
	return t.coordinator.write(ctx, t.state.conversationID, func(ctx context.Context, tx *sql.Tx, record *conversationRecord, now time.Time) error {
		message := conversationMessageRecord{Role: conversation.RoleAssistant, Kind: conversation.MessageText, Text: strings.ReplaceAll(name, "_", " ") + " refused: " + boundRunes(failure.Error(), coordinatorErrorRunes), Delivery: conversation.DeliveryCompleted, Actor: conversation.Actor{Kind: conversation.ActorCoordinator}}
		return t.coordinator.service.appendMessage(ctx, tx, record, &message, now)
	})
}

func coordinatorActionError(err error) error {
	var request *operatortool.RequestError
	if errors.As(err, &request) {
		return &nativeError{Code: cmp.Or(request.Code, "invalid_request"), Message: request.Message, status: http.StatusUnprocessableEntity, publicMessage: true}
	}
	var conflict *operatortool.ConflictError
	if errors.As(err, &conflict) {
		message := conflict.Message
		if message == "" && conflict.Code == "revision_conflict" {
			message = fmt.Sprintf("The item changed after this was proposed; its current revision is %d. Ask for a fresh proposal.", conflict.CurrentRevision)
		} else if message == "" {
			message = "The item changed after this was proposed. Ask for a fresh proposal."
		}
		return &nativeError{Code: conflict.Code, Message: message, CurrentRevision: tracker.Revision(conflict.CurrentRevision), status: http.StatusConflict, publicMessage: true}
	}
	var native *nativeError
	if errors.As(err, &native) {
		return errors.New(native.Message)
	}
	if errors.Is(err, operatortool.ErrAccessDenied) || errors.Is(err, operatortool.ErrInvalidArguments) || errors.Is(err, mutation.ErrConflict) || errors.Is(err, errCoordinatorToolArguments) || errors.Is(err, errCoordinatorProjectUnreadable) {
		return err
	}
	return errors.New("project change is unavailable")
}

func (s *Service) publishCoordinatorDecision(ctx context.Context, action chat.Action) error {
	if action.ConversationID == "" || action.Mutation.Source != "chat" {
		return nil
	}
	coordinator, ok := s.conversations.coordinator.(*conversationTurnCoordinator)
	if !ok {
		return operatortool.ErrAccessDenied
	}
	return coordinator.write(ctx, action.ConversationID, func(ctx context.Context, tx *sql.Tx, record *conversationRecord, now time.Time) error {
		if string(record.ProjectID) != action.ProjectID || string(record.OrganizationID) != action.OrganizationID {
			return operatortool.ErrAccessDenied
		}
		var posted bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM conversation_messages WHERE conversation_id=? AND json_extract(data_json, '$.operator_action_result')=?)", record.ID, action.ID).Scan(&posted); err != nil {
			return err
		}
		if posted {
			return nil
		}
		data, err := json.Marshal(map[string]string{"operator_action_result": action.ID})
		if err != nil {
			return err
		}
		text := fmt.Sprintf("%s: %s. %s", action.Title, action.Status, action.Result)
		message := conversationMessageRecord{Role: conversation.RoleAssistant, Kind: conversation.MessageText, Text: text, Data: data, Delivery: conversation.DeliveryCompleted, Actor: conversation.Actor{Kind: conversation.ActorCoordinator}}
		return s.conversations.appendMessage(ctx, tx, record, &message, now)
	})
}

func (s *Service) postConversationAction(c echo.Context) error {
	var action chat.Action
	if err := decodeAPIJSON(c, &action); err != nil {
		return invalidAPIRequest(c, err)
	}
	scope := nativeRequestScope(c)
	ctx := c.Request().Context()
	readCtx, cancelRead := context.WithTimeout(ctx, hubReadTimeout)
	defer cancelRead()
	record, err := s.conversations.store.readConversation(readCtx, s.database.reader, scope.organization, scope.project, c.Param("conversation"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if err := s.conversations.authorizeWrite(readCtx, s.database.reader, scope, record); err != nil {
		return s.nativeAPIError(c, err)
	}
	if (!coordinatorSettingsTool(string(action.Kind)) && !coordinatorWorkflowMutation(string(action.Kind)) && !coordinatorCatalogWrite(string(action.Kind)) && !slices.Contains([]string{operatortool.StopRun, operatortool.FileIssue, operatortool.MoveItem, operatortool.EditItem, operatortool.AddComment, "update_project_integration", "set_runner_tier", "convert_runner_scope", "promote_sprites_token", "set_sprite_pool", "scale_up_sprite_pool", string(chat.ActionIssueSplit), string(chat.ActionArchiveItems)}, string(action.Kind))) || action.ProjectID != string(scope.project) || action.RequestID == "" || len(action.RequestID) > 128 {
		return invalidAPIRequest(c, operatortool.ErrInvalidArguments)
	}
	identity := operatortool.ConnectionIdentity(ctx)
	action.ConversationID, action.OrganizationID = record.ID, string(scope.organization)
	action.ID = action.RequestID
	action.Mutation = mutation.Metadata{PrincipalID: identity.PrincipalID, OrganizationID: identity.OrganizationID, ProjectID: action.ProjectID, ResourceID: action.IssueID, Action: string(action.Kind), Source: "chat", CorrelationID: newNativeID("luna")}
	action.Mutation, err = action.Mutation.Bind(action.RequestID, action.Arguments)
	if err != nil {
		return invalidAPIRequest(c, err)
	}
	result, err := s.executeCoordinatorAction(ctx, action)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	action.Status, action.Result = chat.ActionSucceeded, result.Message
	if err := s.publishCoordinatorDecision(ctx, action); err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, result)
}
