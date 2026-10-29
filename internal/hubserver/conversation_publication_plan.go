package hubserver

import (
	"context"
	"encoding/json"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
)

type conversationPublicationPlan struct {
	receiptLogs      [][]any
	toolRevisions    map[string]legacyToolRevision
	prepared         preparedConversationContents
	reads            *conversationArchiveReads
	retryMessage     *conversationMessageRecord
	issueAllocation  *conversationIssueAllocation
	service          *conversationService
	record           conversationRecord
	original         conversationRecord
	insertedMessages int64
	creation         *conversationRecord
	messageIDs       []string
	messageCursor    int
	questionIDs      []string
	questionCursor   int
	now              time.Time
	revisions        map[string]int64
	messages         map[string]conversationMessageRecord
	questions        map[string]conversationQuestionRecord
}

func (p *conversationPublicationPlan) event(ctx context.Context, body any) error {
	ctx = p.context(ctx)
	if question, ok := body.(conversation.Question); ok {
		if err := p.prepareQuestion(ctx, conversationQuestionRecord{Question: question}); err != nil {
			return err
		}
	}
	raw, err := marshalNative(compactConversationEvent(body))
	if err != nil {
		return err
	}
	_, err = p.prepare(ctx, []byte(raw))
	return err
}

func (p *conversationPublicationPlan) prepare(ctx context.Context, content []byte) (attachment.ArchiveReference, error) {
	ctx = p.context(ctx)
	organization := string(p.record.OrganizationID)
	key := organization + "/" + artifact.Digest(content)
	if ref, ok := p.prepared[key]; ok {
		p.reads.store(organization+"/"+contentArchiveID(organization, ref), content)
		return ref, nil
	}
	ref, err := p.service.store.publishPayload(ctx, organization, content)
	if err != nil {
		return ref, err
	}
	p.prepared[key] = ref
	p.reads.store(organization+"/"+contentArchiveID(organization, ref), content)
	return ref, nil
}

func (p *conversationPublicationPlan) message(ctx context.Context, message conversationMessageRecord) (conversationMessageRecord, error) {
	ctx = p.context(ctx)
	raw, err := messageContent(message)
	if err != nil {
		return message, err
	}
	ref, err := p.prepare(ctx, raw)
	if err != nil {
		return message, err
	}
	message.ContentID = contentArchiveID(string(p.record.OrganizationID), ref)
	message.contentHash = artifact.Digest(raw)
	return message, nil
}

func (p *conversationPublicationPlan) appendMessage(ctx context.Context, message *conversationMessageRecord) error {
	ctx = p.context(ctx)
	if message.ID == "" {
		message.ID = conversation.NewMessageID()
	}
	p.messageIDs = append(p.messageIDs, message.ID)
	message.ConversationID = p.record.ID
	if message.CreatedAt.IsZero() {
		message.CreatedAt = p.now
	}
	if message.UpdatedAt.IsZero() {
		message.UpdatedAt = message.CreatedAt
	}
	prepared, err := p.message(ctx, *message)
	if err != nil {
		return err
	}
	prepared.Revision = 1
	if p.revisions == nil {
		p.revisions = map[string]int64{}
	}
	p.revisions[message.ID] = 1
	p.rememberMessage(prepared)
	if p.record.LastMessageAt == nil || p.record.LastMessageAt.Before(message.CreatedAt) {
		at := message.CreatedAt
		p.record.LastMessageAt = &at
	}
	if p.record.Status == conversation.StatusSettled {
		p.record.Status = conversation.StatusActive
		p.record.SettledAt = nil
		if err := p.saveConversation(ctx); err != nil {
			return err
		}
	}
	return p.event(ctx, projectMessage(prepared))
}

func (p *conversationPublicationPlan) nextMessageRevision(ctx context.Context, messageID string) (int64, error) {
	ctx = p.context(ctx)
	if p.revisions == nil {
		p.revisions = map[string]int64{}
	}
	revision, ok := p.revisions[messageID]
	if !ok {
		if err := p.service.store.reader.QueryRowContext(ctx, "SELECT revision FROM conversation_messages WHERE conversation_id=? AND id=?", p.record.ID, messageID).Scan(&revision); err != nil {
			return 0, err
		}
	}
	revision++
	p.revisions[messageID] = revision
	return revision, nil
}

