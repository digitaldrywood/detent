package postgres

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
)

func (s *session) OrganizationModels(ctx context.Context) (policystore.OrganizationModels, error) {
	row := policystore.OrganizationModels{OrganizationID: s.organization}
	var key, digest pgtype.Text
	var size, content pgtype.Int8
	err := s.db.QueryRow(ctx, `SELECT model_selection_revision,model_selection_object_key,model_selection_sha256,model_selection_bytes,model_selection_content_bytes FROM organizations WHERE organization_id=$1`, s.organization).Scan(&row.Revision, &key, &digest, &size, &content)
	if err != nil {
		return row, mapError(ctx, err)
	}
	if row.Revision > 0 {
		row.Payload = eventstore.Reference(key.String, digest.String, size.Int64, content.Int64)
		if err := row.Payload.Validate(string(s.organization)); err != nil {
			return row, eventstore.ErrInvalidRecord
		}
	}
	return row, nil
}

func (s *session) SetOrganizationModels(ctx context.Context, row policystore.OrganizationModels, expected int64) error {
	if row.OrganizationID != s.organization {
		return corestore.ErrTenantMismatch
	}
	if expected < 0 || row.Revision != expected+1 || row.Payload.Validate(string(s.organization)) != nil || row.Payload.BindingID != "" || row.Payload.CustomerOwned || row.Payload.SchemaVersion != 1 {
		return eventstore.ErrInvalidRecord
	}
	result, err := s.db.Exec(ctx, `UPDATE organizations SET model_selection_revision=$2,model_selection_object_key=$3,model_selection_sha256=$4,model_selection_bytes=$5,model_selection_content_bytes=$6 WHERE organization_id=$1 AND model_selection_revision=$7`, s.organization, row.Revision, row.Payload.Key, row.Payload.SHA256, row.Payload.CompressedSize, row.Payload.UncompressedSize, expected)
	if err != nil {
		return mapError(ctx, err)
	}
	changed := result.RowsAffected()
	if changed != 1 {
		return corestore.ErrRevisionConflict
	}
	return nil
}

func (s *session) RequireOrganizationModels(ctx context.Context, expected int64) error {
	var revision int64
	if err := s.db.QueryRow(ctx, `SELECT model_selection_revision FROM organizations WHERE organization_id=$1 FOR SHARE`, s.organization).Scan(&revision); err != nil {
		return mapError(ctx, err)
	}
	if revision != expected {
		return corestore.ErrRevisionConflict
	}
	return nil
}
