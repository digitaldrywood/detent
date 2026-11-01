package postgres

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lib/pq"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var _ conversationstore.Reader = (*session)(nil)
var _ conversationstore.Writer = (*session)(nil)

func (s *session) Conversation(ctx context.Context, project tracker.ProjectID, id string) (conversationstore.Conversation, error) {
	return scanPGConversation(s.db.QueryRowContext(ctx, conversationSelect+` WHERE organization_id=$1 AND project_id=$2 AND id=$3`, s.organization, project, id))
}

func (s *session) LockConversation(ctx context.Context, project tracker.ProjectID, id string) (conversationstore.Conversation, error) {
	return scanPGConversation(s.db.QueryRowContext(ctx, conversationSelect+` WHERE organization_id=$1 AND project_id=$2 AND id=$3 FOR UPDATE OF c`, s.organization, project, id))
}

func (s *session) ConversationByID(ctx context.Context, id string) (conversationstore.Conversation, error) {
	return scanPGConversation(s.db.QueryRowContext(ctx, conversationSelect+` WHERE organization_id=$1 AND id=$2`, s.organization, id))
}

func (s *session) ConversationByExecutionLease(ctx context.Context, leaseID string) (conversationstore.Conversation, error) {
	return scanPGConversation(s.db.QueryRowContext(ctx, conversationSelect+` WHERE organization_id=$1 AND execution_lease_id=$2`, s.organization, leaseID))
}

func (s *session) ConversationByWorkItem(ctx context.Context, project tracker.ProjectID, item string) (conversationstore.Conversation, error) {
	return scanPGConversation(s.db.QueryRowContext(ctx, conversationSelect+` WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3`, s.organization, project, item))
}

func (s *session) ConversationByCreationKey(ctx context.Context, principal, key string) (conversationstore.Conversation, error) {
	return scanPGConversation(s.db.QueryRowContext(ctx, conversationSelect+` WHERE organization_id=$1 AND owner_principal_id=$2 AND creation_key=$3`, s.organization, principal, key))
}

const conversationSelect = `SELECT c.organization_id,c.project_id,c.id,c.creation_key,c.creation_hash,c.owner_principal_id,c.owner_subject,c.origin,c.title,c.visibility,c.status,
c.subject_work_item_id,c.work_item_id,c.linked_at,c.revision,c.provider_thread_id,c.provider_thread_runner_id,c.provider_thread_origin,
c.execution_json,c.event_seq,c.preferences_json,c.read_audience,c.created_at,c.updated_at,c.last_message_at,c.settled_at,c.archived,
c.content_attachment_id,(SELECT count(*) FROM conversation_messages m WHERE m.organization_id=c.organization_id AND m.conversation_id=c.id)
FROM conversations c`

func scanPGConversation(row interface{ Scan(...any) error }) (conversationstore.Conversation, error) {
	var record conversationstore.Conversation
	var subject, workItem, linked, lastMessage, settled sql.NullString
	var execution, preferences []byte
	err := row.Scan(&record.OrganizationID, &record.ProjectID, &record.ID, &record.CreationKey, &record.CreationHash, &record.OwnerPrincipalID, &record.OwnerSubject, &record.Origin,
		&record.Title, &record.Visibility, &record.Status, &subject, &workItem, &linked, &record.Revision,
		&record.ProviderThreadID, &record.ProviderThreadRunnerID, &record.ProviderThreadOrigin, &execution, &record.EventSequence,
		&preferences, &record.ReadAudience, &record.CreatedAt, &record.UpdatedAt, &lastMessage, &settled, &record.Archived,
		&record.ContentReference, &record.MessageCount)
	if err != nil {
		return record, err
	}
	record.SubjectWorkItemID = subject.String
	record.WorkItemID = workItem.String
	if parsed, valid, parseErr := pgNullTime(linked); parseErr != nil {
		return record, parseErr
	} else if valid {
		record.LinkedAt = &parsed
	}
	if parsed, valid, parseErr := pgNullTime(lastMessage); parseErr != nil {
		return record, parseErr
	} else if valid {
		record.LastMessageAt = &parsed
	}
	if parsed, valid, parseErr := pgNullTime(settled); parseErr != nil {
		return record, parseErr
	} else if valid {
		record.SettledAt = &parsed
	}
	if err := json.Unmarshal(execution, &record.Execution); err != nil {
		return record, fmt.Errorf("decode conversation execution: %w", err)
	}
	if err := json.Unmarshal(preferences, &record.Preferences); err != nil {
		return record, fmt.Errorf("decode conversation preferences: %w", err)
	}
	return record, nil
}

func pgNullTime(value sql.NullString) (time.Time, bool, error) {
	if !value.Valid {
		return time.Time{}, false, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value.String)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("decode conversation time: %w", err)
	}
	return parsed, true, nil
}

