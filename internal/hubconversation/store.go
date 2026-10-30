package hubconversation

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type Scanner interface{ Scan(...any) error }

type StoreHost interface {
	HasPublicationPlan(context.Context) bool
	ConversationDeltaBody(messageID string, seq int64, text string) map[string]any
	ConversationStale(message string) error
	ProjectMessage(record MessageRecord) MessageResource
	RecordContentArchive(ctx context.Context, tx *sql.Tx, organization, project, uploader string, ref attachment.ArchiveReference, now time.Time) error
	ResolveStorageJSONWith(ctx context.Context, raw []byte, read func(context.Context, string, attachment.ArchiveReference) ([]byte, error)) ([]byte, error)
	ParsedTime(string) (*time.Time, error)
	MessageInserted(context.Context)
	ValidateSubject(context.Context, *sql.Tx, Record) error
	ArchiveConversationQuery(ctx context.Context, query Queryer, id string, before int64) (Queryer, error)
	ConversationReadProjection(ctx context.Context, query Queryer, id string) string
	MarshalNative(value any) (string, error)
	NativeInvalid(message string) error
	NativeNotFound() error
	NativePublicInvalid(message string) error
	NullString(value string) any
	ParseTimeValue(value string) (time.Time, error)
	PayloadNativeID(ctx context.Context, prefix string) string
	PreserveLegacyToolRevision(ctx context.Context, tx *sql.Tx, message MessageRecord) error
	QuerySQL(ctx context.Context, query Queryer, statement string, args ...any) (*sql.Rows, error)
	ResolveConversationWorkItems(ctx context.Context, query Queryer, records []*Record) error
	ResolveMessageAttachments(ctx context.Context, query Queryer, messages []*MessageRecord) error
	ResolveMessageReferences(ctx context.Context, query Queryer, messages []*MessageRecord) error
	ProjectList(values []tracker.ProjectID) (any, error)
}

type Store struct {
	db       *sql.DB
	reader   *sql.DB
	contents attachment.ArchiveContentStore
	host     StoreHost
}

func NewStore(db, reader *sql.DB, contents attachment.ArchiveContentStore, host StoreHost) *Store {
	return &Store{db: db, reader: reader, contents: contents, host: host}
}

const (
	OriginUser                    = "user"
	OriginWorker                  = "worker"
	conversationSnapshotQuestions = 20
	conversationMessagePage       = 50
	conversationSettleBatch       = 100
	conversationReferencePage     = 100
)

// requireLinkAvailable validates a work item link in code: the issue must
// exist in the conversation's scope and no other conversation may already be
// its canonical conversation. The partial unique index is the backstop.
func (s *Store) requireLinkAvailable(ctx context.Context, tx *sql.Tx, record Record) error {
	if record.WorkItemID == "" {
		return nil
	}
	var exists int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM issues WHERE organization_id = ? AND project_id = ? AND native_id = ?", record.OrganizationID, record.ProjectID, record.WorkItemID).Scan(&exists); err != nil {
		return fmt.Errorf("check conversation issue: %w", err)
	}
	if exists == 0 {
		return s.host.NativeNotFound()
	}
	var existing string
	err := tx.QueryRowContext(ctx, "SELECT id FROM conversations WHERE organization_id = ? AND project_id = ? AND work_item_id = ? AND id != ?", record.OrganizationID, record.ProjectID, record.WorkItemID, record.ID).Scan(&existing)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("check conversation link: %w", err)
	default:
		return &LinkedError{ExistingConversationID: existing}
	}
}

func (s *Store) CreateConversation(ctx context.Context, tx *sql.Tx, record *Record) error {
	if err := s.ValidateRecord(record); err != nil {
		return err
	}
	if record.Revision == 0 {
		record.Revision = 1
	}
	if record.SubjectWorkItemID != "" {
		if err := s.host.ValidateSubject(ctx, tx, *record); err != nil {
			return err
		}
	}
	if err := s.requireLinkAvailable(ctx, tx, *record); err != nil {
		return err
	}
	contentID, err := s.ConversationRecordStorage(ctx, tx, *record)
	if err != nil {
		return err
	}
	execution, preferences := "{}", "{}"
	if _, err := tx.ExecContext(ctx, `INSERT INTO conversations (`+Columns+`,conversion_state)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,2)`,
		record.ID, record.OrganizationID, record.ProjectID, record.OwnerPrincipalID, record.OwnerSubject, record.Origin, record.Title, record.Visibility, record.Status,
		s.host.NullString(record.SubjectWorkItemID), s.host.NullString(record.WorkItemID), NullTime(record.LinkedAt), record.Revision, record.ProviderThreadID, record.ProviderThreadRunnerID, record.ProviderThreadOrigin, execution, record.EventSeq,
		preferences, Time(record.CreatedAt), Time(record.UpdatedAt), NullTime(record.LastMessageAt), NullTime(record.SettledAt), record.Archived, contentID,
	); err != nil {
		return fmt.Errorf("insert conversation: %w", LinkError(err))
	}
	if err := ExecutionQueryMetadata(record.Execution).Write(ctx, tx, "conversations", "id=?", record.ID); err != nil {
		return err
	}
	record.ContentID = contentID
	return nil
}

func (s *Store) ReadConversation(ctx context.Context, query Queryer, organization tracker.OrganizationID, project tracker.ProjectID, id string) (Record, error) {
	record, err := s.ReadConversationRow(ctx, query, organization, project, id)
	if err != nil {
		return record, err
	}
	if err := s.ResolveConversationRecord(ctx, query, &record); err != nil {
		return record, fmt.Errorf("read conversation: %w", err)
	}
	return record, nil
}

func (s *Store) ReadConversationRow(ctx context.Context, query Queryer, organization tracker.OrganizationID, project tracker.ProjectID, id string) (Record, error) {
	record, err := s.ScanConversation(query.QueryRowContext(ctx, "SELECT "+s.host.ConversationReadProjection(ctx, query, id)+" FROM conversations WHERE organization_id = ? AND project_id = ? AND id = ?", organization, project, id))
	if errors.Is(err, sql.ErrNoRows) {
		return record, s.host.NativeNotFound()
	}
	if err != nil {
		return record, fmt.Errorf("read conversation: %w", err)
	}
	if err := s.host.ResolveConversationWorkItems(ctx, query, []*Record{&record}); err != nil {
		return record, err
	}
	return record, nil
}

