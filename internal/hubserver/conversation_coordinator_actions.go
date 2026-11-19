package hubserver

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) bindCoordinatorConnection(ctx context.Context, key string) (context.Context, error) {
	connection := operatortool.CurrentConnection(ctx)
	if !connection.Identity.Valid() || key == "" {
		return ctx, nil
	}
	origin := cmp.Or(connection.Identity.SessionID, connection.Identity.CredentialID)
	if entryOrigin, ok := ctx.Value(hubscope.OperatorEntryOriginKey{}).(string); ok {
		if entryOrigin == "" {
			return ctx, operatortool.ErrAccessDenied
		}
		connection.Origin = entryOrigin
		origin += ":" + entryOrigin
	}
	digest := sha256.Sum256([]byte(origin + ":" + connection.Identity.OrganizationID + ":" + connection.Identity.PrincipalID + ":" + key))
	connection.ID = "luna_" + hex.EncodeToString(digest[:])
	connection.Client = "Luna"
	ctx = operatortool.WithConnection(ctx, connection)
	if s.operatorChat == nil {
		return ctx, fmt.Errorf("bind coordinator connection: %w", chat.ErrUnavailable)
	}
	return ctx, s.operatorChat.AttachConnection(ctx)
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
	if age := t.coordinator.server.config.Clock().Sub(message.CreatedAt); age >= 0 && age <= coordinatorTurnTimeout {
		ctx = context.WithValue(ctx, operatorAccessTimeKey{}, message.CreatedAt)
	}
	service := t.coordinator.server.operatorChat
	ctx = context.WithValue(ctx, operatorChatOrganizationKey{}, record.OrganizationID)
	if err := service.RestoreConnection(ctx, data.ConnectionID); err != nil {
		return nil, err
	}
	return service.OriginatingContext(ctx, data.ConnectionID, data.PrincipalID, string(record.OrganizationID))
}

type coordinatorMoveAnswerAttempt struct {
	checked   bool
	attemptID string
}

func (a coordinatorMoveAnswerAttempt) recheck(ctx context.Context, w tenantstore.Writer, issueID int64) error {
	if !a.checked {
		return nil
	}
	current := ""
	attempt, err := w.LatestAttempt(ctx, issueID)
	if err == nil {
		current = attempt.ID
	} else if !errors.Is(err, corestore.ErrNotFound) {
		return err
	}
	if current != a.attemptID {
		return corestore.ErrSerializationFailure
	}
	return nil
}

type coordinatorAnswerComment struct {
	comment corestore.Comment
	version eventstore.CollaborationVersion
	event   eventstore.Event
}

func (s *Service) prepareCoordinatorAnswerComment(ctx context.Context, scope hubscope.NativeScope, item corestore.WorkItem, conversationID string, request tracker.Transition, now time.Time) (*coordinatorAnswerComment, error) {
	content := eventstore.NewContent(s.runDetailStore())
	actor := scope.Actor()
	body := strings.TrimSpace(request.ReasonDetail)
	provenance := tracker.Provenance{Provider: "chat", ExternalID: conversationID + ":" + request.IdempotencyKey, AuthorID: actor.PrincipalID}
	comment := tracker.NativeComment{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: item.NativeID, ID: hubapi.NewID("cmt"), Revision: 1, Sequence: item.EventSequence + 1, Body: body, Actor: actor, Provenance: &provenance, CreatedAt: now, UpdatedAt: now}
	bodyRaw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	bodyRef, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.Organization}, bodyRaw)
	if err != nil {
		return nil, err
	}
	commentRaw, err := json.Marshal(comment)
	if err != nil {
		return nil, err
	}
	version, err := content.PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: item.NativeID, RecordID: comment.ID, Revision: 1}, commentRaw)
	if err != nil {
		return nil, err
	}
	eventRaw, err := json.Marshal(tracker.CollaborationEvent{ID: hubapi.NewID("evt"), OrganizationID: scope.Organization, ProjectID: scope.Project, AggregateType: "issue", AggregateID: item.NativeID, Type: "comment.created", SchemaVersion: 1, Actor: actor, RecordedAt: now, Data: tracker.CollaborationData{Revision: 1, CommentID: comment.ID}})
	if err != nil {
		return nil, err
	}
	event, err := content.PrepareEvent(ctx, scope.Organization, eventRaw)
	if err != nil {
		return nil, err
	}
	sourceKey := provenance.Provider + ":" + provenance.ExternalID
	row := corestore.Comment{OrganizationID: scope.Organization, ID: comment.ID, ProjectID: scope.Project, WorkItemID: item.NativeID, Revision: 1, Sequence: comment.Sequence, BodyObjectKey: bodyRef.PayloadObjectKey, ActorKind: actor.Kind, ActorPrincipalID: actor.PrincipalID, ProvenanceProvider: &provenance.Provider, ProvenanceExternalID: &provenance.ExternalID, SourceKey: &sourceKey, SourceRevision: 1, SourceObservedAt: &now, CreatedAt: now, UpdatedAt: now}
	return &coordinatorAnswerComment{comment: row, version: version, event: event}, nil
}

func (c *coordinatorAnswerComment) write(ctx context.Context, w tenantstore.Writer) error {
	if _, err := w.CreateComment(ctx, c.comment); err != nil {
		return err
	}
	if _, err := w.CreateCollaborationVersion(ctx, c.version); err != nil {
		return err
	}
	_, err := w.AppendEvent(ctx, c.event)
	return err
}

