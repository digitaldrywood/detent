package postgres

import (
	"context"
	"database/sql"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) RecordAccessAudit(ctx context.Context, row accessstore.Audit) (int64, error) {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return 0, err
	}
	var key, digest, compressed, content, version any
	if row.Mutation != nil {
		if err := row.Mutation.Validate(string(s.organization)); err != nil {
			return 0, err
		}
		key, digest, compressed, content, version = row.Mutation.Key, row.Mutation.SHA256, row.Mutation.CompressedSize, row.Mutation.UncompressedSize, row.Mutation.SchemaVersion
	}
	err := s.db.QueryRowContext(ctx, `INSERT INTO hosted_audit(organization_id,id,session_id,actual_actor,effective_user,reason,event,route,project_id,status,started_at,expires_at,recorded_at,recorded_at_ns,mutation_object_key,mutation_sha256,mutation_bytes,mutation_content_bytes,mutation_format_version,account_kind,account_retry,key_id,key_kind,key_owner,resource)
 VALUES($1,COALESCE(NULLIF($2,0),nextval('hosted_audit_id_seq')),$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25) RETURNING id`, s.organization, row.ID, row.SessionID, row.ActualActor, row.EffectiveUser, row.Reason, row.Event, row.Route, row.ProjectID, row.Status, row.StartedAt, row.ExpiresAt, row.RecordedAt, row.RecordedAtNS, key, digest, compressed, content, version, row.AccountKind, row.AccountRetry, row.KeyID, row.KeyKind, row.KeyOwner, row.Resource).Scan(&row.ID)
	if err != nil {
		return 0, mapError(ctx, err)
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO hosted_audit_times(organization_id,audit_id,recorded_at_ns) VALUES($1,$2,$3)`, s.organization, row.ID, row.RecordedAtNS)
	return row.ID, mapError(ctx, err)
}

func (s *session) AccessAudit(ctx context.Context, id int64) (accessstore.Audit, error) {
	var row accessstore.Audit
	var key, digest sql.NullString
	var compressed, content, version sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT organization_id,id,session_id,actual_actor,effective_user,reason,event,route,project_id,status,started_at,expires_at,recorded_at,recorded_at_ns,mutation_object_key,mutation_sha256,mutation_bytes,mutation_content_bytes,mutation_format_version,account_kind,account_retry,key_id,key_kind,key_owner,resource FROM hosted_audit WHERE organization_id=$1 AND id=$2`, s.organization, id).Scan(&row.OrganizationID, &row.ID, &row.SessionID, &row.ActualActor, &row.EffectiveUser, &row.Reason, &row.Event, &row.Route, &row.ProjectID, &row.Status, &row.StartedAt, &row.ExpiresAt, &row.RecordedAt, &row.RecordedAtNS, &key, &digest, &compressed, &content, &version, &row.AccountKind, &row.AccountRetry, &row.KeyID, &row.KeyKind, &row.KeyOwner, &row.Resource)
	if err != nil {
		return row, mapError(ctx, err)
	}
	if key.Valid {
		row.Mutation = &attachment.ArchiveReference{Key: key.String, SHA256: digest.String, CompressedSize: compressed.Int64, UncompressedSize: content.Int64, SchemaVersion: int(version.Int64), Codec: "gzip", Kind: "archive", Retention: attachment.ArchiveRetention}
	}
	return row, nil
}

func (s *session) UpdateAccessKeyAudit(ctx context.Context, id int64, key, operation string, project tracker.ProjectID, resource string, status int) error {
	result, err := s.db.ExecContext(ctx, `UPDATE hosted_audit SET route=$4,project_id=$5,resource=$6,status=$7 WHERE organization_id=$1 AND id=$2 AND key_id=$3`, s.organization, id, key, operation, project, resource, status)
	if err != nil {
		return mapError(ctx, err)
	}
	n, err := result.RowsAffected()
	return affected(ctx, n, err)
}
