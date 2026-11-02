package postgres

import (
	"context"
	"database/sql"
	"errors"
	"slices"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) ArtifactReference(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, artifactID string, revision int64) (artifact.Reference, error) {
	if project == "" || item == "" || artifactID == "" || revision < 1 {
		return artifact.Reference{}, corestore.ErrNotFound
	}
	row, err := s.queries.ArtifactReferenceGet(ctx, pgquery.ArtifactReferenceGetParams{OrganizationID: s.organization, ProjectID: project, WorkItemID: string(item), ArtifactID: artifactID, Revision: revision})
	if err != nil {
		return artifact.Reference{}, mapError(ctx, err)
	}
	return artifactReferenceRecord(row), nil
}

func (s *session) ArtifactReferences(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, limit int) ([]artifact.Reference, error) {
	if project == "" || item == "" || limit < 1 || limit > 64 {
		return nil, corestore.ErrInvalidPage
	}
	rows, err := s.queries.ArtifactReferenceListLatest(ctx, pgquery.ArtifactReferenceListLatestParams{OrganizationID: s.organization, ProjectID: project, WorkItemID: string(item), PageLimit: int32(limit)})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	result := make([]artifact.Reference, 0, len(rows))
	for _, row := range rows {
		result = append(result, artifactReferenceRecord(row))
	}
	return result, nil
}

func (s *session) AppendArtifactReference(ctx context.Context, reference artifact.Reference) (artifact.Reference, error) {
	if err := s.checkOrganization(tracker.OrganizationID(reference.OrganizationID)); err != nil {
		return artifact.Reference{}, err
	}
	reference.ExpiresAt = recordTime(reference.ExpiresAt)
	reference.ObservedAt = recordTime(reference.ObservedAt)
	if !validArtifactReference(reference) {
		return artifact.Reference{}, corestore.ErrInvalidCursor
	}
	row, err := s.queries.ArtifactReferenceInsert(ctx, pgquery.ArtifactReferenceInsertParams{
		OrganizationID: tracker.OrganizationID(reference.OrganizationID), ProjectID: tracker.ProjectID(reference.ProjectID), WorkItemID: reference.WorkItemID,
		ServiceID: reference.ServiceID, ArtifactID: reference.ArtifactID, Revision: reference.Revision, ManifestID: reference.ManifestID,
		SchemaVersion: int16(reference.SchemaVersion), RunID: reference.RunID, AttemptID: reference.AttemptID, VersionID: reference.VersionID,
		Sha256: reference.SHA256, Kind: reference.Kind, State: reference.State, Availability: reference.Availability,
		Bytes: reference.Bytes, Objects: int32(reference.Objects), ExpiresAt: recordTime(reference.ExpiresAt), ObservedAt: recordTime(reference.ObservedAt),
	})
	if errors.Is(err, sql.ErrNoRows) {
		stored, readErr := s.queries.ArtifactReferenceGet(ctx, pgquery.ArtifactReferenceGetParams{OrganizationID: s.organization, ProjectID: tracker.ProjectID(reference.ProjectID), WorkItemID: reference.WorkItemID, ArtifactID: reference.ArtifactID, Revision: reference.Revision})
		if readErr == nil {
			current := artifactReferenceRecord(stored)
			if !sameArtifactReference(current, reference) {
				return artifact.Reference{}, corestore.ErrRevisionConflict
			}
			return current, nil
		}
		return artifact.Reference{}, mapError(ctx, readErr)
	}
	if err != nil {
		return artifact.Reference{}, mapError(ctx, err)
	}
	return artifactReferenceRecord(row), nil
}

func sameArtifactReference(left, right artifact.Reference) bool {
	left.ExpiresAt = left.ExpiresAt.UTC()
	left.ObservedAt = left.ObservedAt.UTC()
	right.ExpiresAt = right.ExpiresAt.UTC()
	right.ObservedAt = right.ObservedAt.UTC()
	return left == right
}

func validArtifactReference(reference artifact.Reference) bool {
	return reference.SchemaVersion == 1 && reference.Validate() == nil && artifact.ValidID(reference.ServiceID, "service") && artifact.ValidID(reference.ArtifactID, "artifact") && artifact.ValidID(reference.ManifestID, "manifest") && reference.Revision >= 1 && artifact.ValidHash(reference.SHA256, 64) && slices.Contains([]string{"log", "diff", "screenshot", "video"}, reference.Kind) && slices.Contains([]string{"partial", "complete", "interrupted"}, reference.State) && reference.Availability == "available" && reference.Bytes >= 0 && reference.Bytes <= artifact.MaxArtifactBytes && reference.Objects >= 0 && reference.Objects <= artifact.MaxObjects && !reference.ExpiresAt.IsZero() && !reference.ObservedAt.IsZero() && reference.ExpiresAt.After(reference.ObservedAt)
}

func artifactReferenceRecord(row pgquery.ArtifactReference) artifact.Reference {
	return artifact.Reference{
		SchemaVersion: int(row.SchemaVersion),
		Scope:         artifact.Scope{OrganizationID: string(row.OrganizationID), ProjectID: string(row.ProjectID), WorkItemID: row.WorkItemID, RunID: row.RunID, AttemptID: row.AttemptID, VersionID: row.VersionID},
		ServiceID:     row.ServiceID, ArtifactID: row.ArtifactID, ManifestID: row.ManifestID, Revision: row.Revision,
		SHA256: row.Sha256, Kind: row.Kind, State: row.State, Availability: row.Availability,
		Bytes: row.Bytes, Objects: int(row.Objects), ExpiresAt: row.ExpiresAt, ObservedAt: row.ObservedAt,
	}
}
