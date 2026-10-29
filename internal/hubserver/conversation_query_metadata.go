package hubserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
)

type conversationQueryMetadata struct {
	columns []string
	values  []any
}

func (m *conversationQueryMetadata) add(column string, value any) {
	m.columns = append(m.columns, column+"=?")
	m.values = append(m.values, value)
}

func (m conversationQueryMetadata) write(ctx context.Context, tx *sql.Tx, table, where string, args ...any) error {
	values := append(append([]any{}, m.values...), args...)
	var statement strings.Builder
	statement.WriteString("UPDATE ")
	statement.WriteString(table)
	statement.WriteString(" SET query_metadata_version=1,")
	statement.WriteString(strings.Join(m.columns, ","))
	statement.WriteString(" WHERE ")
	statement.WriteString(where)
	_, err := tx.ExecContext(ctx, statement.String(), values...)
	return err
}

func executionQueryMetadata(execution conversation.Execution) conversationQueryMetadata {
	var m conversationQueryMetadata
	m.add("execution_status", string(execution.Status))
	m.add("execution_lease_id", execution.Owner.LeaseID)
	m.add("execution_settled", execution.Settled)
	return m
}

type queryMessageData struct {
	Tool                 *string `json:"tool"`
	ItemID               *string `json:"item_id"`
	Status               *string `json:"status"`
	OperatorActionResult *string `json:"operator_action_result"`
	OperatorAction       *struct {
		Action *struct {
			ID    *string `json:"id"`
			Agent *string `json:"agent"`
		} `json:"action"`
	} `json:"operator_action"`
}

func messageQueryMetadata(raw []byte, actor conversation.ActorKind) (conversationQueryMetadata, error) {
	var data queryMessageData
	if trimmed := bytes.TrimSpace(raw); len(trimmed) > 0 && trimmed[0] != '{' {
		raw = []byte("{}")
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return conversationQueryMetadata{}, err
	}
	var m conversationQueryMetadata
	m.add("actor_kind", string(actor))
	m.add("tool_name", data.Tool)
	m.add("tool_item_id", data.ItemID)
	m.add("tool_status", data.Status)
	var id, agent *string
	if data.OperatorAction != nil && data.OperatorAction.Action != nil {
		id, agent = data.OperatorAction.Action.ID, data.OperatorAction.Action.Agent
	}
	m.add("operator_action_id", id)
	m.add("operator_action_agent", agent)
	m.add("operator_action_result", data.OperatorActionResult)
	return m, nil
}

func eventQueryMetadata(raw []byte) (conversationQueryMetadata, error) {
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		return conversationQueryMetadata{}, err
	}
	var m conversationQueryMetadata
	for _, field := range []struct{ column, key string }{
		{"message_id", "message_id"}, {"object_id", "id"}, {"message_role", "role"}, {"message_kind", "kind"},
		{"attempt_id", "attempt_id"}, {"execution_status", "status"}, {"thread_id", "thread_id"}, {"turn_id", "turn_id"},
		{"snapshot_title", "title"}, {"snapshot_visibility", "visibility"}, {"snapshot_status", "status"},
		{"snapshot_subject_work_item_id", "subject_work_item_id"}, {"snapshot_work_item_id", "work_item_id"}, {"snapshot_linked_at", "linked_at"},
	} {
		var value *string
		if data := body[field.key]; len(data) > 0 {
			if err := json.Unmarshal(data, &value); err != nil {
				return m, err
			}
		}
		m.add(field.column, value)
	}
	for _, field := range []struct{ column, key string }{
		{"delta_offset", "seq"}, {"delta_bytes", "text_bytes"}, {"message_revision", "message_revision"},
	} {
		var value *int64
		if data := body[field.key]; len(data) > 0 {
			if err := json.Unmarshal(data, &value); err != nil {
				return m, err
			}
		}
		m.add(field.column, value)
	}
	var preferences struct {
		Model  *string `json:"model"`
		Effort *string `json:"reasoning_effort"`
		Access *string `json:"access"`
	}
	var key *string
	if data := body["preferences"]; len(data) > 0 && string(data) != "null" {
		if err := json.Unmarshal(data, &preferences); err != nil {
			return m, err
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, data); err != nil {
			return m, err
		}
		digest := sha256.Sum256(compact.Bytes())
		value := hex.EncodeToString(digest[:])
		key = &value
	}
	m.add("snapshot_preferences_key", key)
	m.add("snapshot_model", preferences.Model)
	m.add("snapshot_effort", preferences.Effort)
	m.add("snapshot_access", preferences.Access)
	return m, nil
}

