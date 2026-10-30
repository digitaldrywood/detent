package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubconversation"
)

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
		m.Add("receipt_status", string(receipt.Status))
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
		m.Add("through_sequence", summary.ThroughSequence)
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
	if err := metadata.Write(ctx, tx, family.table, where, args...); err != nil {
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
		for i, column := range m.Columns {
			columns = append(columns, strings.TrimSuffix(column, "=?"))
			marks = append(marks, "?")
			bound = append(bound, m.Values[i])
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
		return s.prepareCollaborationPayloadMetadata(ctx, row, raw)
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

type conversationQueryMetadata = hubconversation.QueryMetadata

type queryMessageData = hubconversation.QueryMessageData

func executionQueryMetadata(execution conversation.Execution) conversationQueryMetadata {
	return hubconversation.ExecutionQueryMetadata(execution)
}

func messageQueryMetadata(raw []byte, actor conversation.ActorKind) (conversationQueryMetadata, error) {
	return hubconversation.MessageQueryMetadata(raw, actor)
}

func eventQueryMetadata(raw []byte) (conversationQueryMetadata, error) {
	return hubconversation.EventQueryMetadata(raw)
}

func activityQueryMetadata(raw []byte) (conversationQueryMetadata, error) {
	return hubconversation.ActivityQueryMetadata(raw)
}