func (s *session) Conversations(ctx context.Context, filter conversationstore.ConversationQuery) (conversationstore.ConversationPage, error) {
	if filter.Limit <= 0 || filter.Limit > corestore.MaxPageSize {
		return conversationstore.ConversationPage{}, corestore.ErrInvalidPage
	}
	if filter.ProjectID != "" && len(filter.ProjectIDs) > 0 {
		return conversationstore.ConversationPage{}, corestore.ErrInvalidPage
	}
	args := []any{s.organization}
	where := ` WHERE c.organization_id=$1 AND c.origin='user' AND c.archived=false AND (c.visibility='shared' OR c.owner_principal_id=$2) AND (c.read_audience='' OR c.read_audience=$3)`
	args = append(args, filter.PrincipalID, filter.ReadAudience)
	add := func(expression string, values ...any) { where += expression; args = append(args, values...) }
	if filter.ProjectID != "" {
		add(fmt.Sprintf(" AND c.project_id=$%d", len(args)+1), filter.ProjectID)
	}
	if filter.ProjectIDs != nil {
		if len(filter.ProjectIDs) == 0 {
			return conversationstore.ConversationPage{Items: []conversationstore.Conversation{}}, nil
		}
		projects := make([]string, len(filter.ProjectIDs))
		for i, id := range filter.ProjectIDs {
			projects[i] = string(id)
		}
		add(fmt.Sprintf(" AND c.project_id=ANY($%d::text[])", len(args)+1), pq.Array(projects))
	}
	if filter.Title != "" {
		add(fmt.Sprintf(" AND lower(c.title) LIKE $%d ESCAPE '\\'", len(args)+1), pgConversationTitlePattern(filter.Title))
	}
	if filter.SubjectWorkItemID != "" {
		add(fmt.Sprintf(" AND c.subject_work_item_id=$%d AND c.owner_principal_id=$%d AND c.visibility='private'", len(args)+1, len(args)+2), filter.SubjectWorkItemID, filter.PrincipalID)
	}
	if filter.Settled != nil {
		state := conversation.StatusActive
		if *filter.Settled {
			state = conversation.StatusSettled
		}
		add(fmt.Sprintf(" AND c.status=$%d", len(args)+1), state)
	}
	if filter.Cursor != "" {
		cursor, err := decodePGConversationCursor(filter.Cursor)
		if err != nil {
			return conversationstore.ConversationPage{}, corestore.ErrInvalidCursor
		}
		add(fmt.Sprintf(" AND (CASE WHEN c.status='settled' THEN 1 ELSE 0 END>$%d OR (CASE WHEN c.status='settled' THEN 1 ELSE 0 END=$%d AND (COALESCE(c.last_message_at,c.updated_at)<$%d OR (COALESCE(c.last_message_at,c.updated_at)=$%d AND c.id<$%d))))", len(args)+1, len(args)+2, len(args)+3, len(args)+4, len(args)+5), cursor.Settled, cursor.Settled, cursor.Activity, cursor.Activity, cursor.ID)
	}
	args = append(args, filter.Limit+1)
	rows, err := s.db.QueryContext(ctx, conversationSelect+where+fmt.Sprintf(" ORDER BY CASE WHEN c.status='settled' THEN 1 ELSE 0 END,COALESCE(c.last_message_at,c.updated_at) DESC,c.id DESC LIMIT $%d", len(args)), args...)
	if err != nil {
		return conversationstore.ConversationPage{}, mapError(ctx, err)
	}
	defer rows.Close()
	page := conversationstore.ConversationPage{Items: make([]conversationstore.Conversation, 0, filter.Limit)}
	for rows.Next() {
		record, err := scanPGConversation(rows)
		if err != nil {
			return page, err
		}
		page.Items = append(page.Items, record)
	}
	if err := rows.Err(); err != nil {
		return page, mapError(ctx, err)
	}
	if len(page.Items) > filter.Limit {
		page.Items = page.Items[:filter.Limit]
		last := page.Items[len(page.Items)-1]
		activity := last.UpdatedAt
		if last.LastMessageAt != nil {
			activity = *last.LastMessageAt
		}
		settled := 0
		if last.Status == conversation.StatusSettled {
			settled = 1
		}
		encoded, err := json.Marshal(pgConversationCursor{Activity: activity.UTC().Format(time.RFC3339Nano), ID: last.ID, Settled: settled})
		if err != nil {
			return page, fmt.Errorf("encode conversation cursor: %w", err)
		}
		page.Next = base64.RawURLEncoding.EncodeToString(encoded)
	}
	return page, nil
}

func (s *session) LifecycleConversations(ctx context.Context, filter conversationstore.LifecycleQuery) (conversationstore.ConversationPage, error) {
	if err := checkPage(filter.Limit); err != nil {
		return conversationstore.ConversationPage{}, err
	}
	where := ` WHERE c.organization_id=$1`
	args := []any{s.organization}
	switch filter.Kind {
	case conversationstore.LifecycleRestart:
		where += ` AND c.work_item_id IS NULL`
	case conversationstore.LifecycleExecution:
		where += ` AND (c.execution_status IN ('starting','running','waiting_input','interrupting') OR (c.execution_status IN ('completed','interrupted','failed','unknown') AND c.execution_settled=false AND c.execution_lease_id IS NOT NULL))`
	case conversationstore.LifecycleSettle:
		if filter.Before.IsZero() {
			return conversationstore.ConversationPage{}, corestore.ErrInvalidPage
		}
		args = append(args, filter.Before)
		where += fmt.Sprintf(` AND c.status='active' AND COALESCE(c.last_message_at,c.updated_at)<$%d AND c.execution_status IN ('idle','completed','interrupted','failed','unknown')`, len(args))
	default:
		return conversationstore.ConversationPage{}, corestore.ErrInvalidPage
	}
	if filter.AfterID != "" {
		args = append(args, filter.AfterID)
		where += fmt.Sprintf(" AND c.id>$%d", len(args))
	}
	args = append(args, filter.Limit+1)
	rows, err := s.db.QueryContext(ctx, conversationSelect+where+fmt.Sprintf(" ORDER BY c.id LIMIT $%d", len(args)), args...)
	if err != nil {
		return conversationstore.ConversationPage{}, mapError(ctx, err)
	}
	defer rows.Close()
	page := conversationstore.ConversationPage{Items: make([]conversationstore.Conversation, 0, filter.Limit)}
	for rows.Next() {
		record, err := scanPGConversation(rows)
		if err != nil {
			return page, err
		}
		page.Items = append(page.Items, record)
	}
	if err := rows.Err(); err != nil {
		return page, mapError(ctx, err)
	}
	if len(page.Items) > filter.Limit {
		page.Items = page.Items[:filter.Limit]
		page.Next = page.Items[len(page.Items)-1].ID
	}
	return page, nil
}

