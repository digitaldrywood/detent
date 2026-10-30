package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
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

func (s *conversationStore) payloadStorage(ctx context.Context, tx *sql.Tx, id string, raw []byte, now time.Time) (string, error) {
	_, _, contentID, err := s.messageStorage(ctx, tx, conversationMessageRecord{ConversationID: id, Data: raw, CreatedAt: now})
	return contentID, err
}

func (s *conversationStore) preparedRecordStorage(ctx context.Context, tx *sql.Tx, organization, project, uploader string, content []byte, now time.Time) (string, error) {
	key := organization + "/" + artifact.Digest(content)
	prepared, ok := ctx.Value(conversationContentsKey{}).(preparedConversationContents)
	ref, found := prepared[key]
	if !ok || !found {
		ref, found = conversationArchiveCache(ctx).reference(key)
	}
	if !found {
		return "", errors.New("conversation payload was not published before the transaction")
	}
	if err := ref.Validate(organization); err != nil {
		return "", err
	}
	if err := s.recordContentArchive(ctx, tx, organization, project, uploader, ref, now); err != nil {
		return "", err
	}
	return contentArchiveID(organization, ref), nil
}

func (s *conversationStore) conversationRecordStorage(ctx context.Context, tx *sql.Tx, record conversationRecord) (string, error) {
	raw, err := recordPayload(record)
	if err != nil {
		return "", err
	}
	content, err := messageContent(conversationMessageRecord{Data: raw})
	if err != nil {
		return "", err
	}
	return s.preparedRecordStorage(ctx, tx, string(record.OrganizationID), string(record.ProjectID), record.OwnerPrincipalID, content, record.CreatedAt)
}

func (s *conversationStore) questionStorage(ctx context.Context, tx *sql.Tx, record conversationQuestionRecord) (string, error) {
	raw, err := questionPayload(record)
	if err != nil {
		return "", err
	}
	return s.payloadStorage(ctx, tx, record.ConversationID, raw, record.CreatedAt)
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

func (s *conversationStore) readConversationRecord(ctx context.Context, query nativeQueryer, row conversationScanner) (conversationRecord, error) {
	record, err := scanConversation(row)
	if err != nil {
		return record, err
	}
	err = s.resolveConversationRecord(ctx, query, &record)
	return record, err
}

func (s *conversationStore) decodeReceipt(ctx context.Context, query nativeQueryer, id, raw, contentID string) (*conversation.Receipt, error) {
	if contentID != "" {
		content, err := s.readPayload(ctx, query, id, contentID)
		if err != nil {
			return nil, err
		}
		raw = string(content)
	}
	return decodeConversationReceipt(raw)
}

func (s *conversationStore) resolveQuestion(ctx context.Context, query nativeQueryer, record *conversationQuestionRecord) error {
	if record.ContentID == "" {
		return nil
	}
	raw, err := s.readPayload(ctx, query, record.ConversationID, record.ContentID)
	if err != nil {
		return err
	}
	var payload conversationQuestionContent
	if err := json.Unmarshal(raw, &payload); err != nil {
		return err
	}
	record.Prompts, record.Answers = payload.Prompts, payload.Answers
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
