package postgres

import (
	"context"
	"fmt"

	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) AccessSecret(ctx context.Context, project tracker.ProjectID, kind string) (accessstore.Secret, error) {
	row := accessstore.Secret{OrganizationID: s.organization, ProjectID: project, Kind: kind}
	query := `SELECT organization_slug,ciphertext,nonce,wrapped_data_key,master_key_version,updated_at FROM organization_secrets WHERE organization_id=$1 AND kind=$2`
	args := []any{s.organization, kind}
	if project != "" {
		query = `SELECT organization_slug,ciphertext,nonce,wrapped_data_key,master_key_version,updated_at FROM project_secrets WHERE organization_id=$1 AND kind=$2 AND project_id=$3`
		args = append(args, project)
	}
	err := s.db.QueryRow(ctx, query, args...).Scan(&row.OrganizationSlug, &row.Envelope.Ciphertext, &row.Envelope.Nonce, &row.Envelope.WrappedKey, &row.Envelope.Version, &row.UpdatedAt)
	return row, mapError(ctx, err)
}

func (s *session) AccessSecrets(ctx context.Context, afterProject tracker.ProjectID, afterKind string, limit int) ([]accessstore.Secret, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, `SELECT organization_id,project_id,kind,organization_slug,ciphertext,nonce,wrapped_data_key,master_key_version,updated_at FROM (
 SELECT organization_id,project_id,kind,organization_slug,ciphertext,nonce,wrapped_data_key,master_key_version,updated_at FROM project_secrets WHERE organization_id=$1
 UNION ALL SELECT organization_id,'' AS project_id,kind,organization_slug,ciphertext,nonce,wrapped_data_key,master_key_version,updated_at FROM organization_secrets WHERE organization_id=$1
 ) secrets WHERE (project_id,kind)>($2,$3) ORDER BY project_id,kind LIMIT $4`, s.organization, afterProject, afterKind, limit)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	result := []accessstore.Secret{}
	for rows.Next() {
		var row accessstore.Secret
		if err := rows.Scan(&row.OrganizationID, &row.ProjectID, &row.Kind, &row.OrganizationSlug, &row.Envelope.Ciphertext, &row.Envelope.Nonce, &row.Envelope.WrappedKey, &row.Envelope.Version, &row.UpdatedAt); err != nil {
			return nil, mapError(ctx, err)
		}
		result = append(result, row)
	}
	return result, mapError(ctx, rows.Err())
}

func (s *session) StoreAccessSecret(ctx context.Context, row accessstore.Secret, audit accessstore.SecretAudit) error {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return err
	}
	if audit.OrganizationID != row.OrganizationID || audit.ProjectID != row.ProjectID || audit.Kind != row.Kind || audit.KeyVersion != row.Envelope.Version {
		return accessstore.ErrDenied
	}
	var err error
	if row.ProjectID == "" {
		_, err = s.db.Exec(ctx, `INSERT INTO organization_secrets(organization_id,kind,organization_slug,ciphertext,nonce,wrapped_data_key,master_key_version,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)
 ON CONFLICT(organization_id,kind) DO UPDATE SET organization_slug=excluded.organization_slug,ciphertext=excluded.ciphertext,nonce=excluded.nonce,wrapped_data_key=excluded.wrapped_data_key,master_key_version=excluded.master_key_version,updated_at=excluded.updated_at`, row.OrganizationID, row.Kind, row.OrganizationSlug, row.Envelope.Ciphertext, row.Envelope.Nonce, row.Envelope.WrappedKey, row.Envelope.Version, row.UpdatedAt)
	} else {
		_, err = s.db.Exec(ctx, `INSERT INTO project_secrets(organization_id,project_id,kind,organization_slug,ciphertext,nonce,wrapped_data_key,master_key_version,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)
 ON CONFLICT(organization_id,project_id,kind) DO UPDATE SET organization_slug=excluded.organization_slug,ciphertext=excluded.ciphertext,nonce=excluded.nonce,wrapped_data_key=excluded.wrapped_data_key,master_key_version=excluded.master_key_version,updated_at=excluded.updated_at`, row.OrganizationID, row.ProjectID, row.Kind, row.OrganizationSlug, row.Envelope.Ciphertext, row.Envelope.Nonce, row.Envelope.WrappedKey, row.Envelope.Version, row.UpdatedAt)
	}
	if err != nil {
		return mapError(ctx, err)
	}
	return s.RecordAccessSecretAudit(ctx, audit)
}

