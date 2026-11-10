package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
)

type conversationQueryMetadata struct {
	project func(context.Context, *sql.Tx) error
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
	metadata, err := conversationstore.ProjectEventMetadata(raw)
	if err != nil {
		return conversationQueryMetadata{}, err
	}
	var m conversationQueryMetadata
	m.add("message_id", metadata.MessageID)
	m.add("object_id", metadata.ObjectID)
	m.add("message_role", metadata.Role)
	m.add("message_kind", metadata.Kind)
	m.add("attempt_id", metadata.AttemptID)
	m.add("execution_status", metadata.Status)
	m.add("thread_id", metadata.ThreadID)
	m.add("turn_id", metadata.TurnID)
	m.add("snapshot_title", metadata.Title)
	m.add("snapshot_visibility", metadata.Visibility)
	m.add("snapshot_status", metadata.Status)
	m.add("snapshot_subject_work_item_id", metadata.SubjectWorkItem)
	m.add("snapshot_work_item_id", metadata.WorkItem)
	m.add("snapshot_linked_at", metadata.LinkedAt)
	m.add("delta_offset", metadata.DeltaOffset)
	m.add("delta_bytes", metadata.DeltaBytes)
	m.add("message_revision", metadata.MessageRevision)
	m.add("snapshot_preferences_key", metadata.PreferencesKey)
	m.add("snapshot_model", metadata.Model)
	m.add("snapshot_effort", metadata.Effort)
	m.add("snapshot_access", metadata.Access)
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
		return s.prepareCollaborationPayloadMetadata(ctx, row, raw)
	case "conversation_events":
		m, err = eventQueryMetadata(converted)
	default:
		return conversationQueryMetadata{}, nil
	}
	return m, err
}