func (s *session) PendingConversations(ctx context.Context, afterID string, limit int) (conversationstore.PendingConversationPage, error) {
	if err := checkPage(limit); err != nil {
		return conversationstore.PendingConversationPage{}, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT c.id,c.work_item_id IS NOT NULL FROM conversations c WHERE c.organization_id=$1 AND c.id>$2 AND EXISTS (SELECT 1 FROM conversation_messages m WHERE m.organization_id=c.organization_id AND m.conversation_id=c.id AND m.role='user' AND m.delivery IN ('saved','queued')) ORDER BY c.id LIMIT $3`, s.organization, afterID, limit+1)
	if err != nil {
		return conversationstore.PendingConversationPage{}, mapError(ctx, err)
	}
	defer rows.Close()
	page := conversationstore.PendingConversationPage{Items: make([]conversationstore.PendingConversation, 0, limit)}
	for rows.Next() {
		var item conversationstore.PendingConversation
		if err := rows.Scan(&item.ID, &item.Linked); err != nil {
			return page, mapError(ctx, err)
		}
		page.Items = append(page.Items, item)
	}
	if err := rows.Err(); err != nil {
		return page, mapError(ctx, err)
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.Next = page.Items[len(page.Items)-1].ID
	}
	return page, nil
}

type pgConversationCursor struct {
	Activity string `json:"a"`
	ID       string `json:"i"`
	Settled  int    `json:"s"`
}

func decodePGConversationCursor(value string) (pgConversationCursor, error) {
	var cursor pgConversationCursor
	data, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return cursor, err
	}
	if err := json.Unmarshal(data, &cursor); err != nil || cursor.ID == "" || cursor.Settled < 0 || cursor.Settled > 1 {
		return cursor, corestore.ErrInvalidCursor
	}
	if _, err := time.Parse(time.RFC3339Nano, cursor.Activity); err != nil {
		return cursor, err
	}
	return cursor, nil
}

func pgConversationTitlePattern(value string) string {
	pattern := "%"
	return pattern + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(strings.ToLower(value)) + "%"
}

func (s *session) Messages(ctx context.Context, id string, beforeSequence int64, limit int) (conversationstore.MessagePage, error) {
	if err := checkPage(limit); err != nil {
		return conversationstore.MessagePage{}, err
	}
	var rows *sql.Rows
	var err error
	if beforeSequence > 0 {
		rows, err = s.db.QueryContext(ctx, `SELECT organization_id,conversation_id,id,seq,role,kind,text,data_json,delivery,attempt_id,thread_id,turn_id,provider_item_id,actor_json,command_key,created_at,updated_at,content_attachment_id,revision,COALESCE(text_bytes,0),COALESCE(stored_text_bytes,0),tool_name,tool_item_id,tool_status,operator_action_id,operator_action_agent,operator_action_result FROM conversation_messages WHERE organization_id=$1 AND conversation_id=$2 AND seq<$3 ORDER BY seq DESC LIMIT $4`, s.organization, id, beforeSequence, limit+1)
	} else {
		rows, err = s.db.QueryContext(ctx, `SELECT organization_id,conversation_id,id,seq,role,kind,text,data_json,delivery,attempt_id,thread_id,turn_id,provider_item_id,actor_json,command_key,created_at,updated_at,content_attachment_id,revision,COALESCE(text_bytes,0),COALESCE(stored_text_bytes,0),tool_name,tool_item_id,tool_status,operator_action_id,operator_action_agent,operator_action_result FROM conversation_messages WHERE organization_id=$1 AND conversation_id=$2 ORDER BY seq DESC LIMIT $3`, s.organization, id, limit+1)
	}
	if err != nil {
		return conversationstore.MessagePage{}, mapError(ctx, err)
	}
	defer rows.Close()
	page := conversationstore.MessagePage{Items: make([]conversationstore.Message, 0, limit)}
	for rows.Next() {
		m, err := scanPGMessage(rows)
		if err != nil {
			return page, err
		}
		page.Items = append(page.Items, m)
	}
	if err := rows.Err(); err != nil {
		return page, mapError(ctx, err)
	}
	if len(page.Items) > limit {
		page.Next = page.Items[limit-1].Sequence
		page.Items = page.Items[:limit]
	}
	for left, right := 0, len(page.Items)-1; left < right; left, right = left+1, right-1 {
		page.Items[left], page.Items[right] = page.Items[right], page.Items[left]
	}
	return page, nil
}

func (s *session) FilteredMessages(ctx context.Context, filter conversationstore.MessageFilter) (conversationstore.MessagePage, error) {
	if err := checkPage(filter.Limit); err != nil {
		return conversationstore.MessagePage{}, err
	}
	if strings.TrimSpace(filter.ConversationID) == "" {
		return conversationstore.MessagePage{}, errors.New("conversation ID is required")
	}
	conditions := []string{"organization_id=$1", "conversation_id=$2"}
	args := []any{s.organization, filter.ConversationID}
	add := func(condition string, value any) {
		args = append(args, value)
		conditions = append(conditions, fmt.Sprintf(condition, len(args)))
	}
	if filter.AttemptID != "" {
		add("attempt_id=$%d", filter.AttemptID)
	}
	if filter.TurnID != "" {
		add("turn_id=$%d", filter.TurnID)
	}
	if filter.Role != "" {
		add("role=$%d", filter.Role)
	}
	if filter.Kind != "" {
		add("kind=$%d", filter.Kind)
	}
	if filter.BeforeSequence > 0 {
		add("seq<$%d", filter.BeforeSequence)
	}
	if filter.AfterSequence > 0 {
		add("seq>$%d", filter.AfterSequence)
	}
	if len(filter.Deliveries) > 0 {
		args = append(args, pq.Array(filter.Deliveries))
		conditions = append(conditions, fmt.Sprintf("delivery=ANY($%d)", len(args)))
	}
	args = append(args, filter.Limit+1)
	query := `SELECT organization_id,conversation_id,id,seq,role,kind,text,data_json,delivery,attempt_id,thread_id,turn_id,provider_item_id,actor_json,command_key,created_at,updated_at,content_attachment_id,revision,COALESCE(text_bytes,0),COALESCE(stored_text_bytes,0),tool_name,tool_item_id,tool_status,operator_action_id,operator_action_agent,operator_action_result FROM conversation_messages WHERE ` + strings.Join(conditions, " AND ") + fmt.Sprintf(" ORDER BY seq DESC LIMIT $%d", len(args))
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return conversationstore.MessagePage{}, mapError(ctx, err)
	}
	defer rows.Close()
	page := conversationstore.MessagePage{Items: make([]conversationstore.Message, 0, filter.Limit)}
	for rows.Next() {
		message, err := scanPGMessage(rows)
		if err != nil {
			return page, err
		}
		page.Items = append(page.Items, message)
	}
	if err := rows.Err(); err != nil {
		return page, mapError(ctx, err)
	}
	if len(page.Items) > filter.Limit {
		page.Next = page.Items[filter.Limit-1].Sequence
		page.Items = page.Items[:filter.Limit]
	}
	for left, right := 0, len(page.Items)-1; left < right; left, right = left+1, right-1 {
		page.Items[left], page.Items[right] = page.Items[right], page.Items[left]
	}
	return page, nil
}

func scanPGMessage(row interface{ Scan(...any) error }) (conversationstore.Message, error) {
	var m conversationstore.Message
	var actor []byte
	var toolName, toolID, toolStatus, actionID, actionAgent, actionResult sql.NullString
	err := row.Scan(&m.OrganizationID, &m.ConversationID, &m.ID, &m.Sequence, &m.Role, &m.Kind, &m.Text, &m.Data, &m.Delivery, &m.AttemptID, &m.ThreadID, &m.TurnID, &m.ProviderItemID, &actor, &m.CommandKey, &m.CreatedAt, &m.UpdatedAt, &m.ContentReference, &m.Revision, &m.TextBytes, &m.StoredTextBytes, &toolName, &toolID, &toolStatus, &actionID, &actionAgent, &actionResult)
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(actor, &m.Actor); err != nil {
		return m, fmt.Errorf("decode conversation message actor: %w", err)
	}
	m.ToolName = pgStringPtr(toolName)
	m.ToolItemID = pgStringPtr(toolID)
	m.ToolStatus = pgStringPtr(toolStatus)
	m.OperatorActionID = pgStringPtr(actionID)
	m.OperatorAgent = pgStringPtr(actionAgent)
	m.OperatorResult = pgStringPtr(actionResult)
	return m, nil
}

func pgStringPtr(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	text := value.String
	return &text
}

func (s *session) Message(ctx context.Context, conversationID, id string) (conversationstore.Message, error) {
	row := s.db.QueryRowContext(ctx, `SELECT organization_id,conversation_id,id,seq,role,kind,text,data_json,delivery,attempt_id,thread_id,turn_id,provider_item_id,actor_json,command_key,created_at,updated_at,content_attachment_id,revision,COALESCE(text_bytes,0),COALESCE(stored_text_bytes,0),tool_name,tool_item_id,tool_status,operator_action_id,operator_action_agent,operator_action_result FROM conversation_messages WHERE organization_id=$1 AND conversation_id=$2 AND id=$3`, s.organization, conversationID, id)
	return scanPGMessage(row)
}

func (s *session) MessageByCommandKey(ctx context.Context, conversationID, key string) (conversationstore.Message, bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT organization_id,conversation_id,id,seq,role,kind,text,data_json,delivery,attempt_id,thread_id,turn_id,provider_item_id,actor_json,command_key,created_at,updated_at,content_attachment_id,revision,COALESCE(text_bytes,0),COALESCE(stored_text_bytes,0),tool_name,tool_item_id,tool_status,operator_action_id,operator_action_agent,operator_action_result FROM conversation_messages WHERE organization_id=$1 AND conversation_id=$2 AND command_key=$3 ORDER BY seq LIMIT 1`, s.organization, conversationID, key)
	message, err := scanPGMessage(row)
	if errors.Is(err, sql.ErrNoRows) {
		return conversationstore.Message{}, false, nil
	}
	return message, err == nil, mapError(ctx, err)
}

