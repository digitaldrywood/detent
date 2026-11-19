package coordinator

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/conversations"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runner"
)

func PublishDelta(ctx context.Context, write TurnWriter, id string, assistant conversations.ConversationMessageRecord, text string) (conversations.ConversationMessageRecord, error) {
	updated := assistant
	err := write(ctx, id, func(ctx context.Context, tx tenantstore.Writer, _ *conversations.ConversationRecord, now time.Time) error {
		return AppendDelta(ctx, tx, &updated, text, now)
	}, func(ctx context.Context, p *conversations.ConversationPublicationPlan) error {
		return p.AppendDelta(ctx, assistant, text)
	})
	return updated, err
}

type TurnEventDeps struct {
	Write           TurnWriter
	PrepareContents func(context.Context, string, ...conversations.ConversationMessageRecord) (context.Context, error)
	Logger          *slog.Logger
}

func (d TurnEventDeps) PublishToolUpdate(ctx context.Context, id, threadID string, messages map[string]conversations.ConversationMessageRecord, update runner.AgentUpdate) {
	summary := ToolSummary(update)
	data, err := json.Marshal(map[string]string{"tool": update.Tool, "item_id": update.ItemID, "status": update.Status})
	if err != nil {
		return
	}
	prepared, err := d.PrepareContents(ctx, id, conversations.ConversationMessageRecord{Role: conversation.RoleSystem, Kind: conversation.MessageTool, Text: summary, Data: data, Delivery: conversation.DeliveryCompleted})
	if err != nil {
		d.Logger.Warn("publish tool content", "error", err)
		return
	}
	ctx = prepared
	existing, known := messages[update.ItemID]
	var message conversations.ConversationMessageRecord
	err = d.Write(ctx, id, func(ctx context.Context, tx tenantstore.Writer, record *conversations.ConversationRecord, now time.Time) error {
		if known && update.ItemID != "" {
			existing.Text = summary
			existing.Data = data
			if err := UpdateMessage(ctx, tx, existing, now); err != nil {
				return err
			}
			messages[update.ItemID] = existing
			return nil
		}
		if err := AppendMessage(ctx, tx, record, &message, now); err != nil {
			return err
		}
		if update.ItemID != "" {
			messages[update.ItemID] = message
		}
		return nil
	}, func(ctx context.Context, p *conversations.ConversationPublicationPlan) error {
		if known && update.ItemID != "" {
			updated := existing
			updated.Text, updated.Data = summary, data
			return p.UpdateMessage(ctx, updated)
		}
		if message.ID == "" {
			message = conversations.ConversationMessageRecord{Role: conversation.RoleSystem, Kind: conversation.MessageTool, Text: summary, Data: data, Delivery: conversation.DeliveryCompleted, ThreadID: threadID, Actor: conversation.Actor{Kind: conversation.ActorCoordinator}}
		}
		return p.AppendMessage(ctx, &message)
	})
	if err != nil {
		d.Logger.Warn("coordinator could not persist tool message", "conversation_id", id, "error", err)
	}
}
