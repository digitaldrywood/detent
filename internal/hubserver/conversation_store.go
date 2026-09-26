package hubserver

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// conversationStore is the durable storage for the hosted conversation
// product. Transactional operations take the caller's *sql.Tx so that state
// and the events describing it commit together.
type conversationStore struct {
	db *sql.DB
}

func newConversationStore(db *sql.DB) *conversationStore {
	return &conversationStore{db: db}
}

// errConversationRevision reports a compare-and-set failure on a conversation.
type errConversationRevision struct {
	ConversationID string
	Current        int64
}

func (e *errConversationRevision) Error() string {
	return fmt.Sprintf("conversation %s revision conflict: current %d", e.ConversationID, e.Current)
}

// errConversationLinked reports a violated one-issue-one-conversation constraint.
type errConversationLinked struct {
	ExistingConversationID string
}

func (e *errConversationLinked) Error() string {
	return "conversation_already_linked: " + e.ExistingConversationID
}

// errConversationStarted reports an attempt that already started a conversation turn.
type errConversationStarted struct {
	AttemptID      string
	ConversationID string
}

func (e *errConversationStarted) Error() string {
	return fmt.Sprintf("attempt %s already started conversation %s", e.AttemptID, e.ConversationID)
}

func conversationInvalidCursor() error {
	return &nativeError{Code: "invalid_request", Message: "Cursor is not valid", status: http.StatusUnprocessableEntity}
}

type conversationRecord struct {
	ID               string
	OrganizationID   tracker.OrganizationID
	ProjectID        tracker.ProjectID
	OwnerPrincipalID string
	OwnerSubject     string
	Title            string
	Visibility       conversation.Visibility
	Status           conversation.Status
	WorkItemID       string
	LinkedAt         *time.Time
	// WorkItem caches the linked issue summary the projections report. It
	// is filled by resolveConversationWorkItems on every read.
	WorkItem         *conversationWorkItem
	Revision         int64
	ProviderThreadID string
	// ProviderThreadRunnerID names the runner whose provider login produced
	// ProviderThreadID. It is empty for the transitional hub-side
	// coordinator, so no runner resumes a thread it cannot reach.
	ProviderThreadRunnerID string
	// ProviderThreadOrigin names the kind of turn that produced
	// ProviderThreadID: conversation.ThreadOriginCoordinator or
	// conversation.ThreadOriginWorker. A thread carries the instructions and
	// the permission set of the turn that opened it, so only a turn of the
	// same kind resumes it (decisions section 9.3). It is empty for threads
	// recorded before the column existed, which therefore never resume.
	ProviderThreadOrigin string
	Execution            conversation.Execution
	EventSeq             int64
	CreatedAt            time.Time
	UpdatedAt            time.Time
	LastMessageAt        *time.Time
	// SettledAt is when the conversation last became settled, nil while it
	// is active (decisions section 14).
	SettledAt *time.Time
	// Preferences are the conversation's turn preferences. Every field is
	// "auto" until the user overrides it.
	Preferences conversation.Preferences
	// MessageCount is the conversation's whole history, filled by every
	// read so the audience preview can report it (decisions section 10.5).
	MessageCount int64
}

type conversationMessageRecord struct {
	ID             string
	ConversationID string
	Seq            int64
	Role           conversation.Role
	Kind           conversation.MessageKind
	Text           string
	Data           json.RawMessage
	Delivery       conversation.Delivery
	AttemptID      string
	ThreadID       string
	TurnID         string
	ProviderItemID string
	Actor          conversation.Actor
	CommandKey     string
	// References are the issues and conversations the message names, filled
	// by every read that projects the message (decisions section 14).
	References []conversationMessageReference
	// Attachments are the uploads bound to the message, filled by the same
	// reads as References (decisions section 17.1).
	Attachments []conversationAttachmentRecord
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// conversationMessageReference is one stored, resolved reference: the hub
// only records targets it could resolve, so a row always names something
// that exists.
type conversationMessageReference struct {
	Kind  conversation.ReferenceKind
	ID    string
	Label string
}

type conversationQuestionRecord struct {
	conversation.Question
	RequestID string
}

// conversationTime renders timestamps for the conversation tables with a
// fixed width so that ordering and cursor comparison in SQL match time
// order. The value stays RFC 3339 and parses with parseTimeValue.
func conversationTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
}

func conversationNullTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return conversationTime(*value)
}

func conversationParseNullTime(value sql.NullString) (*time.Time, error) {
	if !value.Valid || strings.TrimSpace(value.String) == "" {
		var none *time.Time
		return none, nil
	}
	return parsedTime(value.String)
}

const conversationColumns = `id, organization_id, project_id, owner_principal_id, owner_subject, title, visibility, status,
 work_item_id, linked_at, revision, provider_thread_id, provider_thread_runner_id, provider_thread_origin, execution_json, event_seq, preferences_json, created_at, updated_at, last_message_at, settled_at`

// conversationMessageCount counts the conversation's whole history for the
// resource's message_count (decisions section 10.5). It is a correlated
// subquery rather than a second query because the hub pool holds a single
// connection and a read must stay one statement.
const conversationMessageCount = `(SELECT count(*) FROM conversation_messages cm WHERE cm.conversation_id = conversations.id)`

// conversationReadColumns is conversationColumns plus the derived columns a
// read reports. Only conversationColumns names real columns, so it is the
// list an INSERT uses.
const conversationReadColumns = conversationColumns + ", " + conversationMessageCount

type conversationScanner interface {
	Scan(dest ...any) error
}

