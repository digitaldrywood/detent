package conversations

import (
	"context"
	"encoding/json"

	"github.com/digitaldrywood/detent/internal/conversation"
)

type ConversationRecordContent struct {
	Execution   conversation.Execution   `json:"execution"`
	Preferences conversation.Preferences `json:"preferences"`
}

type ConversationQuestionContent struct {
	Prompts []conversation.Prompt `json:"prompts"`
	Answers map[string][]string   `json:"answers"`
}

func RecordPayload(record ConversationRecord) ([]byte, error) {
	return json.Marshal(ConversationRecordContent{Execution: record.Execution, Preferences: record.Preferences.Normalized()})
}

func QuestionPayload(record ConversationQuestionRecord) ([]byte, error) {
	prompts, answers := record.Prompts, record.Answers
	if prompts == nil {
		prompts = []conversation.Prompt{}
	}
	if answers == nil {
		answers = map[string][]string{}
	}
	return json.Marshal(ConversationQuestionContent{Prompts: prompts, Answers: answers})
}

func (s *ConversationStore) resolveConversationRecord(ctx context.Context, query Queryer, record *ConversationRecord) error {
	if record.ContentID == "" {
		return nil
	}
	raw, err := s.readPayload(ctx, query, record.ID, record.ContentID)
	if err != nil {
		return err
	}
	var payload ConversationRecordContent
	if err := json.Unmarshal(raw, &payload); err != nil {
		return err
	}
	record.Execution, record.Preferences = payload.Execution, payload.Preferences.Normalized()
	return nil
}

func (s *ConversationStore) readPayload(ctx context.Context, query Queryer, id, contentID string) ([]byte, error) {
	var organization string
	if err := query.QueryRowContext(ctx, "SELECT organization_id FROM conversations WHERE id=?", id).Scan(&organization); err != nil {
		return nil, err
	}
	content, err := s.readConversationArchive(ctx, query, organization, contentID)
	if err != nil {
		return nil, err
	}
	var payload ConversationContent
	if err := json.Unmarshal(content, &payload); err != nil {
		return nil, err
	}
	return payload.Data, nil
}

func (p *ConversationPublicationPlan) prepareRecord(ctx context.Context, record ConversationRecord) error {
	raw, err := RecordPayload(record)
	if err != nil {
		return err
	}
	content, err := MessageContent(ConversationMessageRecord{Data: raw})
	if err != nil {
		return err
	}
	_, err = p.Prepare(ctx, content)
	return err
}

func (p *ConversationPublicationPlan) PrepareQuestion(ctx context.Context, question ConversationQuestionRecord) error {
	raw, err := QuestionPayload(question)
	if err != nil {
		return err
	}
	return p.PrepareValue(ctx, raw)
}

func (p *ConversationPublicationPlan) PrepareValue(ctx context.Context, raw []byte) error {
	content, err := MessageContent(ConversationMessageRecord{Data: raw})
	if err != nil {
		return err
	}
	_, err = p.Prepare(ctx, content)
	return err
}
