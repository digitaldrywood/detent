package conversations

import (
	"context"
	"encoding/json"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

type ConversationPublicationPlan struct {
	Coordinator      bool
	EventCursor      int
	Events           []any
	Prepared         PreparedConversationContents
	Reads            *ConversationArchiveReads
	retryMessage     *ConversationMessageRecord
	Service          *ConversationService
	Record           ConversationRecord
	Original         ConversationRecord
	insertedMessages int64
	messageIDs       []string
	questionIDs      []string
	questionCursor   int
	Now              time.Time
	revisions        map[string]int64
	Messages         map[string]ConversationMessageRecord
}

func (p *ConversationPublicationPlan) Event(ctx context.Context, body any) error {
	p.Events = append(p.Events, body)
	ctx = p.Context(ctx)
	if question, ok := body.(conversation.Question); ok {
		if err := p.PrepareQuestion(ctx, ConversationQuestionRecord{Question: question}); err != nil {
			return err
		}
	}
	raw, err := hubapi.MarshalString(compactConversationEvent(body))
	if err != nil {
		return err
	}
	_, err = p.Prepare(ctx, []byte(raw))
	if err == nil && p.Coordinator {
		p.Record.EventSeq++
	}
	return err
}

func (p *ConversationPublicationPlan) Prepare(ctx context.Context, content []byte) (attachment.ArchiveReference, error) {
	ctx = p.Context(ctx)
	organization := string(p.Record.OrganizationID)
	key := organization + "/" + artifact.Digest(content)
	if ref, ok := p.Prepared[key]; ok {
		p.Reads.store(organization+"/"+ContentArchiveID(organization, ref), content)
		return ref, nil
	}
	ref, err := p.Service.Store.PublishPayload(ctx, organization, content)
	if err != nil {
		return ref, err
	}
	p.Prepared[key] = ref
	p.Reads.store(organization+"/"+ContentArchiveID(organization, ref), content)
	return ref, nil
}

func (p *ConversationPublicationPlan) Message(ctx context.Context, message ConversationMessageRecord) (ConversationMessageRecord, error) {
	ctx = p.Context(ctx)
	raw, err := MessageContent(message)
	if err != nil {
		return message, err
	}
	ref, err := p.Prepare(ctx, raw)
	if err != nil {
		return message, err
	}
	message.ContentID = ContentArchiveID(string(p.Record.OrganizationID), ref)
	message.contentHash = artifact.Digest(raw)
	return message, nil
}

func (p *ConversationPublicationPlan) AppendMessage(ctx context.Context, message *ConversationMessageRecord) error {
	ctx = p.Context(ctx)
	if message.ID == "" {
		message.ID = conversation.NewMessageID()
	}
	p.messageIDs = append(p.messageIDs, message.ID)
	message.ConversationID = p.Record.ID
	if message.CreatedAt.IsZero() {
		message.CreatedAt = p.Now
	}
	if message.UpdatedAt.IsZero() {
		message.UpdatedAt = message.CreatedAt
	}
	prepared, err := p.Message(ctx, *message)
	if err != nil {
		return err
	}
	prepared.Revision = 1
	if p.revisions == nil {
		p.revisions = map[string]int64{}
	}
	p.revisions[message.ID] = 1
	p.RememberMessage(prepared)
	if p.Coordinator {
		p.Record.MessageCount++
	}
	if p.Record.LastMessageAt == nil || p.Record.LastMessageAt.Before(message.CreatedAt) {
		at := message.CreatedAt
		p.Record.LastMessageAt = &at
	}
	if p.Record.Status == conversation.StatusSettled {
		p.Record.Status = conversation.StatusActive
		p.Record.SettledAt = nil
		if err := p.SaveConversation(ctx); err != nil {
			return err
		}
	}
	return p.Event(ctx, ProjectMessage(prepared))
}

func (p *ConversationPublicationPlan) nextMessageRevision(ctx context.Context, messageID string) (int64, error) {
	ctx = p.Context(ctx)
	if p.revisions == nil {
		p.revisions = map[string]int64{}
	}
	revision, ok := p.revisions[messageID]
	if !ok {
		err := p.Service.Server.config().TenantRepository.ReadTenant(ctx, p.Record.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			message, err := r.Message(ctx, p.Record.ID, messageID)
			revision = message.Revision
			return err
		})
		if err != nil {
			return 0, err
		}
	}
	revision++
	p.revisions[messageID] = revision
	return revision, nil
}