func scanConversation(row conversationScanner) (conversationRecord, error) {
	var record conversationRecord
	var workItem, linkedAt, lastMessageAt, settledAt sql.NullString
	var execution, preferences, created, updated string
	if err := row.Scan(
		&record.ID, &record.OrganizationID, &record.ProjectID, &record.OwnerPrincipalID, &record.OwnerSubject, &record.Title, &record.Visibility, &record.Status,
		&workItem, &linkedAt, &record.Revision, &record.ProviderThreadID, &record.ProviderThreadRunnerID, &record.ProviderThreadOrigin, &execution, &record.EventSeq, &preferences, &created, &updated, &lastMessageAt, &settledAt,
		&record.MessageCount,
	); err != nil {
		return record, err
	}
	record.WorkItemID = workItem.String
	if err := json.Unmarshal([]byte(execution), &record.Execution); err != nil {
		return record, fmt.Errorf("decode conversation %s execution: %w", record.ID, err)
	}
	if err := json.Unmarshal([]byte(preferences), &record.Preferences); err != nil {
		return record, fmt.Errorf("decode conversation %s preferences: %w", record.ID, err)
	}
	record.Preferences = record.Preferences.Normalized()
	var err error
	if record.CreatedAt, err = parseTimeValue(created); err != nil {
		return record, fmt.Errorf("decode conversation %s created_at: %w", record.ID, err)
	}
	if record.UpdatedAt, err = parseTimeValue(updated); err != nil {
		return record, fmt.Errorf("decode conversation %s updated_at: %w", record.ID, err)
	}
	if record.LinkedAt, err = conversationParseNullTime(linkedAt); err != nil {
		return record, fmt.Errorf("decode conversation %s linked_at: %w", record.ID, err)
	}
	if record.LastMessageAt, err = conversationParseNullTime(lastMessageAt); err != nil {
		return record, fmt.Errorf("decode conversation %s last_message_at: %w", record.ID, err)
	}
	if record.SettledAt, err = conversationParseNullTime(settledAt); err != nil {
		return record, fmt.Errorf("decode conversation %s settled_at: %w", record.ID, err)
	}
	return record, nil
}

func validateConversationRecord(record *conversationRecord) error {
	if record.ID == "" {
		record.ID = conversation.NewConversationID()
	}
	if err := conversation.ValidateConversationID(record.ID); err != nil {
		return nativeInvalid(err.Error())
	}
	if record.OrganizationID == "" || record.ProjectID == "" {
		return nativeInvalid("A conversation requires an organization and project")
	}
	if strings.TrimSpace(record.OwnerPrincipalID) == "" {
		return nativeInvalid("A conversation requires an owner principal")
	}
	if !record.Visibility.Valid() {
		return nativeInvalid("Conversation visibility is not valid")
	}
	if !record.Status.Valid() {
		return nativeInvalid("Conversation status is not valid")
	}
	if record.Execution.Status == "" {
		record.Execution.Status = conversation.ExecutionIdle
	}
	if !record.Execution.Status.Valid() {
		return nativeInvalid("Conversation execution status is not valid")
	}
	if record.WorkItemID != "" && record.LinkedAt == nil {
		return nativeInvalid("A linked conversation requires linked_at")
	}
	record.Preferences = record.Preferences.Normalized()
	return nil
}

// requireLinkAvailable validates a work item link in code: the issue must
// exist in the conversation's scope and no other conversation may already be
// its canonical conversation. The partial unique index is the backstop.
func (s *conversationStore) requireLinkAvailable(ctx context.Context, tx *sql.Tx, record conversationRecord) error {
	if record.WorkItemID == "" {
		return nil
	}
	var exists int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM issues WHERE organization_id = ? AND project_id = ? AND native_id = ?", record.OrganizationID, record.ProjectID, record.WorkItemID).Scan(&exists); err != nil {
		return fmt.Errorf("check conversation issue: %w", err)
	}
	if exists == 0 {
		return nativeNotFound()
	}
	var existing string
	err := tx.QueryRowContext(ctx, "SELECT id FROM conversations WHERE organization_id = ? AND project_id = ? AND work_item_id = ? AND id != ?", record.OrganizationID, record.ProjectID, record.WorkItemID, record.ID).Scan(&existing)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("check conversation link: %w", err)
	default:
		return &errConversationLinked{ExistingConversationID: existing}
	}
}

func conversationLinkError(err error) error {
	if err != nil && strings.Contains(err.Error(), "conversations_work_item_idx") {
		return &errConversationLinked{}
	}
	return err
}

