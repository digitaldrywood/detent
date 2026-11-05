package postgres

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) AuthorizeArtifactUpload(ctx context.Context, reservation artifact.Reservation, now time.Time) error {
	if err := s.checkOrganization(tracker.OrganizationID(reservation.OrganizationID)); err != nil {
		return err
	}
	project := tracker.ProjectID(reservation.ProjectID)
	if _, err := s.queries.ArtifactUploadAuthorizationInsert(ctx, pgquery.ArtifactUploadAuthorizationInsertParams{OrganizationID: s.organization, ProjectID: project, ServiceID: reservation.ServiceID, IdempotencyKey: reservation.Key, WorkItemID: reservation.WorkItemID, RunID: reservation.RunID, AttemptID: reservation.AttemptID, Kind: reservation.Kind, Bytes: reservation.Bytes, CreatedAt: recordTime(now)}); err != nil {
		return mapError(ctx, err)
	}
	matches, err := s.queries.ArtifactUploadAuthorizationMatches(ctx, pgquery.ArtifactUploadAuthorizationMatchesParams{OrganizationID: s.organization, ProjectID: project, ServiceID: reservation.ServiceID, IdempotencyKey: reservation.Key, WorkItemID: reservation.WorkItemID, RunID: reservation.RunID, AttemptID: reservation.AttemptID, Kind: reservation.Kind, Bytes: reservation.Bytes})
	if err != nil {
		return mapError(ctx, err)
	}
	if !matches {
		return corestore.ErrRevisionConflict
	}
	return nil
}

func (s *session) BindArtifactUpload(ctx context.Context, reference artifact.Reference) error {
	if err := s.checkOrganization(tracker.OrganizationID(reference.OrganizationID)); err != nil {
		return err
	}
	project, id := tracker.ProjectID(reference.ProjectID), reference.ArtifactID
	bound, err := s.queries.ArtifactUploadAuthorizationBound(ctx, pgquery.ArtifactUploadAuthorizationBoundParams{OrganizationID: s.organization, ProjectID: project, ArtifactID: &id, ServiceID: reference.ServiceID, WorkItemID: reference.WorkItemID, RunID: reference.RunID, AttemptID: reference.AttemptID, Kind: reference.Kind, Bytes: reference.Bytes})
	if err != nil {
		return mapError(ctx, err)
	}
	if bound {
		return nil
	}
	rows, err := s.queries.ArtifactUploadAuthorizationBind(ctx, pgquery.ArtifactUploadAuthorizationBindParams{ArtifactID: &id, OrganizationID: s.organization, ProjectID: project, ServiceID: reference.ServiceID, AttemptID: reference.AttemptID, Kind: reference.Kind, WorkItemID: reference.WorkItemID, RunID: reference.RunID, Bytes: reference.Bytes})
	if err != nil {
		return mapError(ctx, err)
	}
	if rows != 1 {
		return corestore.ErrNotFound
	}
	return nil
}