func (p *conversationPublicationPlan) updateMessage(ctx context.Context, message conversationMessageRecord) error {
	ctx = p.context(ctx)
	if message.Kind == conversation.MessageTool && message.Actor.Kind == conversation.ActorCoordinator {
		var err error
		ctx, err = p.service.store.prepareLegacyToolRevision(ctx, p.record.ID, message.ID)
		if err != nil {
			return err
		}
		if revisions, ok := ctx.Value(legacyToolRevisionKey{}).(map[string]legacyToolRevision); ok {
			p.toolRevisions = revisions
		}
		if contents, ok := ctx.Value(conversationContentsKey{}).(preparedConversationContents); ok {
			p.prepared = contents
		}
	}
	prepared, err := p.message(ctx, message)
	if err != nil {
		return err
	}
	prepared.UpdatedAt = p.now
	prepared.Revision, err = p.nextMessageRevision(ctx, message.ID)
	if err != nil {
		return err
	}
	p.rememberMessage(prepared)
	return p.event(ctx, projectMessage(prepared))
}

func (p *conversationPublicationPlan) appendDelta(ctx context.Context, message conversationMessageRecord, text string) error {
	ctx = p.context(ctx)
	return p.appendDeltaAt(ctx, message, int64(len(message.Text)), text)
}

func (p *conversationPublicationPlan) appendDeltaAt(ctx context.Context, message conversationMessageRecord, offset int64, text string) error {
	ctx = p.context(ctx)
	revision, err := p.nextMessageRevision(ctx, message.ID)
	if err != nil {
		return err
	}
	if err := p.event(ctx, conversationDeltaFact{Text: text, Revision: revision, MessageID: message.ID, Offset: offset, Bytes: len(text)}); err != nil {
		return err
	}
	message.Text += text
	message.TextBytes = offset + int64(len(text))
	message.Revision, message.UpdatedAt = revision, p.now
	p.rememberMessage(message)
	return nil
}

func (p *conversationPublicationPlan) rememberMessage(message conversationMessageRecord) {
	if p.messages == nil {
		p.messages = map[string]conversationMessageRecord{}
	}
	p.messages[message.ID] = message
}

func (p *conversationPublicationPlan) currentMessage(message conversationMessageRecord) conversationMessageRecord {
	if current, ok := p.messages[message.ID]; ok {
		return current
	}
	return message
}

func (p *conversationPublicationPlan) saveConversation(ctx context.Context) error {
	ctx = p.context(ctx)
	p.record.Revision++
	p.record.UpdatedAt = p.now
	if err := p.prepareRecord(ctx, p.record); err != nil {
		return err
	}
	return p.event(ctx, projectConversation(p.record))
}

func (p *conversationPublicationPlan) updateExecution(ctx context.Context, execution conversation.Execution) error {
	ctx = p.context(ctx)
	execution.UpdatedAt = p.now
	p.record.Execution = execution
	if err := p.saveConversation(ctx); err != nil {
		return err
	}
	return p.event(ctx, projectExecution(execution))
}

func (p *conversationPublicationPlan) recordReceipt(ctx context.Context, receipt conversation.Receipt) error {
	ctx = p.context(ctx)
	receipt.UpdatedAt = p.now
	raw, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	if err := p.prepareValue(ctx, raw); err != nil {
		return err
	}
	return p.event(ctx, receipt)
}

func (p *conversationPublicationPlan) setControlDelivery(ctx context.Context, message conversationMessageRecord, delivery conversation.Delivery, failure *conversation.ReceiptError) error {
	ctx = p.context(ctx)
	message.Delivery = delivery
	if err := p.updateMessage(ctx, message); err != nil {
		return err
	}
	if message.CommandKey == "" {
		return nil
	}
	receipt, found, err := p.service.readReceipt(ctx, p.service.store.reader, message.ConversationID, message.CommandKey)
	if err != nil || !found {
		return err
	}
	receipt.Status = delivery
	receipt.Error = failure
	if receipt.MessageID == "" {
		receipt.MessageID = message.ID
	}
	return p.recordReceipt(ctx, receipt)
}
