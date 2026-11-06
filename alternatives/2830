package postgres

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
)

func (s *session) ReceiptExpiryPage(ctx context.Context, cutoff time.Time, limit int) ([]receiptstore.Record, error) {
	if cutoff.IsZero() || limit < 1 || limit > receiptstore.MaxRetentionBatch {
		return nil, corestore.ErrInvalidPage
	}
	rows, err := s.db.QueryContext(ctx, `SELECT state.organization_id,state.actor_id,state.operation,state.command_key,state.request_hash,c.created_at,c.payload_object_key,c.payload_sha256,c.payload_bytes,c.payload_content_bytes,state.response_claimed,state.response_pending,state.response_request_id,state.response_expired,state.response_replay_until_ns,state.response_reconciliation_hash
FROM native_command_states state JOIN native_commands c USING(organization_id,actor_id,operation,command_key)
WHERE state.organization_id=$1 AND NOT state.response_claimed AND NOT state.response_pending AND NOT state.response_expired
AND state.response_replay_until_ns>0 AND state.response_replay_until_ns<$2
AND (state.operation<>'urgent_runner_update' OR state.response_reconciliation_hash<>'')
ORDER BY state.response_replay_until_ns,state.actor_id,state.operation,state.command_key LIMIT $3`, s.organization, cutoff.UnixNano(), limit)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	records := []receiptstore.Record{}
	for rows.Next() {
		var record receiptstore.Record
		if err := rows.Scan(&record.OrganizationID, &record.ActorID, &record.Operation, &record.CommandKey, &record.RequestHash, &record.CreatedAt, &record.PayloadObjectKey, &record.PayloadSHA256, &record.PayloadBytes, &record.PayloadContentBytes, &record.ResponseClaimed, &record.ResponsePending, &record.ResponseRequestID, &record.ResponseExpired, &record.ResponseReplayUntilNs, &record.ResponseReconciliationHash); err != nil {
			return nil, mapError(ctx, err)
		}
		records = append(records, record)
	}
	return records, mapError(ctx, rows.Err())
}

func (s *session) ExpireReceipt(ctx context.Context, expected receiptstore.Record, cutoff time.Time) (bool, error) {
	if expected.OrganizationID != s.organization || cutoff.IsZero() {
		return false, corestore.ErrInvalidPage
	}
	current, err := s.lockReceipt(ctx, expected.ActorID, expected.Operation, expected.CommandKey)
	if err != nil {
		return false, err
	}
	if current.RequestHash != expected.RequestHash || current.PayloadSHA256 != expected.PayloadSHA256 {
		return false, corestore.ErrRevisionConflict
	}
	if current.ResponseClaimed || current.ResponsePending || current.ResponseExpired || current.ResponseReplayUntilNs <= 0 || current.ResponseReplayUntilNs >= cutoff.UnixNano() || current.Operation == "urgent_runner_update" && current.ResponseReconciliationHash == "" {
		return false, nil
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE native_command_states SET response_expired=true WHERE organization_id=$1 AND actor_id=$2 AND operation=$3 AND command_key=$4`, s.organization, current.ActorID, current.Operation, current.CommandKey); err != nil {
		return false, mapError(ctx, err)
	}
	if err := s.releaseReceipt(ctx, current); err != nil {
		return false, err
	}
	return true, nil
}
