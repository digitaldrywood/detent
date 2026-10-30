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

func (s *conversationStore) operations() *hubconversation.Store {
	return hubconversation.newStore(s.db, s.reader, s.contents, conversationPersistenceAdapter{store: s})
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
	return hubconversation.newStore(nil, nil, nil, conversationPersistenceAdapter{}).ScanConversation(row)
}

func scanConversationMessage(row conversationScanner) (conversationMessageRecord, error) {
	return hubconversation.newStore(nil, nil, nil, conversationPersistenceAdapter{}).ScanMessage(row)
}

func scanConversationQuestion(row conversationScanner) (conversationQuestionRecord, error) {
	return hubconversation.newStore(nil, nil, nil, conversationPersistenceAdapter{}).ScanQuestion(row)
}

func validateConversationRecord(record *conversationRecord) error {
	return hubconversation.newStore(nil, nil, nil, conversationPersistenceAdapter{}).ValidateRecord(record)
}

func validateConversationMessage(message *conversationMessageRecord) error {
	return hubconversation.newStore(nil, nil, nil, conversationPersistenceAdapter{}).ValidateMessage(message)
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
