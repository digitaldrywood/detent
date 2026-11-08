package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
)

func (s *session) ReserveCatalogUpload(ctx context.Context, arg CatalogReservation) (CatalogUpload, error) {
	row := arg.Upload
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return CatalogUpload{}, err
	}
	if row.ID == "" || row.IdempotencyKey == "" || len(row.IdempotencyKey) > 128 || !artifact.ValidHash(row.RequestHash, 64) || row.RequestedBytes < 1 || row.RequestedBytes > artifact.MaxArtifactBytes || row.PartLimit < 1 || row.CreatedAt.IsZero() || !row.UploadDeadline.After(row.CreatedAt) || !row.ExpiresAt.After(row.CreatedAt) || arg.RetainedLimit < 0 || arg.ReservedLimit < 0 {
		return CatalogUpload{}, artifact.ErrInvalid
	}
	if _, err := s.queries.ShareActiveProject(ctx, pgquery.ShareActiveProjectParams{OrganizationID: s.organization, ID: row.ProjectID}); err != nil {
		return CatalogUpload{}, mapError(ctx, err)
	}
	usage, err := s.lockCatalogUsage(ctx)
	if err != nil {
		return CatalogUpload{}, err
	}
	existing, err := s.queries.CatalogUploadByKey(ctx, pgquery.CatalogUploadByKeyParams{OrganizationID: s.organization, ProjectID: row.ProjectID, ServiceID: row.ServiceID, IdempotencyKey: row.IdempotencyKey})
	if err == nil {
		if existing.RequestHash != row.RequestHash || existing.WorkItemID != row.WorkItemID || existing.RunID != row.RunID || existing.AttemptID != row.AttemptID || existing.VersionID != row.VersionID || existing.LeaseID != row.LeaseID || existing.FencingToken != row.FencingToken || existing.RequestedBytes != row.RequestedBytes || existing.Kind != row.Kind {
			return CatalogUpload{}, corestore.ErrRevisionConflict
		}
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return CatalogUpload{}, mapError(ctx, err)
	}
	if row.RequestedBytes > arg.ReservedLimit || row.RequestedBytes > arg.RetainedLimit || usage.ReservedBytes > arg.ReservedLimit-row.RequestedBytes || usage.RetainedBytes > arg.RetainedLimit-row.RequestedBytes || usage.ReservedBytes > arg.RetainedLimit-row.RequestedBytes-usage.RetainedBytes {
		return CatalogUpload{}, artifact.ErrQuota
	}
	result, err := s.queries.CreateCatalogUpload(ctx, pgquery.CreateCatalogUploadParams{OrganizationID: row.OrganizationID, ProjectID: row.ProjectID, ServiceID: row.ServiceID, ID: row.ID, IdempotencyKey: row.IdempotencyKey, RequestHash: row.RequestHash, WorkItemID: row.WorkItemID, RunID: row.RunID, AttemptID: row.AttemptID, VersionID: row.VersionID, Kind: row.Kind, LeaseID: row.LeaseID, FencingToken: row.FencingToken, RequestedBytes: row.RequestedBytes, PartLimit: row.PartLimit, CreatedAt: row.CreatedAt, UploadDeadline: row.UploadDeadline, ExpiresAt: row.ExpiresAt})
	if err != nil {
		return CatalogUpload{}, mapError(ctx, err)
	}
	if err := s.adjustCatalogUsage(ctx, row.RequestedBytes, 0); err != nil {
		return CatalogUpload{}, err
	}
	return result, nil
}
func (s *session) PutCatalogObject(ctx context.Context, row CatalogObject, at time.Time) (CatalogObject, error) {
	scope := CatalogScope{OrganizationID: row.OrganizationID, ProjectID: row.ProjectID, ServiceID: row.ServiceID, UploadID: row.UploadID}
	upload, err := s.lockCatalogUpload(ctx, scope)
	if err != nil {
		return CatalogObject{}, err
	}
	if row.ID == "" || !artifact.ValidHash(row.Sha256, 64) || row.Size < 0 || row.Size > artifact.MaxArtifactBytes || row.Sequence < 0 || row.Sequence >= artifact.MaxObjects || row.ByteOffset < 0 || at.IsZero() {
		return CatalogObject{}, artifact.ErrInvalid
	}
	existing, err := s.queries.CatalogObject(ctx, pgquery.CatalogObjectParams{OrganizationID: s.organization, ProjectID: row.ProjectID, ServiceID: row.ServiceID, UploadID: row.UploadID, Sequence: row.Sequence})
	if err == nil {
		if existing.ID != row.ID || existing.Sha256 != row.Sha256 || existing.Size != row.Size || existing.MediaType != row.MediaType || existing.Path != row.Path || existing.Side != row.Side || existing.ByteOffset != row.ByteOffset {
			return CatalogObject{}, corestore.ErrRevisionConflict
		}
		if existing.VerifiedAt == nil && existing.DiscardedAt == nil {
			if err := s.checkCatalogObjectAdmission(ctx, row, upload, 0); err != nil {
				return CatalogObject{}, err
			}
		}
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return CatalogObject{}, mapError(ctx, err)
	}
	if upload.State != "uploading" {
		return CatalogObject{}, corestore.ErrRevisionConflict
	}
	if !at.Before(upload.UploadDeadline) {
		return CatalogObject{}, artifact.ErrExpired
	}
	if err := s.checkCatalogObjectAdmission(ctx, row, upload, row.Size); err != nil {
		return CatalogObject{}, err
	}
	result, err := s.queries.CreateCatalogObject(ctx, pgquery.CreateCatalogObjectParams{OrganizationID: row.OrganizationID, ProjectID: row.ProjectID, ServiceID: row.ServiceID, UploadID: row.UploadID, Sequence: row.Sequence, ID: row.ID, MediaType: row.MediaType, Size: row.Size, Sha256: row.Sha256, Path: row.Path, Side: row.Side, ByteOffset: row.ByteOffset})
	return result, mapError(ctx, err)
}
func (s *session) VerifyCatalogObject(ctx context.Context, row CatalogObject, at time.Time) (CatalogObject, error) {
	scope := CatalogScope{OrganizationID: row.OrganizationID, ProjectID: row.ProjectID, ServiceID: row.ServiceID, UploadID: row.UploadID}
	upload, err := s.lockCatalogUpload(ctx, scope)
	if err != nil {
		return CatalogObject{}, err
	}
	if at.IsZero() || row.StorageVersion == "" {
		return CatalogObject{}, artifact.ErrInvalid
	}
	current, err := s.queries.CatalogObject(ctx, pgquery.CatalogObjectParams{OrganizationID: s.organization, ProjectID: row.ProjectID, ServiceID: row.ServiceID, UploadID: row.UploadID, Sequence: row.Sequence})
	if err != nil {
		return CatalogObject{}, mapError(ctx, err)
	}
	if current.DiscardedAt != nil {
		return CatalogObject{}, corestore.ErrRevisionConflict
	}
	if current.VerifiedAt != nil {
		if current.StorageVersion != row.StorageVersion {
			return CatalogObject{}, corestore.ErrRevisionConflict
		}
		return current, nil
	}
	if upload.State != "uploading" {
		return CatalogObject{}, corestore.ErrRevisionConflict
	}
	if !at.Before(upload.UploadDeadline) {
		return CatalogObject{}, artifact.ErrExpired
	}
	if current.Sha256 != row.Sha256 || current.Size != row.Size {
		return CatalogObject{}, artifact.ErrIntegrity
	}
	if current.Size > upload.ReservedBytes {
		return CatalogObject{}, artifact.ErrQuota
	}
	if _, err := s.queries.AccountCatalogObject(ctx, pgquery.AccountCatalogObjectParams{OrganizationID: s.organization, ProjectID: row.ProjectID, ServiceID: row.ServiceID, ID: row.UploadID, ReservedBytes: current.Size}); err != nil {
		return CatalogObject{}, updated(ctx, err)
	}
	result, err := s.queries.VerifyCatalogObject(ctx, pgquery.VerifyCatalogObjectParams{OrganizationID: s.organization, ProjectID: row.ProjectID, ServiceID: row.ServiceID, UploadID: row.UploadID, Sequence: row.Sequence, StorageVersion: row.StorageVersion, VerifiedAt: new(at)})
	if err != nil {
		return CatalogObject{}, updated(ctx, err)
	}
	if err := s.adjustCatalogUsage(ctx, -current.Size, current.Size); err != nil {
		return CatalogObject{}, err
	}
	return result, nil
}

