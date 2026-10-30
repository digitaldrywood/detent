package hubserver

import (
	"context"
	"database/sql"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubconversation"
	"time"
)

var errConversationPublicationUncertain = hubconversation.ErrPublicationUncertain

type conversationContent = hubconversation.Content

type preparedConversationContents = hubconversation.PreparedContents

type conversationContentsKey = hubconversation.ContentsKey

type conversationArchiveReadsKey = hubconversation.ArchiveReadsKey

type conversationArchiveReads = hubconversation.ArchiveReads

type conversationReadBatch struct{ *hubconversation.ReadBatch }



type conversationDeltaFact = hubconversation.DeltaFact







func (s *conversationStore) publishPayload(ctx context.Context, organization string, content []byte) (attachment.ArchiveReference, error) {
	return s.operations().PublishPayload(ctx, organization, content)
}

func (s *conversationStore) preparePayload(ctx context.Context, organization string, content []byte) (context.Context, error) {
	return s.operations().PreparePayload(ctx, organization, content)
}

func (s *conversationStore) preparePayloads(ctx context.Context, organization string, contents ...[]byte) (context.Context, error) {
	return s.operations().PreparePayloads(ctx, organization, contents...)
}



func (s *conversationStore) resolveMessageContents(ctx context.Context, query nativeQueryer, messages []*conversationMessageRecord) error {
	return s.operations().ResolveMessageContents(ctx, query, messages)
}



func (s *conversationStore) readConversationArchive(ctx context.Context, query nativeQueryer, organization, contentID string) ([]byte, error) {
	return s.operations().ReadConversationArchive(ctx, query, organization, contentID)
}







func (s *conversationStore) readEventStorage(ctx context.Context, q nativeQueryer, id, contentID string) ([]byte, error) {
	return s.operations().ReadEventStorage(ctx, q, id, contentID)
}



func (s *conversationStore) preparedRecordStorage(ctx context.Context, tx *sql.Tx, organization, project, uploader string, content []byte, now time.Time) (string, error) {
	return s.operations().PreparedRecordStorage(ctx, tx, organization, project, uploader, content, now)
}





func (s *conversationStore) resolveConversationRecord(ctx context.Context, query nativeQueryer, record *conversationRecord) error {
	return s.operations().ResolveConversationRecord(ctx, query, record)
}

func (s *conversationStore) readConversationRecord(ctx context.Context, query nativeQueryer, row conversationScanner) (conversationRecord, error) {
	return s.operations().ReadConversationRecord(ctx, query, row)
}

func (s *conversationStore) decodeReceipt(ctx context.Context, query nativeQueryer, id, raw, contentID string) (*conversation.Receipt, error) {
	return s.operations().DecodeReceipt(ctx, query, id, raw, contentID)
}

func (s *conversationStore) resolveQuestion(ctx context.Context, query nativeQueryer, record *conversationQuestionRecord) error {
	return s.operations().ResolveQuestion(ctx, query, record)
}











func (b *conversationReadBatch) capture(ctx context.Context, query nativeQueryer, contentID string) error {
	return b.Capture(ctx, query, contentID)
}

func (b *conversationReadBatch) captureMessages(ctx context.Context, query nativeQueryer, messages []conversationMessageRecord) error {
	return b.CaptureMessages(ctx, query, messages)
}

func (b *conversationReadBatch) hydrateRecord(ctx context.Context, store *conversationStore, record *conversationRecord) error {
	return b.HydrateRecord(ctx, store.operations(), record)
}

func (b *conversationReadBatch) hydrateMessages(ctx context.Context, store *conversationStore, messages []conversationMessageRecord) error {
	return b.HydrateMessages(ctx, store.operations(), messages)
}

func (b *conversationReadBatch) hydrateQuestions(ctx context.Context, store *conversationStore, questions []conversationQuestionRecord) error {
	return b.HydrateQuestions(ctx, store.operations(), questions)
}

func scanAttachmentArchive(row interface{ Scan(...any) error }) (attachment.ArchiveReference, error) {
	return hubconversation.ScanArchive(row)
}

func messageContent(message conversationMessageRecord) ([]byte, error) {
	return hubconversation.MessageContent(message)
}

func withConversationArchiveReads(ctx context.Context) context.Context {
	return hubconversation.WithArchiveReads(ctx)
}

func conversationArchiveCache(ctx context.Context) *conversationArchiveReads {
	return hubconversation.ArchiveCache(ctx)
}

func newConversationArchiveReads(ctx context.Context) context.Context {
	return hubconversation.NewArchiveReads(ctx)
}

func contentArchiveID(organization string, ref attachment.ArchiveReference) string {
	return hubconversation.ContentArchiveID(organization, ref)
}

func newConversationReadBatch(organization string) *conversationReadBatch {
	return &conversationReadBatch{ReadBatch: hubconversation.NewReadBatch(organization)}
}

func compactConversationEvent(body any) any { return hubconversation.CompactEvent(body) }

func recordPayload(record conversationRecord) ([]byte, error) {
	return hubconversation.RecordPayload(record)
}

func questionPayload(record conversationQuestionRecord) ([]byte, error) {
	return hubconversation.QuestionPayload(record)
}

func rollbackConversationPreparation(tx *sql.Tx) error {
	return hubconversation.RollbackPreparation(tx)
}
