package postgres

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) ArtifactReadGrant(ctx context.Context, project tracker.ProjectID, hash string) (artifactstore.ReadGrant, error) {
	row, err := s.queries.ArtifactReadGrantGet(ctx, pgquery.ArtifactReadGrantGetParams{OrganizationID: s.organization, ProjectID: project, TokenHash: hash})
	return artifactstore.ReadGrant{ProjectID: row.ProjectID, WorkItemID: tracker.NativeWorkItemID(row.WorkItemID), TokenHash: row.TokenHash, PrincipalID: row.PrincipalID, ServiceID: row.ServiceID, ArtifactID: row.ArtifactID, Revision: row.Revision, ExpiresAt: row.ExpiresAt, SessionHash: row.HostedSessionHash}, mapError(ctx, err)
}

func (s *session) CreateArtifactReadGrant(ctx context.Context, grant artifactstore.ReadGrant, now time.Time) error {
	if !artifact.ValidHash(grant.TokenHash, 64) || grant.PrincipalID == "" || grant.ExpiresAt.IsZero() || now.IsZero() {
		return corestore.ErrInvalidCursor
	}
	if _, err := s.db.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "artifact_grants|"+string(s.organization)+"|"+grant.PrincipalID); err != nil {
		return mapError(ctx, err)
	}
	if err := s.queries.ArtifactReadGrantDeleteExpired(ctx, pgquery.ArtifactReadGrantDeleteExpiredParams{OrganizationID: s.organization, PrincipalID: grant.PrincipalID, Now: recordTime(now), BatchLimit: artifactstore.ReadGrantPruneBatch}); err != nil {
		return mapError(ctx, err)
	}
	live, err := s.queries.ArtifactReadGrantLiveCount(ctx, pgquery.ArtifactReadGrantLiveCountParams{OrganizationID: s.organization, PrincipalID: grant.PrincipalID, ExpiresAt: recordTime(now), Limit: artifactstore.ReadGrantLimit})
	if err != nil {
		return mapError(ctx, err)
	}
	if live >= artifactstore.ReadGrantLimit {
		return artifact.ErrQuota
	}
	return mapError(ctx, s.queries.ArtifactReadGrantInsert(ctx, pgquery.ArtifactReadGrantInsertParams{OrganizationID: s.organization, ProjectID: grant.ProjectID, TokenHash: grant.TokenHash, PrincipalID: grant.PrincipalID, ServiceID: grant.ServiceID, ArtifactID: grant.ArtifactID, Revision: grant.Revision, ExpiresAt: recordTime(grant.ExpiresAt), HostedSessionHash: grant.SessionHash}))
}

func (s *session) PruneExpiredArtifactReadGrants(ctx context.Context, now time.Time, limit int) (int64, error) {
	if now.IsZero() || limit < 1 || limit > artifactstore.MaxReadGrantPruneBatch {
		return 0, corestore.ErrInvalidPage
	}
	count, err := s.queries.ArtifactReadGrantPruneExpired(ctx, pgquery.ArtifactReadGrantPruneExpiredParams{OrganizationID: s.organization, Now: recordTime(now), BatchLimit: int32(limit)})
	return count, mapError(ctx, err)
}
