package hubserver

import (
	"context"
	"database/sql"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubconversation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// conversationStore is the durable storage for the hosted conversation
// product. Transactional operations take the caller's *sql.Tx so that state
// and the events describing it commit together.
type conversationStore struct {
	db       *sql.DB
	reader   *sql.DB
	contents attachment.ArchiveContentStore
	database *database
}

func newConversationStore(db *sql.DB) *conversationStore {
	return &conversationStore{db: db}
}

const (
	conversationOriginUser   = hubconversation.OriginUser
	conversationOriginWorker = hubconversation.OriginWorker
)

type conversationScanner interface {
	Scan(dest ...any) error
}

// Message references.

func (s *conversationStore) operations() *hubconversation.Store {
	return hubconversation.NewStore(s.db, s.reader, s.contents, conversationPersistenceAdapter{store: s})
}

type conversationPersistenceAdapter struct{ store *conversationStore }

type conversationListQuery = hubconversation.ListQuery

type conversationNormalizeSummary = hubconversation.NormalizeSummary

type workItemReference = hubconversation.WorkItemReference

type errConversationRevision = hubconversation.RevisionError

type errConversationLinked = hubconversation.LinkedError

type errConversationStarted = hubconversation.StartedError

const conversationColumns = hubconversation.Columns

const conversationReadColumns = hubconversation.ReadColumns

const conversationMessageColumns = hubconversation.MessageColumns

const conversationMessageReadColumns = hubconversation.MessageReadColumns

const conversationQuestionColumns = hubconversation.QuestionColumns

func (s *conversationStore) createConversation(ctx context.Context, tx *sql.Tx, record *conversationRecord) error {
	return s.operations().CreateConversation(ctx, tx, record)
}

func (s *conversationStore) readConversation(ctx context.Context, query hubconversation.Queryer, organization tracker.OrganizationID, project tracker.ProjectID, id string) (conversationRecord, error) {
	return s.operations().ReadConversation(ctx, query, organization, project, id)
}

func (s *conversationStore) readConversationRow(ctx context.Context, query hubconversation.Queryer, organization tracker.OrganizationID, project tracker.ProjectID, id string) (conversationRecord, error) {
	return s.operations().ReadConversationRow(ctx, query, organization, project, id)
}

func (s *conversationStore) readConversationByID(ctx context.Context, query hubconversation.Queryer, id string) (conversationRecord, error) {
	return s.operations().ReadConversationByID(ctx, query, id)
}

func (s *conversationStore) listConversations(ctx context.Context, query hubconversation.Queryer, filter conversationListQuery) ([]conversationRecord, string, error) {
	return s.operations().ListConversations(ctx, query, filter)
}

func (s *conversationStore) updateConversation(ctx context.Context, tx *sql.Tx, record *conversationRecord, expectedRevision int64) error {
	return s.operations().UpdateConversation(ctx, tx, record, expectedRevision)
}

func (s *conversationStore) appendMessage(ctx context.Context, tx *sql.Tx, message *conversationMessageRecord) error {
	return s.operations().AppendMessage(ctx, tx, message)
}

func (s *conversationStore) updateMessage(ctx context.Context, tx *sql.Tx, message conversationMessageRecord) error {
	return s.operations().UpdateMessage(ctx, tx, message)
}

func (s *conversationStore) listMessages(ctx context.Context, query hubconversation.Queryer, conversationID string, beforeSeq int64, limit int) ([]conversationMessageRecord, error) {
	return s.operations().ListMessages(ctx, query, conversationID, beforeSeq, limit)
}

func (s *conversationStore) listMessageRows(ctx context.Context, query hubconversation.Queryer, conversationID string, beforeSeq int64, limit int) ([]conversationMessageRecord, error) {
	return s.operations().ListMessageRows(ctx, query, conversationID, beforeSeq, limit)
}

func (s *conversationStore) upsertQuestion(ctx context.Context, tx *sql.Tx, record conversationQuestionRecord) error {
	return s.operations().UpsertQuestion(ctx, tx, record)
}

func (s *conversationStore) readQuestion(ctx context.Context, query hubconversation.Queryer, conversationID, id string) (conversationQuestionRecord, error) {
	return s.operations().ReadQuestion(ctx, query, conversationID, id)
}

func (s *conversationStore) listPendingQuestions(ctx context.Context, query hubconversation.Queryer, conversationID string) ([]conversationQuestionRecord, error) {
	return s.operations().ListPendingQuestions(ctx, query, conversationID)
}

func (s *conversationStore) listSnapshotQuestions(ctx context.Context, query hubconversation.Queryer, conversationID, attemptID string, limit int) ([]conversationQuestionRecord, error) {
	return s.operations().ListSnapshotQuestions(ctx, query, conversationID, attemptID, limit)
}

func (s *conversationStore) listSnapshotQuestionRows(ctx context.Context, query hubconversation.Queryer, conversationID, attemptID string, limit int) ([]conversationQuestionRecord, error) {
	return s.operations().ListSnapshotQuestionRows(ctx, query, conversationID, attemptID, limit)
}

func (s *conversationStore) reserveCommand(ctx context.Context, tx *sql.Tx, conversationID, key string, kind conversation.CommandKind, requestHash string, now time.Time) (*conversation.Receipt, bool, error) {
	return s.operations().ReserveCommand(ctx, tx, conversationID, key, kind, requestHash, now)
}

func (s *conversationStore) updateReceipt(ctx context.Context, tx *sql.Tx, conversationID, key string, receipt conversation.Receipt) error {
	return s.operations().UpdateReceipt(ctx, tx, conversationID, key, receipt)
}

func (s *conversationStore) appendEvent(ctx context.Context, tx *sql.Tx, conversationID string, eventType conversation.EventType, body any, now time.Time) (int64, error) {
	return s.operations().AppendEvent(ctx, tx, conversationID, eventType, body, now)
}

func (s *conversationStore) listEvents(ctx context.Context, query hubconversation.Queryer, conversationID string, afterSeq int64, limit int) ([]conversation.Event, error) {
	return s.operations().ListEvents(ctx, query, conversationID, afterSeq, limit)
}

func (s *conversationStore) recordStart(ctx context.Context, tx *sql.Tx, attemptID, conversationID string, now time.Time) error {
	return s.operations().RecordStart(ctx, tx, attemptID, conversationID, now)
}

func (s *conversationStore) recordAudience(ctx context.Context, tx *sql.Tx, conversationID, actorPrincipalID string, from, to conversation.Visibility, now time.Time) error {
	return s.operations().RecordAudience(ctx, tx, conversationID, actorPrincipalID, from, to, now)
}

func (s *conversationStore) normalizeAfterRestart(ctx context.Context, now time.Time) (conversationNormalizeSummary, error) {
	return s.operations().NormalizeAfterRestart(ctx, now)
}

func (s *conversationStore) replaceMessageReferences(ctx context.Context, tx *sql.Tx, message conversationMessageRecord, now time.Time) error {
	return s.operations().ReplaceMessageReferences(ctx, tx, message, now)
}

func (s *conversationStore) listWorkItemReferences(ctx context.Context, query hubconversation.Queryer, organization tracker.OrganizationID, project tracker.ProjectID, workItemID string, limit int) ([]workItemReference, error) {
	return s.operations().ListWorkItemReferences(ctx, query, organization, project, workItemID, limit)
}

func (s *conversationStore) listSettleCandidates(ctx context.Context, query hubconversation.Queryer, before time.Time, limit int) ([]conversationRecord, error) {
	return s.operations().ListSettleCandidates(ctx, query, before, limit)
}

func (a conversationPersistenceAdapter) ValidateSubject(ctx context.Context, tx *sql.Tx, record conversationRecord) error {
	_, _, err := readNativeIssue(ctx, tx, nativeScope{organization: record.OrganizationID, project: record.ProjectID}, record.SubjectWorkItemID)
	return err
}

func (a conversationPersistenceAdapter) ArchiveConversationQuery(ctx context.Context, query hubconversation.Queryer, id string, before int64) (hubconversation.Queryer, error) {
	return archiveConversationQuery(ctx, query, id, before)
}

func (a conversationPersistenceAdapter) ConversationReadProjection(ctx context.Context, query hubconversation.Queryer, id string) string {
	return conversationReadProjection(ctx, query, id)
}

func (a conversationPersistenceAdapter) MarshalNative(value any) (string, error) {
	return marshalNative(value)
}

func (a conversationPersistenceAdapter) NativeInvalid(message string) error {
	return nativeInvalid(message)
}

func (a conversationPersistenceAdapter) NativeNotFound() error {
	return nativeNotFound()
}

func (a conversationPersistenceAdapter) NativePublicInvalid(message string) error {
	return nativePublicInvalid(message)
}

func (a conversationPersistenceAdapter) NullString(value string) any {
	return nullString(value)
}

func (a conversationPersistenceAdapter) ParseTimeValue(value string) (time.Time, error) {
	return parseTimeValue(value)
}

func (a conversationPersistenceAdapter) PayloadNativeID(ctx context.Context, prefix string) string {
	return payloadNativeID(ctx, prefix)
}

func (a conversationPersistenceAdapter) QuerySQL(ctx context.Context, query hubconversation.Queryer, statement string, args ...any) (*sql.Rows, error) {
	return querySQL(ctx, query, statement, args...)
}

func (a conversationPersistenceAdapter) ResolveConversationWorkItems(ctx context.Context, query hubconversation.Queryer, records []*conversationRecord) error {
	return resolveConversationWorkItems(ctx, query, records)
}

func (a conversationPersistenceAdapter) ProjectList(values []tracker.ProjectID) (any, error) {
	return sqlList(values)
}

func (a conversationPersistenceAdapter) MessageInserted(ctx context.Context) {
	if plan, ok := ctx.Value(conversationPublicationKey{}).(*conversationPublicationPlan); ok {
		plan.insertedMessages++
	}
}

func conversationTime(value time.Time) string { return hubconversation.Time(value) }

func conversationNullTime(value *time.Time) any { return hubconversation.NullTime(value) }

func decodeConversationReceipt(encoded string) (*conversation.Receipt, error) {
	return hubconversation.DecodeReceipt(encoded)
}

func scanConversation(row conversationScanner) (conversationRecord, error) {
	return hubconversation.NewStore(nil, nil, nil, conversationPersistenceAdapter{}).ScanConversation(row)
}

func scanConversationMessage(row conversationScanner) (conversationMessageRecord, error) {
	return hubconversation.NewStore(nil, nil, nil, conversationPersistenceAdapter{}).ScanMessage(row)
}

func scanConversationQuestion(row conversationScanner) (conversationQuestionRecord, error) {
	return hubconversation.NewStore(nil, nil, nil, conversationPersistenceAdapter{}).ScanQuestion(row)
}

func validateConversationRecord(record *conversationRecord) error {
	return hubconversation.NewStore(nil, nil, nil, conversationPersistenceAdapter{}).ValidateRecord(record)
}

func validateConversationMessage(message *conversationMessageRecord) error {
	return hubconversation.NewStore(nil, nil, nil, conversationPersistenceAdapter{}).ValidateMessage(message)
}

func (a conversationPersistenceAdapter) ParsedTime(value string) (*time.Time, error) {
	return parsedTime(value)
}

func (a conversationPersistenceAdapter) HasPublicationPlan(ctx context.Context) bool {
	return ctx.Value(conversationPublicationKey{}) != nil
}

func (a conversationPersistenceAdapter) ConversationStale(message string) error {
	return conversationStale(message)
}

func (a conversationPersistenceAdapter) RecordContentArchive(ctx context.Context, tx *sql.Tx, organization, project, uploader string, ref attachment.ArchiveReference, now time.Time) error {
	return a.store.recordContentArchive(ctx, tx, organization, project, uploader, ref, now)
}

func (a conversationPersistenceAdapter) ResolveStorageJSONWith(ctx context.Context, raw []byte, read func(context.Context, string, attachment.ArchiveReference) ([]byte, error)) ([]byte, error) {
	return resolveStorageJSONWith(ctx, raw, read)
}

func (a conversationPersistenceAdapter) StringList(values []string) (any, error) {
	return sqlList(values)
}
func (a conversationPersistenceAdapter) CursorExpired() error { return errConversationCursorExpired }

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
