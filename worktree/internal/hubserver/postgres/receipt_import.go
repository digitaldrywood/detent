package postgres

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
)

var _ receiptstore.ImportWriter = (*session)(nil)

func (s *session) ImportReceipt(ctx context.Context, source receiptstore.Import) (receiptstore.Record, error) {
	record := source.Record
	if source.SnapshotID == "" || len(source.SnapshotID) > 200 || strings.TrimSpace(source.SnapshotID) != source.SnapshotID || !artifact.ValidHash(source.SourceHash, 64) || source.ObservedAt.IsZero() || record.CreatedAt.IsZero() {
		return receiptstore.Record{}, eventstore.ErrInvalidRecord
	}
	if !record.ResponseClaimed && !record.ResponsePending && !record.ResponseExpired && record.ResponseReplayUntilNs > 0 && record.ResponseReplayUntilNs < source.ObservedAt.UnixNano() && (record.Operation != "urgent_runner_update" || record.ResponseReconciliationHash != "") {
		record.ResponseExpired = true
	}
	if err := s.checkReceipt(record); err != nil {
		return receiptstore.Record{}, err
	}
	var snapshot, sourceHash, requestHash string
	err := s.db.QueryRow(ctx, `SELECT source_snapshot_id,source_record_hash,request_hash FROM native_command_states WHERE organization_id=$1 AND actor_id=$2 AND operation=$3 AND command_key=$4 FOR UPDATE`, s.organization, record.ActorID, record.Operation, record.CommandKey).Scan(&snapshot, &sourceHash, &requestHash)
	if err == nil {
		if requestHash != record.RequestHash || snapshot != "" && (snapshot != source.SnapshotID || sourceHash != source.SourceHash) {
			return receiptstore.Record{}, corestore.ErrRevisionConflict
		}
		return s.Receipt(ctx, record.ActorID, record.Operation, record.CommandKey)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return receiptstore.Record{}, mapError(ctx, err)
	}
	previous, previousErr := s.Receipt(ctx, record.ActorID, record.Operation, record.CommandKey)
	if previousErr == nil && previous.RequestHash != record.RequestHash {
		return receiptstore.Record{}, corestore.ErrRevisionConflict
	}
	if previousErr != nil && !errors.Is(previousErr, corestore.ErrNotFound) {
		return receiptstore.Record{}, previousErr
	}
	if previousErr == nil {
		return receiptstore.Record{}, receiptstore.ErrImmutableReceiptPresent
	}
	_, err = s.db.Exec(ctx, `INSERT INTO native_command_states (organization_id,actor_id,operation,command_key,request_hash,created_at,response_claimed,response_pending,response_request_id,response_expired,response_replay_until_ns,response_reconciliation_hash,source_snapshot_id,source_record_hash) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`, s.organization, record.ActorID, record.Operation, record.CommandKey, record.RequestHash, recordTime(record.CreatedAt), record.ResponseClaimed, record.ResponsePending, record.ResponseRequestID, record.ResponseExpired, record.ResponseReplayUntilNs, record.ResponseReconciliationHash, source.SnapshotID, source.SourceHash)
	if err != nil {
		return receiptstore.Record{}, mapError(ctx, err)
	}
	if !record.ResponseClaimed && !record.ResponseExpired {
		if err := s.insertReceiptBody(ctx, record); err != nil {
			return receiptstore.Record{}, err
		}
	}
	return s.Receipt(ctx, record.ActorID, record.Operation, record.CommandKey)
}

func (s *session) ReceiptSource(ctx context.Context, actor, operation, key string) (string, string, error) {
	var snapshot, sourceHash string
	err := s.db.QueryRow(ctx, `SELECT source_snapshot_id,source_record_hash FROM native_command_states WHERE organization_id=$1 AND actor_id=$2 AND operation=$3 AND command_key=$4`, s.organization, actor, operation, key).Scan(&snapshot, &sourceHash)
	return snapshot, sourceHash, mapError(ctx, err)
}
