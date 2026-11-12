package hubserver

import (
	"context"
	"encoding/json"

	"github.com/digitaldrywood/detent/internal/conversation"
)

type conversationRecordContent struct {
	Execution   conversation.Execution   `json:"execution"`
	Preferences conversation.Preferences `json:"preferences"`
}

type conversationQuestionContent struct {
	Prompts []conversation.Prompt `json:"prompts"`
	Answers map[string][]string   `json:"answers"`
}

func recordPayload(record conversationRecord) ([]byte, error) {
	return json.Marshal(conversationRecordContent{Execution: record.Execution, Preferences: record.Preferences.Normalized()})
}

func questionPayload(record conversationQuestionRecord) ([]byte, error) {
	prompts, answers := record.Prompts, record.Answers
	if prompts == nil {
		prompts = []conversation.Prompt{}
	}
	if answers == nil {
		answers = map[string][]string{}
	}
	return json.Marshal(conversationQuestionContent{Prompts: prompts, Answers: answers})
}

func (s *conversationStore) resolveConversationRecord(ctx context.Context, query nativeQueryer, record *conversationRecord) error {
	if record.ContentID == "" {
		return nil
	}
	raw, err := s.readPayload(ctx, query, record.ID, record.ContentID)
	if err != nil {
		return err
	}
	var payload conversationRecordContent
	if err := json.Unmarshal(raw, &payload); err != nil {
		return err
	}
	record.Execution, record.Preferences = payload.Execution, payload.Preferences.Normalized()
	return nil
}

func (s *conversationStore) readPayload(ctx context.Context, query nativeQueryer, id, contentID string) ([]byte, error) {
	var organization string
	if err := query.QueryRowContext(ctx, "SELECT organization_id FROM conversations WHERE id=?", id).Scan(&organization); err != nil {
		return nil, err
	}
	content, err := s.readConversationArchive(ctx, query, organization, contentID)
	if err != nil {
		return nil, err
	}
	var payload conversationContent
	if err := json.Unmarshal(content, &payload); err != nil {
		return nil, err
	}
	return payload.Data, nil
}

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
