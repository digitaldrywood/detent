package postgres

import (
	"context"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
)

type CatalogServiceUsageInput = pgquery.CatalogServiceUsageParams

func (s *session) CatalogServiceUsage(ctx context.Context, arg CatalogServiceUsageInput) (CatalogUsage, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return CatalogUsage{}, err
	}
	row, err := s.queries.CatalogServiceUsage(ctx, arg)
	if err != nil {
		return CatalogUsage{}, mapError(ctx, err)
	}
	if !row.HasCounter && row.Active {
		return CatalogUsage{}, artifact.ErrIntegrity
	}
	return CatalogUsage{OrganizationID: s.organization, RetainedBytes: row.RetainedBytes, ReservedBytes: row.ReservedBytes}, nil
}

func (s *session) ensureCatalogServiceUsage(ctx context.Context, service string) (CatalogUsage, error) {
	row, err := s.CatalogServiceUsage(ctx, CatalogServiceUsageInput{OrganizationID: s.organization, ServiceID: service})
	if err != nil {
		return CatalogUsage{}, err
	}
	if err := s.queries.EnsureCatalogServiceUsage(ctx, pgquery.EnsureCatalogServiceUsageParams{OrganizationID: s.organization, ServiceID: service}); err != nil {
		return CatalogUsage{}, mapError(ctx, err)
	}
	return row, nil
}
