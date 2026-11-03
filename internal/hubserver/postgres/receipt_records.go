package postgres

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
)

var _ receiptstore.Writer = (*session)(nil)

func (s *session) ReceiptMember(ctx context.Context, actor string) (accessstore.Member, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT organization_id,user_id,email,membership_id,role,active,principal_id,notify_issues,created_at,updated_at FROM hosted_members WHERE organization_id=$1 AND principal_id=$2 LIMIT 2`+s.accessLock(), s.organization, actor)
	if err != nil {
		return accessstore.Member{}, mapError(ctx, err)
	}
	defer rows.Close()
	var member accessstore.Member
	count := 0
	for rows.Next() {
		count++
		if err := rows.Scan(&member.OrganizationID, &member.UserID, &member.Email, &member.MembershipID, &member.Role, &member.Active, &member.PrincipalID, &member.NotifyIssues, &member.CreatedAt, &member.UpdatedAt); err != nil {
			return accessstore.Member{}, mapError(ctx, err)
		}
	}
	if err := rows.Err(); err != nil {
		return accessstore.Member{}, mapError(ctx, err)
	}
	if count == 0 {
		return accessstore.Member{}, corestore.ErrNotFound
	}
	if count != 1 {
		return accessstore.Member{}, accessstore.ErrDenied
	}
	return member, nil
}

func (s *session) insertReceipt(ctx context.Context, record receiptstore.Record) (receiptstore.Record, bool, error) {
	if err := s.checkReceipt(record); err != nil {
		return receiptstore.Record{}, false, err
	}
	if previous, err := s.Receipt(ctx, record.ActorID, record.Operation, record.CommandKey); err == nil {
		if previous.RequestHash != record.RequestHash {
			return receiptstore.Record{}, false, corestore.ErrRevisionConflict
		}
		return previous, false, nil
	} else if !errors.Is(err, corestore.ErrNotFound) {
		return receiptstore.Record{}, false, err
	}
	record.CreatedAt = recordTime(record.CreatedAt)
	result, err := s.db.ExecContext(ctx, `INSERT INTO native_command_states (organization_id,actor_id,operation,command_key,request_hash,created_at,response_claimed,response_pending,response_request_id,response_expired,response_replay_until_ns,response_reconciliation_hash) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) ON CONFLICT DO NOTHING`, s.organization, record.ActorID, record.Operation, record.CommandKey, record.RequestHash, record.CreatedAt, record.ResponseClaimed, record.ResponsePending, record.ResponseRequestID, record.ResponseExpired, record.ResponseReplayUntilNs, record.ResponseReconciliationHash)
	if err != nil {
		return receiptstore.Record{}, false, mapError(ctx, err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return receiptstore.Record{}, false, err
	}
	if inserted == 0 {
		stored, err := s.Receipt(ctx, record.ActorID, record.Operation, record.CommandKey)
		if err == nil && stored.RequestHash != record.RequestHash {
			err = corestore.ErrRevisionConflict
		}
		return stored, false, err
	}
	if !record.ResponseClaimed && !record.ResponseExpired {
		if err := s.insertReceiptBody(ctx, record); err != nil {
			return receiptstore.Record{}, false, err
		}
	}
	stored, err := s.Receipt(ctx, record.ActorID, record.Operation, record.CommandKey)
	return stored, true, err
}

func (s *session) checkReceipt(record receiptstore.Record) error {
	if err := s.checkOrganization(record.OrganizationID); err != nil {
		return err
	}
	if record.ActorID == "" || record.Operation == "" || record.CommandKey == "" || !artifact.ValidHash(record.RequestHash, 64) || record.ResponseReplayUntilNs < 0 {
		return eventstore.ErrInvalidRecord
	}
	if record.ResponseClaimed && (!record.ResponsePending || record.ResponseExpired || record.ResponseRequestID != "") || record.ResponseReconciliationHash != "" && !artifact.ValidHash(record.ResponseReconciliationHash, 64) {
		return eventstore.ErrInvalidRecord
	}
	if record.ResponseClaimed || record.ResponseExpired {
		return nil
	}
	return validRecordPayload(record.OrganizationID, record.PayloadObjectKey, record.PayloadSHA256, record.PayloadBytes, record.PayloadContentBytes)
}

func (s *session) insertReceiptBody(ctx context.Context, record receiptstore.Record) error {
	_, err := s.queries.RecordInsertReceipt(ctx, pgquery.RecordInsertReceiptParams{
		OrganizationID: s.organization, ActorID: record.ActorID, Operation: record.Operation, CommandKey: record.CommandKey,
		RequestHash: record.RequestHash, CreatedAt: record.CreatedAt, PayloadObjectKey: record.PayloadObjectKey,
		PayloadSHA256: record.PayloadSHA256, PayloadBytes: record.PayloadBytes, PayloadContentBytes: record.PayloadContentBytes,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return corestore.ErrRevisionConflict
	}
	if err != nil {
		return mapError(ctx, err)
	}
	return s.ownReceipt(ctx, record)
}

func receiptOwnerID(record receiptstore.Record) string {
	return artifact.Digest([]byte(strings.Join([]string{record.ActorID, record.Operation, record.CommandKey}, "\x00")))
}

func (s *session) ownReceipt(ctx context.Context, record receiptstore.Record) error {
	ref := eventstore.Reference(record.PayloadObjectKey, record.PayloadSHA256, record.PayloadBytes, record.PayloadContentBytes)
	id := "att_" + artifact.Digest([]byte(string(s.organization) + "\n" + ref.Key))[:32]
	object, err := s.RegisterArchiveObject(ctx, artifactstore.ArchiveObject{OrganizationID: s.organization, ID: id, Reference: ref, CreatedAt: record.CreatedAt})
	if err != nil {
		return err
	}
	return s.AddArchiveOwner(ctx, artifactstore.ArchiveOwner{OrganizationID: s.organization, ArchiveID: object.ID, Kind: "native-command", ID: receiptOwnerID(record), CreatedAt: record.CreatedAt})
}

func (s *session) releaseReceipt(ctx context.Context, record receiptstore.Record) error {
	ref := eventstore.Reference(record.PayloadObjectKey, record.PayloadSHA256, record.PayloadBytes, record.PayloadContentBytes)
	object, err := s.ArchiveObjectByReference(ctx, ref)
	if errors.Is(err, corestore.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.RemoveArchiveOwner(ctx, artifactstore.ArchiveOwner{OrganizationID: s.organization, ArchiveID: object.ID, Kind: "native-command", ID: receiptOwnerID(record), CreatedAt: record.CreatedAt})
}

func (s *session) ClaimReceipt(ctx context.Context, record receiptstore.Record) (receiptstore.Record, bool, error) {
	if !record.ResponseClaimed {
		return receiptstore.Record{}, false, eventstore.ErrInvalidRecord
	}
	record.CreatedAt = recordTime(record.CreatedAt)
	if record.ResponseReplayUntilNs == 0 {
		record.ResponseReplayUntilNs = record.CreatedAt.Add(receiptstore.ReplayRetention).UnixNano()
	}
	return s.insertReceipt(ctx, record)
}

func (s *session) CompleteReceipt(ctx context.Context, record receiptstore.Record) (receiptstore.Record, error) {
	if err := s.checkReceipt(record); err != nil {
		return receiptstore.Record{}, err
	}
	if record.ResponseClaimed || record.ResponseExpired {
		return receiptstore.Record{}, eventstore.ErrInvalidRecord
	}
	previous, err := s.lockReceipt(ctx, record.ActorID, record.Operation, record.CommandKey)
	if err != nil {
		return receiptstore.Record{}, err
	}
	if previous.RequestHash != record.RequestHash {
		return receiptstore.Record{}, corestore.ErrRevisionConflict
	}
	if !previous.ResponseClaimed {
		return previous, nil
	}
	record.CreatedAt = recordTime(record.CreatedAt)
	record.ResponseReplayUntilNs = record.CreatedAt.Add(receiptstore.ReplayRetention).UnixNano()
	if err := s.insertReceiptBody(ctx, record); err != nil {
		return receiptstore.Record{}, err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE native_command_states SET created_at=$5,response_claimed=false,response_pending=$6,response_request_id=$7,response_replay_until_ns=$8,response_reconciliation_hash=$9 WHERE organization_id=$1 AND actor_id=$2 AND operation=$3 AND command_key=$4`, s.organization, record.ActorID, record.Operation, record.CommandKey, record.CreatedAt, record.ResponsePending, record.ResponseRequestID, record.ResponseReplayUntilNs, record.ResponseReconciliationHash)
	if err != nil {
		return receiptstore.Record{}, mapError(ctx, err)
	}
	return s.Receipt(ctx, record.ActorID, record.Operation, record.CommandKey)
}