func (s *session) ConversationEvents(ctx context.Context, conversationID string, after int64, limit int) (conversationstore.EventPage, error) {
	if err := checkPage(limit); err != nil {
		return conversationstore.EventPage{}, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT organization_id,conversation_id,seq,type,body_json,created_at,content_attachment_id FROM conversation_events WHERE organization_id=$1 AND conversation_id=$2 AND seq>$3 ORDER BY seq LIMIT $4`, s.organization, conversationID, after, limit+1)
	if err != nil {
		return conversationstore.EventPage{}, mapError(ctx, err)
	}
	defer rows.Close()
	page := conversationstore.EventPage{Items: make([]conversationstore.Event, 0, limit)}
	for rows.Next() {
		var e conversationstore.Event
		if err := rows.Scan(&e.OrganizationID, &e.ConversationID, &e.Seq, &e.Type, &e.Data, &e.CreatedAt, &e.ContentReference); err != nil {
			return page, mapError(ctx, err)
		}
		page.Items = append(page.Items, e)
	}
	if err := rows.Err(); err != nil {
		return page, mapError(ctx, err)
	}
	if len(page.Items) > limit {
		page.Next = page.Items[limit-1].Seq
		page.Items = page.Items[:limit]
	}
	return page, nil
}

func (s *session) Question(ctx context.Context, conversationID, id string) (conversationstore.Question, error) {
	row := s.db.QueryRowContext(ctx, `SELECT organization_id,conversation_id,id,message_id,request_id,attempt_id,thread_id,turn_id,status,prompts_json,answers_json,answered_by,expires_at,created_at,updated_at,content_attachment_id FROM conversation_questions WHERE organization_id=$1 AND conversation_id=$2 AND id=$3`, s.organization, conversationID, id)
	return scanPGQuestion(ctx, row)
}

func (s *session) Questions(ctx context.Context, conversationID, attemptID string, limit int) ([]conversationstore.Question, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT organization_id,conversation_id,id,message_id,request_id,attempt_id,thread_id,turn_id,status,prompts_json,answers_json,answered_by,expires_at,created_at,updated_at,content_attachment_id FROM conversation_questions WHERE organization_id=$1 AND conversation_id=$2 AND ($3='' OR attempt_id=$3) ORDER BY created_at,id LIMIT $4`, s.organization, conversationID, attemptID, limit)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	out := make([]conversationstore.Question, 0, limit)
	for rows.Next() {
		q, err := scanPGQuestion(ctx, rows)
		if err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, mapError(ctx, rows.Err())
}

func (s *session) OutstandingQuestions(ctx context.Context, conversationID, attemptID, turnID, afterID string, limit int) (conversationstore.QuestionPage, error) {
	if err := checkPage(limit); err != nil {
		return conversationstore.QuestionPage{}, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT organization_id,conversation_id,id,message_id,request_id,attempt_id,thread_id,turn_id,status,prompts_json,answers_json,answered_by,expires_at,created_at,updated_at,content_attachment_id FROM conversation_questions WHERE organization_id=$1 AND conversation_id=$2 AND ($3='' OR attempt_id=$3) AND ($4='' OR turn_id=$4) AND id>$5 AND status IN ('pending','sending','sent') ORDER BY id LIMIT $6`, s.organization, conversationID, attemptID, turnID, afterID, limit+1)
	if err != nil {
		return conversationstore.QuestionPage{}, mapError(ctx, err)
	}
	defer rows.Close()
	page := conversationstore.QuestionPage{Items: make([]conversationstore.Question, 0, limit)}
	for rows.Next() {
		question, err := scanPGQuestion(ctx, rows)
		if err != nil {
			return page, err
		}
		page.Items = append(page.Items, question)
	}
	if err := rows.Err(); err != nil {
		return page, mapError(ctx, err)
	}
	if len(page.Items) > limit {
		page.Next = page.Items[limit-1].ID
		page.Items = page.Items[:limit]
	}
	return page, nil
}

func (s *session) RestartQuestions(ctx context.Context, conversationID, afterID string, limit int) (conversationstore.QuestionPage, error) {
	if err := checkPage(limit); err != nil {
		return conversationstore.QuestionPage{}, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT organization_id,conversation_id,id,message_id,request_id,attempt_id,thread_id,turn_id,status,prompts_json,answers_json,answered_by,expires_at,created_at,updated_at,content_attachment_id FROM conversation_questions WHERE organization_id=$1 AND conversation_id=$2 AND id>$3 AND status IN ('pending','sending') ORDER BY id LIMIT $4`, s.organization, conversationID, afterID, limit+1)
	if err != nil {
		return conversationstore.QuestionPage{}, mapError(ctx, err)
	}
	defer rows.Close()
	page := conversationstore.QuestionPage{Items: make([]conversationstore.Question, 0, limit)}
	for rows.Next() {
		question, err := scanPGQuestion(ctx, rows)
		if err != nil {
			return page, err
		}
		page.Items = append(page.Items, question)
	}
	if err := rows.Err(); err != nil {
		return page, mapError(ctx, err)
	}
	if len(page.Items) > limit {
		page.Next = page.Items[limit-1].ID
		page.Items = page.Items[:limit]
	}
	return page, nil
}

func scanPGQuestion(ctx context.Context, row interface{ Scan(...any) error }) (conversationstore.Question, error) {
	var q conversationstore.Question
	var prompts, answers []byte
	var expires sql.NullTime
	err := row.Scan(&q.OrganizationID, &q.ConversationID, &q.ID, &q.MessageID, &q.RequestID, &q.Owner.AttemptID, &q.Owner.ThreadID, &q.Owner.TurnID, &q.Status, &prompts, &answers, &q.AnsweredBy, &expires, &q.CreatedAt, &q.UpdatedAt, &q.ContentReference)
	if err != nil {
		return q, mapError(ctx, err)
	}
	if err = json.Unmarshal(prompts, &q.Prompts); err != nil {
		return q, fmt.Errorf("decode question prompts: %w", err)
	}
	if err = json.Unmarshal(answers, &q.Answers); err != nil {
		return q, fmt.Errorf("decode question answers: %w", err)
	}
	if expires.Valid {
		q.ExpiresAt = &expires.Time
	}
	return q, nil
}

func (s *session) Command(ctx context.Context, conversationID, key string) (conversationstore.Command, bool, error) {
	var c conversationstore.Command
	var receipt []byte
	err := s.db.QueryRowContext(ctx, `SELECT organization_id,conversation_id,key,kind,request_hash,receipt_json,created_at,updated_at,content_attachment_id FROM conversation_commands WHERE organization_id=$1 AND conversation_id=$2 AND key=$3`, s.organization, conversationID, key).Scan(&c.OrganizationID, &c.ConversationID, &c.Key, &c.Kind, &c.RequestHash, &receipt, &c.CreatedAt, &c.UpdatedAt, &c.ContentReference)
	if errors.Is(err, sql.ErrNoRows) {
		return c, false, nil
	}
	if err != nil {
		return c, false, mapError(ctx, err)
	}
	if err = json.Unmarshal(receipt, &c.Receipt); err != nil {
		return c, false, fmt.Errorf("decode command receipt: %w", err)
	}
	return c, true, nil
}

func (s *session) PendingCommands(ctx context.Context, conversationID, afterKey string, limit int) (conversationstore.CommandPage, error) {
	if err := checkPage(limit); err != nil {
		return conversationstore.CommandPage{}, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT organization_id,conversation_id,key,kind,request_hash,receipt_json,created_at,updated_at,content_attachment_id FROM conversation_commands WHERE organization_id=$1 AND conversation_id=$2 AND key>$3 AND receipt_status=ANY($4) ORDER BY key LIMIT $5`, s.organization, conversationID, afterKey, pq.Array([]string{"sending", "sent", "responding"}), limit+1)
	if err != nil {
		return conversationstore.CommandPage{}, mapError(ctx, err)
	}
	defer rows.Close()
	page := conversationstore.CommandPage{Items: make([]conversationstore.Command, 0, limit)}
	for rows.Next() {
		var command conversationstore.Command
		var receipt []byte
		if err := rows.Scan(&command.OrganizationID, &command.ConversationID, &command.Key, &command.Kind, &command.RequestHash, &receipt, &command.CreatedAt, &command.UpdatedAt, &command.ContentReference); err != nil {
			return page, mapError(ctx, err)
		}
		if err := json.Unmarshal(receipt, &command.Receipt); err != nil {
			return page, fmt.Errorf("decode command receipt: %w", err)
		}
		page.Items = append(page.Items, command)
	}
	if err := rows.Err(); err != nil {
		return page, mapError(ctx, err)
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.Next = page.Items[len(page.Items)-1].Key
	}
	return page, nil
}

func (s *session) TurnBatch(ctx context.Context, attemptID, batchKey string) (int64, bool, error) {
	var sequence int64
	err := s.db.QueryRowContext(ctx, `SELECT event_seq FROM conversation_turn_batches WHERE organization_id=$1 AND attempt_id=$2 AND batch_key=$3`, s.organization, attemptID, batchKey).Scan(&sequence)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, mapError(ctx, err)
	}
	return sequence, true, nil
}

func (s *session) HasConversationStart(ctx context.Context, conversationID string) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM conversation_starts WHERE organization_id=$1 AND conversation_id=$2)`, s.organization, conversationID).Scan(&exists)
	return exists, mapError(ctx, err)
}

func (s *session) MessageReferences(ctx context.Context, conversationID, messageID string) ([]conversationstore.MessageReference, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT organization_id,conversation_id,message_id,position,target_kind,target_id,label,created_at FROM message_references WHERE organization_id=$1 AND conversation_id=$2 AND message_id=$3 ORDER BY position`, s.organization, conversationID, messageID)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	out := []conversationstore.MessageReference{}
	for rows.Next() {
		var r conversationstore.MessageReference
		if err := rows.Scan(&r.OrganizationID, &r.ConversationID, &r.MessageID, &r.Position, &r.Kind, &r.TargetID, &r.Label, &r.CreatedAt); err != nil {
			return nil, mapError(ctx, err)
		}
		out = append(out, r)
	}
	return out, mapError(ctx, rows.Err())
}

func (s *session) ToolRevisions(ctx context.Context, conversationID, messageID string, limit int) ([]conversationstore.ToolRevision, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT organization_id,conversation_id,message_id,revision,text,tool,item_id,status,content_attachment_id FROM conversation_tool_revisions WHERE organization_id=$1 AND conversation_id=$2 AND message_id=$3 ORDER BY revision DESC LIMIT $4`, s.organization, conversationID, messageID, limit)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	out := []conversationstore.ToolRevision{}
	for rows.Next() {
		var r conversationstore.ToolRevision
		if err := rows.Scan(&r.OrganizationID, &r.ConversationID, &r.MessageID, &r.Revision, &r.Text, &r.Tool, &r.ItemID, &r.Status, &r.ContentReference); err != nil {
			return nil, mapError(ctx, err)
		}
		out = append(out, r)
	}
	return out, mapError(ctx, rows.Err())
}

