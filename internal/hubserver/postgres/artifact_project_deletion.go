package postgres

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) PruneProjectArtifactBindings(ctx context.Context, project tracker.ProjectID, limit int) (bool, error) {
	if limit < 1 || limit > artifactstore.ProjectRetirementBatch {
		return false, corestore.ErrInvalidPage
	}
	now, err := s.queries.LeaseTime(ctx)
	if err != nil {
		return false, mapError(ctx, err)
	}
	if err := s.RequireProjectIdle(ctx, project, now); err != nil {
		return false, err
	}
	if _, err := s.queries.DeleteArtifactProjectReadGrants(ctx, pgquery.DeleteArtifactProjectReadGrantsParams{OrganizationID: s.organization, ProjectID: project, BatchLimit: int32(limit)}); err != nil {
		return false, mapError(ctx, err)
	}
	if _, err := s.queries.DeleteArtifactProjectExpiredUploadAuthorizations(ctx, pgquery.DeleteArtifactProjectExpiredUploadAuthorizationsParams{OrganizationID: s.organization, ProjectID: project, Now: now, BatchLimit: int32(limit)}); err != nil {
		return false, mapError(ctx, err)
	}
	authorizations, err := s.queries.ArtifactProjectHasAuthorizations(ctx, pgquery.ArtifactProjectHasAuthorizationsParams{OrganizationID: s.organization, ProjectID: project})
	if err != nil || authorizations.Bool {
		return false, mapError(ctx, err)
	}
	services, err := s.queries.ArtifactProjectBindingIDs(ctx, pgquery.ArtifactProjectBindingIDsParams{OrganizationID: s.organization, ProjectID: project, BatchLimit: int32(limit)})
	if err != nil {
		return false, mapError(ctx, err)
	}
	if err := s.queries.DeleteArtifactProjectCatalogSettings(ctx, pgquery.DeleteArtifactProjectCatalogSettingsParams{OrganizationID: s.organization, ProjectID: project, ServiceIds: services}); err != nil {
		return false, mapError(ctx, err)
	}
	if err := s.queries.DeleteArtifactProjectBindings(ctx, pgquery.DeleteArtifactProjectBindingsParams{OrganizationID: s.organization, ProjectID: project, ServiceIds: services}); err != nil {
		return false, mapError(ctx, err)
	}
	pending, err := s.queries.ArtifactProjectHasMetadata(ctx, pgquery.ArtifactProjectHasMetadataParams{OrganizationID: s.organization, ProjectID: project})
	return !pending.Bool, mapError(ctx, err)
}

func (s *session) requireProjectArtifactBindingsAccounted(ctx context.Context, project tracker.ProjectID) error {
	pending, err := s.queries.ArtifactProjectHasMetadata(ctx, pgquery.ArtifactProjectHasMetadataParams{OrganizationID: s.organization, ProjectID: project})
	if err != nil {
		return mapError(ctx, err)
	}
	if pending.Bool {
		return projectstore.ErrActiveWork
	}
	return nil
}
