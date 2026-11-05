package postgres

import (
	"context"
	"slices"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) ArtifactService(ctx context.Context, project tracker.ProjectID, id string) (artifact.Binding, error) {
	row, err := s.queries.ArtifactServiceGet(ctx, pgquery.ArtifactServiceGetParams{OrganizationID: s.organization, ProjectID: project, ID: id})
	return artifactServiceBinding(row), mapError(ctx, err)
}

func (s *session) ArtifactServices(ctx context.Context, project tracker.ProjectID) ([]artifact.Binding, error) {
	rows, err := s.queries.ArtifactServiceList(ctx, pgquery.ArtifactServiceListParams{OrganizationID: s.organization, ProjectID: project})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	result := make([]artifact.Binding, 0, len(rows))
	for _, row := range rows {
		result = append(result, artifactServiceBinding(row))
	}
	return result, nil
}

func (s *session) BindArtifactService(ctx context.Context, project tracker.ProjectID, binding artifact.Binding) error {
	if !artifact.ValidID(binding.ServiceID, "service") || !artifact.ValidOrigin(binding.Origin) || !slices.Contains([]string{"customer", "hosted"}, binding.Mode) || binding.Mode == "hosted" && !binding.HostedOptIn || binding.PublisherTokenID == "" {
		return corestore.ErrInvalidCursor
	}
	count, err := s.queries.ArtifactServiceBind(ctx, pgquery.ArtifactServiceBindParams{OrganizationID: s.organization, ProjectID: project, ID: binding.ServiceID, Origin: binding.Origin, BindingMode: binding.Mode, HostedOptIn: binding.HostedOptIn, PublisherTokenID: binding.PublisherTokenID})
	if err != nil {
		return mapError(ctx, err)
	}
	if count != 1 {
		return corestore.ErrNotFound
	}
	return nil
}

func artifactServiceBinding(row pgquery.ArtifactService) artifact.Binding {
	return artifact.Binding{ServiceID: row.ID, Origin: row.Origin, Mode: row.BindingMode, HostedOptIn: row.HostedOptIn, PublisherTokenID: row.PublisherTokenID}
}
