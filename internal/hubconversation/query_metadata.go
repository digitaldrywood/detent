package hubconversation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/digitaldrywood/detent/internal/conversation"
)

type QueryMetadata struct {
	Project func(context.Context, *sql.Tx) error
	Columns []string
	Values  []any
}

func (m *QueryMetadata) Add(column string, value any) {
	m.Columns = append(m.Columns, column+"=?")
	m.Values = append(m.Values, value)
}

func (m QueryMetadata) Write(ctx context.Context, tx *sql.Tx, table, where string, args ...any) error {
	values := append(append([]any{}, m.Values...), args...)
	var statement strings.Builder
	statement.WriteString("UPDATE ")
	statement.WriteString(table)
	statement.WriteString(" SET query_metadata_version=1,")
	statement.WriteString(strings.Join(m.Columns, ","))
	statement.WriteString(" WHERE ")
	statement.WriteString(where)
	_, err := tx.ExecContext(ctx, statement.String(), values...)
	return err
}

func ExecutionQueryMetadata(execution conversation.Execution) QueryMetadata {
	var m QueryMetadata
	m.Add("execution_status", string(execution.Status))
	m.Add("execution_lease_id", execution.Owner.LeaseID)
	m.Add("execution_settled", execution.Settled)
	return m
}

type QueryMessageData struct {
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

func MessageQueryMetadata(raw []byte, actor conversation.ActorKind) (QueryMetadata, error) {
	var data QueryMessageData
	if trimmed := bytes.TrimSpace(raw); len(trimmed) > 0 && trimmed[0] != '{' {
		raw = []byte("{}")
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return QueryMetadata{}, err
	}
	var m QueryMetadata
	m.Add("actor_kind", string(actor))
	m.Add("tool_name", data.Tool)
	m.Add("tool_item_id", data.ItemID)
	m.Add("tool_status", data.Status)
	var id, agent *string
	if data.OperatorAction != nil && data.OperatorAction.Action != nil {
		id, agent = data.OperatorAction.Action.ID, data.OperatorAction.Action.Agent
	}
	m.Add("operator_action_id", id)
	m.Add("operator_action_agent", agent)
	m.Add("operator_action_result", data.OperatorActionResult)
	return m, nil
}

func EventQueryMetadata(raw []byte) (QueryMetadata, error) {
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		return QueryMetadata{}, err
	}
	var m QueryMetadata
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
		m.Add(field.column, value)
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
		m.Add(field.column, value)
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
	m.Add("snapshot_preferences_key", key)
	m.Add("snapshot_model", preferences.Model)
	m.Add("snapshot_effort", preferences.Effort)
	m.Add("snapshot_access", preferences.Access)
	return m, nil
}

func ActivityQueryMetadata(raw []byte) (QueryMetadata, error) {
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
		return QueryMetadata{}, err
	}
	var m QueryMetadata
	var attempt *string
	present := false
	if data.Run != nil {
		attempt = data.Run.AttemptID
		if r := data.Run.Runtime; r != nil && r.Activity != nil {
			spans := bytes.TrimSpace(r.Activity.Spans)
			present = len(spans) > 0 && spans[0] == '['
		}
	}
	m.Add("run_attempt_id", attempt)
	m.Add("activity_spans_present", present)
	return m, nil
}
