package postgres

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
)

type CatalogWrite struct {
	CatalogScope
	ID             string
	Sequence       int32
	WriteID        string
	Outcome        string
	StorageVersion string
	At             time.Time
}

func (s *session) BeginCatalogObjectWrite(ctx context.Context, arg CatalogWrite) (CatalogObject, error) {
	upload, err := s.lockCatalogUpload(ctx, arg.CatalogScope)
	if err != nil {
		return CatalogObject{}, err
	}
	row, err := s.queries.CatalogObject(ctx, pgquery.CatalogObjectParams{OrganizationID: s.organization, ProjectID: arg.ProjectID, ServiceID: arg.ServiceID, UploadID: arg.UploadID, Sequence: arg.Sequence})
	if err != nil {
		return CatalogObject{}, mapError(ctx, err)
	}
	if row.ID != arg.ID || row.WriteState != "ready" || row.VerifiedAt != nil || row.DiscardedAt != nil || upload.State != "uploading" {
		return CatalogObject{}, corestore.ErrRevisionConflict
	}
	if arg.At.IsZero() || !artifact.ValidID(arg.WriteID, "write") {
		return CatalogObject{}, artifact.ErrInvalid
	}
	if !arg.At.Before(upload.UploadDeadline) || !arg.At.Before(upload.ExpiresAt) {
		return CatalogObject{}, artifact.ErrExpired
	}
	row, err = s.queries.SetCatalogObjectWrite(ctx, pgquery.SetCatalogObjectWriteParams{OrganizationID: s.organization, ProjectID: arg.ProjectID, ServiceID: arg.ServiceID, UploadID: arg.UploadID, Sequence: arg.Sequence, WriteState: "writing", WriteID: arg.WriteID})
	return row, mapError(ctx, err)
}

func (s *session) FinishCatalogObjectWrite(ctx context.Context, arg CatalogWrite) (CatalogObject, error) {
	if _, err := s.lockCatalogUpload(ctx, arg.CatalogScope); err != nil {
		return CatalogObject{}, err
	}
	row, err := s.queries.CatalogObject(ctx, pgquery.CatalogObjectParams{OrganizationID: s.organization, ProjectID: arg.ProjectID, ServiceID: arg.ServiceID, UploadID: arg.UploadID, Sequence: arg.Sequence})
	if err != nil {
		return CatalogObject{}, mapError(ctx, err)
	}
	if row.ID != arg.ID {
		return CatalogObject{}, corestore.ErrRevisionConflict
	}
	state, id, version, err := catalogWriteOutcome(row.WriteState, row.WriteID, row.StorageVersion, arg)
	if err != nil {
		return CatalogObject{}, err
	}
	row, err = s.queries.SetCatalogObjectWrite(ctx, pgquery.SetCatalogObjectWriteParams{OrganizationID: s.organization, ProjectID: arg.ProjectID, ServiceID: arg.ServiceID, UploadID: arg.UploadID, Sequence: arg.Sequence, WriteState: state, WriteID: id, StorageVersion: version})
	return row, mapError(ctx, err)
}

func (s *session) BeginCatalogManifestWrite(ctx context.Context, arg CatalogWrite) (CatalogManifestIntent, error) {
	upload, err := s.lockCatalogUpload(ctx, arg.CatalogScope)
	if err != nil {
		return CatalogManifestIntent{}, err
	}
	row, err := s.queries.CatalogManifestIntent(ctx, pgquery.CatalogManifestIntentParams{OrganizationID: s.organization, ProjectID: arg.ProjectID, ServiceID: arg.ServiceID, UploadID: arg.UploadID, ID: arg.ID})
	if err != nil {
		return CatalogManifestIntent{}, mapError(ctx, err)
	}
	if row.WriteState != "ready" || row.State != "pending" || upload.State != "uploading" {
		return CatalogManifestIntent{}, corestore.ErrRevisionConflict
	}
	if arg.At.IsZero() || !artifact.ValidID(arg.WriteID, "write") {
		return CatalogManifestIntent{}, artifact.ErrInvalid
	}
	if !arg.At.Before(upload.ExpiresAt) || row.ManifestState != "interrupted" && !arg.At.Before(upload.UploadDeadline) {
		return CatalogManifestIntent{}, artifact.ErrExpired
	}
	row, err = s.queries.SetCatalogManifestWrite(ctx, pgquery.SetCatalogManifestWriteParams{OrganizationID: s.organization, ProjectID: arg.ProjectID, ServiceID: arg.ServiceID, UploadID: arg.UploadID, ID: arg.ID, WriteState: "writing", WriteID: arg.WriteID})
	return row, mapError(ctx, err)
}

func (s *session) FinishCatalogManifestWrite(ctx context.Context, arg CatalogWrite) (CatalogManifestIntent, error) {
	if _, err := s.lockCatalogUpload(ctx, arg.CatalogScope); err != nil {
		return CatalogManifestIntent{}, err
	}
	row, err := s.queries.CatalogManifestIntent(ctx, pgquery.CatalogManifestIntentParams{OrganizationID: s.organization, ProjectID: arg.ProjectID, ServiceID: arg.ServiceID, UploadID: arg.UploadID, ID: arg.ID})
	if err != nil {
		return CatalogManifestIntent{}, mapError(ctx, err)
	}
	state, id, version, err := catalogWriteOutcome(row.WriteState, row.WriteID, row.StorageVersion, arg)
	if err != nil {
		return CatalogManifestIntent{}, err
	}
	row, err = s.queries.SetCatalogManifestWrite(ctx, pgquery.SetCatalogManifestWriteParams{OrganizationID: s.organization, ProjectID: arg.ProjectID, ServiceID: arg.ServiceID, UploadID: arg.UploadID, ID: arg.ID, WriteState: state, WriteID: id, StorageVersion: version})
	return row, mapError(ctx, err)
}

func catalogWriteOutcome(state, id, version string, arg CatalogWrite) (string, string, string, error) {
	if !artifact.ValidID(arg.WriteID, "write") {
		return "", "", "", artifact.ErrInvalid
	}
	if state == "written" {
		if arg.Outcome == "rejected" || arg.Outcome == "written" && arg.StorageVersion != version {
			return "", "", "", corestore.ErrRevisionConflict
		}
		return state, id, version, nil
	}
	switch arg.Outcome {
	case "rejected":
		if state != "ready" && (state != "writing" && state != "rejected" || id != arg.WriteID) {
			return "", "", "", corestore.ErrRevisionConflict
		}
		return "rejected", arg.WriteID, "", nil
	case "uncertain", "written":
		if id != arg.WriteID || state != "writing" && state != "uncertain" {
			return "", "", "", corestore.ErrRevisionConflict
		}
		if arg.Outcome == "written" && arg.StorageVersion == "" {
			return "", "", "", artifact.ErrInvalid
		}
		return arg.Outcome, id, arg.StorageVersion, nil
	default:
		return "", "", "", artifact.ErrInvalid
	}
}
