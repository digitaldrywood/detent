package coordinator

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

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/conversations"

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

type AnswerComment struct {
	comment corestore.Comment
	version eventstore.CollaborationVersion
	event   eventstore.Event
}

type MoveAnswerAttempt struct {
	checked          bool
	attemptID        string
	BlockerAttemptID string
}

func BindConnection(ctx context.Context, key string, attach func(context.Context) error) (context.Context, error) {
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
	return ctx, attach(ctx)
}

func (d ActionContextDeps) Context(ctx context.Context, record conversations.ConversationRecord, users []conversations.ConversationMessageRecord) (context.Context, error) {
	if len(users) == 0 {
		return nil, operatortool.ErrAccessDenied
	}
	message := users[len(users)-1]
	var data struct {
		ConnectionID string `json:"operator_connection"`
		PrincipalID  string `json:"operator_principal"`
	}
	if json.Unmarshal(message.Data, &data) != nil || data.ConnectionID == "" {
		return nil, operatortool.ErrAccessDenied
	}
	if age := d.Now().Sub(message.CreatedAt); age >= 0 && age <= d.TurnTimeout {
		ctx = d.AccessTime(ctx, message.CreatedAt)
	}
	ctx = d.Organization(ctx, record.OrganizationID)
	if err := d.RestoreConnection(ctx, data.ConnectionID); err != nil {
		return nil, err
	}
	return d.OriginatingContext(ctx, data.ConnectionID, data.PrincipalID, string(record.OrganizationID))
}

