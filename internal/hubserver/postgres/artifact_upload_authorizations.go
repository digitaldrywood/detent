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

func (s *session) AuthorizeArtifactUpload(ctx context.Context, reservation artifact.Reservation, now time.Time) error {
	if err := s.checkOrganization(tracker.OrganizationID(reservation.OrganizationID)); err != nil {
		return err
	}
	if now.IsZero() {
		return corestore.ErrInvalidCursor
	}
	project, now := tracker.ProjectID(reservation.ProjectID), recordTime(now)
	expires := now.Add(artifactstore.UploadAuthorizationLifetime)
	if err := s.queries.ArtifactUploadAuthorizationPruneAttempt(ctx, pgquery.ArtifactUploadAuthorizationPruneAttemptParams{OrganizationID: s.organization, ProjectID: project, AttemptID: reservation.AttemptID, Now: now, BatchLimit: artifactstore.UploadAuthorizationPruneBatch}); err != nil {
		return mapError(ctx, err)
	}
	refreshed, err := s.queries.ArtifactUploadAuthorizationRefresh(ctx, pgquery.ArtifactUploadAuthorizationRefreshParams{ExpiresAt: expires, OrganizationID: s.organization, ProjectID: project, ServiceID: reservation.ServiceID, IdempotencyKey: reservation.Key, WorkItemID: reservation.WorkItemID, RunID: reservation.RunID, AttemptID: reservation.AttemptID, Kind: reservation.Kind, Bytes: reservation.Bytes})
	if err != nil || refreshed == 1 {
		return mapError(ctx, err)
	}
	live, err := s.queries.ArtifactUploadAuthorizationLiveCount(ctx, pgquery.ArtifactUploadAuthorizationLiveCountParams{OrganizationID: s.organization, ProjectID: project, AttemptID: reservation.AttemptID, Now: now, PageLimit: artifactstore.UploadAuthorizationLimit})
	if err != nil {
		return mapError(ctx, err)
	}
	if live >= artifactstore.UploadAuthorizationLimit {
		return artifact.ErrQuota
	}
	inserted, err := s.queries.ArtifactUploadAuthorizationInsert(ctx, pgquery.ArtifactUploadAuthorizationInsertParams{OrganizationID: s.organization, ProjectID: project, ServiceID: reservation.ServiceID, IdempotencyKey: reservation.Key, WorkItemID: reservation.WorkItemID, RunID: reservation.RunID, AttemptID: reservation.AttemptID, Kind: reservation.Kind, Bytes: reservation.Bytes, CreatedAt: now, ExpiresAt: expires})
	if err != nil {
		return mapError(ctx, err)
	}
	if inserted != 1 {
		return corestore.ErrRevisionConflict
	}
	return nil
}

func (s *session) ConsumeArtifactUpload(ctx context.Context, reference artifact.Reference, now time.Time) error {
	if err := s.checkOrganization(tracker.OrganizationID(reference.OrganizationID)); err != nil {
		return err
	}
	project := tracker.ProjectID(reference.ProjectID)
	published, err := s.queries.ArtifactReferencePublished(ctx, pgquery.ArtifactReferencePublishedParams{OrganizationID: s.organization, ProjectID: project, ArtifactID: reference.ArtifactID, ServiceID: reference.ServiceID, WorkItemID: reference.WorkItemID, RunID: reference.RunID, AttemptID: reference.AttemptID, Kind: reference.Kind})
	if err != nil || published {
		return mapError(ctx, err)
	}
	consumed, err := s.queries.ArtifactUploadAuthorizationConsume(ctx, pgquery.ArtifactUploadAuthorizationConsumeParams{OrganizationID: s.organization, ProjectID: project, ServiceID: reference.ServiceID, AttemptID: reference.AttemptID, Kind: reference.Kind, Now: recordTime(now), WorkItemID: reference.WorkItemID, RunID: reference.RunID, Bytes: reference.Bytes})
	if err != nil {
		return mapError(ctx, err)
	}
	if consumed != 1 {
		return corestore.ErrNotFound
	}
	return nil
}

func (s *session) PruneExpiredArtifactUploadAuthorizations(ctx context.Context, limit int) (int64, error) {
	if limit < 1 || limit > artifactstore.MaxUploadAuthorizationPruneBatch {
		return 0, corestore.ErrInvalidPage
	}
	var count int64
	err := s.db.QueryRowContext(ctx, `SELECT prune_expired_artifact_upload_authorizations($1)`, limit).Scan(&count)
	return count, mapError(ctx, err)
}
