package postgres

import (
	"context"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type CatalogPolicyInput = pgquery.CatalogPolicyParams

func (s *session) CatalogPolicy(ctx context.Context, arg CatalogPolicyInput) (CatalogBinding, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return CatalogBinding{}, err
	}
	if !s.readOnly {
		row, err := s.queries.LockCatalogPolicy(ctx, pgquery.LockCatalogPolicyParams(arg))
		return row, mapError(ctx, err)
	}
	row, err := s.queries.CatalogPolicy(ctx, arg)
	return row, mapError(ctx, err)
}

func (s *session) SetCatalogPolicy(ctx context.Context, project tracker.ProjectID, service string, p artifact.Policy) (CatalogBinding, error) {
	if err := p.Validate(); err != nil {
		return CatalogBinding{}, err
	}
	row, err := s.queries.SetCatalogPolicy(ctx, pgquery.SetCatalogPolicyParams{OrganizationID: s.organization, ProjectID: project, ServiceID: service, PolicyID: p.ID, RetainedLimit: p.Limits.RetainedBytes, ReservedLimit: p.Limits.ReservedBytes, ArtifactLimit: p.Limits.ArtifactBytes, UploadLimit: p.Limits.UploadBytes, RetentionSeconds: p.Limits.RetentionSeconds, AbandonedSeconds: p.AbandonedUploadSeconds, DeletionSeconds: p.DeletionRecordSeconds, BackupSeconds: p.BackupSeconds})
	return row, mapError(ctx, err)
}