func (s MoveAnswerDeps) Check(ctx context.Context, scope hubscope.NativeScope, issue tracker.NativeIssue, target, answer string) (MoveAnswerAttempt, error) {
	if !strings.EqualFold(issue.State, "Blocked") || strings.EqualFold(target, issue.State) {
		return MoveAnswerAttempt{}, nil
	}
	var attemptID string
	err := s.ReadTenant(ctx, scope.Organization, func(r tenantstore.Reader) error {
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
		return MoveAnswerAttempt{}, err
	}
	humanAction := false
	if attemptID != "" {
		attempt, err := s.ReadAttempt(ctx, scope, string(issue.WorkItemID), attemptID)
		if err != nil {
			return MoveAnswerAttempt{}, err
		}
		humanAction = attempt.Disposition != nil && attempt.Disposition.Status == "blocked" && attempt.Disposition.HumanAction
	}
	if humanAction && strings.TrimSpace(answer) == "" {
		return MoveAnswerAttempt{}, &operatortool.RequestError{Code: "invalid_request", Message: "Provide the human's answer to the recorded human_action question in reason_detail before moving this item out of Blocked"}
	}
	result := MoveAnswerAttempt{checked: true, attemptID: attemptID}
	if humanAction {
		result.BlockerAttemptID = attemptID
	}
	return result, nil
}

func (s ActionHTTPDeps) Post(c echo.Context) error {
	var action chat.Action
	if err := hubapi.DecodeAPIJSON(c, &action); err != nil {
		return s.InvalidRequest(c, err)
	}
	scope := hubscope.NativeRequestScope(c)
	ctx := c.Request().Context()
	readCtx, cancelRead := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancelRead()
	record, err := s.ReadConversation(readCtx, scope, c.Param("conversation"))
	if err != nil {
		return s.APIError(c, err)
	}
	if err := s.Authorize(readCtx, scope, true); err != nil {
		return s.APIError(c, err)
	}
	if (!SettingsTool(string(action.Kind)) && !WorkflowMutation(string(action.Kind)) && !s.CatalogWrite(string(action.Kind)) && !slices.Contains([]string{operatortool.StopRun, operatortool.FileIssue, operatortool.MoveItem, operatortool.EditItem, operatortool.AddComment, "update_project_integration", "set_runner_tier", "convert_runner_scope", "promote_sprites_token", "set_sprite_pool", "scale_up_sprite_pool", string(chat.ActionIssueSplit), string(chat.ActionArchiveItems)}, string(action.Kind))) || action.ProjectID != string(scope.Project) || action.RequestID == "" || len(action.RequestID) > 128 {
		return s.InvalidRequest(c, operatortool.ErrInvalidArguments)
	}
	identity := operatortool.ConnectionIdentity(ctx)
	action.ConversationID, action.OrganizationID = record.ID, string(scope.Organization)
	action.ID = action.RequestID
	action.Mutation = mutation.Metadata{PrincipalID: identity.PrincipalID, OrganizationID: identity.OrganizationID, ProjectID: action.ProjectID, ResourceID: action.IssueID, Action: string(action.Kind), Source: "chat", CorrelationID: hubapi.NewID("luna")}
	action.Mutation, err = action.Mutation.Bind(action.RequestID, action.Arguments)
	if err != nil {
		return s.InvalidRequest(c, err)
	}
	result, err := s.Execute(ctx, action)
	if err != nil {
		return s.APIError(c, err)
	}
	action.Status, action.Result = chat.ActionSucceeded, result.Message
	if err := s.Publish(ctx, action); err != nil {
		return s.APIError(c, err)
	}
	return c.JSON(http.StatusOK, result)
}

func ActionTools() []runner.AgentTool {
	return []runner.AgentTool{
		Tool("get_project_integration", "Read this project's integration settings and GitHub transport availability. Runner PR landing is selected by the approved repository policy.", `{"type":"object","properties":{},"additionalProperties":false}`),
		Tool("update_project_integration", "Preview Hub integration or Cloud-owned workflow Markdown changes (lanes, transitions and dispatch). Repository-owned definitions require a reviewed source change and policy approval. This does not change the runner's approved PR landing policy. The client controls confirmation and submits the proposed call.", `{"type":"object","properties":{"workflow_markdown":{"type":"string","maxLength":131072},"repository_enabled":{"type":"boolean"},"intake":{"type":"string","enum":["disabled","manual"]},"projection":{"type":"string","enum":["disabled","summary"]}},"additionalProperties":false}`),
		Tool("move_item", "Preview moving a native issue to a workflow state. Retry a blocked issue by moving it to Todo. When leaving Blocked after a human_action question, supply reason_detail quoting the human's chosen answer; it is recorded as an item comment and transition reason. Ask for the answer if missing. Current workflow rules and revision apply; the client controls confirmation and submission.", `{"type":"object","required":["work_item_id","state"],"properties":{"work_item_id":{"type":"string"},"state":{"type":"string"},"reason_detail":{"type":"string"}},"additionalProperties":false}`),
		Tool("edit_item", "Preview editing an issue's title, body, labels or priority (0 urgent through 3 low). The client controls confirmation and submission.", `{"type":"object","required":["work_item_id"],"properties":{"work_item_id":{"type":"string"},"title":{"type":"string"},"body":{"type":"string"},"labels":{"type":"array","items":{"type":"string"}},"priority":{"type":"integer","minimum":0,"maximum":3}},"additionalProperties":false}`),
		Tool(string(chat.ActionArchiveItems), "Preview archiving a set of native issues in this project. The client controls confirmation and submission. Running and Merging issues are refused. Archived issues leave the board and dispatch and can be restored.", `{"type":"object","required":["work_item_ids"],"properties":{"work_item_ids":{"type":"array","minItems":1,"maxItems":50,"uniqueItems":true,"items":{"type":"string"}}},"additionalProperties":false}`),
		Tool("add_comment", "Preview adding an issue comment. The client controls confirmation and submits the exact comment.", `{"type":"object","required":["work_item_id","body"],"properties":{"work_item_id":{"type":"string"},"body":{"type":"string"}},"additionalProperties":false}`),
	}
}

func PrepareAnswerComment(ctx context.Context, scope hubscope.NativeScope, item corestore.WorkItem, conversationID string, request tracker.Transition, now time.Time, details attachment.DetailStore) (*AnswerComment, error) {
	content := eventstore.NewContent(details)
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
	return &AnswerComment{comment: row, version: version, event: event}, nil
}

func (c *AnswerComment) Write(ctx context.Context, w tenantstore.Writer) error {
	if _, err := w.CreateComment(ctx, c.comment); err != nil {
		return err
	}
	if _, err := w.CreateCollaborationVersion(ctx, c.version); err != nil {
		return err
	}
	_, err := w.AppendEvent(ctx, c.event)
	return err
}

func (a MoveAnswerAttempt) Recheck(ctx context.Context, w tenantstore.Writer, issueID int64) error {
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

func PublishDecision(ctx context.Context, action chat.Action, write TurnWriter) error {
	data, err := json.Marshal(map[string]string{"operator_action_result": action.ID})
	if err != nil {
		return err
	}
	text := fmt.Sprintf("%s: %s. %s", action.Title, action.Status, action.Result)
	message := conversations.ConversationMessageRecord{Role: conversation.RoleAssistant, Kind: conversation.MessageText, Text: text, Data: data, Delivery: conversation.DeliveryCompleted, Actor: conversation.Actor{Kind: conversation.ActorCoordinator}}
	return write(ctx, action.ConversationID, func(ctx context.Context, tx tenantstore.Writer, record *conversations.ConversationRecord, now time.Time) error {
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
		return AppendMessage(ctx, tx, record, &message, now)
	}, func(ctx context.Context, p *conversations.ConversationPublicationPlan) error {
		if string(p.Record.ProjectID) != action.ProjectID || string(p.Record.OrganizationID) != action.OrganizationID {
			return operatortool.ErrAccessDenied
		}
		return p.AppendMessage(ctx, &message)
	})
}