func (s *session) PendingUserMessages(ctx context.Context, conversationID string) (int, error) {
	var pending int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM conversation_messages WHERE organization_id=$1 AND conversation_id=$2 AND role='user' AND delivery IN ('saved','queued','sending')`, s.organization, conversationID).Scan(&pending)
	return pending, mapError(ctx, err)
}

func (s *session) ConversationContent(ctx context.Context, id string) (conversationstore.ContentObject, error) {
	var object conversationstore.ContentObject
	var ref attachment.ArchiveReference
	err := s.db.QueryRowContext(ctx, `SELECT organization_id,id,object_key,sha256,compressed_size,uncompressed_size,codec,schema_version,kind,retention,binding_id,customer_owned,created_at FROM conversation_content_objects WHERE organization_id=$1 AND id=$2`, s.organization, id).Scan(
		&object.OrganizationID, &object.ID, &ref.Key, &ref.SHA256, &ref.CompressedSize, &ref.UncompressedSize, &ref.Codec, &ref.SchemaVersion, &ref.Kind, &ref.Retention, &ref.BindingID, &ref.CustomerOwned, &object.CreatedAt)
	if err != nil {
		return object, mapError(ctx, err)
	}
	object.Reference = ref
	if err := ref.Validate(string(s.organization)); err != nil {
		return conversationstore.ContentObject{}, corestore.ErrInvalidPage
	}
	return object, nil
}

func (s *session) PutConversationContent(ctx context.Context, object conversationstore.ContentObject) error {
	if err := s.checkOrganization(object.OrganizationID); err != nil {
		return err
	}
	if err := object.Reference.Validate(string(s.organization)); err != nil || object.ID != pgConversationContentID(string(s.organization), object.Reference) || object.CreatedAt.IsZero() {
		return corestore.ErrInvalidPage
	}
	ref := object.Reference
	_, err := s.db.ExecContext(ctx, `INSERT INTO conversation_content_objects (organization_id,id,object_key,sha256,compressed_size,uncompressed_size,codec,schema_version,kind,retention,binding_id,customer_owned,created_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) ON CONFLICT (organization_id,id) DO NOTHING`, object.OrganizationID, object.ID, ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize, ref.Codec, ref.SchemaVersion, ref.Kind, ref.Retention, ref.BindingID, ref.CustomerOwned, object.CreatedAt)
	if err != nil {
		return mapError(ctx, err)
	}
	stored, err := s.ConversationContent(ctx, object.ID)
	if err != nil {
		return err
	}
	if stored.Reference != ref {
		return corestore.ErrUniqueViolation
	}
	return nil
}

func pgConversationContentID(organization string, ref attachment.ArchiveReference) string {
	return "att_" + artifact.Digest([]byte(organization + "\n" + ref.Key))[:32]
}

func (s *session) CreateConversation(ctx context.Context, c conversationstore.Conversation) (conversationstore.Conversation, error) {
	if err := s.checkOrganization(c.OrganizationID); err != nil {
		return c, err
	}
	if err := validatePGConversation(&c); err != nil {
		return c, err
	}
	execution, err := json.Marshal(c.Execution)
	if err != nil {
		return c, err
	}
	preferences, err := json.Marshal(c.Preferences)
	if err != nil {
		return c, err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO conversations (organization_id,id,creation_key,creation_hash,project_id,owner_principal_id,owner_subject,origin,title,visibility,status,subject_work_item_id,work_item_id,linked_at,revision,provider_thread_id,provider_thread_runner_id,provider_thread_origin,execution_json,execution_status,execution_lease_id,execution_settled,event_seq,preferences_json,read_audience,created_at,updated_at,last_message_at,settled_at,archived,content_attachment_id) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,NULLIF($12,''),NULLIF($13,''),$14,$15,$16,$17,$18,$19,$20,NULLIF($21,''),$22,$23,$24,$25,$26,$27,$28,$29,$30,$31)`, c.OrganizationID, c.ID, c.CreationKey, c.CreationHash, c.ProjectID, c.OwnerPrincipalID, c.OwnerSubject, c.Origin, c.Title, c.Visibility, c.Status, c.SubjectWorkItemID, c.WorkItemID, c.LinkedAt, c.Revision, c.ProviderThreadID, c.ProviderThreadRunnerID, c.ProviderThreadOrigin, string(execution), c.Execution.Status, c.Execution.Owner.LeaseID, c.Execution.Settled, c.EventSequence, string(preferences), c.ReadAudience, c.CreatedAt, c.UpdatedAt, c.LastMessageAt, c.SettledAt, c.Archived, c.ContentReference)
	if err != nil {
		return c, mapError(ctx, err)
	}
	return c, nil
}