func (s *session) DeleteAccessSecret(ctx context.Context, project tracker.ProjectID, kind string, audit accessstore.SecretAudit) error {
	if err := s.checkOrganization(audit.OrganizationID); err != nil {
		return err
	}
	if audit.ProjectID != project || audit.Kind != kind {
		return accessstore.ErrDenied
	}
	var err error
	if project == "" {
		_, err = s.db.Exec(ctx, `DELETE FROM organization_secrets WHERE organization_id=$1 AND kind=$2`, s.organization, kind)
	} else {
		_, err = s.db.Exec(ctx, `DELETE FROM project_secrets WHERE organization_id=$1 AND project_id=$2 AND kind=$3`, s.organization, project, kind)
	}
	if err != nil {
		return mapError(ctx, err)
	}
	return s.RecordAccessSecretAudit(ctx, audit)
}

func (s *session) RecordAccessSecretAudit(ctx context.Context, audit accessstore.SecretAudit) error {
	if err := s.checkOrganization(audit.OrganizationID); err != nil {
		return err
	}
	var err error
	if audit.ProjectID == "" {
		_, err = s.db.Exec(ctx, `INSERT INTO organization_secret_audit(organization_id,actor,kind,key_version,event,recorded_at) VALUES($1,$2,$3,$4,$5,$6)`, s.organization, audit.Actor, audit.Kind, audit.KeyVersion, audit.Event, audit.RecordedAt)
	} else {
		_, err = s.db.Exec(ctx, `INSERT INTO project_secret_audit(organization_id,project_id,actor,kind,key_version,event,recorded_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, s.organization, audit.ProjectID, audit.Actor, audit.Kind, audit.KeyVersion, audit.Event, audit.RecordedAt)
	}
	if err != nil {
		return fmt.Errorf("record secret audit: %w", mapError(ctx, err))
	}
	return nil
}

func (s *session) RewrapAccessSecret(ctx context.Context, prior accessstore.Secret, envelope hubsecrets.Envelope, audit accessstore.SecretAudit) error {
	if err := s.checkOrganization(prior.OrganizationID); err != nil {
		return err
	}
	if audit.OrganizationID != prior.OrganizationID || audit.ProjectID != prior.ProjectID || audit.Kind != prior.Kind || audit.KeyVersion != envelope.Version {
		return accessstore.ErrDenied
	}
	query := `UPDATE organization_secrets SET wrapped_data_key=$3,master_key_version=$4 WHERE organization_id=$1 AND kind=$2 AND wrapped_data_key=$5 AND master_key_version=$6 AND ciphertext=$7 AND nonce=$8 AND updated_at=$9`
	args := []any{s.organization, prior.Kind, envelope.WrappedKey, envelope.Version, prior.Envelope.WrappedKey, prior.Envelope.Version, prior.Envelope.Ciphertext, prior.Envelope.Nonce, prior.UpdatedAt}
	if prior.ProjectID != "" {
		query = `UPDATE project_secrets SET wrapped_data_key=$3,master_key_version=$4 WHERE organization_id=$1 AND kind=$2 AND wrapped_data_key=$5 AND master_key_version=$6 AND ciphertext=$7 AND nonce=$8 AND updated_at=$9 AND project_id=$10`
		args = append(args, prior.ProjectID)
	}
	result, err := s.db.Exec(ctx, query, args...)
	if err != nil {
		return mapError(ctx, err)
	}
	n := result.RowsAffected()
	if n == 0 {
		return corestore.ErrRevisionConflict
	}
	return s.RecordAccessSecretAudit(ctx, audit)
}
