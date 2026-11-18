package conversations

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// conversationStore is the durable storage for the hosted conversation
// product. Transactional operations take the caller's *sql.Tx so that state
// and the events describing it commit together.
type ConversationStore struct {
	Contents         attachment.ArchiveContentStore
	ResolveWorkItems func(context.Context, Queryer, []*ConversationRecord) error
}

func NewConversationStore(_ *sql.DB) *ConversationStore {
	return &ConversationStore{}
}

// errConversationRevision reports a compare-and-set failure on a conversation.
type ErrConversationRevision struct {
	ConversationID string
	Current        int64
}

func (e *ErrConversationRevision) Error() string {
	return fmt.Sprintf("conversation %s revision conflict: current %d", e.ConversationID, e.Current)
}

// errConversationLinked reports a violated one-issue-one-conversation constraint.
type ErrConversationLinked struct {
	ExistingConversationID string
}

func (e *ErrConversationLinked) Error() string {
	return "conversation_already_linked: " + e.ExistingConversationID
}

const (
	ConversationOriginUser   = "user"
	ConversationOriginWorker = "worker"
)

type ConversationRecord struct {
	ContentID         string
	ID                string
	CreationKey       string
	CreationHash      string
	ReadAudience      string
	OrganizationID    tracker.OrganizationID
	ProjectID         tracker.ProjectID
	OwnerPrincipalID  string
	OwnerSubject      string
	Origin            string
	Title             string
	Visibility        conversation.Visibility
	Status            conversation.Status
	Archived          bool
	SubjectWorkItemID string
	WorkItemID        string
	LinkedAt          *time.Time
	// WorkItem caches the linked issue summary the projections report. It
	// is filled by resolveConversationWorkItems on every read.
	WorkItem         *ConversationWorkItem
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
	// same kind resumes it. It is empty for threads
	// recorded before the column existed, which therefore never resume.
	ProviderThreadOrigin  string
	Execution             conversation.Execution
	ExecutionContinuation bool
	EventSeq              int64
	CreatedAt             time.Time
	UpdatedAt             time.Time
	LastMessageAt         *time.Time
	// SettledAt is when the conversation last became settled, nil while it
	// is active.
	SettledAt *time.Time
	// Preferences are the conversation's turn preferences. Every field is
	// "auto" until the user overrides it.
	Preferences conversation.Preferences
	// MessageCount is the conversation's whole history, filled by every
	// read so the audience preview can report it.
	MessageCount int64
}

type ConversationMessageRecord struct {
	Revision        int64
	ID              string
	ContentID       string
	contentHash     string
	TextBytes       int64
	StoredTextBytes sql.NullInt64
	ConversationID  string
	Seq             int64
	Role            conversation.Role
	Kind            conversation.MessageKind
	Text            string
	Data            json.RawMessage
	Delivery        conversation.Delivery
	AttemptID       string
	ThreadID        string
	TurnID          string
	ProviderItemID  string
	Actor           conversation.Actor
	CommandKey      string
	// References are the issues and conversations the message names, filled
	// by every read that projects the message.
	References []ConversationMessageReference
	// Attachments are the uploads bound to the message, filled by the same
	// reads as References.
	Attachments []ConversationAttachmentRecord
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// conversationMessageReference is one stored, resolved reference: the hub
// only records targets it could resolve, so a row always names something
// that exists.
type ConversationMessageReference struct {
	URL   string
	Kind  conversation.ReferenceKind
	ID    string
	Label string
}

type ConversationQuestionRecord struct {
	ContentID string
	conversation.Question
	RequestID string
}

func conversationParseNullTime(value sql.NullString) (*time.Time, error) {
	if !value.Valid || strings.TrimSpace(value.String) == "" {
		var none *time.Time
		return none, nil
	}
	parsed, err := hubapi.ParseTime(value.String)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

const conversationColumns = `id, organization_id, project_id, owner_principal_id, owner_subject, origin, title, visibility, status,
 subject_work_item_id, work_item_id, linked_at, revision, provider_thread_id, provider_thread_runner_id, provider_thread_origin, execution_json, event_seq, preferences_json, created_at, updated_at, last_message_at, settled_at, archived, content_attachment_id`

// conversationMessageCount counts the conversation's whole history for the
// resource's message_count. It is a correlated
// subquery rather than a second query because the hub pool holds a single
// connection and a read must stay one statement.
const conversationMessageCount = `((SELECT count(*) FROM conversation_messages cm WHERE cm.conversation_id = conversations.id)
+ COALESCE((SELECT sum(n.retired_rows) FROM native_archive_sections n WHERE n.organization_id=conversations.organization_id AND n.project_id=conversations.project_id AND n.work_item_id=conversations.work_item_id AND n.kind='conversation' AND n.subject_id=conversations.id),0))`

// conversationReadColumns is conversationColumns plus the derived columns a
// read reports. Only conversationColumns names real columns, so it is the
// list an INSERT uses.
const conversationReadColumns = conversationColumns + ", " + conversationMessageCount + ", read_audience"

type conversationScanner interface {
	Scan(dest ...any) error
}

func scanConversation(row conversationScanner) (ConversationRecord, error) {
	var record ConversationRecord
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
	if record.CreatedAt, err = hubapi.ParseTime(created); err != nil {
		return record, fmt.Errorf("decode conversation %s created_at: %w", record.ID, err)
	}
	if record.UpdatedAt, err = hubapi.ParseTime(updated); err != nil {
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

func (s *ConversationStore) readConversationRow(ctx context.Context, query Queryer, organization tracker.OrganizationID, project tracker.ProjectID, id string) (ConversationRecord, error) {
	record, err := scanConversation(query.QueryRowContext(ctx, "SELECT "+conversationReadProjection(ctx, query, id)+" FROM conversations WHERE organization_id = ? AND project_id = ? AND id = ?", organization, project, id))
	if errors.Is(err, sql.ErrNoRows) {
		return record, hubapi.NotFound()
	}
	if err != nil {
		return record, fmt.Errorf("read conversation: %w", err)
	}
	if err := s.ResolveWorkItems(ctx, query, []*ConversationRecord{&record}); err != nil {
		return record, err
	}
	return record, nil
}

// conversationListQuery is the filter a listing applies. Everything is
// pushed into SQL: the hub pool holds a single connection, so a page must
// not be filtered in Go and then re-fetched.
type ConversationListQuery struct {
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

// conversationNormalizeSummary counts the rows restart normalization
// rewrote, so the hub can report what a restart changed.
type conversationNormalizeSummary struct {
	Conversations int64
	Messages      int64
	Questions     int64
	Receipts      int64
}

func ConversationTime(value time.Time) string {
	return hubapi.ConversationTime(value)
}