func activityQueryMetadata(raw []byte) (conversationQueryMetadata, error) {
	var data struct {
		Run *struct {
			AttemptID *string `json:"attempt_id"`
			Runtime   *struct {
				Activity *struct {
					Spans json.RawMessage `json:"spans"`
				} `json:"activity"`
			} `json:"runtime"`
		} `json:"run"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return conversationQueryMetadata{}, err
	}
	var m conversationQueryMetadata
	var attempt *string
	present := false
	if data.Run != nil {
		attempt = data.Run.AttemptID
		if r := data.Run.Runtime; r != nil && r.Activity != nil {
			spans := bytes.TrimSpace(r.Activity.Spans)
			present = len(spans) > 0 && spans[0] == '['
		}
	}
	m.add("run_attempt_id", attempt)
	m.add("activity_spans_present", present)
	return m, nil
}

type conversationMetadataFamily struct{ table, payload string }

var conversationMetadataFamilies = []conversationMetadataFamily{
	{"conversations", "execution_json"},
	{"conversation_commands", "receipt_json"},
	{"conversation_messages", "data_json"},
	{"conversation_events", "body_json"},
	{"event_compactions", "summary_json"},
	{"collaboration_events", "data_json"},
}

func (s *Service) convertConversationQueryMetadata(ctx context.Context) error {
	remaining := storageConversionRows
	for _, family := range conversationMetadataFamilies {
		if remaining == 0 {
			break
		}
		extra := "''"
		if family.table == "conversation_messages" {
			extra = "actor_json"
		}
		var query strings.Builder
		query.WriteString("SELECT rowid,")
		query.WriteString(family.payload)
		query.WriteString(",")
		query.WriteString(extra)
		query.WriteString(" FROM ")
		query.WriteString(family.table)
		query.WriteString(" WHERE query_metadata_version=0 ORDER BY rowid LIMIT ?")
		type candidate struct {
			row        int64
			raw, extra string
		}
		candidates, err := func() ([]candidate, error) {
			rows, err := s.database.reader.QueryContext(ctx, query.String(), remaining)
			if err != nil {
				return nil, err
			}
			defer rows.Close()

			var candidates []candidate
			for rows.Next() {
				var c candidate
				if err := rows.Scan(&c.row, &c.raw, &c.extra); err != nil {
					rows.Close()
					return nil, err
				}
				candidates = append(candidates, c)
			}
			if err := errors.Join(rows.Err(), rows.Close()); err != nil {
				return nil, err
			}
			return candidates, nil
		}()
		if err != nil {
			return err
		}
		for _, c := range candidates {
			metadata, err := decodeConversationQueryMetadata(family.table, c.raw, c.extra)
			if err != nil {
				return fmt.Errorf("decode %s query metadata: %w", family.table, err)
			}
			if err := s.writeConversationQueryMetadata(ctx, family, c.row, c.raw, c.extra, metadata); err != nil {
				return err
			}
			remaining--
		}
	}
	return nil
}

func decodeConversationQueryMetadata(table, raw, extra string) (conversationQueryMetadata, error) {
	var m conversationQueryMetadata
	switch table {
	case "conversations":
		var execution conversation.Execution
		if err := json.Unmarshal([]byte(raw), &execution); err != nil {
			return m, err
		}
		return executionQueryMetadata(execution), nil
	case "conversation_commands":
		receipt, err := decodeConversationReceipt(raw)
		if err != nil {
			return m, err
		}
		m.add("receipt_status", string(receipt.Status))
	case "conversation_messages":
		var actor conversation.Actor
		if err := json.Unmarshal([]byte(extra), &actor); err != nil {
			return m, err
		}
		return messageQueryMetadata([]byte(raw), actor.Kind)
	case "conversation_events":
		return eventQueryMetadata([]byte(raw))
	case "event_compactions":
		var summary struct {
			ThroughSequence int64 `json:"through_sequence"`
		}
		if err := json.Unmarshal([]byte(raw), &summary); err != nil {
			return m, err
		}
		m.add("through_sequence", summary.ThroughSequence)
	case "collaboration_events":
		return activityQueryMetadata([]byte(raw))
	default:
		return m, errors.New("unknown conversation metadata family")
	}
	return m, nil
}

func (s *Service) writeConversationQueryMetadata(ctx context.Context, family conversationMetadataFamily, row int64, raw, extra string, metadata conversationQueryMetadata) error {
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	where := "rowid=? AND query_metadata_version=0 AND " + family.payload + "=?"
	args := []any{row, raw}
	if family.table == "conversation_messages" {
		where += " AND actor_json=?"
		args = append(args, extra)
	}
	if err := metadata.write(ctx, tx, family.table, where, args...); err != nil {
		return err
	}
	return tx.Commit()
}

type legacyCoordinatorResult struct{ id, raw string }

func (s *Service) legacyCoordinatorResults(ctx context.Context, conversationID, actionID string) ([]legacyCoordinatorResult, error) {
	rows, err := s.database.reader.QueryContext(ctx, "SELECT id,data_json FROM conversation_messages WHERE conversation_id=? AND query_metadata_version=0 LIMIT ?", conversationID, storageConversionRows+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var results []legacyCoordinatorResult
	processed := 0
	for rows.Next() {
		processed++
		if processed > storageConversionRows {
			return nil, errors.New("conversation action metadata conversion is pending")
		}
		var c legacyCoordinatorResult
		if err := rows.Scan(&c.id, &c.raw); err != nil {
			return nil, err
		}
		var data queryMessageData
		if err := json.Unmarshal([]byte(c.raw), &data); err != nil {
			return nil, err
		}
		if data.OperatorActionResult != nil && *data.OperatorActionResult == actionID {
			results = append(results, c)
		}
	}
	return results, rows.Err()
}

type legacyConversationContinuation struct {
	id                         string
	head, continued, completed int64
}

func (c *conversationService) prepareLegacyContinuation(ctx context.Context, scope nativeScope, item string) (legacyConversationContinuation, error) {
	var result legacyConversationContinuation
	record, err := c.readLinkedConversation(ctx, c.store.reader, scope, item)
	if err != nil {
		var failure *nativeError
		if errors.As(err, &failure) && failure.status == 404 {
			return result, nil
		}
		return result, err
	}
	result.head = record.EventSeq
	rows, err := c.store.reader.QueryContext(ctx, `SELECT seq,type,body_json,content_attachment_id FROM conversation_events WHERE conversation_id=? AND query_metadata_version=0 AND type IN ('message.updated','execution.updated') ORDER BY seq LIMIT ?`, record.ID, eventCompactionBatch+1)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	type candidate struct {
		seq                int64
		kind, raw, content string
	}
	var candidates []candidate
	for rows.Next() {
		var event candidate
		if err := rows.Scan(&event.seq, &event.kind, &event.raw, &event.content); err != nil {
			return result, err
		}
		candidates = append(candidates, event)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return result, err
	}
	processed := 0
	for _, event := range candidates {
		processed++
		if processed > eventCompactionBatch {
			return result, errors.New("conversation continuation metadata conversion is pending")
		}
		seq, kind, raw := event.seq, event.kind, event.raw
		result.id = record.ID
		if event.content != "" {
			content, err := c.store.readEventStorage(ctx, c.store.reader, record.ID, event.content)
			if err != nil {
				return result, err
			}
			raw = string(content)
		}
		var body struct {
			Role      string `json:"role"`
			Kind      string `json:"kind"`
			AttemptID string `json:"attempt_id"`
			Status    string `json:"status"`
		}
		if err := json.Unmarshal([]byte(raw), &body); err != nil {
			return result, err
		}
		if kind == "message.updated" && body.Role == "user" && body.Kind == "continue" && body.AttemptID != "" {
			result.continued = seq
		}
		if kind == "execution.updated" && body.Status == "completed" {
			result.completed = seq
		}
	}
	return result, nil
}

type legacyToolRevisionKey struct{}
type legacyToolRevision struct {
	payload          json.RawMessage
	raw, actor, text string
	revision         int64
	data             queryMessageData
	actorKind        conversation.ActorKind
}

func (s *conversationStore) prepareLegacyToolRevision(ctx context.Context, id, messageID string) (context.Context, error) {
	prepared, ok := ctx.Value(legacyToolRevisionKey{}).(map[string]legacyToolRevision)
	if !ok || prepared == nil {
		prepared = map[string]legacyToolRevision{}
	}
	if _, ready := prepared[messageID]; ready {
		return ctx, nil
	}
	rows, err := s.reader.QueryContext(ctx, `SELECT id,data_json,actor_json,text,revision,query_metadata_version,actor_kind,tool_name,tool_item_id,tool_status FROM conversation_messages WHERE conversation_id=? AND id=? AND kind='tool' AND content_attachment_id=''`, id, messageID)
	if err != nil {
		return ctx, err
	}
	defer rows.Close()
	var pending []legacyToolRevision
	for rows.Next() {
		var id string
		var c legacyToolRevision
		var version int
		var actorKind, tool, item, status sql.NullString
		if err := rows.Scan(&id, &c.raw, &c.actor, &c.text, &c.revision, &version, &actorKind, &tool, &item, &status); err != nil {
			return ctx, err
		}
		if err := json.Unmarshal([]byte(c.raw), &c.data); err != nil {
			return ctx, err
		}
		var actor conversation.Actor
		if err := json.Unmarshal([]byte(c.actor), &actor); err != nil {
			return ctx, err
		}
		c.actorKind = actor.Kind
		if version == 1 {
			c.actorKind = conversation.ActorKind(actorKind.String)
			c.data.Tool, c.data.ItemID, c.data.Status = &tool.String, &item.String, &status.String
		}
		value := func(value *string) string {
			if value == nil {
				return ""
			}
			return *value
		}
		c.payload, err = json.Marshal(map[string]string{"tool": value(c.data.Tool), "item_id": value(c.data.ItemID), "status": value(c.data.Status)})
		if err != nil {
			return ctx, err
		}
		prepared[id] = c
		pending = append(pending, c)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return ctx, err
	}
	if len(pending) > 0 {
		var organization string
		if err := s.reader.QueryRowContext(ctx, "SELECT organization_id FROM conversations WHERE id=?", id).Scan(&organization); err != nil {
			return ctx, err
		}
		var contents [][]byte
		for _, old := range pending {
			if old.actorKind != conversation.ActorCoordinator {
				continue
			}
			content, err := messageContent(conversationMessageRecord{Text: old.text, Data: old.payload})
			if err != nil {
				return ctx, err
			}
			contents = append(contents, content)
		}
		ctx, err = s.preparePayloads(ctx, organization, contents...)
		if err != nil {
			return ctx, err
		}
	}
	return context.WithValue(ctx, legacyToolRevisionKey{}, prepared), nil
}

func (s *conversationStore) preserveLegacyToolRevision(ctx context.Context, tx *sql.Tx, message conversationMessageRecord) error {
	var pending bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM conversation_messages WHERE conversation_id=? AND id=? AND kind='tool' AND content_attachment_id='')`, message.ConversationID, message.ID).Scan(&pending); err != nil || !pending {
		return err
	}
	prepared, ready := ctx.Value(legacyToolRevisionKey{}).(map[string]legacyToolRevision)
	if !ready {
		return errors.New("conversation tool revision was not prepared")
	}
	old, ok := prepared[message.ID]
	if !ok {
		return errors.New("conversation tool revision was not prepared")
	}
	var unchanged bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM conversation_messages WHERE conversation_id=? AND id=? AND data_json=? AND actor_json=? AND text=? AND revision=?)`, message.ConversationID, message.ID, old.raw, old.actor, old.text, old.revision).Scan(&unchanged); err != nil {
		return err
	}
	if !unchanged {
		return errors.New("conversation tool revision changed after preparation")
	}
	if old.actorKind != conversation.ActorCoordinator {
		return nil
	}
	value := func(p *string) string {
		if p != nil {
			return *p
		}
		return ""
	}
	_, _, contentID, err := s.messageStorage(ctx, tx, conversationMessageRecord{ConversationID: message.ConversationID, Text: old.text, Data: old.payload, CreatedAt: message.CreatedAt})
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO conversation_tool_revisions(conversation_id,message_id,revision,text,tool,item_id,status,content_attachment_id) VALUES(?,?,?,'',?,?,?,?)`, message.ConversationID, message.ID, old.revision, value(old.data.Tool), value(old.data.ItemID), value(old.data.Status), contentID)
	return err
}

