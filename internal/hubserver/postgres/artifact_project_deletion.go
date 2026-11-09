package postgres

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) pruneProjectArtifactBindings(ctx context.Context, project tracker.ProjectID) error {
	if err := s.queries.DeleteArtifactProjectReadGrants(ctx, pgquery.DeleteArtifactProjectReadGrantsParams{OrganizationID: s.organization, ProjectID: project}); err != nil {
		return mapError(ctx, err)
	}
	if err := s.queries.DeleteArtifactProjectUploadAuthorizations(ctx, pgquery.DeleteArtifactProjectUploadAuthorizationsParams{OrganizationID: s.organization, ProjectID: project}); err != nil {
		return mapError(ctx, err)
	}
	if err := s.queries.DeleteArtifactProjectCatalogSettings(ctx, pgquery.DeleteArtifactProjectCatalogSettingsParams{OrganizationID: s.organization, ProjectID: project}); err != nil {
		return mapError(ctx, err)
	}
	return mapError(ctx, s.queries.DeleteArtifactProjectBindings(ctx, pgquery.DeleteArtifactProjectBindingsParams{OrganizationID: s.organization, ProjectID: project}))
}
