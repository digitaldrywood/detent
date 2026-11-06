package postgres

import (
	"context"
	"strings"
	"time"

	"github.com/lib/pq"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
)

func (s *session) ReceiptMetadataBodyCount(ctx context.Context, records []receiptstore.Record) (int64, error) {
	if len(records) < 1 || len(records) > receiptstore.MaxImportBatch {
		return 0, corestore.ErrInvalidPage
	}
	actors, operations, keys := []string{}, []string{}, []string{}
	for _, record := range records {
		if record.OrganizationID != s.organization || record.ActorID == "" || record.Operation == "" || record.CommandKey == "" {
			return 0, eventstore.ErrInvalidRecord
		}
		actors, operations, keys = append(actors, record.ActorID), append(operations, record.Operation), append(keys, record.CommandKey)
	}
	var count int64
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM native_commands c JOIN unnest($2::text[],$3::text[],$4::text[]) AS source(actor_id,operation,command_key) USING(actor_id,operation,command_key) WHERE c.organization_id=$1`, s.organization, pq.Array(actors), pq.Array(operations), pq.Array(keys)).Scan(&count)
	return count, mapError(ctx, err)
}

func (s *session) receiptStateSources(sources []receiptstore.Import) ([]receiptstore.Record, []any, error) {
	if len(sources) < 1 || len(sources) > receiptstore.MaxImportBatch {
		return nil, nil, corestore.ErrInvalidPage
	}
	actors, operations, keys, hashes, created, requests, reconciliations, snapshots, sourceHashes := []string{}, []string{}, []string{}, []string{}, []string{}, []string{}, []string{}, []string{}, []string{}
	claimed, pending, expired, deadlines := []bool{}, []bool{}, []bool{}, []int64{}
	records := make([]receiptstore.Record, 0, len(sources))
	seen := map[[3]string]bool{}
	for _, source := range sources {
		record := source.Record
		if source.SnapshotID == "" || len(source.SnapshotID) > 200 || strings.TrimSpace(source.SnapshotID) != source.SnapshotID || !artifact.ValidHash(source.SourceHash, 64) || source.ObservedAt.IsZero() || record.CreatedAt.IsZero() {
			return nil, nil, eventstore.ErrInvalidRecord
		}
		if !record.ResponseClaimed && !record.ResponsePending && !record.ResponseExpired && record.ResponseReplayUntilNs > 0 && record.ResponseReplayUntilNs < source.ObservedAt.UnixNano() && (record.Operation != "urgent_runner_update" || record.ResponseReconciliationHash != "") {
			record.ResponseExpired = true
		}
		if err := s.checkReceipt(record); err != nil {
			return nil, nil, err
		}
		key := [3]string{record.ActorID, record.Operation, record.CommandKey}
		if seen[key] || !record.ResponseClaimed && !record.ResponseExpired || record.PayloadObjectKey != "" {
			return nil, nil, eventstore.ErrInvalidRecord
		}
		seen[key] = true
		records = append(records, record)
		actors, operations, keys = append(actors, record.ActorID), append(operations, record.Operation), append(keys, record.CommandKey)
		hashes, created = append(hashes, record.RequestHash), append(created, recordTime(record.CreatedAt).Format(time.RFC3339Nano))
		claimed, pending, expired, deadlines = append(claimed, record.ResponseClaimed), append(pending, record.ResponsePending), append(expired, record.ResponseExpired), append(deadlines, record.ResponseReplayUntilNs)
		requests, reconciliations = append(requests, record.ResponseRequestID), append(reconciliations, record.ResponseReconciliationHash)
		snapshots, sourceHashes = append(snapshots, source.SnapshotID), append(sourceHashes, source.SourceHash)
	}
	args := []any{s.organization, pq.Array(actors), pq.Array(operations), pq.Array(keys), pq.Array(hashes), pq.Array(created), pq.Array(claimed), pq.Array(pending), pq.Array(requests), pq.Array(expired), pq.Array(deadlines), pq.Array(reconciliations), pq.Array(snapshots), pq.Array(sourceHashes)}
	return records, args, nil
}

const receiptSourceRows = `SELECT $1::text organization_id,* FROM unnest($2::text[],$3::text[],$4::text[],$5::text[],$6::timestamptz[],$7::boolean[],$8::boolean[],$9::text[],$10::boolean[],$11::bigint[],$12::text[],$13::text[],$14::text[]) AS input(actor_id,operation,command_key,request_hash,created_at,response_claimed,response_pending,response_request_id,response_expired,response_replay_until_ns,response_reconciliation_hash,source_snapshot_id,source_record_hash)`

func (s *session) ImportReceiptStates(ctx context.Context, sources []receiptstore.Import) error {
	records, args, err := s.receiptStateSources(sources)
	if err != nil {
		return err
	}
	if count, err := s.ReceiptMetadataBodyCount(ctx, records); err != nil {
		return err
	} else if count != 0 {
		return receiptstore.ErrImmutableReceiptPresent
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO native_command_states (organization_id,actor_id,operation,command_key,request_hash,created_at,response_claimed,response_pending,response_request_id,response_expired,response_replay_until_ns,response_reconciliation_hash,source_snapshot_id,source_record_hash) `+receiptSourceRows+` WHERE true ON CONFLICT(organization_id,actor_id,operation,command_key) DO UPDATE SET request_hash=native_command_states.request_hash`, args...); err != nil {
		return mapError(ctx, err)
	}
	if count, err := s.ReceiptMetadataBodyCount(ctx, records); err != nil {
		return err
	} else if count != 0 {
		return receiptstore.ErrImmutableReceiptPresent
	}
	return s.verifyReceiptStates(ctx, args, len(sources))
}

func (s *session) VerifyReceiptStates(ctx context.Context, sources []receiptstore.Import) error {
	_, args, err := s.receiptStateSources(sources)
	if err != nil {
		return err
	}
	return s.verifyReceiptStates(ctx, args, len(sources))
}

func (s *session) verifyReceiptStates(ctx context.Context, args []any, count int) error {
	var verified int
	err := s.db.QueryRowContext(ctx, `WITH source AS (`+receiptSourceRows+`) SELECT count(*) FROM source JOIN native_command_states state USING(organization_id,actor_id,operation,command_key) WHERE state.request_hash=source.request_hash AND state.created_at=source.created_at AND state.response_claimed=source.response_claimed AND state.response_pending=source.response_pending AND state.response_request_id=source.response_request_id AND state.response_expired=source.response_expired AND state.response_replay_until_ns=source.response_replay_until_ns AND state.response_reconciliation_hash=source.response_reconciliation_hash AND state.source_snapshot_id=source.source_snapshot_id AND state.source_record_hash=source.source_record_hash`, args...).Scan(&verified)
	if err != nil {
		return mapError(ctx, err)
	}
	if verified != count {
		return corestore.ErrRevisionConflict
	}
	return nil
}
