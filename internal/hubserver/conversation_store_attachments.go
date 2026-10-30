package hubserver

import (
	"context"
	"database/sql"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubconversation"
)

type legacyToolRevisionKey = hubconversation.LegacyToolRevisionKey

type legacyToolRevision = hubconversation.LegacyToolRevision

func (s *conversationStore) readAttachment(ctx context.Context, query nativeQueryer, conversationID, id string) (conversationAttachmentRecord, error) {
	return s.operations().ReadAttachment(ctx, query, conversationID, id)
}

func (s *conversationStore) readAttachmentByRef(ctx context.Context, query nativeQueryer, ref string) (conversationAttachmentRecord, bool, error) {
	return s.operations().ReadAttachmentByRef(ctx, query, ref)
}

func (s *conversationStore) insertAttachment(ctx context.Context, tx *sql.Tx, record conversationAttachmentRecord, content []byte) error {
	return s.operations().InsertAttachment(ctx, tx, record, content)
}

func (s *conversationStore) readAttachmentContent(ctx context.Context, query nativeQueryer, ref string) ([]byte, error) {
	return s.operations().ReadAttachmentContent(ctx, query, ref)
}

func (s *conversationStore) deleteAttachment(ctx context.Context, tx *sql.Tx, id string, now time.Time) error {
	return s.operations().DeleteAttachment(ctx, tx, id, now)
}

func (s *conversationStore) bindAttachment(ctx context.Context, tx *sql.Tx, conversationID, principalID, attachmentID, messageID string) (bool, error) {
	return s.operations().BindAttachment(ctx, tx, conversationID, principalID, attachmentID, messageID)
}

func (s *conversationStore) listExpiredAttachments(ctx context.Context, query nativeQueryer, now time.Time, limit int) ([]conversationAttachmentRecord, error) {
	return s.operations().ListExpiredAttachments(ctx, query, now, limit)
}

func (s *conversationStore) prepareLegacyToolRevision(ctx context.Context, id, messageID string) (context.Context, error) {
	return s.operations().PrepareLegacyToolRevision(ctx, id, messageID)
}

func (s *conversationStore) deleteConversation(ctx context.Context, tx *sql.Tx, record conversationRecord) error {
	return s.operations().DeleteConversation(ctx, tx, record)
}

func (s *conversationStore) replayEvents(ctx context.Context, reader *sql.DB, id string, after int64) ([]conversation.Event, error) {
	return s.operations().ReplayEvents(ctx, reader, id, after)
}

func conversationAttachmentRef(conversationID, principalID, key string) string {
	return hubconversation.AttachmentRef(conversationID, principalID, key)
}

func resolveMessageAttachments(ctx context.Context, query nativeQueryer, messages []*conversationMessageRecord) error {
	return hubconversation.NewStore(nil, nil, nil, conversationPersistenceAdapter{}).ResolveMessageAttachments(ctx, query, messages)
}

func conversationDeltaBody(messageID string, seq int64, text string) map[string]any {
	return hubconversation.DeltaBody(messageID, seq, text)
}

func resolveMessageReferences(ctx context.Context, query nativeQueryer, messages []*conversationMessageRecord) error {
	return hubconversation.NewStore(nil, nil, nil, conversationPersistenceAdapter{}).ResolveMessageReferences(ctx, query, messages)
}
