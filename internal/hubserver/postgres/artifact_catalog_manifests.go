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

func (s *session) CommitCatalogManifest(ctx context.Context, row CatalogManifest) (CatalogManifest, error) {
	scope := CatalogScope{OrganizationID: row.OrganizationID, ProjectID: row.ProjectID, ServiceID: row.ServiceID, UploadID: row.UploadID}
	upload, err := s.lockCatalogUpload(ctx, scope)
	if err != nil {
		return CatalogManifest{}, err
	}
	if row.ID == "" || row.Revision < 1 || row.RetentionPolicyID == "" || !artifact.ValidHash(row.Sha256, 64) || row.BodyObjectKey == "" || row.BodyBytes < 1 || row.BodyBytes > artifact.MaxManifestBytes || row.ObservedAt.IsZero() || row.State != "partial" && row.State != "complete" && row.State != "interrupted" || !row.CreatedAt.Equal(upload.CreatedAt) || !row.ExpiresAt.Equal(upload.ExpiresAt) {
		return CatalogManifest{}, artifact.ErrInvalid
	}
	existing, err := s.queries.CatalogManifest(ctx, pgquery.CatalogManifestParams{OrganizationID: s.organization, ProjectID: row.ProjectID, ServiceID: row.ServiceID, UploadID: row.UploadID, Revision: row.Revision})
	if err == nil {
		if existing.ID != row.ID || existing.Sha256 != row.Sha256 || existing.BodyObjectKey != row.BodyObjectKey || existing.State != row.State || existing.TotalBytes != row.TotalBytes || existing.Objects != row.Objects || existing.BodyBytes != row.BodyBytes {
			return CatalogManifest{}, corestore.ErrRevisionConflict
		}
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return CatalogManifest{}, mapError(ctx, err)
	}
	if upload.State != "uploading" || row.Revision != upload.ManifestRevision+1 {
		return CatalogManifest{}, corestore.ErrRevisionConflict
	}
	if row.State != "interrupted" && !row.ObservedAt.Before(upload.UploadDeadline) {
		return CatalogManifest{}, artifact.ErrExpired
	}
	if row.BodyBytes > upload.ReservedBytes {
		return CatalogManifest{}, artifact.ErrQuota
	}
	totals, err := s.queries.CatalogObjectTotals(ctx, pgquery.CatalogObjectTotalsParams{OrganizationID: s.organization, ProjectID: row.ProjectID, ServiceID: row.ServiceID, UploadID: row.UploadID})
	if err != nil {
		return CatalogManifest{}, mapError(ctx, err)
	}
	if totals.VerifiedCount != int64(row.Objects) || totals.VerifiedBytes != row.TotalBytes || row.State != "partial" && totals.PendingCount != 0 {
		return CatalogManifest{}, corestore.ErrRevisionConflict
	}
	intent, err := s.queries.CatalogManifestIntentByKey(ctx, pgquery.CatalogManifestIntentByKeyParams{OrganizationID: s.organization, ProjectID: row.ProjectID, ServiceID: row.ServiceID, UploadID: row.UploadID, BodyObjectKey: row.BodyObjectKey})
	if err != nil {
		return CatalogManifest{}, mapError(ctx, err)
	}
	if intent.WriteState != "written" || intent.ManifestState != row.State || intent.State != "pending" || intent.StorageVersion == "" || intent.Revision != row.Revision || intent.Sha256 != row.Sha256 || intent.BodyBytes != row.BodyBytes || row.State != "partial" && upload.ManifestReservedBytes != intent.BodyBytes {
		return CatalogManifest{}, corestore.ErrRevisionConflict
	}
	if _, err := s.queries.AccountCatalogManifestIntent(ctx, pgquery.AccountCatalogManifestIntentParams{OrganizationID: s.organization, ProjectID: row.ProjectID, ServiceID: row.ServiceID, ID: row.UploadID, ManifestReservedBytes: -intent.BodyBytes}); err != nil {
		return CatalogManifest{}, updated(ctx, err)
	}
	if _, err := s.queries.CommitCatalogManifestIntent(ctx, pgquery.CommitCatalogManifestIntentParams{OrganizationID: s.organization, ProjectID: row.ProjectID, ServiceID: row.ServiceID, UploadID: row.UploadID, ID: intent.ID}); err != nil {
		return CatalogManifest{}, updated(ctx, err)
	}
	result, err := s.queries.CreateCatalogManifest(ctx, pgquery.CreateCatalogManifestParams{OrganizationID: row.OrganizationID, ProjectID: row.ProjectID, ServiceID: row.ServiceID, UploadID: row.UploadID, Revision: row.Revision, ID: row.ID, State: row.State, Sha256: row.Sha256, BodyObjectKey: row.BodyObjectKey, BodyBytes: row.BodyBytes, TotalBytes: row.TotalBytes, Objects: row.Objects, RetentionPolicyID: row.RetentionPolicyID, CaptureBase: row.CaptureBase, CaptureHead: row.CaptureHead, CaptureMergeBase: row.CaptureMergeBase, CaptureContextLines: row.CaptureContextLines, CaptureFileContext: row.CaptureFileContext, CaptureWorkingTree: row.CaptureWorkingTree, CreatedAt: row.CreatedAt, ExpiresAt: row.ExpiresAt, ObservedAt: row.ObservedAt})
	if err != nil {
		return CatalogManifest{}, mapError(ctx, err)
	}
	sealed, err := s.queries.SealCatalogUpload(ctx, pgquery.SealCatalogUploadParams{OrganizationID: s.organization, ProjectID: row.ProjectID, ServiceID: row.ServiceID, ID: row.UploadID, Revision: row.Revision, State: row.State, BodyBytes: row.BodyBytes})
	if err != nil {
		return CatalogManifest{}, updated(ctx, err)
	}
	if err := s.adjustCatalogUsage(ctx, upload.ServiceID, upload.BindingMode, sealed.ReservedBytes-upload.ReservedBytes, row.BodyBytes); err != nil {
		return CatalogManifest{}, err
	}
	if err := s.queries.EnqueueCatalogPublication(ctx, pgquery.EnqueueCatalogPublicationParams{OrganizationID: s.organization, ProjectID: row.ProjectID, ServiceID: row.ServiceID, ManifestID: row.ID, UpdatedAt: row.ObservedAt}); err != nil {
		return CatalogManifest{}, mapError(ctx, err)
	}
	return result, nil
}
func (s *session) RecordCatalogPublication(ctx context.Context, arg CatalogPublicationResult) (CatalogPublication, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return CatalogPublication{}, err
	}
	if arg.UpdatedAt.IsZero() || arg.Outcome == "committed" && arg.ExternalReceipt == "" {
		return CatalogPublication{}, artifact.ErrInvalid
	}
	if _, err := s.lockCatalogUsage(ctx); err != nil {
		return CatalogPublication{}, err
	}
	result, err := s.queries.RecordCatalogPublication(ctx, arg)
	if errors.Is(err, pgx.ErrNoRows) {
		result, err = s.queries.CatalogPublication(ctx, pgquery.CatalogPublicationParams{OrganizationID: s.organization, ProjectID: arg.ProjectID, ServiceID: arg.ServiceID, ManifestID: arg.ManifestID})
		if err == nil && (result.Outcome != arg.Outcome || result.ExternalReceipt != arg.ExternalReceipt) {
			return CatalogPublication{}, corestore.ErrRevisionConflict
		}
	}
	return result, mapError(ctx, err)
}
func (s *session) BeginCatalogDeletion(ctx context.Context, scope CatalogScope) (CatalogUpload, error) {
	row, err := s.lockCatalogUpload(ctx, scope)
	if err != nil {
		return CatalogUpload{}, err
	}
	if row.State == "deleted" || row.State == "deletion_pending" {
		return row, nil
	}
	result, err := s.queries.BeginCatalogDeletion(ctx, pgquery.BeginCatalogDeletionParams{OrganizationID: s.organization, ProjectID: scope.ProjectID, ServiceID: scope.ServiceID, ID: scope.UploadID})
	return result, updated(ctx, err)
}
func (s *session) FinishCatalogDeletion(ctx context.Context, arg CatalogDeletion) (CatalogUpload, error) {
	row, err := s.lockCatalogUpload(ctx, arg.CatalogScope)
	if err != nil {
		return CatalogUpload{}, err
	}
	if arg.At.IsZero() || arg.ReceiptObjectKey == "" {
		return CatalogUpload{}, artifact.ErrInvalid
	}
	if row.State == "deleted" {
		if row.DeletionReceiptObjectKey == nil || *row.DeletionReceiptObjectKey != arg.ReceiptObjectKey {
			return CatalogUpload{}, corestore.ErrRevisionConflict
		}
		return row, nil
	}
	if row.State != "deletion_pending" {
		return CatalogUpload{}, corestore.ErrRevisionConflict
	}
	unsettled, err := s.queries.CatalogUnsettledWrites(ctx, pgquery.CatalogUnsettledWritesParams{OrganizationID: s.organization, ProjectID: arg.ProjectID, ServiceID: arg.ServiceID, UploadID: arg.UploadID})
	if err != nil {
		return CatalogUpload{}, mapError(ctx, err)
	}
	totals, err := s.queries.CatalogObjectTotals(ctx, pgquery.CatalogObjectTotalsParams{OrganizationID: s.organization, ProjectID: arg.ProjectID, ServiceID: arg.ServiceID, UploadID: arg.UploadID})
	if err != nil {
		return CatalogUpload{}, mapError(ctx, err)
	}
	if unsettled || totals.PendingCount != 0 {
		return CatalogUpload{}, corestore.ErrRevisionConflict
	}
	result, err := s.queries.FinishCatalogDeletion(ctx, pgquery.FinishCatalogDeletionParams{OrganizationID: s.organization, ProjectID: arg.ProjectID, ServiceID: arg.ServiceID, ID: arg.UploadID, DeletedAt: new(arg.At), DeletionReceiptObjectKey: new(arg.ReceiptObjectKey)})
	if err != nil {
		return CatalogUpload{}, updated(ctx, err)
	}
	if err := s.adjustCatalogUsage(ctx, row.ServiceID, row.BindingMode, -row.ReservedBytes, -row.RetainedBytes); err != nil {
		return CatalogUpload{}, err
	}
	return result, nil
}
func (s *session) PruneCatalogUpload(ctx context.Context, scope CatalogScope, before time.Time) error {
	if before.IsZero() {
		return artifact.ErrInvalid
	}
	if _, err := s.lockCatalogUpload(ctx, scope); err != nil {
		return err
	}
	count, err := s.queries.PruneCatalogUpload(ctx, pgquery.PruneCatalogUploadParams{OrganizationID: s.organization, ProjectID: scope.ProjectID, ServiceID: scope.ServiceID, ID: scope.UploadID, DeletedAt: new(before)})
	if err := affected(ctx, count, err); err != nil {
		return err
	}
	return mapError(ctx, s.queries.PruneEmptyCatalogServiceUsage(ctx, pgquery.PruneEmptyCatalogServiceUsageParams{OrganizationID: s.organization, ServiceID: scope.ServiceID}))
}
