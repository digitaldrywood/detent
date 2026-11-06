package postgres

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
)

func (s *session) OperatorSession(ctx context.Context, id string, after time.Time) (accessstore.OperatorSession, error) {
	row := accessstore.OperatorSession{OrganizationID: s.organization, ID: id}
	err := s.db.QueryRow(ctx, `SELECT principal_id,credential_id,session_id,client,origin_key_id,origin_binding,origin_subject,origin_issuer,origin_generation,last_used_at FROM operator_chat_sessions WHERE organization_id=$1 AND connection_id=$2 AND last_used_at >= $3`, s.organization, id, after).Scan(&row.PrincipalID, &row.CredentialID, &row.SessionID, &row.Client, &row.Origin.KeyID, &row.Origin.CredentialBinding, &row.Origin.Subject, &row.Origin.Issuer, &row.Origin.Generation, &row.LastUsedAt)
	return row, mapError(ctx, err)
}

func (s *session) SaveOperatorSession(ctx context.Context, row accessstore.OperatorSession, limit int) error {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return err
	}
	if row.ID == "" || len(row.ID) > 256 || row.PrincipalID == "" || row.CredentialID == "" || row.LastUsedAt.IsZero() || limit < 1 || limit > 10000 {
		return accessstore.ErrDenied
	}
	if err := s.LockAccessOrganization(ctx); err != nil {
		return err
	}
	result, err := s.db.Exec(ctx, `INSERT INTO operator_chat_sessions(organization_id,connection_id,principal_id,credential_id,session_id,client,origin_key_id,origin_binding,origin_subject,origin_issuer,origin_generation,last_used_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) ON CONFLICT(organization_id,connection_id) DO UPDATE SET last_used_at=excluded.last_used_at WHERE operator_chat_sessions.principal_id=excluded.principal_id AND operator_chat_sessions.credential_id=excluded.credential_id AND operator_chat_sessions.session_id=excluded.session_id AND operator_chat_sessions.origin_key_id=excluded.origin_key_id AND operator_chat_sessions.origin_binding=excluded.origin_binding AND operator_chat_sessions.origin_subject=excluded.origin_subject AND operator_chat_sessions.origin_issuer=excluded.origin_issuer AND operator_chat_sessions.origin_generation=excluded.origin_generation`, s.organization, row.ID, row.PrincipalID, row.CredentialID, row.SessionID, row.Client, row.Origin.KeyID, row.Origin.CredentialBinding, row.Origin.Subject, row.Origin.Issuer, row.Origin.Generation, row.LastUsedAt)
	if err != nil {
		return mapError(ctx, err)
	}
	n := result.RowsAffected()
	if n != 1 {
		return accessstore.ErrDenied
	}
	_, err = s.db.Exec(ctx, `DELETE FROM operator_chat_sessions WHERE organization_id=$1 AND connection_id=(SELECT connection_id FROM operator_chat_sessions WHERE organization_id=$1 ORDER BY last_used_at DESC,connection_id DESC OFFSET $2 LIMIT 1)`, s.organization, limit)
	return mapError(ctx, err)
}
