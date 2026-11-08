package postgres

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
)

type CatalogIntentTransition struct {
	CatalogScope
	ID               string
	State            string
	StorageVersion   string
	ReceiptObjectKey string
}

func (s *session) CatalogManifestIntent(ctx context.Context, arg CatalogManifestIntentInput) (CatalogManifestIntent, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return CatalogManifestIntent{}, err
	}
	row, err := s.queries.CatalogManifestIntent(ctx, arg)
	return row, mapError(ctx, err)
}

func (s *session) CatalogManifestIntents(ctx context.Context, arg CatalogManifestIntentsInput) ([]CatalogManifestIntent, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return nil, err
	}
	if err := checkPage(int(arg.Limit)); err != nil {
		return nil, err
	}
	rows, err := s.queries.CatalogManifestIntents(ctx, arg)
	return rows, mapError(ctx, err)
}

func (s *session) PrepareCatalogManifestIntent(ctx context.Context, row CatalogManifestIntent, at time.Time) (CatalogManifestIntent, error) {
	scope := CatalogScope{OrganizationID: row.OrganizationID, ProjectID: row.ProjectID, ServiceID: row.ServiceID, UploadID: row.UploadID}
	upload, err := s.lockCatalogUpload(ctx, scope)
	if err != nil {
		return CatalogManifestIntent{}, err
	}
	if !slices.Contains([]string{"partial", "complete", "interrupted"}, row.ManifestState) || row.ID == "" || len(row.ID) > 128 || !artifact.ValidHash(row.Sha256, 64) || row.BodyObjectKey == "" || row.Revision < 1 || row.BodyBytes < 1 || row.BodyBytes > artifact.MaxManifestBytes || at.IsZero() {
		return CatalogManifestIntent{}, artifact.ErrInvalid
	}
	existing, err := s.queries.ActiveCatalogManifestIntent(ctx, pgquery.ActiveCatalogManifestIntentParams{OrganizationID: s.organization, ProjectID: row.ProjectID, ServiceID: row.ServiceID, UploadID: row.UploadID, Revision: row.Revision, Sha256: row.Sha256})
	if err == nil {
		if existing.BodyBytes != row.BodyBytes || existing.ManifestState != row.ManifestState {
			return CatalogManifestIntent{}, corestore.ErrRevisionConflict
		}
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return CatalogManifestIntent{}, mapError(ctx, err)
	}
	existing, err = s.queries.CatalogManifestIntent(ctx, pgquery.CatalogManifestIntentParams{OrganizationID: s.organization, ProjectID: row.ProjectID, ServiceID: row.ServiceID, UploadID: row.UploadID, ID: row.ID})
	if err == nil {
		if existing.Revision != row.Revision || existing.BodyObjectKey != row.BodyObjectKey || existing.Sha256 != row.Sha256 || existing.BodyBytes != row.BodyBytes {
			return CatalogManifestIntent{}, corestore.ErrRevisionConflict
		}
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return CatalogManifestIntent{}, mapError(ctx, err)
	}
	if !at.Before(upload.ExpiresAt) || row.ManifestState != "interrupted" && !at.Before(upload.UploadDeadline) {
		return CatalogManifestIntent{}, artifact.ErrExpired
	}
	if upload.State != "uploading" || row.Revision != upload.ManifestRevision+1 {
		return CatalogManifestIntent{}, corestore.ErrRevisionConflict
	}
	totals, err := s.queries.CatalogObjectTotals(ctx, pgquery.CatalogObjectTotalsParams{OrganizationID: s.organization, ProjectID: row.ProjectID, ServiceID: row.ServiceID, UploadID: row.UploadID})
	if err != nil {
		return CatalogManifestIntent{}, mapError(ctx, err)
	}
	if row.BodyBytes > upload.ReservedBytes-upload.ManifestReservedBytes-totals.PendingBytes {
		return CatalogManifestIntent{}, artifact.ErrQuota
	}
	if _, err := s.queries.AccountCatalogManifestIntent(ctx, pgquery.AccountCatalogManifestIntentParams{OrganizationID: s.organization, ProjectID: row.ProjectID, ServiceID: row.ServiceID, ID: row.UploadID, ManifestReservedBytes: row.BodyBytes}); err != nil {
		return CatalogManifestIntent{}, updated(ctx, err)
	}
	result, err := s.queries.CreateCatalogManifestIntent(ctx, pgquery.CreateCatalogManifestIntentParams{OrganizationID: s.organization, ProjectID: row.ProjectID, ServiceID: row.ServiceID, UploadID: row.UploadID, ID: row.ID, Revision: row.Revision, BodyObjectKey: row.BodyObjectKey, Sha256: row.Sha256, BodyBytes: row.BodyBytes, CreatedAt: at, ManifestState: row.ManifestState})
	return result, mapError(ctx, err)
}

func (s *session) TransitionCatalogManifestIntent(ctx context.Context, arg CatalogIntentTransition) (CatalogManifestIntent, error) {
	if _, err := s.lockCatalogUpload(ctx, arg.CatalogScope); err != nil {
		return CatalogManifestIntent{}, err
	}
	input := pgquery.CatalogManifestIntentParams{OrganizationID: s.organization, ProjectID: arg.ProjectID, ServiceID: arg.ServiceID, UploadID: arg.UploadID, ID: arg.ID}
	current, err := s.queries.CatalogManifestIntent(ctx, input)
	if err != nil {
		return CatalogManifestIntent{}, mapError(ctx, err)
	}
	var result CatalogManifestIntent
	switch arg.State {
	case "verified":
		if arg.StorageVersion == "" {
			return CatalogManifestIntent{}, artifact.ErrInvalid
		}
		if current.StorageVersion == arg.StorageVersion && current.State == "committed" {
			return current, nil
		}
		result, err = s.queries.VerifyCatalogManifestIntent(ctx, pgquery.VerifyCatalogManifestIntentParams{OrganizationID: s.organization, ProjectID: arg.ProjectID, ServiceID: arg.ServiceID, UploadID: arg.UploadID, ID: arg.ID, StorageVersion: arg.StorageVersion})
	case "discarding":
		if current.State == "discarded" {
			return current, nil
		}
		result, err = s.queries.BeginCatalogManifestDiscard(ctx, pgquery.BeginCatalogManifestDiscardParams(input))
	case "discarded":
		if arg.ReceiptObjectKey == "" {
			return CatalogManifestIntent{}, artifact.ErrInvalid
		}
		if current.State == "discarded" {
			if current.ReceiptObjectKey != arg.ReceiptObjectKey {
				return CatalogManifestIntent{}, corestore.ErrRevisionConflict
			}
			return current, nil
		}
		result, err = s.queries.FinishCatalogManifestDiscard(ctx, pgquery.FinishCatalogManifestDiscardParams{OrganizationID: s.organization, ProjectID: arg.ProjectID, ServiceID: arg.ServiceID, UploadID: arg.UploadID, ID: arg.ID, ReceiptObjectKey: arg.ReceiptObjectKey})
		if err != nil {
			return CatalogManifestIntent{}, updated(ctx, err)
		}
		if _, err := s.queries.AccountCatalogManifestIntent(ctx, pgquery.AccountCatalogManifestIntentParams{OrganizationID: s.organization, ProjectID: arg.ProjectID, ServiceID: arg.ServiceID, ID: arg.UploadID, ManifestReservedBytes: -current.BodyBytes}); err != nil {
			return CatalogManifestIntent{}, updated(ctx, err)
		}
	default:
		return CatalogManifestIntent{}, artifact.ErrInvalid
	}
	return result, updated(ctx, err)
}