func (s *session) DiscardCatalogObject(ctx context.Context, row CatalogObject, at time.Time, receipt string) (CatalogObject, error) {
	scope := CatalogScope{OrganizationID: row.OrganizationID, ProjectID: row.ProjectID, ServiceID: row.ServiceID, UploadID: row.UploadID}
	if _, err := s.lockCatalogUpload(ctx, scope); err != nil {
		return CatalogObject{}, err
	}
	if at.IsZero() || receipt == "" {
		return CatalogObject{}, artifact.ErrInvalid
	}
	current, err := s.queries.CatalogObject(ctx, pgquery.CatalogObjectParams{OrganizationID: s.organization, ProjectID: row.ProjectID, ServiceID: row.ServiceID, UploadID: row.UploadID, Sequence: row.Sequence})
	if err != nil {
		return CatalogObject{}, mapError(ctx, err)
	}
	if current.VerifiedAt != nil {
		return CatalogObject{}, corestore.ErrRevisionConflict
	}
	if current.DiscardedAt != nil {
		if current.DiscardReceiptObjectKey == nil || *current.DiscardReceiptObjectKey != receipt {
			return CatalogObject{}, corestore.ErrRevisionConflict
		}
		return current, nil
	}
	result, err := s.queries.DiscardCatalogObject(ctx, pgquery.DiscardCatalogObjectParams{OrganizationID: s.organization, ProjectID: row.ProjectID, ServiceID: row.ServiceID, UploadID: row.UploadID, Sequence: row.Sequence, DiscardedAt: new(at), DiscardReceiptObjectKey: new(receipt)})
	return result, updated(ctx, err)
}

func (s *session) checkCatalogObjectAdmission(ctx context.Context, row CatalogObject, upload CatalogUpload, additional int64) error {
	if row.Size > upload.PartLimit {
		return artifact.ErrQuota
	}
	totals, err := s.queries.CatalogObjectTotals(ctx, pgquery.CatalogObjectTotalsParams{OrganizationID: s.organization, ProjectID: row.ProjectID, ServiceID: row.ServiceID, UploadID: row.UploadID})
	if err != nil {
		return mapError(ctx, err)
	}
	if totals.PendingBytes > upload.ReservedBytes || additional > upload.ReservedBytes-totals.PendingBytes {
		return artifact.ErrQuota
	}
	return nil
}
