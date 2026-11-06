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
	receipt, _, _, found, err := readOperatorMutation(ctx, s.db, m)
	return receipt, found, err
}

type operatorMutationReader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func readOperatorMutation(ctx context.Context, db operatorMutationReader, m mutation.Metadata) (OperatorReceipt, int64, string, bool, error) {
	receipt, id, raw, found, err := readOperatorMutationRows(ctx, db, m, `SELECT id,metadata_json,retry_identity,input_hash,correlation_id,projection_ready FROM workflow_phase_events WHERE phase_type='operator_action' AND retry_identity=? LIMIT 1`, m.RetryIdentity)
	if found || err != nil {
		return receipt, id, raw, found, err
	}
	return readOperatorMutationRows(ctx, db, m, `SELECT id,metadata_json,retry_identity,input_hash,correlation_id,projection_ready FROM workflow_phase_events WHERE phase_type='operator_action' AND projection_ready IS NULL ORDER BY id LIMIT 129`)
}

func readOperatorMutationRows(ctx context.Context, db operatorMutationReader, m mutation.Metadata, query string, args ...any) (OperatorReceipt, int64, string, bool, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return OperatorReceipt{}, 0, "", false, err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		count++
		var id int64
		var raw string
		var retry, hash, correlation sql.NullString
		var ready sql.NullInt64
		if err := rows.Scan(&id, &raw, &retry, &hash, &correlation, &ready); err != nil {
			return OperatorReceipt{}, 0, "", false, err
		}
		var receipt OperatorReceipt
		if err := json.Unmarshal([]byte(raw), &receipt); err != nil {
			if ready.Valid || projectPhase(raw).retry.String == m.RetryIdentity {
				return OperatorReceipt{}, 0, "", false, err
			}
			continue
		}
		if ready.Valid {
			receipt.RetryIdentity, receipt.InputHash, receipt.CorrelationID = retry.String, hash.String, correlation.String
		}
		if receipt.RetryIdentity != m.RetryIdentity {
			continue
		}
		if receipt.InputHash != m.InputHash {
			return receipt, id, raw, true, mutation.ErrConflict
		}
		return receipt, id, raw, true, nil
	}
	if err := rows.Err(); err != nil {
		return OperatorReceipt{}, 0, "", false, err
	}
	if count == 129 {
		return OperatorReceipt{}, 0, "", false, mutation.ErrUncertain
	}
	return OperatorReceipt{}, 0, "", false, nil
}

func (s *sqliteStore) ReserveOperatorMutation(ctx context.Context, m mutation.Metadata) (bool, error) {
	raw, err := json.Marshal(OperatorReceipt{Metadata: m, Outcome: "pending"})
	if err != nil {
		return false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, _, _, found, err := readOperatorMutation(ctx, tx, m); err != nil || found {
		if errors.Is(err, mutation.ErrConflict) {
			return false, nil
		}
		return false, err
	}
	now := time.Now().UTC()
	projection := projectPhase(string(raw))
	result, err := tx.ExecContext(ctx, `INSERT INTO workflow_phase_events(project_id,issue_id,phase_type,phase_name,status,started_at,event_day,endpoint_family,metadata_json,retry_identity,input_hash,correlation_id,projection_ready) VALUES(?,?,'operator_action',?,'pending',?,?,'mcp',?,?,?,?,1) ON CONFLICT DO NOTHING`, m.ProjectID, m.ResourceID, m.Action, now.Format(time.RFC3339Nano), now.Format("2006-01-02"), string(raw), projection.retry, projection.hash, projection.correlation)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return count == 1, nil
}

func (s *sqliteStore) ClaimOperatorMutation(ctx context.Context, m mutation.Metadata) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	receipt, id, previous, found, err := readOperatorMutation(ctx, tx, m)
	if err != nil || !found {
		if errors.Is(err, mutation.ErrConflict) {
			return false, nil
		}
		return false, err
	}
	if receipt.CorrelationID != m.CorrelationID {
		return false, nil
	}
	raw, err := json.Marshal(OperatorReceipt{Metadata: m, Outcome: "pending"})
	if err != nil {
		return false, err
	}
	projection := projectPhase(string(raw))
	existingProjection := projectPhase(previous)
	result, err := tx.ExecContext(ctx, `UPDATE workflow_phase_events SET status='running',metadata_json=?,retry_identity=?,input_hash=?,correlation_id=?,terminal_outcome=NULL,projection_ready=1 WHERE id=? AND metadata_json=? AND status='pending' AND CASE WHEN projection_ready IS NULL THEN ? ELSE retry_identity END=? AND CASE WHEN projection_ready IS NULL THEN ? ELSE input_hash END=? AND CASE WHEN projection_ready IS NULL THEN ? ELSE correlation_id END=?`, string(raw), projection.retry, projection.hash, projection.correlation, id, previous, existingProjection.retry, m.RetryIdentity, existingProjection.hash, m.InputHash, existingProjection.correlation, m.CorrelationID)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return count == 1, nil
}

func (s *sqliteStore) CompleteOperatorMutation(ctx context.Context, receipt OperatorReceipt) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	existing, id, previous, found, err := readOperatorMutation(ctx, tx, receipt.Metadata)
	if err != nil {
		if errors.Is(err, mutation.ErrConflict) {
			return mutation.ErrUncertain
		}
		return err
	}
	if !found || existing.CorrelationID != receipt.CorrelationID {
		return mutation.ErrUncertain
	}
	receipt.CompletedAt = time.Now().UTC()
	raw, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	projection := projectPhase(string(raw))
	existingProjection := projectPhase(previous)
	result, err := tx.ExecContext(ctx, `UPDATE workflow_phase_events SET issue_id=?,identifier=?,issue_url=?,status=?,finished_at=?,metadata_json=?,retry_identity=?,input_hash=?,correlation_id=?,terminal_outcome=NULL,projection_ready=1 WHERE id=? AND metadata_json=? AND status IN ('pending','running') AND CASE WHEN projection_ready IS NULL THEN ? ELSE retry_identity END=? AND CASE WHEN projection_ready IS NULL THEN ? ELSE input_hash END=? AND CASE WHEN projection_ready IS NULL THEN ? ELSE correlation_id END=?`, receipt.ResourceID, receipt.Identifier, receipt.URL, receipt.Outcome, receipt.CompletedAt.Format(time.RFC3339Nano), string(raw), projection.retry, projection.hash, projection.correlation, id, previous, existingProjection.retry, receipt.RetryIdentity, existingProjection.hash, receipt.InputHash, existingProjection.correlation, receipt.CorrelationID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return mutation.ErrUncertain
	}
	return tx.Commit()
}
