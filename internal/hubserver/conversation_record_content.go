package hubserver

import (
	"context"
)

func (p *conversationPublicationPlan) prepareRecord(ctx context.Context, record conversationRecord) error {
	raw, err := recordPayload(record)
	if err != nil {
		return err
	}
	content, err := messageContent(conversationMessageRecord{Data: raw})
	if err != nil {
		return err
	}
	_, err = p.prepare(ctx, content)
	return err
}

func (p *conversationPublicationPlan) prepareQuestion(ctx context.Context, question conversationQuestionRecord) error {
	raw, err := questionPayload(question)
	if err != nil {
		return err
	}
	return p.prepareValue(ctx, raw)
}

func (p *conversationPublicationPlan) prepareValue(ctx context.Context, raw []byte) error {
	content, err := messageContent(conversationMessageRecord{Data: raw})
	if err != nil {
		return err
	}
	_, err = p.prepare(ctx, content)
	return err
}