func (p *ConversationPublicationPlan) UpdateMessage(ctx context.Context, message ConversationMessageRecord) error {
	ctx = p.Context(ctx)
	prepared, err := p.Message(ctx, message)
	if err != nil {
		return err
	}
	prepared.UpdatedAt = p.Now
	prepared.Revision, err = p.nextMessageRevision(ctx, message.ID)
	if err != nil {
		return err
	}
	p.RememberMessage(prepared)
	return p.Event(ctx, ProjectMessage(prepared))
}

func (p *ConversationPublicationPlan) AppendDelta(ctx context.Context, message ConversationMessageRecord, text string) error {
	ctx = p.Context(ctx)
	return p.appendDeltaAt(ctx, message, int64(len(message.Text)), text)
}

func (p *ConversationPublicationPlan) appendDeltaAt(ctx context.Context, message ConversationMessageRecord, offset int64, text string) error {
	ctx = p.Context(ctx)
	revision, err := p.nextMessageRevision(ctx, message.ID)
	if err != nil {
		return err
	}
	if err := p.Event(ctx, ConversationDeltaFact{Text: text, Revision: revision, MessageID: message.ID, Offset: offset, Bytes: len(text)}); err != nil {
		return err
	}
	message.Text += text
	message.TextBytes = offset + int64(len(text))
	message.Revision, message.UpdatedAt = revision, p.Now
	p.RememberMessage(message)
	return nil
}

func (p *ConversationPublicationPlan) RememberMessage(message ConversationMessageRecord) {
	if p.Messages == nil {
		p.Messages = map[string]ConversationMessageRecord{}
	}
	p.Messages[message.ID] = message
}

func (p *ConversationPublicationPlan) SaveConversation(ctx context.Context) error {
	ctx = p.Context(ctx)
	p.Record.Revision++
	p.Record.UpdatedAt = p.Now
	if err := p.prepareRecord(ctx, p.Record); err != nil {
		return err
	}
	return p.Event(ctx, ProjectConversation(p.Record))
}

func (p *ConversationPublicationPlan) UpdateExecution(ctx context.Context, execution conversation.Execution) error {
	ctx = p.Context(ctx)
	execution.UpdatedAt = p.Now
	p.Record.Execution = execution
	if err := p.SaveConversation(ctx); err != nil {
		return err
	}
	return p.Event(ctx, ProjectExecution(execution))
}

func (p *ConversationPublicationPlan) RecordReceipt(ctx context.Context, receipt conversation.Receipt) error {
	ctx = p.Context(ctx)
	receipt.UpdatedAt = p.Now
	raw, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	if err := p.PrepareValue(ctx, raw); err != nil {
		return err
	}
	return p.Event(ctx, receipt)
}

func (p *ConversationPublicationPlan) SetControlDelivery(ctx context.Context, message ConversationMessageRecord, delivery conversation.Delivery, failure *conversation.ReceiptError) error {
	ctx = p.Context(ctx)
	message.Delivery = delivery
	if err := p.UpdateMessage(ctx, message); err != nil {
		return err
	}
	if message.CommandKey == "" {
		return nil
	}
	var receipt conversation.Receipt
	var found bool
	err := p.Service.Server.config().TenantRepository.ReadTenant(ctx, p.Record.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		command, exists, err := r.Command(ctx, message.ConversationID, message.CommandKey)
		receipt, found = command.Receipt, exists
		return err
	})
	if err != nil || !found {
		return err
	}
	receipt.Status = delivery
	receipt.Error = failure
	if receipt.MessageID == "" {
		receipt.MessageID = message.ID
	}
	return p.RecordReceipt(ctx, receipt)
}
