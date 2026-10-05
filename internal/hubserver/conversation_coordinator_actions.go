package hubserver

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) bindCoordinatorConnection(ctx context.Context, key string) (context.Context, error) {
	connection := operatortool.CurrentConnection(ctx)
	if s.config.Hosted == nil || connection.Identity.SessionID == "" || key == "" {
		return ctx, nil
	}
	digest := sha256.Sum256([]byte(connection.Identity.SessionID + ":" + connection.Identity.PrincipalID + ":" + key))
	connection.ID = "luna_" + hex.EncodeToString(digest[:])
	connection.Client = "Luna"
	connection.RequireConfirmation = true
	ctx = operatortool.WithConnection(ctx, connection)
	return ctx, s.operatorChat.AttachConnection(ctx)
}

func coordinatorConnectionData(ctx context.Context) json.RawMessage {
	connection := operatortool.CurrentConnection(ctx)
	if !connection.RequireConfirmation {
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
		coordinatorTool("update_project_integration", "Preview available Hub integration changes. This does not change the runner's approved PR landing policy. The user must approve the exact change in chat before it runs.", `{"type":"object","properties":{"repository_enabled":{"type":"boolean"},"intake":{"type":"string","enum":["disabled","manual"]},"projection":{"type":"string","enum":["disabled","summary"]}},"additionalProperties":false}`),
		coordinatorTool("move_item", "Preview moving a native issue to a workflow state. Retry a blocked issue by moving it to Todo. Current workflow rules and revision apply; the user must approve in chat.", `{"type":"object","required":["work_item_id","state"],"properties":{"work_item_id":{"type":"string"},"state":{"type":"string"}},"additionalProperties":false}`),
		coordinatorTool("edit_item", "Preview editing an issue's title, body, labels or priority (0 urgent through 3 low). The user must approve in chat.", `{"type":"object","required":["work_item_id"],"properties":{"work_item_id":{"type":"string"},"title":{"type":"string"},"body":{"type":"string"},"labels":{"type":"array","items":{"type":"string"}},"priority":{"type":"integer","minimum":0,"maximum":3}},"additionalProperties":false}`),
		coordinatorTool(string(chat.ActionArchiveItems), "Preview archiving a set of native issues in this project. Nothing is archived until one explicit chat approval. Running and Merging issues are refused. Archived issues leave the board and dispatch and can be restored.", `{"type":"object","required":["work_item_ids"],"properties":{"work_item_ids":{"type":"array","minItems":1,"maxItems":50,"uniqueItems":true,"items":{"type":"string"}}},"additionalProperties":false}`),
		coordinatorTool("add_comment", "Preview adding an issue comment. The user must approve the exact comment in chat.", `{"type":"object","required":["work_item_id","body"],"properties":{"work_item_id":{"type":"string"},"body":{"type":"string"}},"additionalProperties":false}`),
	}
}

type coordinatorActionArguments struct {
	WorkItemID        string    `json:"work_item_id"`
	State             string    `json:"state"`
	Title             *string   `json:"title"`
	Body              *string   `json:"body"`
	Labels            *[]string `json:"labels"`
	Priority          *int      `json:"priority"`
	RepositoryEnabled *bool     `json:"repository_enabled"`
	Intake            *string   `json:"intake"`
	Projection        *string   `json:"projection"`
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
	return t.coordinator.service.server.operatorChat.OriginatingContext(ctx, data.ConnectionID, data.PrincipalID, string(record.OrganizationID))
}

