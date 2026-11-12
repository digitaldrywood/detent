package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

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
