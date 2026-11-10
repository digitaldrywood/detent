package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type pgWorkerPublicationKey struct{}

func preparePGWorkerEvent(ctx context.Context, plan *conversationPublicationPlan, body any) error {
	if message, ok := body.(conversationMessageResource); ok {
		message.Seq = 0
		body = message
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	content, err := messageContent(conversationMessageRecord{Data: raw})
	if err != nil {
		return err
	}
	_, err = plan.prepare(ctx, content)
	return err
}

func appendPreparedPGWorkerEvent(ctx context.Context, writer tenantstore.Writer, organization tracker.OrganizationID, id string, kind conversation.EventType, body any, at time.Time) (bool, error) {
	plan, ok := ctx.Value(pgWorkerPublicationKey{}).(*conversationPublicationPlan)
	if !ok {
		return false, nil
	}
	if message, ok := body.(conversationMessageResource); ok {
		message.Seq = 0
		if kind == conversation.EventMessageUpdated {
			if cached, found := plan.messages[message.ID]; found && cached.ContentID == message.contentID {
				message.Text, message.Data = cached.Text, projectMessage(cached).Data
			}
		}
		body = message
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return true, err
	}
	content, err := messageContent(conversationMessageRecord{Data: raw})
	if err != nil {
		return true, err
	}
	ref, ok := plan.prepared[string(organization)+"/"+artifact.Digest(content)]
	if !ok {
		return false, nil
	}
	_, err = writer.AppendConversationEvent(ctx, conversationstore.Event{OrganizationID: organization, ConversationID: id, ContentReference: contentArchiveID(string(organization), ref), Event: conversation.Event{Type: kind, Data: raw, CreatedAt: at}})
	return true, err
}

func (r *conversationWorkerRouter) preparePGTurnMessages(ctx context.Context, scope nativeScope, plan *conversationPublicationPlan, events []conversationTurnEvent) ([]conversationMessageRecord, error) {
	prepared := make([]conversationMessageRecord, len(events))
	messages := map[string]conversationMessageRecord{}
	providers := map[string]string{}
	execution := plan.record.Execution
	for index, event := range events {
		switch event.Type {
		case "turn_started":
			if thread := strings.TrimSpace(event.ThreadID); thread != "" {
				execution.Owner.ThreadID = thread
			}
			execution.Owner.TurnID = strings.TrimSpace(event.TurnID)
		case "delta":
			id := event.MessageID
			if id == "" {
				id = providers[strings.TrimSpace(event.ProviderItemID)]
			}
			message, found := messages[id]
			if !found {
				if err := r.service.server.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
					var row conversationstore.Message
					if id != "" {
						var err error
						row, err = reader.Message(ctx, plan.record.ID, id)
						if errors.Is(err, corestore.ErrNotFound) {
							return nil
						}
						if err != nil {
							return err
						}
					} else {
						page, err := reader.FilteredMessages(ctx, conversationstore.MessageFilter{ConversationID: plan.record.ID, AttemptID: execution.Owner.AttemptID, ProviderItemID: strings.TrimSpace(event.ProviderItemID), Role: conversation.RoleAssistant, Limit: 1})
						if err != nil || len(page.Items) == 0 {
							return err
						}
						row = page.Items[0]
					}
					if row.AttemptID == execution.Owner.AttemptID && row.Role == conversation.RoleAssistant {
						message, found = conversationMessageFromStore(row), true
					}
					return nil
				}); err != nil {
					return nil, err
				}
			}
			if found && message.ContentID != "" && plan.messages[message.ID].ID == "" {
				var err error
				message, err = r.service.readPublicationMessage(ctx, scope.organization, plan.record.ID, message.ID)
				if err != nil {
					return nil, err
				}
			}
			if !found {
				message = conversationMessageRecord{ID: conversation.NewMessageID(), ConversationID: plan.record.ID, Role: conversation.RoleAssistant, Kind: conversation.MessageText, Delivery: conversation.DeliveryResponding, AttemptID: execution.Owner.AttemptID, ThreadID: execution.Owner.ThreadID, TurnID: execution.Owner.TurnID, ProviderItemID: strings.TrimSpace(event.ProviderItemID), Actor: conversationActorFor(scope), CreatedAt: plan.now.Truncate(time.Microsecond), UpdatedAt: plan.now}
			}
			revision, offset := message.Revision, message.TextBytes
			message.Text += event.Text
			message.TextBytes = offset + int64(len(event.Text))
			message.UpdatedAt = plan.now
			var err error
			message, err = plan.message(ctx, message)
			if err != nil {
				return nil, err
			}
			prepared[index] = message
			prepared[index].Revision = revision
			if !found {
				accepted := message
				accepted.Text, accepted.TextBytes, accepted.Revision = "", 0, 1
				if err := preparePGWorkerEvent(ctx, plan, projectMessage(accepted)); err != nil {
					return nil, err
				}
				revision = 1
			}
			if event.Text != "" {
				revision++
				if err := preparePGWorkerEvent(ctx, plan, conversationDeltaFact{Text: event.Text, Revision: revision, MessageID: message.ID, Offset: offset, Bytes: len(event.Text)}); err != nil {
					return nil, err
				}
			}
			message.Revision = revision
			messages[message.ID], providers[strings.TrimSpace(event.ProviderItemID)] = message, message.ID
			plan.rememberMessage(message)
		case "item", "question_opened":
			message := conversationMessageRecord{Role: conversation.RoleSystem, Kind: conversation.MessageKind(event.Kind), Text: conversationTruncate(event.Summary, conversationMaxSummaryBytes), Delivery: conversation.DeliveryCompleted, ProviderItemID: strings.TrimSpace(event.ProviderItemID), AttemptID: execution.Owner.AttemptID, ThreadID: execution.Owner.ThreadID, TurnID: execution.Owner.TurnID, Actor: conversationActorFor(scope), CreatedAt: plan.now.Truncate(time.Microsecond), UpdatedAt: plan.now}
			if event.Type == "item" {
				var err error
				message.Data, err = conversationItemData(event.Data)
				if err != nil {
					return nil, err
				}
			} else {
				message.Role, message.Kind, message.Text = conversation.RoleAssistant, conversation.MessageStatus, conversationQuestionPrompt
				message.ProviderItemID = ""
				if turn := strings.TrimSpace(event.TurnID); turn != "" {
					message.TurnID = turn
				}
			}
			if err := preparePGConversationMessage(ctx, plan, &message); err != nil {
				return nil, err
			}
			prepared[index] = message
			if err := preparePGWorkerEvent(ctx, plan, projectMessage(message)); err != nil {
				return nil, err
			}
		case "turn_completed":
			turnID := strings.TrimSpace(event.TurnID)
			if err := r.service.server.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
				page, err := reader.FilteredMessages(ctx, conversationstore.MessageFilter{ConversationID: plan.record.ID, AttemptID: execution.Owner.AttemptID, TurnID: turnID, Role: conversation.RoleAssistant, Deliveries: []conversation.Delivery{conversation.DeliveryResponding}, Limit: conversationPGWorkerPageSize})
				if err != nil {
					return err
				}
				if page.Next != 0 {
					return conversationUnsupported("The turn message set exceeds the bounded event batch")
				}
				for _, row := range page.Items {
					if _, found := messages[row.ID]; !found {
						messages[row.ID] = conversationMessageFromStore(row)
					}
				}
				return nil
			}); err != nil {
				return nil, err
			}
			delivery := conversation.DeliveryCompleted
			switch event.Status {
			case "failed":
				delivery = conversation.DeliveryFailed
			case "interrupted", "cancelled":
				delivery = conversation.DeliveryInterrupted
			}
			for id, message := range messages {
				if message.Delivery != conversation.DeliveryResponding || message.TurnID != turnID {
					continue
				}
				if message.ContentID != "" && plan.messages[id].ID == "" {
					var err error
					message, err = r.service.readPublicationMessage(ctx, scope.organization, plan.record.ID, id)
					if err != nil {
						return nil, err
					}
				}
				message.Delivery, message.UpdatedAt = delivery, plan.now
				message.Revision++
				if err := preparePGWorkerEvent(ctx, plan, projectMessage(message)); err != nil {
					return nil, err
				}
				plan.rememberMessage(message)
				messages[id] = message
			}
		}
	}
	return prepared, nil
}