func (t *coordinatorToolset) projectAction(ctx context.Context, record conversationRecord, call runner.AgentToolCall) (any, error) {
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
	if err := decodeCoordinatorArguments(call.Arguments, &args); err != nil {
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
		allowed = []string{"repository_enabled", "intake", "projection"}
	case operatortool.MoveItem:
		allowed = []string{"work_item_id", "state"}
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
	digest := sha256.Sum256([]byte(t.state.users[len(t.state.users)-1].ID + ":" + call.Name + ":" + string(call.Arguments)))
	requestID := "luna_" + hex.EncodeToString(digest[:])
	action := chat.Action{ConversationID: record.ID, Kind: chat.ActionKind(call.Name), ProjectID: string(record.ProjectID), RequestID: requestID, Title: strings.ReplaceAll(call.Name, "_", " "), Material: true}
	if call.Name == "get_project_integration" || call.Name == "update_project_integration" {
		current, err := s.projectIntegration(ctx, s.database.db, scope)
		if err != nil {
			return nil, err
		}
		if call.Name == "get_project_integration" {
			return current, nil
		}
		if len(fields) == 0 {
			return nil, operatortool.ErrInvalidArguments
		}
		if !*current.GitHubTransportAvailable {
			return nil, nativeInvalid("Hub GitHub integration is unavailable. Associate the runner checkout in project setup and approve the runner's repository policy with GitHub PR landing enabled.")
		}
		input := operatortool.IntegrationInput{ExpectedRevision: current.Revision, Intake: current.Intake, Projection: current.Projection, RepositoryEnabled: current.RepositoryEnabled}
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
		action.Description = fmt.Sprintf("GitHub pull-request mode: %t → %t; intake: %s → %s; projection: %s → %s (revision %d).", current.RepositoryEnabled, input.RepositoryEnabled, current.Intake, input.Intake, current.Projection, input.Projection, current.Revision)
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
		request := operatortool.WorkArguments{ProjectID: action.ProjectID, Identifier: args.WorkItemID, ExpectedRevision: int64(current.Revision), Title: args.Title, Body: args.Body, Labels: args.Labels, Priority: args.Priority}
		if call.Name == operatortool.MoveItem {
			if strings.TrimSpace(args.State) == "" || len(args.State) > 256 {
				return nil, operatortool.ErrInvalidArguments
			}
			action.TargetState = args.State
			request.State = args.State
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
	}
	return t.submitCoordinatorAction(ctx, record, call, action)
}

func (t *coordinatorToolset) submitCoordinatorAction(ctx context.Context, record conversationRecord, call runner.AgentToolCall, action chat.Action) (any, error) {
	if action.RequestID == "" {
		digest := sha256.Sum256([]byte(t.state.users[len(t.state.users)-1].ID + ":" + call.Name + ":" + string(call.Arguments)))
		action.RequestID = "luna_" + hex.EncodeToString(digest[:])
	}
	s := t.coordinator.service.server
	identity := operatortool.ConnectionIdentity(ctx)
	action.Mutation = mutation.Metadata{PrincipalID: identity.PrincipalID, OrganizationID: identity.OrganizationID, ProjectID: action.ProjectID, ResourceID: action.IssueID, Action: call.Name, Source: "chat", CorrelationID: newNativeID("luna")}
	var err error
	action.Mutation, err = action.Mutation.Bind(action.RequestID, action.Arguments)
	if err != nil {
		return nil, err
	}
	previous, replay, err := s.operatorChat.RetryResult(ctx, action.Kind, action.RequestID, action.Arguments)
	if err != nil {
		return nil, err
	}
	if replay {
		action = previous
	} else {
		action, err = s.operatorChat.Submit(ctx, action)
	}
	if err != nil {
		return nil, err
	}
	approvalURL := s.hostedPath("/chat/approval") + "?connection_id=" + action.ConnectionID
	if !replay {
		if err := t.coordinator.write(ctx, record.ID, func(ctx context.Context, tx *sql.Tx, record *conversationRecord, now time.Time) error {
			data, err := json.Marshal(map[string]any{"operator_approval": map[string]string{"url": approvalURL, "action_id": action.ID}})
			if err != nil {
				return err
			}
			message := conversationMessageRecord{Role: conversation.RoleSystem, Kind: conversation.MessageStatus, Text: "Review and approve this change.", Data: data, Delivery: conversation.DeliveryCompleted, Actor: conversation.Actor{Kind: conversation.ActorCoordinator}}
			return t.coordinator.service.appendMessage(ctx, tx, record, &message, now)
		}); err != nil {
			return nil, err
		}
	}
	return map[string]any{"action_id": action.ID, "status": action.Status, "preview": action.Arguments, "approval": "The approval form is shown in chat. Wait for the user's decision; never claim the change has run before approval."}, nil
}

func (s *Service) executeCoordinatorAction(ctx context.Context, action chat.Action) (chat.ActionExecution, error) {
	if action.Mutation.Confirmation != "approved" || action.ConversationID == "" {
		return chat.ActionExecution{}, operatortool.ErrAccessDenied
	}
	ctx, err := s.authorizeCoordinatorAction(ctx, action)
	if err != nil {
		return chat.ActionExecution{}, err
	}
	ctx = mutation.WithContext(ctx, action.Mutation)
	if action.Kind == chat.ActionArchiveItems {
		return s.executeCoordinatorIssueArchive(ctx, action)
	}
	if action.Kind == chat.ActionIssueSplit {
		return s.executeCoordinatorIssueSplit(ctx, action)
	}
	if coordinatorSpriteMutation(string(action.Kind)) {
		return s.executeCoordinatorSpriteAction(ctx, action)
	}
	if string(action.Kind) == "update_project_integration" {
		raw, err := (hubProjectExecutor{s}).command(ctx, operatortool.Call{Name: string(action.Kind), Arguments: action.Arguments}, projectCommandExecute)
		if err != nil {
			return chat.ActionExecution{}, coordinatorActionError(err)
		}
		raw, err = safeProjectCommandResult(string(action.Kind), raw)
		if err != nil {
			return chat.ActionExecution{}, err
		}
		return chat.ActionExecution{Message: "Integration updated: " + string(raw)}, nil
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
	var request operatortool.WorkArguments
	if err := operatortool.DecodeArguments(action.Arguments, &request); err != nil {
		return chat.ActionExecution{}, err
	}
	if request.ProjectID != action.ProjectID || request.Identifier != action.IssueID {
		return chat.ActionExecution{}, operatortool.ErrAccessDenied
	}
	var raw json.RawMessage
	if action.Kind == chat.ActionMoveItem {
		raw, err = s.transitionNativeIssueCommand(ctx, scope, request.Identifier, tracker.Transition{Mutation: tracker.MutationForContext(ctx, action.RequestID), ExpectedRevision: tracker.Revision(request.ExpectedRevision), State: request.State, Reason: "user_requested"})
	} else {
		if _, err := operatortool.DecodeWorkArguments(string(action.Kind), action.Arguments); err != nil {
			return chat.ActionExecution{}, err
		}
		raw, err = s.operatorNativeWork(ctx, scope, string(action.Kind), request)
	}
	if err != nil {
		return chat.ActionExecution{}, coordinatorActionError(err)
	}
	return chat.ActionExecution{Message: strings.ReplaceAll(string(action.Kind), "_", " ") + " succeeded: " + string(raw), ResourceID: action.IssueID}, nil
}

func (s *Service) authorizeCoordinatorAction(ctx context.Context, action chat.Action) (context.Context, error) {
	requirement := operatortool.Requirement{Scope: apikey.ScopeWrite, ProjectID: action.ProjectID, ResourceKind: "work_item", ResourceID: action.IssueID}
	if string(action.Kind) == "update_project_integration" || coordinatorSpriteMutation(string(action.Kind)) {
		requirement.Scope, requirement.ResourceKind, requirement.ResourceID = apikey.ScopeAdmin, "", ""
	}
	if action.Kind == chat.ActionArchiveItems {
		requirement.ResourceKind = ""
	}
	ctx, err := operatortool.AuthorizeCurrent(ctx, requirement)
	if err != nil {
		return nil, err
	}
	record, err := s.conversations.store.readConversation(ctx, s.database.db, tracker.OrganizationID(action.OrganizationID), tracker.ProjectID(action.ProjectID), action.ConversationID)
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
