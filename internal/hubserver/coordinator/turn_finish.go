package coordinator

import (
	"context"
	"encoding/json"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/conversations"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

const ErrorRunes = 2000

type TurnFinishDeps struct {
	PrepareContents func(context.Context, string, ...conversations.ConversationMessageRecord) (context.Context, error)
	PrepareUsage    func(context.Context, conversations.ConversationUsage) (func(context.Context, tenantstore.Writer) error, error)
	Write           TurnWriter
}

// Finish persists the outcome of the turn for the assistant message, the
// user messages and the execution state in one transaction.
func (d TurnFinishDeps) Finish(ctx context.Context, id, threadID string, assistant *conversations.ConversationMessageRecord, users []conversations.ConversationMessageRecord, budgetStopped bool, outcome conversation.Delivery, runErr error, usage *conversations.ConversationUsage) error {
	userDelivery := conversation.DeliveryDelivered
	var failure *conversation.ReceiptError
	execution := conversation.Execution{Status: conversation.ExecutionIdle, Owner: conversation.Owner{ThreadID: threadID}}
	switch outcome {
	case conversation.DeliveryFailed:
		userDelivery = conversation.DeliveryFailed
		execution.Status = conversation.ExecutionFailed
		execution.Error = conversations.BoundRunes(runErr.Error(), ErrorRunes)
		failure = &conversation.ReceiptError{Code: "coordinator_error", Message: execution.Error}
	case conversation.DeliveryUnknown:
		userDelivery = conversation.DeliveryUnknown
		execution.Status = conversation.ExecutionUnknown
		failure = &conversation.ReceiptError{Code: "unknown", Message: "The coordinator turn stopped before it answered"}
	}
	if budgetStopped {
		userDelivery = conversation.DeliverySaved
		failure = nil
	}

	assistant.Delivery = outcome
	assistant.ThreadID = threadID
	if outcome == conversation.DeliveryFailed {
		data, err := json.Marshal(map[string]string{"error": execution.Error})
		if err != nil {
			return err
		}
		assistant.Data = data
	}
	prepared, err := d.PrepareContents(ctx, id, *assistant)
	if err != nil {
		return err
	}
	ctx = prepared
	var writeUsage func(context.Context, tenantstore.Writer) error
	if usage != nil {
		writeUsage, err = d.PrepareUsage(ctx, *usage)
		if err != nil {
			return err
		}
	}
	return d.Write(ctx, id, func(ctx context.Context, tx tenantstore.Writer, record *conversations.ConversationRecord, now time.Time) error {
		if usage != nil {
			if err := writeUsage(ctx, tx); err != nil {
				return err
			}
		}
		if err := UpdateMessage(ctx, tx, *assistant, now); err != nil {
			return err
		}
		for i := range users {
			// setControlDelivery rather than updateMessage: the receipt must
			// follow the message so the client can offer the retry command
			// a failed or unknown turn leaves as the only way forward.
			if err := SetControlDelivery(ctx, tx, &users[i], userDelivery, failure, now); err != nil {
				return err
			}
		}
		if threadID != "" {
			record.ProviderThreadID = threadID
		}
		return UpdateExecution(ctx, tx, record, execution, now)
	}, func(ctx context.Context, p *conversations.ConversationPublicationPlan) error {
		if err := p.UpdateMessage(ctx, *assistant); err != nil {
			return err
		}
		for _, message := range users {
			if err := p.SetControlDelivery(ctx, message, userDelivery, failure); err != nil {
				return err
			}
		}
		if threadID != "" {
			p.Record.ProviderThreadID = threadID
		}
		return p.UpdateExecution(ctx, execution)
	})
}