func (s *Service) validateCoordinatorMoveAnswer(ctx context.Context, scope hubscope.NativeScope, issue tracker.NativeIssue, target, answer string) error {
	_, err := s.checkCoordinatorMoveAnswer(ctx, scope, issue, target, answer)
	return err
}

func (s *Service) checkCoordinatorMoveAnswer(ctx context.Context, scope hubscope.NativeScope, issue tracker.NativeIssue, target, answer string) (coordinatorMoveAnswerAttempt, error) {
	if !strings.EqualFold(issue.State, "Blocked") || strings.EqualFold(target, issue.State) || strings.TrimSpace(answer) != "" {
		return coordinatorMoveAnswerAttempt{}, nil
	}
	var attemptID string
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		item, err := r.WorkItem(ctx, scope.Project, issue.WorkItemID)
		if err != nil {
			return err
		}
		attempt, err := r.LatestAttempt(ctx, item.ID)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		attemptID = attempt.ID
		return err
	})
	if err != nil {
		return coordinatorMoveAnswerAttempt{}, err
	}
	humanAction := false
	if attemptID != "" {
		attempt, err := s.readNativeAttemptTenant(ctx, scope, string(issue.WorkItemID), attemptID)
		if err != nil {
			return coordinatorMoveAnswerAttempt{}, err
		}
		humanAction = attempt.Disposition != nil && attempt.Disposition.Status == "blocked" && attempt.Disposition.HumanAction
	}
	if humanAction {
		return coordinatorMoveAnswerAttempt{}, &operatortool.RequestError{Code: "invalid_request", Message: "Provide the human's answer to the recorded human_action question in reason_detail before moving this item out of Blocked"}
	}
	return coordinatorMoveAnswerAttempt{checked: true, attemptID: attemptID}, nil
}

func (s *Service) publishCoordinatorDecision(ctx context.Context, action chat.Action) error {
	if action.ConversationID == "" || action.Mutation.Source != "chat" {
		return nil
	}
	coordinator, ok := s.conversations.Coordinator.(*conversationTurnCoordinator)
	if !ok {
		return operatortool.ErrAccessDenied
	}
	data, err := json.Marshal(map[string]string{"operator_action_result": action.ID})
	if err != nil {
		return err
	}
	text := fmt.Sprintf("%s: %s. %s", action.Title, action.Status, action.Result)
	message := conversationMessageRecord{Role: conversation.RoleAssistant, Kind: conversation.MessageText, Text: text, Data: data, Delivery: conversation.DeliveryCompleted, Actor: conversation.Actor{Kind: conversation.ActorCoordinator}}
	return coordinator.write(ctx, action.ConversationID, func(ctx context.Context, tx tenantstore.Writer, record *conversationRecord, now time.Time) error {
		if string(record.ProjectID) != action.ProjectID || string(record.OrganizationID) != action.OrganizationID {
			return operatortool.ErrAccessDenied
		}
		page, err := tx.FilteredMessages(ctx, conversationstore.MessageFilter{ConversationID: record.ID, OperatorResult: action.ID, Limit: 1})
		if err != nil {
			return err
		}
		posted := len(page.Items) > 0
		if posted {
			return nil
		}
		return coordinator.appendMessage(ctx, tx, record, &message, now)
	}, func(ctx context.Context, p *conversationPublicationPlan) error {
		if string(p.Record.ProjectID) != action.ProjectID || string(p.Record.OrganizationID) != action.OrganizationID {
			return operatortool.ErrAccessDenied
		}
		return p.AppendMessage(ctx, &message)
	})
}

func (s *Service) postConversationAction(c echo.Context) error {
	var action chat.Action
	if err := hubapi.DecodeAPIJSON(c, &action); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	scope := hubscope.NativeRequestScope(c)
	ctx := c.Request().Context()
	readCtx, cancelRead := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancelRead()
	record, err := s.readPGConversation(readCtx, scope, c.Param("conversation"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if err := s.authorizeConversationScopePG(readCtx, scope, true); err != nil {
		return s.nativeAPIError(c, err)
	}
	if (!coordinatorSettingsTool(string(action.Kind)) && !coordinatorWorkflowMutation(string(action.Kind)) && !coordinatorCatalogWrite(string(action.Kind)) && !slices.Contains([]string{operatortool.StopRun, operatortool.FileIssue, operatortool.MoveItem, operatortool.EditItem, operatortool.AddComment, "update_project_integration", "set_runner_tier", "convert_runner_scope", "promote_sprites_token", "set_sprite_pool", "scale_up_sprite_pool", string(chat.ActionIssueSplit), string(chat.ActionArchiveItems)}, string(action.Kind))) || action.ProjectID != string(scope.Project) || action.RequestID == "" || len(action.RequestID) > 128 {
		return s.invalidAPIRequest(c, operatortool.ErrInvalidArguments)
	}
	identity := operatortool.ConnectionIdentity(ctx)
	action.ConversationID, action.OrganizationID = record.ID, string(scope.Organization)
	action.ID = action.RequestID
	action.Mutation = mutation.Metadata{PrincipalID: identity.PrincipalID, OrganizationID: identity.OrganizationID, ProjectID: action.ProjectID, ResourceID: action.IssueID, Action: string(action.Kind), Source: "chat", CorrelationID: hubapi.NewID("luna")}
	action.Mutation, err = action.Mutation.Bind(action.RequestID, action.Arguments)
	if err != nil {
		return s.invalidAPIRequest(c, err)
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