func resolveConversationEventMetadata(ctx context.Context, q nativeQueryer, id, query string, args ...any) (string, []any, error) {
	rows, err := q.QueryContext(ctx, `SELECT seq,body_json FROM conversation_events WHERE conversation_id=? AND query_metadata_version=0 ORDER BY seq LIMIT ?`, id, eventCompactionBatch)
	if err != nil {
		return "", nil, err
	}
	defer rows.Close()
	var values []string
	var bound []any
	var columns []string
	for rows.Next() {
		var seq int64
		var raw string
		if err := rows.Scan(&seq, &raw); err != nil {
			rows.Close()
			return "", nil, err
		}
		m, err := eventQueryMetadata([]byte(raw))
		if err != nil {
			rows.Close()
			return "", nil, err
		}
		columns = columns[:0]
		marks := []string{"?"}
		bound = append(bound, seq)
		for i, column := range m.columns {
			columns = append(columns, strings.TrimSuffix(column, "=?"))
			marks = append(marks, "?")
			bound = append(bound, m.values[i])
		}
		values = append(values, "("+strings.Join(marks, ",")+")")
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return "", nil, err
	}
	if len(values) == 0 {
		return query, args, nil
	}
	selection := []string{"e.conversation_id", "e.seq", "e.type", "e.body_json", "e.created_at", "CASE WHEN d.seq IS NOT NULL THEN 1 ELSE e.query_metadata_version END AS query_metadata_version"}
	for _, column := range columns {
		selection = append(selection, "CASE WHEN d.seq IS NOT NULL THEN d."+column+" ELSE e."+column+" END AS "+column)
	}
	prefix := "WITH decoded_event_metadata(seq," + strings.Join(columns, ",") + ") AS (VALUES " + strings.Join(values, ",") + "), resolved_conversation_events AS (SELECT " + strings.Join(selection, ",") + " FROM conversation_events e LEFT JOIN decoded_event_metadata d ON d.seq=e.seq WHERE e.conversation_id=?) "
	bound = append(bound, id)
	bound = append(bound, args...)
	return prefix + strings.ReplaceAll(query, "conversation_events", "resolved_conversation_events"), bound, nil
}