func (s *conversationStore) createConversation(ctx context.Context, tx *sql.Tx, record *conversationRecord) error {
	if err := validateConversationRecord(record); err != nil {
		return err
	}
	if record.Revision == 0 {
		record.Revision = 1
	}
	if err := s.requireLinkAvailable(ctx, tx, *record); err != nil {
		return err
	}
	execution, err := marshalNative(record.Execution)
	if err != nil {
		return fmt.Errorf("encode conversation execution: %w", err)
	}
	preferences, err := marshalNative(record.Preferences)
	if err != nil {
		return fmt.Errorf("encode conversation preferences: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO conversations (`+conversationColumns+`)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		record.ID, record.OrganizationID, record.ProjectID, record.OwnerPrincipalID, record.OwnerSubject, record.Title, record.Visibility, record.Status,
		nullString(record.WorkItemID), conversationNullTime(record.LinkedAt), record.Revision, record.ProviderThreadID, record.ProviderThreadRunnerID, record.ProviderThreadOrigin, execution, record.EventSeq,
		preferences, conversationTime(record.CreatedAt), conversationTime(record.UpdatedAt), conversationNullTime(record.LastMessageAt), conversationNullTime(record.SettledAt),
	); err != nil {
		return fmt.Errorf("insert conversation: %w", conversationLinkError(err))
	}
	return nil
}

func (s *conversationStore) readConversation(ctx context.Context, query nativeQueryer, organization tracker.OrganizationID, project tracker.ProjectID, id string) (conversationRecord, error) {
	record, err := scanConversation(query.QueryRowContext(ctx, "SELECT "+conversationReadColumns+" FROM conversations WHERE organization_id = ? AND project_id = ? AND id = ?", organization, project, id))
	if errors.Is(err, sql.ErrNoRows) {
		return record, nativeNotFound()
	}
	if err != nil {
		return record, fmt.Errorf("read conversation: %w", err)
	}
	if err := resolveConversationWorkItems(ctx, query, []*conversationRecord{&record}); err != nil {
		return record, err
	}
	return record, nil
}

// readConversationByID reads a conversation by identifier alone. It is for
// paths that already hold the conversation's authority, such as a message
// update inside a bound turn; the scoped read stays readConversation.
func (s *conversationStore) readConversationByID(ctx context.Context, query nativeQueryer, id string) (conversationRecord, error) {
	record, err := scanConversation(query.QueryRowContext(ctx, "SELECT "+conversationReadColumns+" FROM conversations WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return record, nativeNotFound()
	}
	if err != nil {
		return record, fmt.Errorf("read conversation: %w", err)
	}
	return record, nil
}

type conversationCursor struct {
	Activity string `json:"a"`
	ID       string `json:"i"`
	// Settled is the sort group of the page's last row: 0 active, 1 settled.
	// A listing without a settled filter sorts active first, so the cursor
	// has to carry the group as well as the activity (decisions section 14).
	Settled int `json:"s"`
}

// conversationSettledRank is the leading sort key: active conversations come
// before settled ones.
const conversationSettledRank = "CASE WHEN status = 'settled' THEN 1 ELSE 0 END"

func conversationRank(record conversationRecord) int {
	if record.Status == conversation.StatusSettled {
		return 1
	}
	return 0
}

func encodeConversationCursor(record conversationRecord) string {
	activity := record.UpdatedAt
	if record.LastMessageAt != nil {
		activity = *record.LastMessageAt
	}
	encoded, err := json.Marshal(conversationCursor{Activity: conversationTime(activity), ID: record.ID, Settled: conversationRank(record)})
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(encoded)
}

func decodeConversationCursor(value string) (conversationCursor, error) {
	var cursor conversationCursor
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return cursor, conversationInvalidCursor()
	}
	if err := json.Unmarshal(decoded, &cursor); err != nil || cursor.Activity == "" || cursor.ID == "" || cursor.Settled < 0 || cursor.Settled > 1 {
		return cursor, conversationInvalidCursor()
	}
	if _, err := parseTimeValue(cursor.Activity); err != nil {
		return cursor, conversationInvalidCursor()
	}
	return cursor, nil
}

// conversationListQuery is the filter a listing applies. Everything is
// pushed into SQL: the hub pool holds a single connection, so a page must
// not be filtered in Go and then re-fetched.
type conversationListQuery struct {
	Organization tracker.OrganizationID
	// Project restricts to one project. Empty lists across the
	// organization, subject to Projects.
	Project tracker.ProjectID
	// Projects restricts an organization-wide listing to the projects the
	// actor can read. A nil slice means every project; an empty non-nil
	// slice means none.
	Projects  []tracker.ProjectID
	Principal string
	Cursor    string
	Limit     int
	// Settled restricts the listing to settled or to active conversations.
	// Nil lists both, active first (decisions section 14).
	Settled *bool
	// Title is a case-insensitive substring the title must contain.
	Title string
}

// conversationTitleFilter renders the LIKE pattern for a title substring,
// escaping the wildcards so that a search for "100%" is not a prefix match.
func conversationTitleFilter(needle string) string {
	escaped := strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(strings.ToLower(needle))
	return "%" + escaped + "%"
}

// listConversations returns the conversations visible to principal: the
// principal's own private conversations and every shared conversation in
// scope, most recent activity first. The returned cursor is empty on the
// last page.
func (s *conversationStore) listConversations(ctx context.Context, query nativeQueryer, filter conversationListQuery) ([]conversationRecord, string, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	var where strings.Builder
	where.WriteString("organization_id = ? AND (visibility = 'shared' OR owner_principal_id = ?)")
	args := []any{filter.Organization, filter.Principal}
	if filter.Project != "" {
		where.WriteString(" AND project_id = ?")
		args = append(args, filter.Project)
	}
	if filter.Projects != nil {
		encoded, err := marshalNative(filter.Projects)
		if err != nil {
			return nil, "", fmt.Errorf("encode readable projects: %w", err)
		}
		where.WriteString(" AND project_id IN (SELECT value FROM json_each(?))")
		args = append(args, encoded)
	}
	if needle := strings.TrimSpace(filter.Title); needle != "" {
		where.WriteString(` AND lower(title) LIKE ? ESCAPE '\'`)
		args = append(args, conversationTitleFilter(needle))
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
		decoded, err := decodeConversationCursor(filter.Cursor)
		if err != nil {
			return nil, "", err
		}
		// The page continues after the cursor row in the sort order: a later
		// group, or the same group with older activity.
		where.WriteString(" AND (" + conversationSettledRank + " > ? OR (" + conversationSettledRank +
			" = ? AND (COALESCE(last_message_at, updated_at) < ? OR (COALESCE(last_message_at, updated_at) = ? AND id < ?))))")
		args = append(args, decoded.Settled, decoded.Settled, decoded.Activity, decoded.Activity, decoded.ID)
	}
	args = append(args, limit+1)
	rows, err := query.QueryContext(ctx, "SELECT "+conversationReadColumns+" FROM conversations WHERE "+where.String()+
		" ORDER BY "+conversationSettledRank+" ASC, COALESCE(last_message_at, updated_at) DESC, id DESC LIMIT ?", args...)
	if err != nil {
		return nil, "", fmt.Errorf("list conversations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	records := make([]conversationRecord, 0, limit)
	for rows.Next() {
		record, err := scanConversation(rows)
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
		next = encodeConversationCursor(records[limit-1])
	}
	// The rows are drained before the follow-up query: the hub pool holds a
	// single connection.
	pointers := make([]*conversationRecord, 0, len(records))
	for i := range records {
		pointers = append(pointers, &records[i])
	}
	if err := resolveConversationWorkItems(ctx, query, pointers); err != nil {
		return nil, "", err
	}
	return records, next, nil
}

// updateConversation writes every mutable column of record when the stored
// revision equals expectedRevision, and bumps the revision in place.
func (s *conversationStore) updateConversation(ctx context.Context, tx *sql.Tx, record *conversationRecord, expectedRevision int64) error {
	if err := validateConversationRecord(record); err != nil {
		return err
	}
	if err := s.requireLinkAvailable(ctx, tx, *record); err != nil {
		return err
	}
	execution, err := marshalNative(record.Execution)
	if err != nil {
		return fmt.Errorf("encode conversation execution: %w", err)
	}
	preferences, err := marshalNative(record.Preferences)
	if err != nil {
		return fmt.Errorf("encode conversation preferences: %w", err)
	}
	next := expectedRevision + 1
	result, err := tx.ExecContext(ctx, `UPDATE conversations SET owner_principal_id = ?, owner_subject = ?, title = ?, visibility = ?, status = ?,
 work_item_id = ?, linked_at = ?, revision = ?, provider_thread_id = ?, provider_thread_runner_id = ?, provider_thread_origin = ?, execution_json = ?, preferences_json = ?, updated_at = ?, last_message_at = ?, settled_at = ?
WHERE id = ? AND organization_id = ? AND project_id = ? AND revision = ?`,
		record.OwnerPrincipalID, record.OwnerSubject, record.Title, record.Visibility, record.Status,
		nullString(record.WorkItemID), conversationNullTime(record.LinkedAt), next, record.ProviderThreadID, record.ProviderThreadRunnerID, record.ProviderThreadOrigin, execution, preferences, conversationTime(record.UpdatedAt), conversationNullTime(record.LastMessageAt), conversationNullTime(record.SettledAt),
		record.ID, record.OrganizationID, record.ProjectID, expectedRevision,
	)
	if err != nil {
		return fmt.Errorf("update conversation: %w", conversationLinkError(err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("update conversation: %w", err)
	}
	if affected == 1 {
		record.Revision = next
		return nil
	}
	var current int64
	err = tx.QueryRowContext(ctx, "SELECT revision FROM conversations WHERE id = ? AND organization_id = ? AND project_id = ?", record.ID, record.OrganizationID, record.ProjectID).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return nativeNotFound()
	}
	if err != nil {
		return fmt.Errorf("read conversation revision: %w", err)
	}
	return &errConversationRevision{ConversationID: record.ID, Current: current}
}

const conversationMessageColumns = `id, conversation_id, seq, role, kind, text, data_json, delivery, attempt_id, thread_id, turn_id, provider_item_id, actor_json, command_key, created_at, updated_at`

func scanConversationMessage(row conversationScanner) (conversationMessageRecord, error) {
	var message conversationMessageRecord
	var data, actor, created, updated string
	if err := row.Scan(&message.ID, &message.ConversationID, &message.Seq, &message.Role, &message.Kind, &message.Text, &data, &message.Delivery, &message.AttemptID, &message.ThreadID, &message.TurnID, &message.ProviderItemID, &actor, &message.CommandKey, &created, &updated); err != nil {
		return message, err
	}
	message.Data = json.RawMessage(data)
	if err := json.Unmarshal([]byte(actor), &message.Actor); err != nil {
		return message, fmt.Errorf("decode message %s actor: %w", message.ID, err)
	}
	var err error
	if message.CreatedAt, err = parseTimeValue(created); err != nil {
		return message, fmt.Errorf("decode message %s created_at: %w", message.ID, err)
	}
	if message.UpdatedAt, err = parseTimeValue(updated); err != nil {
		return message, fmt.Errorf("decode message %s updated_at: %w", message.ID, err)
	}
	return message, nil
}

func validateConversationMessage(message *conversationMessageRecord) error {
	if message.ID == "" {
		message.ID = conversation.NewMessageID()
	}
	if err := conversation.ValidateMessageID(message.ID); err != nil {
		return nativeInvalid(err.Error())
	}
	if err := conversation.ValidateConversationID(message.ConversationID); err != nil {
		return nativeInvalid(err.Error())
	}
	if !message.Role.Valid() || !message.Kind.Valid() || !message.Delivery.Valid() || !message.Actor.Kind.Valid() {
		return nativeInvalid("Message role, kind, delivery and actor kind must be valid")
	}
	if len(message.Data) == 0 {
		message.Data = json.RawMessage("{}")
	}
	if !json.Valid(message.Data) {
		return nativeInvalid("Message data must be valid JSON")
	}
	if message.UpdatedAt.IsZero() {
		message.UpdatedAt = message.CreatedAt
	}
	return nil
}

// appendMessage allocates the next sequence number inside the transaction
// and records the conversation's last activity.
func (s *conversationStore) appendMessage(ctx context.Context, tx *sql.Tx, message *conversationMessageRecord) error {
	if err := validateConversationMessage(message); err != nil {
		return err
	}
	actor, err := marshalNative(message.Actor)
	if err != nil {
		return fmt.Errorf("encode message actor: %w", err)
	}
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(seq), 0) + 1 FROM conversation_messages WHERE conversation_id = ?", message.ConversationID).Scan(&message.Seq); err != nil {
		return fmt.Errorf("allocate message sequence: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO conversation_messages (`+conversationMessageColumns+`)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		message.ID, message.ConversationID, message.Seq, message.Role, message.Kind, message.Text, string(message.Data), message.Delivery,
		message.AttemptID, message.ThreadID, message.TurnID, message.ProviderItemID, actor, message.CommandKey, conversationTime(message.CreatedAt), conversationTime(message.UpdatedAt),
	); err != nil {
		return fmt.Errorf("insert message: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "UPDATE conversations SET last_message_at = ? WHERE id = ? AND (last_message_at IS NULL OR last_message_at < ?)", conversationTime(message.CreatedAt), message.ConversationID, conversationTime(message.CreatedAt)); err != nil {
		return fmt.Errorf("record conversation activity: %w", err)
	}
	return nil
}

func (s *conversationStore) updateMessage(ctx context.Context, tx *sql.Tx, message conversationMessageRecord) error {
	if err := validateConversationMessage(&message); err != nil {
		return err
	}
	actor, err := marshalNative(message.Actor)
	if err != nil {
		return fmt.Errorf("encode message actor: %w", err)
	}
	result, err := tx.ExecContext(ctx, `UPDATE conversation_messages SET text = ?, data_json = ?, delivery = ?, attempt_id = ?, thread_id = ?, turn_id = ?, provider_item_id = ?, actor_json = ?, updated_at = ?
WHERE id = ? AND conversation_id = ?`,
		message.Text, string(message.Data), message.Delivery, message.AttemptID, message.ThreadID, message.TurnID, message.ProviderItemID, actor, conversationTime(message.UpdatedAt),
		message.ID, message.ConversationID)
	if err != nil {
		return fmt.Errorf("update message: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("update message: %w", err)
	}
	if affected == 0 {
		return nativeNotFound()
	}
	return nil
}

// listMessages returns up to limit messages with seq below beforeSeq (or the
// latest messages when beforeSeq is zero), ordered by seq ascending.
func (s *conversationStore) listMessages(ctx context.Context, query nativeQueryer, conversationID string, beforeSeq int64, limit int) ([]conversationMessageRecord, error) {
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
	rows, err := query.QueryContext(ctx, "SELECT "+conversationMessageColumns+" FROM conversation_messages WHERE conversation_id = ?"+condition+" ORDER BY seq DESC LIMIT ?", args...)
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	defer func() { _ = rows.Close() }()
	messages := make([]conversationMessageRecord, 0, limit)
	for rows.Next() {
		message, err := scanConversationMessage(rows)
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
	// The rows are drained before the follow-up query: the hub pool holds a
	// single connection.
	pointers := make([]*conversationMessageRecord, 0, len(messages))
	for i := range messages {
		pointers = append(pointers, &messages[i])
	}
	if err := resolveMessageReferences(ctx, query, pointers); err != nil {
		return nil, err
	}
	if err := resolveMessageAttachments(ctx, query, pointers); err != nil {
		return nil, err
	}
	return messages, nil
}

const conversationQuestionColumns = `id, conversation_id, message_id, request_id, attempt_id, thread_id, turn_id, status, prompts_json, answers_json, answered_by, expires_at, created_at, updated_at`

func scanConversationQuestion(row conversationScanner) (conversationQuestionRecord, error) {
	var record conversationQuestionRecord
	var prompts, answers, created, updated string
	var expires sql.NullString
	if err := row.Scan(&record.ID, &record.ConversationID, &record.MessageID, &record.RequestID, &record.Owner.AttemptID, &record.Owner.ThreadID, &record.Owner.TurnID, &record.Status, &prompts, &answers, &record.AnsweredBy, &expires, &created, &updated); err != nil {
		return record, err
	}
	if err := json.Unmarshal([]byte(prompts), &record.Prompts); err != nil {
		return record, fmt.Errorf("decode question %s prompts: %w", record.ID, err)
	}
	if err := json.Unmarshal([]byte(answers), &record.Answers); err != nil {
		return record, fmt.Errorf("decode question %s answers: %w", record.ID, err)
	}
	var err error
	if record.ExpiresAt, err = conversationParseNullTime(expires); err != nil {
		return record, fmt.Errorf("decode question %s expires_at: %w", record.ID, err)
	}
	if record.CreatedAt, err = parseTimeValue(created); err != nil {
		return record, fmt.Errorf("decode question %s created_at: %w", record.ID, err)
	}
	if record.UpdatedAt, err = parseTimeValue(updated); err != nil {
		return record, fmt.Errorf("decode question %s updated_at: %w", record.ID, err)
	}
	return record, nil
}

// upsertQuestion inserts a question or replaces its mutable state.
func (s *conversationStore) upsertQuestion(ctx context.Context, tx *sql.Tx, record conversationQuestionRecord) error {
	if err := conversation.ValidateQuestionID(record.ID); err != nil {
		return nativeInvalid(err.Error())
	}
	if err := conversation.ValidateConversationID(record.ConversationID); err != nil {
		return nativeInvalid(err.Error())
	}
	if !record.Status.Valid() {
		return nativeInvalid("Question status is not valid")
	}
	prompts := record.Prompts
	if prompts == nil {
		prompts = []conversation.Prompt{}
	}
	answers := record.Answers
	if answers == nil {
		answers = map[string][]string{}
	}
	promptsJSON, err := marshalNative(prompts)
	if err != nil {
		return fmt.Errorf("encode question prompts: %w", err)
	}
	answersJSON, err := marshalNative(answers)
	if err != nil {
		return fmt.Errorf("encode question answers: %w", err)
	}
	if record.UpdatedAt.IsZero() {
		record.UpdatedAt = record.CreatedAt
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO conversation_questions (`+conversationQuestionColumns+`)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET message_id = excluded.message_id, request_id = excluded.request_id, attempt_id = excluded.attempt_id, thread_id = excluded.thread_id, turn_id = excluded.turn_id,
 status = excluded.status, prompts_json = excluded.prompts_json, answers_json = excluded.answers_json, answered_by = excluded.answered_by, expires_at = excluded.expires_at, updated_at = excluded.updated_at
WHERE conversation_id = excluded.conversation_id`,
		record.ID, record.ConversationID, record.MessageID, record.RequestID, record.Owner.AttemptID, record.Owner.ThreadID, record.Owner.TurnID, record.Status,
		promptsJSON, answersJSON, record.AnsweredBy, conversationNullTime(record.ExpiresAt), conversationTime(record.CreatedAt), conversationTime(record.UpdatedAt),
	); err != nil {
		return fmt.Errorf("upsert question: %w", err)
	}
	return nil
}

func (s *conversationStore) readQuestion(ctx context.Context, query nativeQueryer, conversationID, id string) (conversationQuestionRecord, error) {
	record, err := scanConversationQuestion(query.QueryRowContext(ctx, "SELECT "+conversationQuestionColumns+" FROM conversation_questions WHERE conversation_id = ? AND id = ?", conversationID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return record, nativeNotFound()
	}
	if err != nil {
		return record, fmt.Errorf("read question: %w", err)
	}
	return record, nil
}

// listPendingQuestions returns questions that still await an answer (pending,
// sending or sent), oldest first.
func (s *conversationStore) listPendingQuestions(ctx context.Context, query nativeQueryer, conversationID string) ([]conversationQuestionRecord, error) {
	rows, err := query.QueryContext(ctx, "SELECT "+conversationQuestionColumns+" FROM conversation_questions WHERE conversation_id = ? AND status IN ('pending', 'sending', 'sent') ORDER BY created_at, id", conversationID)
	if err != nil {
		return nil, fmt.Errorf("list pending questions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var records []conversationQuestionRecord
	for rows.Next() {
		record, err := scanConversationQuestion(rows)
		if err != nil {
			return nil, fmt.Errorf("list pending questions: %w", err)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list pending questions: %w", err)
	}
	return records, nil
}

// listSnapshotQuestions returns the questions a reloading client renders:
// every question still awaiting an answer plus the settled questions of the
// current attempt, so a locked card survives a reload. The newest limit
// questions are returned oldest first.
func (s *conversationStore) listSnapshotQuestions(ctx context.Context, query nativeQueryer, conversationID, attemptID string, limit int) ([]conversationQuestionRecord, error) {
	if limit <= 0 {
		limit = conversationSnapshotQuestions
	}
	rows, err := query.QueryContext(ctx, "SELECT "+conversationQuestionColumns+` FROM conversation_questions
WHERE conversation_id = ? AND (status IN ('pending', 'sending', 'sent') OR (? <> '' AND attempt_id = ?))
ORDER BY created_at DESC, id DESC LIMIT ?`, conversationID, attemptID, attemptID, limit)
	if err != nil {
		return nil, fmt.Errorf("list snapshot questions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var records []conversationQuestionRecord
	for rows.Next() {
		record, err := scanConversationQuestion(rows)
		if err != nil {
			return nil, fmt.Errorf("list snapshot questions: %w", err)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list snapshot questions: %w", err)
	}
	slices.Reverse(records)
	return records, nil
}

func decodeConversationReceipt(encoded string) (*conversation.Receipt, error) {
	var receipt conversation.Receipt
	if err := json.Unmarshal([]byte(encoded), &receipt); err != nil {
		return nil, fmt.Errorf("decode receipt: %w", err)
	}
	return &receipt, nil
}

// reserveCommand records the first use of an idempotency key with an initial
// saved receipt. A replay with the same payload returns the stored receipt;
// a different payload returns the stored receipt with conflict set.
func (s *conversationStore) reserveCommand(ctx context.Context, tx *sql.Tx, conversationID, key string, kind conversation.CommandKind, requestHash string, now time.Time) (*conversation.Receipt, bool, error) {
	if strings.TrimSpace(key) == "" || len(key) > conversation.MaxCommandKeyBytes {
		return nil, false, nativeInvalid("An idempotency key of at most 128 bytes is required")
	}
	if !kind.Valid() {
		return nil, false, nativeInvalid("Command kind is not valid")
	}
	var storedHash, encoded string
	err := tx.QueryRowContext(ctx, "SELECT request_hash, receipt_json FROM conversation_commands WHERE conversation_id = ? AND key = ?", conversationID, key).Scan(&storedHash, &encoded)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return nil, false, fmt.Errorf("read command: %w", err)
	default:
		receipt, err := decodeConversationReceipt(encoded)
		if err != nil {
			return nil, false, err
		}
		return receipt, storedHash != requestHash, nil
	}
	initial, err := marshalNative(conversation.Receipt{Key: key, Kind: kind, Status: conversation.DeliverySaved, UpdatedAt: now.UTC()})
	if err != nil {
		return nil, false, fmt.Errorf("encode receipt: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO conversation_commands (conversation_id, key, kind, request_hash, receipt_json, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)",
		conversationID, key, kind, requestHash, initial, conversationTime(now), conversationTime(now)); err != nil {
		return nil, false, fmt.Errorf("insert command: %w", err)
	}
	return nil, false, nil
}

func (s *conversationStore) updateReceipt(ctx context.Context, tx *sql.Tx, conversationID, key string, receipt conversation.Receipt) error {
	if receipt.Key == "" {
		receipt.Key = key
	}
	if !receipt.Status.Valid() {
		return nativeInvalid("Receipt status is not valid")
	}
	encoded, err := marshalNative(receipt)
	if err != nil {
		return fmt.Errorf("encode receipt: %w", err)
	}
	result, err := tx.ExecContext(ctx, "UPDATE conversation_commands SET receipt_json = ?, updated_at = ? WHERE conversation_id = ? AND key = ?", encoded, conversationTime(receipt.UpdatedAt), conversationID, key)
	if err != nil {
		return fmt.Errorf("update receipt: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("update receipt: %w", err)
	}
	if affected == 0 {
		return nativeNotFound()
	}
	return nil
}

// appendEvent increments the conversation's event sequence and stores the
// event in the same transaction, returning the allocated sequence.
func (s *conversationStore) appendEvent(ctx context.Context, tx *sql.Tx, conversationID string, eventType conversation.EventType, body any, now time.Time) (int64, error) {
	if !eventType.Valid() {
		return 0, nativeInvalid("Event type is not valid")
	}
	if body == nil {
		body = map[string]any{}
	}
	encoded, err := marshalNative(body)
	if err != nil {
		return 0, fmt.Errorf("encode event: %w", err)
	}
	var seq int64
	err = tx.QueryRowContext(ctx, "UPDATE conversations SET event_seq = event_seq + 1 WHERE id = ? RETURNING event_seq", conversationID).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nativeNotFound()
	}
	if err != nil {
		return 0, fmt.Errorf("allocate event sequence: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO conversation_events (conversation_id, seq, type, body_json, created_at) VALUES (?, ?, ?, ?, ?)", conversationID, seq, eventType, encoded, conversationTime(now)); err != nil {
		return 0, fmt.Errorf("insert event: %w", err)
	}
	return seq, nil
}

func (s *conversationStore) listEvents(ctx context.Context, query nativeQueryer, conversationID string, afterSeq int64, limit int) ([]conversation.Event, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	rows, err := query.QueryContext(ctx, "SELECT seq, type, body_json, created_at FROM conversation_events WHERE conversation_id = ? AND seq > ? ORDER BY seq LIMIT ?", conversationID, afterSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var events []conversation.Event
	for rows.Next() {
		var event conversation.Event
		var body, created string
		if err := rows.Scan(&event.Seq, &event.Type, &body, &created); err != nil {
			return nil, fmt.Errorf("list events: %w", err)
		}
		event.Data = json.RawMessage(body)
		if event.CreatedAt, err = parseTimeValue(created); err != nil {
			return nil, fmt.Errorf("decode event %d created_at: %w", event.Seq, err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	return events, nil
}

// recordStart records that an attempt started a conversation turn. One
// attempt starts at most one conversation.
func (s *conversationStore) recordStart(ctx context.Context, tx *sql.Tx, attemptID, conversationID string, now time.Time) error {
	if strings.TrimSpace(attemptID) == "" {
		return nativeInvalid("An attempt is required")
	}
	var existing string
	err := tx.QueryRowContext(ctx, "SELECT conversation_id FROM conversation_starts WHERE attempt_id = ?", attemptID).Scan(&existing)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return fmt.Errorf("read conversation start: %w", err)
	default:
		return &errConversationStarted{AttemptID: attemptID, ConversationID: existing}
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO conversation_starts (attempt_id, conversation_id, created_at) VALUES (?, ?, ?)", attemptID, conversationID, conversationTime(now)); err != nil {
		return fmt.Errorf("insert conversation start: %w", err)
	}
	return nil
}

func (s *conversationStore) recordAudience(ctx context.Context, tx *sql.Tx, conversationID, actorPrincipalID string, from, to conversation.Visibility, now time.Time) error {
	if !from.Valid() || !to.Valid() {
		return nativeInvalid("Audience visibility is not valid")
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO conversation_audience_events (id, conversation_id, actor_principal_id, from_visibility, to_visibility, created_at) VALUES (?, ?, ?, ?, ?, ?)",
		newNativeID("aud"), conversationID, actorPrincipalID, from, to, conversationTime(now)); err != nil {
		return fmt.Errorf("insert audience event: %w", err)
	}
	return nil
}

// conversationNormalizeSummary counts the rows restart normalization
// rewrote, so the hub can report what a restart changed.
type conversationNormalizeSummary struct {
	Conversations int64
	Messages      int64
	Questions     int64
	Receipts      int64
}

// normalizeAfterRestart applies the restart rules from internal/conversation
// in one transaction: in-flight executions become unknown, in-flight
// deliveries and receipts become unknown, pending questions expire and
// questions being sent become unknown. The summary counts what changed.
func (s *conversationStore) normalizeAfterRestart(ctx context.Context, now time.Time) (summary conversationNormalizeSummary, resultErr error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return summary, fmt.Errorf("begin conversation normalization: %w", err)
	}
	defer func() {
		if resultErr != nil {
			_ = tx.Rollback()
		}
	}()
	stamp := conversationTime(now)
	type inFlight struct {
		id        string
		revision  int64
		execution conversation.Execution
	}
	pending, err := func() ([]inFlight, error) {
		rows, err := tx.QueryContext(ctx, "SELECT id, revision, execution_json FROM conversations WHERE json_extract(execution_json, '$.status') IN ('starting', 'running', 'waiting_input', 'interrupting')")
		if err != nil {
			return nil, fmt.Errorf("list in-flight conversations: %w", err)
		}
		defer func() { _ = rows.Close() }()
		var pending []inFlight
		for rows.Next() {
			var item inFlight
			var encoded string
			if err := rows.Scan(&item.id, &item.revision, &encoded); err != nil {
				return nil, fmt.Errorf("scan in-flight conversation: %w", err)
			}
			if err := json.Unmarshal([]byte(encoded), &item.execution); err != nil {
				return nil, fmt.Errorf("decode conversation %s execution: %w", item.id, err)
			}
			pending = append(pending, item)
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("list in-flight conversations: %w", err)
		}
		return pending, nil
	}()
	if err != nil {
		return summary, err
	}
	for _, item := range pending {
		execution := conversation.NormalizeExecutionValue(item.execution)
		execution.UpdatedAt = now.UTC()
		encoded, err := marshalNative(execution)
		if err != nil {
			return summary, fmt.Errorf("encode conversation %s execution: %w", item.id, err)
		}
		result, err := tx.ExecContext(ctx, "UPDATE conversations SET execution_json = ?, revision = revision + 1, updated_at = ? WHERE id = ? AND revision = ?", encoded, stamp, item.id, item.revision)
		if err != nil {
			return summary, fmt.Errorf("normalize conversation %s: %w", item.id, err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return summary, fmt.Errorf("count normalized conversation %s: %w", item.id, err)
		}
		summary.Conversations += changed
	}
	for _, statement := range []struct {
		query string
		args  []any
		count *int64
	}{
		{query: "UPDATE conversation_messages SET delivery = ?, updated_at = ? WHERE delivery IN (?, ?, ?)", args: []any{conversation.DeliveryUnknown, stamp, conversation.DeliverySending, conversation.DeliverySent, conversation.DeliveryResponding}, count: &summary.Messages},
		{query: "UPDATE conversation_questions SET status = ?, updated_at = ? WHERE status = ?", args: []any{conversation.QuestionExpired, stamp, conversation.QuestionPending}, count: &summary.Questions},
		{query: "UPDATE conversation_questions SET status = ?, updated_at = ? WHERE status = ?", args: []any{conversation.QuestionUnknown, stamp, conversation.QuestionSending}, count: &summary.Questions},
		{query: "UPDATE conversation_commands SET receipt_json = json_set(receipt_json, '$.status', ?, '$.updated_at', ?), updated_at = ? WHERE json_extract(receipt_json, '$.status') IN (?, ?, ?)", args: []any{conversation.DeliveryUnknown, formatHubTime(now), stamp, conversation.DeliverySending, conversation.DeliverySent, conversation.DeliveryResponding}, count: &summary.Receipts},
	} {
		result, err := tx.ExecContext(ctx, statement.query, statement.args...)
		if err != nil {
			return summary, fmt.Errorf("normalize conversation state: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return summary, fmt.Errorf("count normalized conversation state: %w", err)
		}
		*statement.count += changed
	}
	if err := tx.Commit(); err != nil {
		return summary, fmt.Errorf("commit conversation normalization: %w", err)
	}
	return summary, nil
}

// Message references (decisions section 14).

// replaceMessageReferences rewrites the stored references of one message.
// Re-extraction is idempotent: an assistant message that completes after its
// text streamed in replaces the rows its earlier text produced.
func (s *conversationStore) replaceMessageReferences(ctx context.Context, tx *sql.Tx, message conversationMessageRecord, now time.Time) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM message_references WHERE message_id = ?", message.ID); err != nil {
		return fmt.Errorf("clear message references: %w", err)
	}
	for _, reference := range message.References {
		if !reference.Kind.Valid() || strings.TrimSpace(reference.ID) == "" {
			return nativeInvalid("A message reference requires a known kind and a target")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO message_references (message_id, conversation_id, target_kind, target_id, label, created_at)
VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (message_id, target_kind, target_id) DO NOTHING`,
			message.ID, message.ConversationID, reference.Kind, reference.ID, reference.Label, conversationTime(now)); err != nil {
			return fmt.Errorf("insert message reference: %w", err)
		}
	}
	return nil
}

// resolveMessageReferences fills the references of every message in one
// query. The hub pool holds a single connection, so the caller must have
// drained its own rows first. The rows come back in insertion order, which
// is the order the tokens appeared in the message text: replaceMessageReferences
// rewrites a message's rows as a set, so rowid order is extraction order.
func resolveMessageReferences(ctx context.Context, query nativeQueryer, messages []*conversationMessageRecord) error {
	ids := make([]string, 0, len(messages))
	for _, message := range messages {
		ids = append(ids, message.ID)
	}
	if len(ids) == 0 {
		return nil
	}
	encoded, err := marshalNative(ids)
	if err != nil {
		return fmt.Errorf("encode message ids: %w", err)
	}
	rows, err := query.QueryContext(ctx, `SELECT message_id, target_kind, target_id, label FROM message_references
WHERE message_id IN (SELECT value FROM json_each(?)) ORDER BY message_id, rowid`, encoded)
	if err != nil {
		return fmt.Errorf("read message references: %w", err)
	}
	defer func() { _ = rows.Close() }()
	byMessage := map[string][]conversationMessageReference{}
	for rows.Next() {
		var messageID string
		var reference conversationMessageReference
		if err := rows.Scan(&messageID, &reference.Kind, &reference.ID, &reference.Label); err != nil {
			return fmt.Errorf("read message references: %w", err)
		}
		byMessage[messageID] = append(byMessage[messageID], reference)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read message references: %w", err)
	}
	for _, message := range messages {
		message.References = byMessage[message.ID]
	}
	return nil
}

// workItemReference is one "referenced from" entry the target's activity
// shows: which message in which conversation named it, and when.
type workItemReference struct {
	MessageID      string
	ConversationID string
	Excerpt        string
	CreatedAt      time.Time
	// Visibility and OwnerPrincipalID let the caller apply the conversation
	// read rule: a private chat is invisible to anyone but its owner.
	Visibility       conversation.Visibility
	OwnerPrincipalID string
}

// listWorkItemReferences returns the messages that reference a work item,
// newest first, with the conversation audience of each so the caller can
// drop the ones the reader may not see.
func (s *conversationStore) listWorkItemReferences(ctx context.Context, query nativeQueryer, organization tracker.OrganizationID, project tracker.ProjectID, workItemID string, limit int) ([]workItemReference, error) {
	if limit <= 0 {
		limit = conversationReferencePage
	}
	rows, err := query.QueryContext(ctx, `SELECT r.message_id, r.conversation_id, m.text, r.created_at, c.visibility, c.owner_principal_id
FROM message_references r
JOIN conversation_messages m ON m.id = r.message_id
JOIN conversations c ON c.id = r.conversation_id
WHERE r.target_kind = 'issue' AND r.target_id = ? AND c.organization_id = ? AND c.project_id = ?
ORDER BY r.created_at DESC, r.message_id DESC LIMIT ?`, workItemID, organization, project, limit)
	if err != nil {
		return nil, fmt.Errorf("list work item references: %w", err)
	}
	defer func() { _ = rows.Close() }()
	references := make([]workItemReference, 0, limit)
	for rows.Next() {
		var reference workItemReference
		var created string
		if err := rows.Scan(&reference.MessageID, &reference.ConversationID, &reference.Excerpt, &created, &reference.Visibility, &reference.OwnerPrincipalID); err != nil {
			return nil, fmt.Errorf("list work item references: %w", err)
		}
		if reference.CreatedAt, err = parseTimeValue(created); err != nil {
			return nil, fmt.Errorf("decode reference %s created_at: %w", reference.MessageID, err)
		}
		references = append(references, reference)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list work item references: %w", err)
	}
	return references, nil
}

// listSettleCandidates returns the active conversations whose execution has
// ended or never started and whose last activity is older than before. The
// sweep settles them one transaction at a time, so the list is bounded.
func (s *conversationStore) listSettleCandidates(ctx context.Context, query nativeQueryer, before time.Time, limit int) ([]conversationRecord, error) {
	if limit <= 0 {
		limit = conversationSettleBatch
	}
	rows, err := query.QueryContext(ctx, "SELECT "+conversationReadColumns+` FROM conversations
WHERE status = 'active' AND COALESCE(last_message_at, updated_at) < ?
 AND json_extract(execution_json, '$.status') IN (?, ?, ?, ?, ?)
ORDER BY COALESCE(last_message_at, updated_at) LIMIT ?`, conversationTime(before),
		conversation.ExecutionIdle, conversation.ExecutionCompleted, conversation.ExecutionInterrupted,
		conversation.ExecutionFailed, conversation.ExecutionUnknown, limit)
	if err != nil {
		return nil, fmt.Errorf("list settle candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	records := make([]conversationRecord, 0, limit)
	for rows.Next() {
		record, err := scanConversation(rows)
		if err != nil {
			return nil, fmt.Errorf("list settle candidates: %w", err)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list settle candidates: %w", err)
	}
	return records, nil
}
