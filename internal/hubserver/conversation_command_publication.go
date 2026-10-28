package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type conversationPreparedRetryKey struct{}

type conversationIssueAllocationKey struct{}

type conversationIssueAllocation struct {
	organization tracker.OrganizationID
	project      tracker.ProjectID
	id           tracker.NativeWorkItemID
	number       int
}

func (c *conversationService) reconcilePublishedCommand(ctx context.Context, scope nativeScope, id string, command conversation.Command) (json.RawMessage, bool, error) {
	ctx = context.WithValue(ctx, conversationPublicationKey{}, struct{}{})
	var receipt *conversation.Receipt
	err := c.transact(ctx, func(tx *sql.Tx, now time.Time) error {
		record, err := scanConversation(tx.QueryRowContext(ctx, "SELECT "+conversationColumns+",0,read_audience FROM conversations WHERE organization_id=? AND project_id=? AND id=?", scope.organization, scope.project, id))
		if errors.Is(err, sql.ErrNoRows) {
			return nativeNotFound()
		}
		if err != nil {
			return err
		}
		if err := c.authorizeRead(ctx, tx, scope, record); err != nil {
			return err
		}
		if err := c.authorizeWrite(ctx, tx, scope, record); err != nil {
			return err
		}
		if err := c.requireActorAuthority(ctx, tx, scope, now); err != nil {
			return err
		}
		var hash, encoded string
		err = tx.QueryRowContext(ctx, "SELECT request_hash,receipt_json FROM conversation_commands WHERE conversation_id=? AND key=?", id, command.Key).Scan(&hash, &encoded)
		if err == nil {
			if hash != conversation.RequestHash(command) {
				return &nativeError{Code: "idempotency_conflict", Message: "Idempotency key has different content", status: http.StatusConflict, publicMessage: true}
			}
			receipt, err = decodeConversationReceipt(encoded)
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if command.Kind == conversation.CommandAnswer {
			question, err := c.store.readQuestion(ctx, tx, id, command.QuestionID)
			if err != nil {
				return err
			}
			switch question.Status {
			case conversation.QuestionSending, conversation.QuestionSent, conversation.QuestionAnswered:
				return conversationQuestionAnswered()
			}
		}
		return nil
	})
	if err != nil || receipt == nil {
		return nil, false, err
	}
	raw, err := json.Marshal(receipt)
	return raw, true, err
}

func (p *conversationPublicationPlan) command(ctx context.Context, scope nativeScope, command conversation.Command) error {
	ctx = p.context(ctx)
	if p.record.Status == conversation.StatusSettled {
		p.record.Status = conversation.StatusActive
		p.record.SettledAt = nil
		if err := p.saveConversation(ctx); err != nil {
			return err
		}
	}
	receipt := conversation.Receipt{Key: command.Key, Kind: command.Kind, Status: conversation.DeliverySaved}
	actor := conversationActorFor(scope)
	linked := p.record.WorkItemID != ""
	message := conversationMessageRecord{Role: conversation.RoleUser, Actor: actor, CommandKey: command.Key}
	switch command.Kind {
	case conversation.CommandMessage:
		message.Kind, message.Text, message.Data = conversation.MessageText, command.Text, coordinatorConnectionData(ctx)
		message.Delivery = conversation.DeliverySaved
		if linked {
			message.Delivery = conversation.DeliveryQueued
			message.AttemptID, message.TurnID = p.record.Execution.Owner.AttemptID, p.record.Execution.Owner.TurnID
		}
		if err := p.appendMessage(ctx, &message); err != nil {
			return err
		}
		receipt.Status, receipt.MessageID = message.Delivery, message.ID
	case conversation.CommandAnswer:
		question, err := p.service.store.readQuestion(ctx, p.service.store.reader, p.record.ID, command.QuestionID)
		if err != nil {
			return translateConversationError(err)
		}
		data, err := marshalNative(command.Answers)
		if err != nil {
			return err
		}
		message.Kind, message.Text, message.Data = conversation.MessageAnswer, renderAnswer(question.Question, command.Answers), []byte(data)
		message.Delivery = conversation.DeliveryQueued
		message.AttemptID, message.ThreadID, message.TurnID = question.Owner.AttemptID, question.Owner.ThreadID, question.Owner.TurnID
		if err := p.appendMessage(ctx, &message); err != nil {
			return err
		}
		question.Status, question.Answers, question.AnsweredBy, question.UpdatedAt = conversation.QuestionSending, command.Answers, scope.credential.ID, p.now
		if err := p.event(ctx, question.Question); err != nil {
			return err
		}
		receipt.Status, receipt.MessageID, receipt.QuestionID = conversation.DeliveryQueued, message.ID, question.ID
	case conversation.CommandInterrupt:
		message.Kind, message.Delivery = conversation.MessageInterrupt, conversation.DeliveryQueued
		message.AttemptID, message.ThreadID, message.TurnID = p.record.Execution.Owner.AttemptID, p.record.Execution.Owner.ThreadID, p.record.Execution.Owner.TurnID
		if err := p.appendMessage(ctx, &message); err != nil {
			return err
		}
		execution := p.record.Execution
		execution.Status = conversation.ExecutionInterrupting
		if err := p.updateExecution(ctx, execution); err != nil {
			return err
		}
		receipt.Status, receipt.MessageID = conversation.DeliveryQueued, message.ID
	case conversation.CommandContinue:
		message.Kind, message.Text, message.Delivery = conversation.MessageContinue, command.Text, conversation.DeliverySaved
		if err := p.appendMessage(ctx, &message); err != nil {
			return err
		}
		if err := p.updateExecution(ctx, conversation.Execution{Status: conversation.ExecutionWaitingForRunner, Owner: conversation.Owner{ThreadID: p.record.Execution.Owner.ThreadID}}); err != nil {
			return err
		}
		receipt.MessageID = message.ID
	case conversation.CommandCancel:
		receipt.Status = conversation.DeliveryDelivered
		if err := p.recordReceipt(ctx, receipt); err != nil {
			return err
		}
		receipt.Status = conversation.DeliveryRejected
		receipt.Error = &conversation.ReceiptError{Code: "no_active_turn", Message: "No coordinator turn is running"}
	case conversation.CommandRetry:
		messages, err := p.service.queryMessages(ctx, p.service.store.reader, conversationMessageQuery+"conversation_id=? AND id=?", p.record.ID, command.MessageID)
		if err != nil {
			return err
		}
		if len(messages) == 0 {
			return nativePublicInvalid("The message to retry is not part of this conversation")
		}
		message = messages[0]
		retryMessage := message
		p.retryMessage = &retryMessage
		message.Data = coordinatorConnectionData(ctx)
		message.Delivery = conversation.DeliverySaved
		if linked {
			message.Delivery = conversation.DeliveryQueued
		}
		message.AttemptID, message.ThreadID, message.TurnID = "", "", ""
		if err := p.updateMessage(ctx, message); err != nil {
			return err
		}
		receipt.Status, receipt.MessageID = message.Delivery, message.ID
	}
	return p.recordReceipt(ctx, receipt)
}
