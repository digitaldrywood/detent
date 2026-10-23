package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/mutation"
)

// OperatorReceipt uses the existing operator event record. Pending effects are
// never taken over: uncertain external outcomes must be inspected by an operator.
type OperatorReceipt struct {
	ConfigurationJSON json.RawMessage `json:"configuration_receipt,omitempty"`
	Revision          int64           `json:"revision,omitempty"`
	CommentID         string          `json:"comment_id,omitempty"`
	mutation.Metadata
	Outcome     string    `json:"outcome"`
	Identifier  string    `json:"identifier,omitempty"`
	URL         string    `json:"url,omitempty"`
	CompletedAt time.Time `json:"completed_at,omitzero"`
}

type OperatorMutations interface {
	OperatorMutation(context.Context, mutation.Metadata) (OperatorReceipt, bool, error)
	ReserveOperatorMutation(context.Context, mutation.Metadata) (bool, error)
	ClaimOperatorMutation(context.Context, mutation.Metadata) (bool, error)
	CompleteOperatorMutation(context.Context, OperatorReceipt) error
}

func (s *sqliteStore) OperatorMutation(ctx context.Context, m mutation.Metadata) (OperatorReceipt, bool, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT metadata_json FROM workflow_phase_events WHERE phase_type='operator_action' AND json_valid(metadata_json) AND json_extract(metadata_json,'$.retry_identity') IS NOT NULL AND json_extract(metadata_json,'$.retry_identity')=?`, m.RetryIdentity).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return OperatorReceipt{}, false, nil
	}
	if err != nil {
		return OperatorReceipt{}, false, err
	}
	var receipt OperatorReceipt
	if err := json.Unmarshal([]byte(raw), &receipt); err != nil {
		return receipt, false, err
	}
	if receipt.InputHash != m.InputHash {
		return receipt, true, mutation.ErrConflict
	}
	return receipt, true, nil
}

func (s *sqliteStore) ReserveOperatorMutation(ctx context.Context, m mutation.Metadata) (bool, error) {
	raw, err := json.Marshal(OperatorReceipt{Metadata: m, Outcome: "pending"})
	if err != nil {
		return false, err
	}
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx, `INSERT INTO workflow_phase_events(project_id,issue_id,phase_type,phase_name,status,started_at,event_day,endpoint_family,metadata_json) VALUES(?,?,'operator_action',?,'pending',?,?,'mcp',?) ON CONFLICT DO NOTHING`, m.ProjectID, m.ResourceID, m.Action, now.Format(time.RFC3339Nano), now.Format("2006-01-02"), string(raw))
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

// ClaimOperatorMutation starts only the original submission. A correlation
// belongs to a server-created action, not a client-supplied replay or takeover.
func (s *sqliteStore) ClaimOperatorMutation(ctx context.Context, m mutation.Metadata) (bool, error) {
	raw, err := json.Marshal(OperatorReceipt{Metadata: m, Outcome: "pending"})
	if err != nil {
		return false, err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE workflow_phase_events SET status='running',metadata_json=? WHERE phase_type='operator_action' AND json_valid(metadata_json) AND json_extract(metadata_json,'$.retry_identity') IS NOT NULL AND json_extract(metadata_json,'$.retry_identity')=? AND json_extract(metadata_json,'$.input_hash')=? AND json_extract(metadata_json,'$.correlation_id')=? AND status='pending'`, string(raw), m.RetryIdentity, m.InputHash, m.CorrelationID)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

func (s *sqliteStore) CompleteOperatorMutation(ctx context.Context, receipt OperatorReceipt) error {
	receipt.CompletedAt = time.Now().UTC()
	raw, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE workflow_phase_events SET issue_id=?,identifier=?,issue_url=?,status=?,finished_at=?,metadata_json=? WHERE phase_type='operator_action' AND json_valid(metadata_json) AND json_extract(metadata_json,'$.retry_identity') IS NOT NULL AND json_extract(metadata_json,'$.retry_identity')=? AND json_extract(metadata_json,'$.input_hash')=? AND json_extract(metadata_json,'$.correlation_id')=? AND status IN ('pending','running')`, receipt.ResourceID, receipt.Identifier, receipt.URL, receipt.Outcome, receipt.CompletedAt.Format(time.RFC3339Nano), string(raw), receipt.RetryIdentity, receipt.InputHash, receipt.CorrelationID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err == nil && count != 1 {
		return mutation.ErrUncertain
	}
	return err
}