func (s *session) UpdateConversation(ctx context.Context, c conversationstore.Conversation, expected int64) (conversationstore.Conversation, error) {
	if err := s.checkOrganization(c.OrganizationID); err != nil {
		return c, err
	}
	if err := validatePGConversation(&c); err != nil {
		return c, err
	}
	execution, err := json.Marshal(c.Execution)
	if err != nil {
		return c, err
	}
	preferences, err := json.Marshal(c.Preferences)
	if err != nil {
		return c, err
	}
	err = s.db.QueryRowContext(ctx, `UPDATE conversations SET owner_principal_id=$1,owner_subject=$2,title=$3,visibility=$4,status=$5,subject_work_item_id=NULLIF($6,''),work_item_id=NULLIF($7,''),linked_at=$8,revision=revision+1,provider_thread_id=$9,provider_thread_runner_id=$10,provider_thread_origin=$11,execution_json=$12,execution_status=$13,execution_lease_id=NULLIF($14,''),execution_settled=$15,event_seq=$16,preferences_json=$17,read_audience=$18,updated_at=$19,last_message_at=$20,settled_at=$21,archived=$22,content_attachment_id=$23 WHERE organization_id=$24 AND project_id=$25 AND id=$26 AND revision=$27 RETURNING revision`, c.OwnerPrincipalID, c.OwnerSubject, c.Title, c.Visibility, c.Status, c.SubjectWorkItemID, c.WorkItemID, c.LinkedAt, c.ProviderThreadID, c.ProviderThreadRunnerID, c.ProviderThreadOrigin, string(execution), c.Execution.Status, c.Execution.Owner.LeaseID, c.Execution.Settled, c.EventSequence, string(preferences), c.ReadAudience, c.UpdatedAt, c.LastMessageAt, c.SettledAt, c.Archived, c.ContentReference, c.OrganizationID, c.ProjectID, c.ID, expected).Scan(&c.Revision)
	if err != nil {
		return c, updated(ctx, err)
	}
	return c, nil
}

func (s *session) CreateMessage(ctx context.Context, m conversationstore.Message) (conversationstore.Message, error) {
	if err := s.checkOrganization(m.OrganizationID); err != nil {
		return m, err
	}
	if err := validatePGMessage(&m); err != nil {
		return m, err
	}
	if m.Sequence != 0 {
		return m, corestore.ErrInvalidPage
	}
	var lockedID string
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM conversations WHERE organization_id=$1 AND id=$2 FOR UPDATE`, m.OrganizationID, m.ConversationID).Scan(&lockedID); err != nil {
		return m, mapError(ctx, err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq),0)+1 FROM conversation_messages WHERE organization_id=$1 AND conversation_id=$2`, m.OrganizationID, m.ConversationID).Scan(&m.Sequence); err != nil {
		return m, mapError(ctx, err)
	}
	actor, err := json.Marshal(m.Actor)
	if err != nil {
		return m, err
	}
	if len(m.Data) == 0 {
		m.Data = json.RawMessage(`{}`)
	}
	if m.Revision == 0 {
		m.Revision = 1
	}
	err = s.db.QueryRowContext(ctx, `INSERT INTO conversation_messages (organization_id,conversation_id,id,seq,role,kind,text,data_json,delivery,attempt_id,thread_id,turn_id,provider_item_id,actor_json,command_key,created_at,updated_at,content_attachment_id,revision,text_bytes,stored_text_bytes,tool_name,tool_item_id,tool_status,operator_action_id,operator_action_agent,operator_action_result) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27) RETURNING revision`, m.OrganizationID, m.ConversationID, m.ID, m.Sequence, m.Role, m.Kind, m.Text, string(m.Data), m.Delivery, m.AttemptID, m.ThreadID, m.TurnID, m.ProviderItemID, string(actor), m.CommandKey, m.CreatedAt, m.UpdatedAt, m.ContentReference, m.Revision, m.TextBytes, m.StoredTextBytes, m.ToolName, m.ToolItemID, m.ToolStatus, m.OperatorActionID, m.OperatorAgent, m.OperatorResult).Scan(&m.Revision)
	if err != nil {
		return m, mapError(ctx, err)
	}
	_, err = s.db.ExecContext(ctx, `UPDATE conversations SET last_message_at=GREATEST(COALESCE(last_message_at,$1),$1) WHERE organization_id=$2 AND id=$3`, m.CreatedAt, m.OrganizationID, m.ConversationID)
	return m, mapError(ctx, err)
}

