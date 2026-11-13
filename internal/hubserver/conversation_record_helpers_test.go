package hubserver

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
)

func conversationDeltaBody(messageID string, seq int64, text string) map[string]any {
	return map[string]any{"message_id": messageID, "seq": seq, "text": text}
}

func settleIdleConversationsForTest(c *conversationService, ctx context.Context, now time.Time) (int, error) {
	organization, err := c.LifecycleOrganization()
	if err != nil {
		return 0, err
	}
	return c.SettleIdleConversationsPG(ctx, organization, now)
}

func newTestPublicationPlan(c *conversationService, ctx context.Context, id string) (*conversationPublicationPlan, error) {
	ctx = newConversationArchiveReads(ctx)
	record, err := c.ReadPublicationConversation(ctx, id)
	if err != nil {
		return nil, err
	}
	return &conversationPublicationPlan{Service: c, Record: record, Original: record, Now: c.Server.Now(), Reads: conversationArchiveCache(ctx)}, nil
}

func validateConversationRecord(record *conversationRecord) error {
	if record.ID == "" {
		record.ID = conversation.NewConversationID()
	}
	if err := conversation.ValidateConversationID(record.ID); err != nil {
		return hubapi.NativeInvalid(err.Error())
	}
	if record.OrganizationID == "" || record.ProjectID == "" {
		return hubapi.NativePublicInvalid("A conversation requires an organization and project")
	}
	if strings.TrimSpace(record.OwnerPrincipalID) == "" {
		return hubapi.NativePublicInvalid("A conversation requires an owner principal")
	}
	if record.Origin == "" {
		record.Origin = conversationOriginUser
	}
	if record.Origin != conversationOriginUser && record.Origin != conversationOriginWorker {
		return hubapi.NativePublicInvalid("Conversation origin is not valid")
	}
	if !record.Visibility.Valid() {
		return hubapi.NativePublicInvalid("Conversation visibility is not valid")
	}
	if record.SubjectWorkItemID != "" && (record.WorkItemID != "" || record.Visibility != conversation.VisibilityPrivate) {
		return hubapi.NativePublicInvalid("Issue questions must remain private and unlinked")
	}
	if !record.Status.Valid() {
		return hubapi.NativePublicInvalid("Conversation status is not valid")
	}
	if record.Execution.Status == "" {
		record.Execution.Status = conversation.ExecutionIdle
	}
	if !record.Execution.Status.Valid() {
		return hubapi.NativePublicInvalid("Conversation execution status is not valid")
	}
	if record.WorkItemID != "" && record.LinkedAt == nil {
		return hubapi.NativePublicInvalid("A linked conversation requires linked_at")
	}
	record.Preferences = record.Preferences.Normalized()
	return nil
}

func validateConversationMessage(message *conversationMessageRecord) error {
	if message.ID == "" {
		message.ID = conversation.NewMessageID()
	}
	if err := conversation.ValidateMessageID(message.ID); err != nil {
		return hubapi.NativeInvalid(err.Error())
	}
	if err := conversation.ValidateConversationID(message.ConversationID); err != nil {
		return hubapi.NativeInvalid(err.Error())
	}
	if !message.Role.Valid() || !message.Kind.Valid() || !message.Delivery.Valid() || !message.Actor.Kind.Valid() {
		return hubapi.NativePublicInvalid("Message role, kind, delivery and actor kind must be valid")
	}
	if len(message.Data) == 0 {
		message.Data = json.RawMessage("{}")
	}
	if !json.Valid(message.Data) {
		return hubapi.NativePublicInvalid("Message data must be valid JSON")
	}
	if message.UpdatedAt.IsZero() {
		message.UpdatedAt = message.CreatedAt
	}
	return nil
}