func (s *session) AbandonReceipt(ctx context.Context, actor, operation, key, hash string) error {
	previous, err := s.lockReceipt(ctx, actor, operation, key)
	if errors.Is(err, corestore.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if previous.RequestHash != hash {
		return corestore.ErrRevisionConflict
	}
	if !previous.ResponseClaimed {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM native_command_states WHERE organization_id=$1 AND actor_id=$2 AND operation=$3 AND command_key=$4`, s.organization, actor, operation, key); err != nil {
		return mapError(ctx, err)
	}
	return nil
}

func (s *session) lockReceipt(ctx context.Context, actor, operation, key string) (receiptstore.Record, error) {
	if _, err := s.db.ExecContext(ctx, `SELECT 1 FROM native_command_states WHERE organization_id=$1 AND actor_id=$2 AND operation=$3 AND command_key=$4 FOR UPDATE`, s.organization, actor, operation, key); err != nil {
		return receiptstore.Record{}, mapError(ctx, err)
	}
	return s.Receipt(ctx, actor, operation, key)
}

func (s *session) receiptRecord(ctx context.Context, actor, operation, key string) (receiptstore.Record, error) {
	var record receiptstore.Record
	err := s.db.QueryRowContext(ctx, `SELECT requested.organization_id,requested.actor_id,requested.operation,requested.command_key,COALESCE(c.request_hash,state.request_hash),COALESCE(c.created_at,state.created_at),COALESCE(c.payload_object_key,''),COALESCE(c.payload_sha256,''),COALESCE(c.payload_bytes,0),COALESCE(c.payload_content_bytes,0),COALESCE(state.response_claimed,false),COALESCE(state.response_pending,false),COALESCE(state.response_request_id,''),COALESCE(state.response_expired,false),COALESCE(state.response_replay_until_ns,0),COALESCE(state.response_reconciliation_hash,'')
FROM (SELECT $1::text organization_id,$2::text actor_id,$3::text operation,$4::text command_key) requested
LEFT JOIN native_commands c USING(organization_id,actor_id,operation,command_key)
LEFT JOIN native_command_states state USING(organization_id,actor_id,operation,command_key)
WHERE c.command_key IS NOT NULL OR state.command_key IS NOT NULL`, s.organization, actor, operation, key).Scan(&record.OrganizationID, &record.ActorID, &record.Operation, &record.CommandKey, &record.RequestHash, &record.CreatedAt, &record.PayloadObjectKey, &record.PayloadSHA256, &record.PayloadBytes, &record.PayloadContentBytes, &record.ResponseClaimed, &record.ResponsePending, &record.ResponseRequestID, &record.ResponseExpired, &record.ResponseReplayUntilNs, &record.ResponseReconciliationHash)
	return record, mapError(ctx, err)
}