func conversationIsCompactable(ctx context.Context, q nativeQueryer, id string, now time.Time) (bool, error) {
	var raw string
	var version int
	if err := q.QueryRowContext(ctx, "SELECT execution_json,query_metadata_version FROM conversations WHERE id=?", id).Scan(&raw, &version); err != nil {
		return false, err
	}
	query := compactableConversation
	var args []any
	if version == 0 {
		var execution conversation.Execution
		if err := json.Unmarshal([]byte(raw), &execution); err != nil {
			return false, err
		}
		query = strings.ReplaceAll(query, "c.query_metadata_version=0 OR ", "")
		query = strings.ReplaceAll(query, "c.execution_status", "?")
		query = strings.ReplaceAll(query, "c.execution_lease_id", "?")
		args = []any{formatHubTime(now.Add(-eventRetention)), execution.Status, execution.Owner.LeaseID, formatHubTime(now)}
	} else {
		args = []any{formatHubTime(now.Add(-eventRetention)), formatHubTime(now)}
	}
	var full bool
	args = append([]any{id}, args...)
	err := q.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM conversations c WHERE c.id=? AND "+query+")", args...).Scan(&full)
	return full, err
}

func (s *Service) prepareStorageConversionQueryMetadata(ctx context.Context, family storageConversionFamily, row int64, raw, converted []byte) (conversationQueryMetadata, error) {
	if converted == nil {
		return conversationQueryMetadata{}, nil
	}
	var m conversationQueryMetadata
	var err error
	switch family.name {
	case "runs":
		m, err = activityQueryMetadata(converted)
	case "conversation_events":
		m, err = eventQueryMetadata(converted)
	case "messages":
		var content conversationContent
		if err := json.Unmarshal(raw, &content); err != nil {
			return conversationQueryMetadata{}, err
		}
		var actorJSON string
		if err := s.database.reader.QueryRowContext(ctx, "SELECT actor_json FROM conversation_messages WHERE rowid=?", row).Scan(&actorJSON); err != nil {
			return conversationQueryMetadata{}, err
		}
		var actor conversation.Actor
		if err := json.Unmarshal([]byte(actorJSON), &actor); err != nil {
			return conversationQueryMetadata{}, err
		}
		m, err = messageQueryMetadata(content.Data, actor.Kind)
	default:
		return conversationQueryMetadata{}, nil
	}
	return m, err
}