func (s *session) UpdateMessage(ctx context.Context, m conversationstore.Message, expected int64) (conversationstore.Message, error) {
	if err := s.checkOrganization(m.OrganizationID); err != nil {
		return m, err
	}
	if err := validatePGMessage(&m); err != nil {
		return m, err
	}
	actor, err := json.Marshal(m.Actor)
	if err != nil {
		return m, err
	}
	if len(m.Data) == 0 {
		m.Data = json.RawMessage(`{}`)
	}
	err = s.db.QueryRowContext(ctx, `UPDATE conversation_messages SET role=$1,kind=$2,text=$3,data_json=$4,delivery=$5,attempt_id=$6,thread_id=$7,turn_id=$8,provider_item_id=$9,actor_json=$10,command_key=$11,updated_at=$12,content_attachment_id=$13,revision=revision+1,text_bytes=$14,stored_text_bytes=$15,tool_name=$16,tool_item_id=$17,tool_status=$18,operator_action_id=$19,operator_action_agent=$20,operator_action_result=$21 WHERE organization_id=$22 AND conversation_id=$23 AND id=$24 AND revision=$25 RETURNING revision`, m.Role, m.Kind, m.Text, string(m.Data), m.Delivery, m.AttemptID, m.ThreadID, m.TurnID, m.ProviderItemID, string(actor), m.CommandKey, m.UpdatedAt, m.ContentReference, m.TextBytes, m.StoredTextBytes, m.ToolName, m.ToolItemID, m.ToolStatus, m.OperatorActionID, m.OperatorAgent, m.OperatorResult, m.OrganizationID, m.ConversationID, m.ID, expected).Scan(&m.Revision)
	if err != nil {
		return m, updated(ctx, err)
	}
	return m, nil
}

func (s *session) UpsertQuestion(ctx context.Context, q conversationstore.Question) error {
	if err := s.checkOrganization(q.OrganizationID); err != nil {
		return err
	}
	if err := conversation.ValidateQuestionID(q.ID); err != nil || !q.Status.Valid() || q.ConversationID == "" {
		return corestore.ErrInvalidPage
	}
	prompts, err := json.Marshal(q.Prompts)
	if err != nil {
		return err
	}
	answers, err := json.Marshal(q.Answers)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO conversation_questions (organization_id,conversation_id,id,message_id,request_id,attempt_id,thread_id,turn_id,status,prompts_json,answers_json,answered_by,expires_at,created_at,updated_at,content_attachment_id) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16) ON CONFLICT (organization_id,conversation_id,id) DO UPDATE SET message_id=excluded.message_id,request_id=excluded.request_id,attempt_id=excluded.attempt_id,thread_id=excluded.thread_id,turn_id=excluded.turn_id,status=excluded.status,prompts_json=excluded.prompts_json,answers_json=excluded.answers_json,answered_by=excluded.answered_by,expires_at=excluded.expires_at,updated_at=excluded.updated_at,content_attachment_id=excluded.content_attachment_id`, q.OrganizationID, q.ConversationID, q.ID, q.MessageID, q.RequestID, q.Owner.AttemptID, q.Owner.ThreadID, q.Owner.TurnID, q.Status, string(prompts), string(answers), q.AnsweredBy, q.ExpiresAt, q.CreatedAt, q.UpdatedAt, q.ContentReference)
	return mapError(ctx, err)
}

func (s *session) ReserveCommand(ctx context.Context, c conversationstore.Command) (conversationstore.Command, bool, error) {
	if err := s.checkOrganization(c.OrganizationID); err != nil {
		return c, false, err
	}
	if strings.TrimSpace(c.Key) == "" || len(c.Key) > conversation.MaxCommandKeyBytes || !c.Kind.Valid() || !c.Receipt.Status.Valid() {
		return c, false, corestore.ErrInvalidPage
	}
	receipt, err := json.Marshal(c.Receipt)
	if err != nil {
		return c, false, err
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO conversation_commands (organization_id,conversation_id,key,kind,request_hash,receipt_json,created_at,updated_at,content_attachment_id,receipt_status) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT (organization_id,conversation_id,key) DO NOTHING`, c.OrganizationID, c.ConversationID, c.Key, c.Kind, c.RequestHash, string(receipt), c.CreatedAt, c.UpdatedAt, c.ContentReference, c.Receipt.Status)
	if err != nil {
		return c, false, mapError(ctx, err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return c, false, err
	}
	stored, ok, readErr := s.Command(ctx, c.ConversationID, c.Key)
	if readErr != nil {
		return c, false, readErr
	}
	if !ok {
		return c, false, corestore.ErrNotFound
	}
	if stored.RequestHash != c.RequestHash {
		return stored, false, corestore.ErrUniqueViolation
	}
	return stored, inserted == 1, nil
}

func (s *session) UpdateCommand(ctx context.Context, c conversationstore.Command) error {
	if err := s.checkOrganization(c.OrganizationID); err != nil {
		return err
	}
	receipt, err := json.Marshal(c.Receipt)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE conversation_commands SET receipt_json=$1,receipt_status=$2,updated_at=$3,content_attachment_id=$4 WHERE organization_id=$5 AND conversation_id=$6 AND key=$7`, string(receipt), c.Receipt.Status, c.UpdatedAt, c.ContentReference, c.OrganizationID, c.ConversationID, c.Key)
	if err != nil {
		return mapError(ctx, err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return corestore.ErrNotFound
	}
	return nil
}

func (s *session) AppendConversationEvent(ctx context.Context, e conversationstore.Event) (conversationstore.Event, error) {
	if err := s.checkOrganization(e.OrganizationID); err != nil {
		return e, err
	}
	if len(e.Data) == 0 {
		e.Data = json.RawMessage(`{}`)
	}
	if !e.Type.Valid() || !json.Valid(e.Data) {
		return e, corestore.ErrInvalidPage
	}
	err := s.db.QueryRowContext(ctx, `UPDATE conversations SET event_seq=event_seq+1 WHERE organization_id=$1 AND id=$2 RETURNING event_seq`, e.OrganizationID, e.ConversationID).Scan(&e.Seq)
	if err != nil {
		return e, updated(ctx, err)
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO conversation_events (organization_id,conversation_id,seq,type,body_json,created_at,content_attachment_id) VALUES ($1,$2,$3,$4,$5,$6,$7)`, e.OrganizationID, e.ConversationID, e.Seq, e.Type, string(e.Data), e.CreatedAt, e.ContentReference)
	return e, mapError(ctx, err)
}

