package postgres

import (
	"context"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) ArtifactReadGrant(ctx context.Context, project tracker.ProjectID, hash string) (artifactstore.ReadGrant, error) {
	row, err := s.queries.ArtifactReadGrantGet(ctx, pgquery.ArtifactReadGrantGetParams{OrganizationID: s.organization, ProjectID: project, TokenHash: hash})
	return artifactstore.ReadGrant{ProjectID: row.ProjectID, TokenHash: row.TokenHash, PrincipalID: row.PrincipalID, ServiceID: row.ServiceID, ArtifactID: row.ArtifactID, Revision: row.Revision, ExpiresAt: row.ExpiresAt, SessionHash: row.HostedSessionHash}, mapError(ctx, err)
}

func (s *session) CreateArtifactReadGrant(ctx context.Context, grant artifactstore.ReadGrant) error {
	if !artifact.ValidHash(grant.TokenHash, 64) || grant.PrincipalID == "" || grant.ExpiresAt.IsZero() {
		return corestore.ErrInvalidCursor
	}
	return mapError(ctx, s.queries.ArtifactReadGrantInsert(ctx, pgquery.ArtifactReadGrantInsertParams{OrganizationID: s.organization, ProjectID: grant.ProjectID, TokenHash: grant.TokenHash, PrincipalID: grant.PrincipalID, ServiceID: grant.ServiceID, ArtifactID: grant.ArtifactID, Revision: grant.Revision, ExpiresAt: recordTime(grant.ExpiresAt), HostedSessionHash: grant.SessionHash}))
}