// readConversationByID reads a conversation by identifier alone. It is for
// paths that already hold the conversation's authority, such as a message
// update inside a bound turn; the scoped read stays readConversation.
func (s *Store) ReadConversationByID(ctx context.Context, query Queryer, id string) (Record, error) {
	record, err := s.ReadConversationRecord(ctx, query, query.QueryRowContext(ctx, "SELECT "+s.host.ConversationReadProjection(ctx, query, id)+" FROM conversations WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return record, s.host.NativeNotFound()
	}
	if err != nil {
		return record, fmt.Errorf("read conversation: %w", err)
	}
	return record, nil
}

func (s *Store) ListConversations(ctx context.Context, query Queryer, filter ListQuery) ([]Record, string, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	var where strings.Builder
	where.WriteString("organization_id = ? AND origin = 'user' AND archived = 0 AND (visibility = 'shared' OR owner_principal_id = ?)")
	where.WriteString(" AND (read_audience = '' OR read_audience = ?)")
	args := []any{filter.Organization, filter.Principal, filter.ReadAudience}
	if filter.Project != "" {
		where.WriteString(" AND project_id = ?")
		args = append(args, filter.Project)
	}
	if filter.Projects != nil {
		encoded, err := s.host.ProjectList(filter.Projects)
		if err != nil {
			return nil, "", fmt.Errorf("encode readable projects: %w", err)
		}
		where.WriteString(" AND project_id IN (SELECT value FROM sql_rows(?))")
		args = append(args, encoded)
	}
	if needle := strings.TrimSpace(filter.Title); needle != "" {
		where.WriteString(` AND lower(title) LIKE ? ESCAPE '\'`)
		args = append(args, TitleFilter(needle))
	}
	if filter.SubjectWorkItemID != "" {
		where.WriteString(" AND subject_work_item_id = ? AND owner_principal_id = ? AND visibility = 'private'")
		args = append(args, filter.SubjectWorkItemID, filter.Principal)
	}
	if filter.Settled != nil {
		status := conversation.StatusActive
		if *filter.Settled {
			status = conversation.StatusSettled
		}
		where.WriteString(" AND status = ?")
		args = append(args, status)
	}
	if filter.Cursor != "" {
		decoded, err := s.DecodeCursor(filter.Cursor)
		if err != nil {
			return nil, "", err
		}
		// The page continues after the cursor row in the sort order: a later
		// group, or the same group with older activity.
		where.WriteString(" AND (" + SettledRank + " > ? OR (" + SettledRank +
			" = ? AND (COALESCE(last_message_at, updated_at) < ? OR (COALESCE(last_message_at, updated_at) = ? AND id < ?))))")
		args = append(args, decoded.Settled, decoded.Settled, decoded.Activity, decoded.Activity, decoded.ID)
	}
	args = append(args, limit+1)
	rows, err := s.host.QuerySQL(ctx, query, "SELECT "+ReadColumns+" FROM conversations WHERE "+where.String()+
		" ORDER BY "+SettledRank+" ASC, COALESCE(last_message_at, updated_at) DESC, id DESC LIMIT ?", args...)
	if err != nil {
		return nil, "", fmt.Errorf("list conversations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	records := make([]Record, 0, limit)
	for rows.Next() {
		record, err := s.ScanConversation(rows)
		if err != nil {
			return nil, "", fmt.Errorf("list conversations: %w", err)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("list conversations: %w", err)
	}
	next := ""
	if len(records) > limit {
		records = records[:limit]
		next = EncodeCursor(records[limit-1])
	}
	if err := rows.Close(); err != nil {
		return nil, "", err
	}
	for i := range records {
		if err := s.ResolveConversationRecord(ctx, query, &records[i]); err != nil {
			return nil, "", err
		}
	}
	// The rows are drained before the follow-up query: the hub pool holds a
	// single connection.
	pointers := make([]*Record, 0, len(records))
	for i := range records {
		pointers = append(pointers, &records[i])
	}
	if err := s.host.ResolveConversationWorkItems(ctx, query, pointers); err != nil {
		return nil, "", err
	}
	return records, next, nil
}

// updateConversation writes every mutable column of record when the stored
// revision equals expectedRevision, and bumps the revision in place.
func (s *Store) UpdateConversation(ctx context.Context, tx *sql.Tx, record *Record, expectedRevision int64) error {
	if err := s.ValidateRecord(record); err != nil {
		return err
	}
	if err := s.requireLinkAvailable(ctx, tx, *record); err != nil {
		return err
	}
	contentID, err := s.ConversationRecordStorage(ctx, tx, *record)
	if err != nil {
		return err
	}
	execution, preferences := "{}", "{}"
	next := expectedRevision + 1
	result, err := tx.ExecContext(ctx, `UPDATE conversations SET owner_principal_id = ?, owner_subject = ?, title = ?, visibility = ?, status = ?,
 work_item_id = ?, linked_at = ?, revision = ?, provider_thread_id = ?, provider_thread_runner_id = ?, provider_thread_origin = ?, execution_json = ?, preferences_json = ?, updated_at = ?, last_message_at = ?, settled_at = ?, archived = ?, content_attachment_id = ?, conversion_state=2
WHERE id = ? AND organization_id = ? AND project_id = ? AND revision = ?`,
		record.OwnerPrincipalID, record.OwnerSubject, record.Title, record.Visibility, record.Status,
		s.host.NullString(record.WorkItemID), NullTime(record.LinkedAt), next, record.ProviderThreadID, record.ProviderThreadRunnerID, record.ProviderThreadOrigin, execution, preferences, Time(record.UpdatedAt), NullTime(record.LastMessageAt), NullTime(record.SettledAt), record.Archived, contentID,
		record.ID, record.OrganizationID, record.ProjectID, expectedRevision,
	)
	if err != nil {
		return fmt.Errorf("update conversation: %w", LinkError(err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("update conversation: %w", err)
	}
	if affected == 1 {
		record.Revision = next
		record.ContentID = contentID
		return ExecutionQueryMetadata(record.Execution).Write(ctx, tx, "conversations", "id=?", record.ID)
	}
	var current int64
	err = tx.QueryRowContext(ctx, "SELECT revision FROM conversations WHERE id = ? AND organization_id = ? AND project_id = ?", record.ID, record.OrganizationID, record.ProjectID).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return s.host.NativeNotFound()
	}
	if err != nil {
		return fmt.Errorf("read conversation revision: %w", err)
	}
	return &RevisionError{ConversationID: record.ID, Current: current}
}

// appendMessage allocates the next sequence number inside the transaction
// and records the conversation's last activity.
func (s *Store) AppendMessage(ctx context.Context, tx *sql.Tx, message *MessageRecord) error {
	if err := s.ValidateMessage(message); err != nil {
		return err
	}
	actor, err := s.host.MarshalNative(message.Actor)
	if err != nil {
		return fmt.Errorf("encode message actor: %w", err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT max(
 COALESCE((SELECT MAX(seq) FROM conversation_messages WHERE conversation_id=?),0),
 COALESCE((SELECT MAX(n.last_sequence) FROM native_archive_sections n JOIN conversations c ON c.id=n.subject_id AND c.organization_id=n.organization_id AND c.project_id=n.project_id AND c.work_item_id=n.work_item_id WHERE n.kind='conversation' AND n.subject_id=?),0)
)+1`, message.ConversationID, message.ConversationID).Scan(&message.Seq); err != nil {
		return fmt.Errorf("allocate message sequence: %w", err)
	}
	storedText, storedData, contentID, err := s.MessageStorage(ctx, tx, *message)
	if err != nil {
		return err
	}
	message.ContentID = contentID
	if contentID != "" {
		actor = "{}"
	}
	message.Revision = 1
	if _, err := tx.ExecContext(ctx, `INSERT INTO conversation_messages (`+MessageColumns+`)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		message.ID, message.ConversationID, message.Seq, message.Role, message.Kind, storedText, storedData, message.Delivery,
		message.AttemptID, message.ThreadID, message.TurnID, message.ProviderItemID, actor, message.CommandKey, Time(message.CreatedAt), Time(message.UpdatedAt), contentID, message.Revision,
	); err != nil {
		return fmt.Errorf("insert message: %w", err)
	}
	s.host.MessageInserted(ctx)
	metadata, err := MessageQueryMetadata(message.Data, message.Actor.Kind)
	if err != nil {
		return err
	}
	if contentID != "" {
		metadata.Add("actor_principal_id", message.Actor.PrincipalID)
	} else {
		metadata.Add("actor_principal_id", nil)
	}
	metadata.Add("text_bytes", len(message.Text))
	metadata.Add("stored_text_bytes", len(message.Text))
	if contentID != "" {
		metadata.Add("conversion_state", 2)
	}
	if err := metadata.Write(ctx, tx, "conversation_messages", "id=?", message.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE conversations SET last_message_at = ? WHERE id = ? AND (last_message_at IS NULL OR last_message_at < ?)", Time(message.CreatedAt), message.ConversationID, Time(message.CreatedAt)); err != nil {
		return fmt.Errorf("record conversation activity: %w", err)
	}
	return nil
}

func (s *Store) UpdateMessage(ctx context.Context, tx *sql.Tx, message MessageRecord) error {
	if err := s.ValidateMessage(&message); err != nil {
		return err
	}
	actor, err := s.host.MarshalNative(message.Actor)
	if err != nil {
		return fmt.Errorf("encode message actor: %w", err)
	}
	storedText, storedData, contentID, err := s.MessageStorage(ctx, tx, message)
	if err != nil {
		return err
	}
	if contentID != "" {
		actor = "{}"
	}
	if message.Kind == conversation.MessageTool && message.Actor.Kind == conversation.ActorCoordinator {
		if err := s.host.PreserveLegacyToolRevision(ctx, tx, message); err != nil {
			return err
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE conversation_messages SET text = ?, data_json = ?, delivery = ?, attempt_id = ?, thread_id = ?, turn_id = ?, provider_item_id = ?, actor_json = ?, updated_at = ?, content_attachment_id = ?, revision = revision+1
WHERE id = ? AND conversation_id = ?`,
		storedText, storedData, message.Delivery, message.AttemptID, message.ThreadID, message.TurnID, message.ProviderItemID, actor, Time(message.UpdatedAt), contentID,
		message.ID, message.ConversationID)
	if err != nil {
		return fmt.Errorf("update message: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("update message: %w", err)
	}
	if affected == 0 {
		return s.host.NativeNotFound()
	}
	metadata, err := MessageQueryMetadata(message.Data, message.Actor.Kind)
	if err != nil {
		return err
	}
	if contentID != "" {
		metadata.Add("actor_principal_id", message.Actor.PrincipalID)
	} else {
		metadata.Add("actor_principal_id", nil)
	}
	metadata.Add("text_bytes", len(message.Text))
	metadata.Add("stored_text_bytes", len(message.Text))
	if contentID != "" {
		metadata.Add("conversion_state", 2)
	}
	return metadata.Write(ctx, tx, "conversation_messages", "id=?", message.ID)
}

// listMessages returns up to limit messages with seq below beforeSeq (or the
// latest messages when beforeSeq is zero), ordered by seq ascending.
func (s *Store) ListMessages(ctx context.Context, query Queryer, conversationID string, beforeSeq int64, limit int) ([]MessageRecord, error) {
	var err error
	query, err = s.host.ArchiveConversationQuery(ctx, query, conversationID, beforeSeq)
	if err != nil {
		return nil, err
	}
	messages, err := s.ListMessageRows(ctx, query, conversationID, beforeSeq, limit)
	if err != nil {
		return nil, err
	}
	pointers := make([]*MessageRecord, 0, len(messages))
	for i := range messages {
		pointers = append(pointers, &messages[i])
	}
	if err := s.ResolveMessageContents(ctx, query, pointers); err != nil {
		return nil, err
	}
	return messages, nil
}

func (s *Store) ListMessageRows(ctx context.Context, query Queryer, conversationID string, beforeSeq int64, limit int) ([]MessageRecord, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	args := []any{conversationID}
	condition := ""
	if beforeSeq > 0 {
		condition = " AND seq < ?"
		args = append(args, beforeSeq)
	}
	args = append(args, limit)
	rows, err := query.QueryContext(ctx, "SELECT "+MessageReadColumns+" FROM conversation_messages WHERE conversation_id = ?"+condition+" ORDER BY seq DESC LIMIT ?", args...)
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	defer func() { _ = rows.Close() }()
	messages := make([]MessageRecord, 0, limit)
	for rows.Next() {
		message, err := s.ScanMessage(rows)
		if err != nil {
			return nil, fmt.Errorf("list messages: %w", err)
		}
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	for i, j := 0, len(messages)-1; i < j; i, j = i+1, j-1 {
		messages[i], messages[j] = messages[j], messages[i]
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	pointers := make([]*MessageRecord, 0, len(messages))
	for i := range messages {
		pointers = append(pointers, &messages[i])
	}
	if err := s.host.ResolveMessageReferences(ctx, query, pointers); err != nil {
		return nil, err
	}
	if err := s.host.ResolveMessageAttachments(ctx, query, pointers); err != nil {
		return nil, err
	}
	return messages, nil
}

// upsertQuestion inserts a question or replaces its mutable state.
func (s *Store) UpsertQuestion(ctx context.Context, tx *sql.Tx, record QuestionRecord) error {
	if err := conversation.ValidateQuestionID(record.ID); err != nil {
		return s.host.NativeInvalid(err.Error())
	}
	if err := conversation.ValidateConversationID(record.ConversationID); err != nil {
		return s.host.NativeInvalid(err.Error())
	}
	if !record.Status.Valid() {
		return s.host.NativePublicInvalid("Question status is not valid")
	}
	contentID, err := s.QuestionStorage(ctx, tx, record)
	if err != nil {
		return err
	}
	promptsJSON, answersJSON := "[]", "{}"
	if record.UpdatedAt.IsZero() {
		record.UpdatedAt = record.CreatedAt
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO conversation_questions (`+QuestionColumns+`,conversion_state)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,2)
ON CONFLICT (id) DO UPDATE SET message_id = excluded.message_id, request_id = excluded.request_id, attempt_id = excluded.attempt_id, thread_id = excluded.thread_id, turn_id = excluded.turn_id,
 status = excluded.status, prompts_json = excluded.prompts_json, answers_json = excluded.answers_json, answered_by = excluded.answered_by, expires_at = excluded.expires_at, updated_at = excluded.updated_at, content_attachment_id = excluded.content_attachment_id,conversion_state=2
WHERE conversation_id = excluded.conversation_id`,
		record.ID, record.ConversationID, record.MessageID, record.RequestID, record.Owner.AttemptID, record.Owner.ThreadID, record.Owner.TurnID, record.Status,
		promptsJSON, answersJSON, record.AnsweredBy, NullTime(record.ExpiresAt), Time(record.CreatedAt), Time(record.UpdatedAt), contentID,
	); err != nil {
		return fmt.Errorf("upsert question: %w", err)
	}
	return nil
}

func (s *Store) ReadQuestion(ctx context.Context, query Queryer, conversationID, id string) (QuestionRecord, error) {
	record, err := s.ScanQuestion(query.QueryRowContext(ctx, "SELECT "+QuestionColumns+" FROM conversation_questions WHERE conversation_id = ? AND id = ?", conversationID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return record, s.host.NativeNotFound()
	}
	if err != nil {
		return record, fmt.Errorf("read question: %w", err)
	}
	if err := s.ResolveQuestion(ctx, query, &record); err != nil {
		return record, err
	}
	return record, nil
}

// listPendingQuestions returns questions that still await an answer (pending,
// sending or sent), oldest first.
func (s *Store) ListPendingQuestions(ctx context.Context, query Queryer, conversationID string) ([]QuestionRecord, error) {
	rows, err := query.QueryContext(ctx, "SELECT "+QuestionColumns+" FROM conversation_questions WHERE conversation_id = ? AND status IN ('pending', 'sending', 'sent') ORDER BY created_at, id", conversationID)
	if err != nil {
		return nil, fmt.Errorf("list pending questions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var records []QuestionRecord
	for rows.Next() {
		record, err := s.ScanQuestion(rows)
		if err != nil {
			return nil, fmt.Errorf("list pending questions: %w", err)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list pending questions: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range records {
		if err := s.ResolveQuestion(ctx, query, &records[i]); err != nil {
			return nil, err
		}
	}
	return records, nil
}

// listSnapshotQuestions returns the questions a reloading client renders:
// every question still awaiting an answer plus the settled questions of the
// current attempt, so a locked card survives a reload. The newest limit
// questions are returned oldest first.
func (s *Store) ListSnapshotQuestions(ctx context.Context, query Queryer, conversationID, attemptID string, limit int) ([]QuestionRecord, error) {
	records, err := s.ListSnapshotQuestionRows(ctx, query, conversationID, attemptID, limit)
	if err != nil {
		return nil, err
	}
	for i := range records {
		if err := s.ResolveQuestion(ctx, query, &records[i]); err != nil {
			return nil, err
		}
	}
	return records, nil
}

func (s *Store) ListSnapshotQuestionRows(ctx context.Context, query Queryer, conversationID, attemptID string, limit int) ([]QuestionRecord, error) {
	if limit <= 0 {
		limit = conversationSnapshotQuestions
	}
	rows, err := query.QueryContext(ctx, "SELECT "+QuestionColumns+` FROM conversation_questions
WHERE conversation_id = ? AND (status IN ('pending', 'sending', 'sent') OR (? <> '' AND attempt_id = ?))
ORDER BY created_at DESC, id DESC LIMIT ?`, conversationID, attemptID, attemptID, limit)
	if err != nil {
		return nil, fmt.Errorf("list snapshot questions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var records []QuestionRecord
	for rows.Next() {
		record, err := s.ScanQuestion(rows)
		if err != nil {
			return nil, fmt.Errorf("list snapshot questions: %w", err)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list snapshot questions: %w", err)
	}
	slices.Reverse(records)
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return records, nil
}

// reserveCommand records the first use of an idempotency key with an initial
// saved receipt. A replay with the same payload returns the stored receipt;
// a different payload returns the stored receipt with conflict set.
func (s *Store) ReserveCommand(ctx context.Context, tx *sql.Tx, conversationID, key string, kind conversation.CommandKind, requestHash string, now time.Time) (*conversation.Receipt, bool, error) {
	if strings.TrimSpace(key) == "" || len(key) > conversation.MaxCommandKeyBytes {
		return nil, false, s.host.NativePublicInvalid("An idempotency key of at most 128 bytes is required")
	}
	if !kind.Valid() {
		return nil, false, s.host.NativePublicInvalid("Command kind is not valid")
	}
	var storedHash, encoded, contentID string
	err := tx.QueryRowContext(ctx, "SELECT request_hash, receipt_json, content_attachment_id FROM conversation_commands WHERE conversation_id = ? AND key = ?", conversationID, key).Scan(&storedHash, &encoded, &contentID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return nil, false, fmt.Errorf("read command: %w", err)
	default:
		receipt, err := s.DecodeReceipt(ctx, tx, conversationID, encoded, contentID)
		if err != nil {
			return nil, false, err
		}
		return receipt, storedHash != requestHash, nil
	}
	initial, err := s.host.MarshalNative(conversation.Receipt{Key: key, Kind: kind, Status: conversation.DeliverySaved, UpdatedAt: now.UTC()})
	if err != nil {
		return nil, false, fmt.Errorf("encode receipt: %w", err)
	}
	contentID, err = s.PayloadStorage(ctx, tx, conversationID, []byte(initial), now)
	if err != nil {
		return nil, false, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO conversation_commands (conversation_id, key, kind, request_hash, receipt_json, receipt_status, query_metadata_version, created_at, updated_at, content_attachment_id,conversion_state) VALUES (?, ?, ?, ?, '{}', ?, 1, ?, ?, ?,2)",
		conversationID, key, kind, requestHash, conversation.DeliverySaved, Time(now), Time(now), contentID); err != nil {
		return nil, false, fmt.Errorf("insert command: %w", err)
	}
	return nil, false, nil
}

func (s *Store) UpdateReceipt(ctx context.Context, tx *sql.Tx, conversationID, key string, receipt conversation.Receipt) error {
	if receipt.Key == "" {
		receipt.Key = key
	}
	if !receipt.Status.Valid() {
		return s.host.NativePublicInvalid("Receipt status is not valid")
	}
	encoded, err := s.host.MarshalNative(receipt)
	if err != nil {
		return fmt.Errorf("encode receipt: %w", err)
	}
	contentID, err := s.PayloadStorage(ctx, tx, conversationID, []byte(encoded), receipt.UpdatedAt)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, "UPDATE conversation_commands SET receipt_json = '{}', receipt_status = ?, query_metadata_version=1, updated_at = ?, content_attachment_id = ?, conversion_state=2 WHERE conversation_id = ? AND key = ?", receipt.Status, Time(receipt.UpdatedAt), contentID, conversationID, key)
	if err != nil {
		return fmt.Errorf("update receipt: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("update receipt: %w", err)
	}
	if affected == 0 {
		return s.host.NativeNotFound()
	}
	return nil
}

// appendEvent increments the conversation's event sequence and stores the
// event in the same transaction, returning the allocated sequence.
func (s *Store) AppendEvent(ctx context.Context, tx *sql.Tx, conversationID string, eventType conversation.EventType, body any, now time.Time) (int64, error) {
	if !eventType.Valid() {
		return 0, s.host.NativePublicInvalid("Event type is not valid")
	}
	if body == nil {
		body = map[string]any{}
	}
	encoded, err := s.host.MarshalNative(CompactEvent(body))
	if err != nil {
		return 0, fmt.Errorf("encode event: %w", err)
	}
	var seq int64
	err = tx.QueryRowContext(ctx, "UPDATE conversations SET event_seq = event_seq + 1 WHERE id = ? RETURNING event_seq", conversationID).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, s.host.NativeNotFound()
	}
	if err != nil {
		return 0, fmt.Errorf("allocate event sequence: %w", err)
	}
	stored, contentID, err := s.EventStorage(ctx, tx, conversationID, []byte(encoded), now)
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO conversation_events (conversation_id, seq, type, body_json, created_at, content_attachment_id, conversion_state) VALUES (?, ?, ?, ?, ?, ?, ?)", conversationID, seq, eventType, stored, Time(now), contentID, 2); err != nil {
		return 0, fmt.Errorf("insert event: %w", err)
	}
	metadata, err := EventQueryMetadata([]byte(encoded))
	if err != nil {
		return 0, err
	}
	if err := metadata.Write(ctx, tx, "conversation_events", "conversation_id=? AND seq=?", conversationID, seq); err != nil {
		return 0, err
	}
	return seq, nil
}

func (s *Store) ListEvents(ctx context.Context, query Queryer, conversationID string, afterSeq int64, limit int) ([]conversation.Event, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	rows, err := query.QueryContext(ctx, "SELECT seq, type, body_json, created_at, COALESCE(content_attachment_id,'') FROM conversation_events WHERE conversation_id = ? AND seq > ? ORDER BY seq LIMIT ?", conversationID, afterSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var events []conversation.Event
	var contents []string
	for rows.Next() {
		var event conversation.Event
		var body, created, contentID string
		if err := rows.Scan(&event.Seq, &event.Type, &body, &created, &contentID); err != nil {
			return nil, fmt.Errorf("list events: %w", err)
		}
		event.Data = json.RawMessage(body)
		if event.CreatedAt, err = s.host.ParseTimeValue(created); err != nil {
			return nil, fmt.Errorf("decode event %d created_at: %w", event.Seq, err)
		}
		events = append(events, event)
		contents = append(contents, contentID)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	for index, contentID := range contents {
		if contentID == "" {
			continue
		}
		data, err := s.ReadEventStorage(ctx, query, conversationID, contentID)
		if err != nil {
			return nil, err
		}
		events[index].Data = data
	}
	if err := s.ResolveEventContent(ctx, query, conversationID, events); err != nil {
		return nil, err
	}
	return events, nil
}

// recordStart records that an attempt started a conversation turn. One
// attempt starts at most one conversation.
func (s *Store) RecordStart(ctx context.Context, tx *sql.Tx, attemptID, conversationID string, now time.Time) error {
	if strings.TrimSpace(attemptID) == "" {
		return s.host.NativePublicInvalid("An attempt is required")
	}
	var existing string
	err := tx.QueryRowContext(ctx, "SELECT conversation_id FROM conversation_starts WHERE attempt_id = ?", attemptID).Scan(&existing)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return fmt.Errorf("read conversation start: %w", err)
	default:
		return &StartedError{AttemptID: attemptID, ConversationID: existing}
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO conversation_starts (attempt_id, conversation_id, created_at) VALUES (?, ?, ?)", attemptID, conversationID, Time(now)); err != nil {
		return fmt.Errorf("insert conversation start: %w", err)
	}
	return nil
}

func (s *Store) RecordAudience(ctx context.Context, tx *sql.Tx, conversationID, actorPrincipalID string, from, to conversation.Visibility, now time.Time) error {
	if !from.Valid() || !to.Valid() {
		return s.host.NativePublicInvalid("Audience visibility is not valid")
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO conversation_audience_events (id, conversation_id, actor_principal_id, from_visibility, to_visibility, created_at) VALUES (?, ?, ?, ?, ?, ?)",
		s.host.PayloadNativeID(ctx, "aud"), conversationID, actorPrincipalID, from, to, Time(now)); err != nil {
		return fmt.Errorf("insert audience event: %w", err)
	}
	return nil
}

func (s *Store) NormalizeAfterRestart(ctx context.Context, now time.Time) (NormalizeSummary, error) {
	var summary NormalizeSummary
	materialized, err := s.MaterializeRestartedMessages(ctx, now)
	if err != nil {
		return summary, err
	}
	summary.Messages = materialized
	summary.Conversations, summary.Receipts, err = s.NormalizeRestartedRecords(ctx, now)
	if err != nil {
		return summary, err
	}
	for _, statement := range []struct {
		query string
		args  []any
		count *int64
	}{
		{"UPDATE conversation_messages SET delivery=?,updated_at=? WHERE delivery IN (?,?,?)", []any{conversation.DeliveryUnknown, Time(now), conversation.DeliverySending, conversation.DeliverySent, conversation.DeliveryResponding}, &summary.Messages},
		{"UPDATE conversation_questions SET status=?,updated_at=? WHERE status=?", []any{conversation.QuestionExpired, Time(now), conversation.QuestionPending}, &summary.Questions},
		{"UPDATE conversation_questions SET status=?,updated_at=? WHERE status=?", []any{conversation.QuestionUnknown, Time(now), conversation.QuestionSending}, &summary.Questions},
	} {
		result, err := s.db.ExecContext(ctx, statement.query+" AND conversation_id IN (SELECT id FROM conversations WHERE work_item_id IS NULL)", statement.args...)
		if err != nil {
			return summary, err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return summary, err
		}
		*statement.count += count
	}
	return summary, nil
}

// replaceMessageReferences rewrites the stored references of one message.
// Re-extraction is idempotent: an assistant message that completes after its
// text streamed in replaces the rows its earlier text produced.
func (s *Store) ReplaceMessageReferences(ctx context.Context, tx *sql.Tx, message MessageRecord, now time.Time) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM message_references WHERE message_id = ?", message.ID); err != nil {
		return fmt.Errorf("clear message references: %w", err)
	}
	for _, reference := range message.References {
		if !reference.Kind.Valid() || strings.TrimSpace(reference.ID) == "" {
			return s.host.NativePublicInvalid("A message reference requires a known kind and a target")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO message_references (message_id, conversation_id, target_kind, target_id, label, created_at)
VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (message_id, target_kind, target_id) DO NOTHING`,
			message.ID, message.ConversationID, reference.Kind, reference.ID, reference.Label, Time(now)); err != nil {
			return fmt.Errorf("insert message reference: %w", err)
		}
	}
	return nil
}

// listWorkItemReferences returns the messages that reference a work item,
// newest first, with the conversation audience of each so the caller can
// drop the ones the reader may not see.
func (s *Store) ListWorkItemReferences(ctx context.Context, query Queryer, organization tracker.OrganizationID, project tracker.ProjectID, workItemID string, limit int) ([]WorkItemReference, error) {
	if limit <= 0 {
		limit = conversationReferencePage
	}
	rows, err := query.QueryContext(ctx, `SELECT r.message_id, r.conversation_id, m.text, r.created_at, c.visibility, c.owner_principal_id, c.read_audience, m.content_attachment_id, m.role, COALESCE(m.text_bytes,length(CAST(m.text AS BLOB)))
FROM message_references r
JOIN conversation_messages m ON m.id = r.message_id
JOIN conversations c ON c.id = r.conversation_id
WHERE r.target_kind = 'issue' AND r.target_id = ? AND c.organization_id = ? AND c.project_id = ?
ORDER BY r.created_at DESC, r.message_id DESC LIMIT ?`, workItemID, organization, project, limit)
	if err != nil {
		return nil, fmt.Errorf("list work item references: %w", err)
	}
	defer func() { _ = rows.Close() }()
	references := make([]WorkItemReference, 0, limit)
	var messages []*MessageRecord
	for rows.Next() {
		var reference WorkItemReference
		var message MessageRecord
		var created string
		if err := rows.Scan(&reference.MessageID, &reference.ConversationID, &reference.Excerpt, &created, &reference.Visibility, &reference.OwnerPrincipalID, &reference.ReadAudience, &message.ContentID, &message.Role, &message.TextBytes); err != nil {
			return nil, fmt.Errorf("list work item references: %w", err)
		}
		if reference.CreatedAt, err = s.host.ParseTimeValue(created); err != nil {
			return nil, fmt.Errorf("decode reference %s created_at: %w", reference.MessageID, err)
		}
		message.ID, message.ConversationID, message.Text = reference.MessageID, reference.ConversationID, reference.Excerpt
		messages = append(messages, &message)
		references = append(references, reference)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("list work item references: %w", err)
	}
	if err := s.ResolveMessageContents(WithArchiveReads(ctx), query, messages); err != nil {
		return nil, err
	}
	for index, message := range messages {
		references[index].Excerpt = message.Text
	}
	return references, nil
}

// listSettleCandidates returns the active conversations whose execution has
// ended or never started and whose last activity is older than before. The
// sweep settles them one transaction at a time, so the list is bounded.
func (s *Store) ListSettleCandidates(ctx context.Context, query Queryer, before time.Time, limit int) ([]Record, error) {
	if limit <= 0 {
		limit = conversationSettleBatch
	}
	rows, err := query.QueryContext(ctx, "SELECT "+ReadColumns+` FROM conversations
WHERE status = 'active' AND COALESCE(last_message_at, updated_at) < ?
 AND (execution_status IS NULL OR execution_status IN (?, ?, ?, ?, ?))
ORDER BY COALESCE(last_message_at, updated_at) LIMIT ?`, Time(before),
		conversation.ExecutionIdle, conversation.ExecutionCompleted, conversation.ExecutionInterrupted,
		conversation.ExecutionFailed, conversation.ExecutionUnknown, limit)
	if err != nil {
		return nil, fmt.Errorf("list settle candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	records := make([]Record, 0, limit)
	for rows.Next() {
		record, err := s.ScanConversation(rows)
		if err != nil {
			return nil, fmt.Errorf("list settle candidates: %w", err)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list settle candidates: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	settled := records[:0]
	for i := range records {
		if err := s.ResolveConversationRecord(ctx, query, &records[i]); err != nil {
			return nil, err
		}
		if records[i].Execution.Status == conversation.ExecutionIdle || records[i].Execution.Status.Terminal() {
			settled = append(settled, records[i])
		}
	}
	return settled, nil
}

// RevisionError reports a compare-and-set failure on a conversation.
type RevisionError struct {
	ConversationID string
	Current        int64
}

func (e *RevisionError) Error() string {
	return fmt.Sprintf("conversation %s revision conflict: current %d", e.ConversationID, e.Current)
}

// LinkedError reports a violated one-issue-one-conversation constraint.
type LinkedError struct {
	ExistingConversationID string
}

func (e *LinkedError) Error() string {
	return "conversation_already_linked: " + e.ExistingConversationID
}

// StartedError reports an attempt that already started a conversation turn.
type StartedError struct {
	AttemptID      string
	ConversationID string
}

func (e *StartedError) Error() string {
	return fmt.Sprintf("attempt %s already started conversation %s", e.AttemptID, e.ConversationID)
}

const Columns = `id, organization_id, project_id, owner_principal_id, owner_subject, origin, title, visibility, status,
 subject_work_item_id, work_item_id, linked_at, revision, provider_thread_id, provider_thread_runner_id, provider_thread_origin, execution_json, event_seq, preferences_json, created_at, updated_at, last_message_at, settled_at, archived, content_attachment_id`

// MessageCount counts the conversation's whole history for the
// resource's message_count. It is a correlated
// subquery rather than a second query because the hub pool holds a single
// connection and a read must stay one statement.
const MessageCount = `((SELECT count(*) FROM conversation_messages cm WHERE cm.conversation_id = conversations.id)
+ COALESCE((SELECT sum(n.retired_rows) FROM native_archive_sections n WHERE n.organization_id=conversations.organization_id AND n.project_id=conversations.project_id AND n.work_item_id=conversations.work_item_id AND n.kind='conversation' AND n.subject_id=conversations.id),0))`

// ReadColumns is Columns plus the derived columns a
// read reports. Only Columns names real columns, so it is the
// list an INSERT uses.
const ReadColumns = Columns + ", " + MessageCount + ", read_audience"

// SettledRank is the leading sort key: active conversations come
// before settled ones.
const SettledRank = "CASE WHEN status = 'settled' THEN 1 ELSE 0 END"

// ListQuery is the filter a listing applies. Everything is
// pushed into SQL: the hub pool holds a single connection, so a page must
// not be filtered in Go and then re-fetched.
type ListQuery struct {
	Organization tracker.OrganizationID
	// Project restricts to one project. Empty lists across the
	// organization, subject to Projects.
	Project tracker.ProjectID
	// Projects restricts an organization-wide listing to the projects the
	// actor can read. A nil slice means every project; an empty non-nil
	// slice means none.
	Projects     []tracker.ProjectID
	Principal    string
	ReadAudience string
	Cursor       string
	Limit        int
	// Settled restricts the listing to settled or to active conversations.
	// Nil lists both, active first.
	Settled *bool
	// Title is a case-insensitive substring the title must contain.
	Title             string
	SubjectWorkItemID string
}

const MessageColumns = `id, conversation_id, seq, role, kind, text, data_json, delivery, attempt_id, thread_id, turn_id, provider_item_id, actor_json, command_key, created_at, updated_at, content_attachment_id, revision`

const MessageReadColumns = `id, conversation_id, seq, role, kind, text, data_json, delivery, attempt_id, thread_id, turn_id, provider_item_id, CASE WHEN actor_principal_id IS NULL THEN actor_json ELSE '{}' END, command_key, created_at, updated_at, content_attachment_id, revision, actor_kind, actor_principal_id, COALESCE(text_bytes,length(CAST(text AS BLOB))), stored_text_bytes`

const QuestionColumns = `id, conversation_id, message_id, request_id, attempt_id, thread_id, turn_id, status, prompts_json, answers_json, answered_by, expires_at, created_at, updated_at, content_attachment_id`

// NormalizeSummary counts the rows restart normalization
// rewrote, so the hub can report what a restart changed.
type NormalizeSummary struct {
	Conversations int64
	Messages      int64
	Questions     int64
	Receipts      int64
}

// WorkItemReference is one "referenced from" entry the target's activity
// shows: which message in which conversation named it, and when.
type WorkItemReference struct {
	MessageID      string
	ConversationID string
	Excerpt        string
	CreatedAt      time.Time
	// Visibility and OwnerPrincipalID let the caller apply the conversation
	// read rule: a private chat is invisible to anyone but its owner.
	Visibility       conversation.Visibility
	OwnerPrincipalID string
	ReadAudience     string
}

type Cursor struct {
	Activity string `json:"a"`
	ID       string `json:"i"`
	// Settled is the sort group of the page's last row: 0 active, 1 settled.
	// A listing without a settled filter sorts active first, so the cursor
	// has to carry the group as well as the activity.
	Settled int `json:"s"`
}

// Time renders timestamps for the conversation tables with a
// fixed width so that ordering and cursor comparison in SQL match time
// order. The value stays RFC 3339 and parses with parseTimeValue.
func Time(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
}

func NullTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return Time(*value)
}

func conversationRank(record Record) int {
	if record.Status == conversation.StatusSettled {
		return 1
	}
	return 0
}

func EncodeCursor(record Record) string {
	activity := record.UpdatedAt
	if record.LastMessageAt != nil {
		activity = *record.LastMessageAt
	}
	encoded, err := json.Marshal(Cursor{Activity: Time(activity), ID: record.ID, Settled: conversationRank(record)})
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(encoded)
}

// TitleFilter renders the LIKE pattern for a title substring,
// escaping the wildcards so that a search for "100%" is not a prefix match.
func TitleFilter(needle string) string {
	escaped := strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(strings.ToLower(needle))
	return "%" + escaped + "%"
}

func LinkError(err error) error {
	if err != nil && strings.Contains(err.Error(), "conversations_work_item_idx") {
		return &LinkedError{}
	}
	return err
}

func DecodeReceipt(encoded string) (*conversation.Receipt, error) {
	var receipt conversation.Receipt
	if err := json.Unmarshal([]byte(encoded), &receipt); err != nil {
		return nil, fmt.Errorf("decode receipt: %w", err)
	}
	return &receipt, nil
}

func (s *Store) ParseNullTime(value sql.NullString) (*time.Time, error) {
	if !value.Valid || strings.TrimSpace(value.String) == "" {
		var none *time.Time
		return none, nil
	}
	return s.host.ParsedTime(value.String)
}

func (s *Store) ScanConversation(row Scanner) (Record, error) {
	var record Record
	var subject, workItem, linkedAt, lastMessageAt, settledAt sql.NullString
	var execution, preferences, created, updated string
	if err := row.Scan(
		&record.ID, &record.OrganizationID, &record.ProjectID, &record.OwnerPrincipalID, &record.OwnerSubject, &record.Origin, &record.Title, &record.Visibility, &record.Status,
		&subject, &workItem, &linkedAt, &record.Revision, &record.ProviderThreadID, &record.ProviderThreadRunnerID, &record.ProviderThreadOrigin, &execution, &record.EventSeq, &preferences, &created, &updated, &lastMessageAt, &settledAt, &record.Archived, &record.ContentID,
		&record.MessageCount, &record.ReadAudience,
	); err != nil {
		return record, err
	}
	record.SubjectWorkItemID = subject.String
	record.WorkItemID = workItem.String
	if err := json.Unmarshal([]byte(execution), &record.Execution); err != nil {
		return record, fmt.Errorf("decode conversation %s execution: %w", record.ID, err)
	}
	if err := json.Unmarshal([]byte(preferences), &record.Preferences); err != nil {
		return record, fmt.Errorf("decode conversation %s preferences: %w", record.ID, err)
	}
	record.Preferences = record.Preferences.Normalized()
	var err error
	if record.CreatedAt, err = s.host.ParseTimeValue(created); err != nil {
		return record, fmt.Errorf("decode conversation %s created_at: %w", record.ID, err)
	}
	if record.UpdatedAt, err = s.host.ParseTimeValue(updated); err != nil {
		return record, fmt.Errorf("decode conversation %s updated_at: %w", record.ID, err)
	}
	if record.LinkedAt, err = s.ParseNullTime(linkedAt); err != nil {
		return record, fmt.Errorf("decode conversation %s linked_at: %w", record.ID, err)
	}
	if record.LastMessageAt, err = s.ParseNullTime(lastMessageAt); err != nil {
		return record, fmt.Errorf("decode conversation %s last_message_at: %w", record.ID, err)
	}
	if record.SettledAt, err = s.ParseNullTime(settledAt); err != nil {
		return record, fmt.Errorf("decode conversation %s settled_at: %w", record.ID, err)
	}
	return record, nil
}

func (s *Store) ScanMessage(row Scanner) (MessageRecord, error) {
	var message MessageRecord
	var data, actor, created, updated string
	var actorKind, actorPrincipal sql.NullString
	if err := row.Scan(&message.ID, &message.ConversationID, &message.Seq, &message.Role, &message.Kind, &message.Text, &data, &message.Delivery, &message.AttemptID, &message.ThreadID, &message.TurnID, &message.ProviderItemID, &actor, &message.CommandKey, &created, &updated, &message.ContentID, &message.Revision, &actorKind, &actorPrincipal, &message.TextBytes, &message.StoredTextBytes); err != nil {
		return message, err
	}
	message.Data = json.RawMessage(data)
	if err := json.Unmarshal([]byte(actor), &message.Actor); err != nil {
		return message, fmt.Errorf("decode message %s actor: %w", message.ID, err)
	}
	if actorPrincipal.Valid {
		message.Actor = conversation.Actor{Kind: conversation.ActorKind(actorKind.String), PrincipalID: actorPrincipal.String}
	}
	var err error
	if message.CreatedAt, err = s.host.ParseTimeValue(created); err != nil {
		return message, fmt.Errorf("decode message %s created_at: %w", message.ID, err)
	}
	if message.UpdatedAt, err = s.host.ParseTimeValue(updated); err != nil {
		return message, fmt.Errorf("decode message %s updated_at: %w", message.ID, err)
	}
	return message, nil
}

func (s *Store) ScanQuestion(row Scanner) (QuestionRecord, error) {
	var record QuestionRecord
	var prompts, answers, created, updated string
	var expires sql.NullString
	if err := row.Scan(&record.ID, &record.ConversationID, &record.MessageID, &record.RequestID, &record.Owner.AttemptID, &record.Owner.ThreadID, &record.Owner.TurnID, &record.Status, &prompts, &answers, &record.AnsweredBy, &expires, &created, &updated, &record.ContentID); err != nil {
		return record, err
	}
	if err := json.Unmarshal([]byte(prompts), &record.Prompts); err != nil {
		return record, fmt.Errorf("decode question %s prompts: %w", record.ID, err)
	}
	if err := json.Unmarshal([]byte(answers), &record.Answers); err != nil {
		return record, fmt.Errorf("decode question %s answers: %w", record.ID, err)
	}
	var err error
	if record.ExpiresAt, err = s.ParseNullTime(expires); err != nil {
		return record, fmt.Errorf("decode question %s expires_at: %w", record.ID, err)
	}
	if record.CreatedAt, err = s.host.ParseTimeValue(created); err != nil {
		return record, fmt.Errorf("decode question %s created_at: %w", record.ID, err)
	}
	if record.UpdatedAt, err = s.host.ParseTimeValue(updated); err != nil {
		return record, fmt.Errorf("decode question %s updated_at: %w", record.ID, err)
	}
	return record, nil
}

func (s *Store) ValidateRecord(record *Record) error {
	if record.ID == "" {
		record.ID = conversation.NewConversationID()
	}
	if err := conversation.ValidateConversationID(record.ID); err != nil {
		return s.host.NativeInvalid(err.Error())
	}
	if record.OrganizationID == "" || record.ProjectID == "" {
		return s.host.NativePublicInvalid("A conversation requires an organization and project")
	}
	if strings.TrimSpace(record.OwnerPrincipalID) == "" {
		return s.host.NativePublicInvalid("A conversation requires an owner principal")
	}
	if record.Origin == "" {
		record.Origin = OriginUser
	}
	if record.Origin != OriginUser && record.Origin != OriginWorker {
		return s.host.NativePublicInvalid("Conversation origin is not valid")
	}
	if !record.Visibility.Valid() {
		return s.host.NativePublicInvalid("Conversation visibility is not valid")
	}
	if record.SubjectWorkItemID != "" && (record.WorkItemID != "" || record.Visibility != conversation.VisibilityPrivate) {
		return s.host.NativePublicInvalid("Issue questions must remain private and unlinked")
	}
	if !record.Status.Valid() {
		return s.host.NativePublicInvalid("Conversation status is not valid")
	}
	if record.Execution.Status == "" {
		record.Execution.Status = conversation.ExecutionIdle
	}
	if !record.Execution.Status.Valid() {
		return s.host.NativePublicInvalid("Conversation execution status is not valid")
	}
	if record.WorkItemID != "" && record.LinkedAt == nil {
		return s.host.NativePublicInvalid("A linked conversation requires linked_at")
	}
	record.Preferences = record.Preferences.Normalized()
	return nil
}

func (s *Store) ValidateMessage(message *MessageRecord) error {
	if message.ID == "" {
		message.ID = conversation.NewMessageID()
	}
	if err := conversation.ValidateMessageID(message.ID); err != nil {
		return s.host.NativeInvalid(err.Error())
	}
	if err := conversation.ValidateConversationID(message.ConversationID); err != nil {
		return s.host.NativeInvalid(err.Error())
	}
	if !message.Role.Valid() || !message.Kind.Valid() || !message.Delivery.Valid() || !message.Actor.Kind.Valid() {
		return s.host.NativePublicInvalid("Message role, kind, delivery and actor kind must be valid")
	}
	if len(message.Data) == 0 {
		message.Data = json.RawMessage("{}")
	}
	if !json.Valid(message.Data) {
		return s.host.NativePublicInvalid("Message data must be valid JSON")
	}
	if message.UpdatedAt.IsZero() {
		message.UpdatedAt = message.CreatedAt
	}
	return nil
}

func (s *Store) DecodeCursor(value string) (Cursor, error) {
	var cursor Cursor
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return cursor, s.host.NativePublicInvalid("Cursor is not valid")
	}
	if err := json.Unmarshal(decoded, &cursor); err != nil || cursor.Activity == "" || cursor.ID == "" || cursor.Settled < 0 || cursor.Settled > 1 {
		return cursor, s.host.NativePublicInvalid("Cursor is not valid")
	}
	if _, err := s.host.ParseTimeValue(cursor.Activity); err != nil {
		return cursor, s.host.NativePublicInvalid("Cursor is not valid")
	}
	return cursor, nil
}