func (s *session) RecordStart(ctx context.Context, attemptID, conversationID string, at time.Time) error {
	if attemptID == "" || conversationID == "" {
		return corestore.ErrInvalidPage
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO conversation_starts (organization_id,attempt_id,conversation_id,created_at) VALUES ($1,$2,$3,$4)`, s.organization, attemptID, conversationID, at)
	return mapError(ctx, err)
}

func (s *session) RecordAudience(ctx context.Context, conversationID, actor string, from, to conversation.Visibility, at time.Time) error {
	if conversationID == "" || actor == "" || !from.Valid() || !to.Valid() || from == to {
		return corestore.ErrInvalidPage
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO conversation_audience_events (organization_id,id,conversation_id,actor_principal_id,from_visibility,to_visibility,created_at) VALUES ($1,$2,$3,$4,$5,$6,$7)`, s.organization, conversation.NewMessageID(), conversationID, actor, from, to, at)
	return mapError(ctx, err)
}

func (s *session) RecordTurnBatch(ctx context.Context, attemptID, batchKey string, eventSequence int64, at time.Time) error {
	if attemptID == "" || batchKey == "" || len(batchKey) > 128 || eventSequence < 0 {
		return corestore.ErrInvalidPage
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO conversation_turn_batches (organization_id,attempt_id,batch_key,event_seq,created_at) VALUES ($1,$2,$3,$4,$5)`, s.organization, attemptID, batchKey, eventSequence, at)
	return mapError(ctx, err)
}

func (s *session) ReplaceMessageReferences(ctx context.Context, conversationID, messageID string, references []conversationstore.MessageReference, at time.Time) error {
	if len(references) > conversation.MaxReferences {
		return corestore.ErrInvalidPage
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM message_references WHERE organization_id=$1 AND conversation_id=$2 AND message_id=$3`, s.organization, conversationID, messageID); err != nil {
		return mapError(ctx, err)
	}
	for position, r := range references {
		if r.OrganizationID != s.organization {
			return corestore.ErrTenantMismatch
		}
		if r.ConversationID != conversationID || r.MessageID != messageID || r.TargetID == "" || (r.Kind != conversation.ReferenceIssue && r.Kind != conversation.ReferenceConversation) {
			return corestore.ErrInvalidPage
		}
		r.Position = int64(position)
		if _, err := s.db.ExecContext(ctx, `INSERT INTO message_references (organization_id,conversation_id,message_id,position,target_kind,target_id,label,created_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, r.OrganizationID, r.ConversationID, r.MessageID, r.Position, r.Kind, r.TargetID, r.Label, at); err != nil {
			return mapError(ctx, err)
		}
	}
	return nil
}

func (s *session) PutToolRevision(ctx context.Context, r conversationstore.ToolRevision) error {
	if err := s.checkOrganization(r.OrganizationID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO conversation_tool_revisions (organization_id,conversation_id,message_id,revision,text,tool,item_id,status,content_attachment_id) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT (organization_id,conversation_id,message_id,revision) DO UPDATE SET text=excluded.text,tool=excluded.tool,item_id=excluded.item_id,status=excluded.status,content_attachment_id=excluded.content_attachment_id`, r.OrganizationID, r.ConversationID, r.MessageID, r.Revision, r.Text, r.Tool, r.ItemID, r.Status, r.ContentReference)
	return mapError(ctx, err)
}

func validatePGConversation(c *conversationstore.Conversation) error {
	if c.OrganizationID == "" || c.ProjectID == "" || c.OwnerPrincipalID == "" {
		return corestore.ErrInvalidPage
	}
	if c.ID == "" {
		c.ID = conversation.NewConversationID()
	}
	if err := conversation.ValidateConversationID(c.ID); err != nil {
		return corestore.ErrInvalidPage
	}
	if c.Origin == "" {
		c.Origin = "user"
	}
	if c.Origin != "user" && c.Origin != "worker" || !c.Visibility.Valid() || !c.Status.Valid() {
		return corestore.ErrInvalidPage
	}
	if c.SubjectWorkItemID != "" && (c.WorkItemID != "" || c.Visibility != conversation.VisibilityPrivate) {
		return corestore.ErrInvalidPage
	}
	if c.WorkItemID != "" && c.LinkedAt == nil {
		return corestore.ErrInvalidPage
	}
	if c.Execution.Status == "" {
		c.Execution.Status = conversation.ExecutionIdle
	}
	if !c.Execution.Status.Valid() {
		return corestore.ErrInvalidPage
	}
	if c.Revision == 0 {
		c.Revision = 1
	}
	if c.Revision < 1 {
		return corestore.ErrInvalidPage
	}
	if c.Preferences == (conversation.Preferences{}) {
		c.Preferences = conversation.DefaultPreferences()
	} else {
		c.Preferences = c.Preferences.Normalized()
	}
	if c.CreatedAt.IsZero() || c.UpdatedAt.IsZero() {
		return corestore.ErrInvalidPage
	}
	return nil
}

func validatePGMessage(m *conversationstore.Message) error {
	if m.OrganizationID == "" || m.ConversationID == "" || m.CreatedAt.IsZero() || m.UpdatedAt.IsZero() || !m.Role.Valid() || !m.Kind.Valid() || !m.Delivery.Valid() || !m.Actor.Kind.Valid() {
		return corestore.ErrInvalidPage
	}
	if m.ID == "" {
		m.ID = conversation.NewMessageID()
	}
	if err := conversation.ValidateMessageID(m.ID); err != nil {
		return corestore.ErrInvalidPage
	}
	if len(m.Data) == 0 {
		m.Data = json.RawMessage(`{}`)
	}
	if !json.Valid(m.Data) {
		return corestore.ErrInvalidPage
	}
	if m.TextBytes == 0 {
		m.TextBytes = int64(len(m.Text))
	}
	if m.StoredTextBytes == 0 {
		m.StoredTextBytes = int64(len(m.Text))
	}
	if m.Revision == 0 {
		m.Revision = 1
	}
	return nil
}
