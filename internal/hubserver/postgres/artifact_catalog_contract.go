package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type CatalogUpload = pgquery.ArtifactCatalogUpload
type CatalogObject = pgquery.ArtifactCatalogObject
type CatalogManifest = pgquery.ArtifactCatalogManifest
type CatalogPublication = pgquery.ArtifactCatalogPublication
type CatalogTrafficBucket = pgquery.ArtifactCatalogTraffic
type CatalogTrafficSettings = pgquery.ArtifactCatalogTrafficSetting
type CatalogUsage = pgquery.ArtifactCatalogUsage
type CatalogUploadInput = pgquery.CatalogUploadParams
type CatalogUploadByKeyInput = pgquery.CatalogUploadByKeyParams
type CatalogObjectInput = pgquery.CatalogObjectParams
type CatalogManifestInput = pgquery.CatalogManifestParams
type CatalogPublicationInput = pgquery.CatalogPublicationParams
type CatalogObjectsInput = pgquery.CatalogObjectsParams
type PendingCatalogPublicationsInput = pgquery.PendingCatalogPublicationsParams
type CatalogRetentionCandidatesInput = pgquery.CatalogRetentionCandidatesParams
type CatalogTrafficSettingsInput = pgquery.CatalogTrafficSettingsParams
type CatalogTrafficInput = pgquery.CatalogTrafficParams
type ArtifactCatalogReader interface {
	CatalogUsage(context.Context) (CatalogUsage, error)
	CatalogUpload(context.Context, CatalogUploadInput) (CatalogUpload, error)
	CatalogUploadByKey(context.Context, CatalogUploadByKeyInput) (CatalogUpload, error)
	CatalogObject(context.Context, CatalogObjectInput) (CatalogObject, error)
	CatalogManifest(context.Context, CatalogManifestInput) (CatalogManifest, error)
	CatalogPublication(context.Context, CatalogPublicationInput) (CatalogPublication, error)
	CatalogObjects(context.Context, CatalogObjectsInput) ([]CatalogObject, error)
	PendingCatalogPublications(context.Context, PendingCatalogPublicationsInput) ([]CatalogPublication, error)
	CatalogRetentionCandidates(context.Context, CatalogRetentionCandidatesInput) ([]CatalogUpload, error)
	CatalogTrafficSettings(context.Context, CatalogTrafficSettingsInput) (CatalogTrafficSettings, error)
	CatalogTraffic(context.Context, CatalogTrafficInput) ([]CatalogTrafficBucket, error)
}
type ArtifactCatalogWriter interface {
	ArtifactCatalogReader
	ReserveCatalogUpload(context.Context, CatalogReservation) (CatalogUpload, error)
	PutCatalogObject(context.Context, CatalogObject, time.Time) (CatalogObject, error)
	VerifyCatalogObject(context.Context, CatalogObject, time.Time) (CatalogObject, error)
 DiscardCatalogObject(context.Context, CatalogObject, time.Time, string) (CatalogObject, error)
	CommitCatalogManifest(context.Context, CatalogManifest) (CatalogManifest, error)
	RecordCatalogPublication(context.Context, CatalogPublicationResult) (CatalogPublication, error)
	BeginCatalogDeletion(context.Context, CatalogScope) (CatalogUpload, error)
	FinishCatalogDeletion(context.Context, CatalogDeletion) (CatalogUpload, error)
	PruneCatalogUpload(context.Context, CatalogScope, time.Time) error
	RecordCatalogTraffic(context.Context, CatalogTrafficReport) error
	PutCatalogTrafficSettings(context.Context, CatalogTrafficPolicy) (CatalogTrafficSettings, error)
	PruneCatalogTraffic(context.Context, time.Time, int) error
}

func (r *Repository) ReadArtifactCatalog(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, run func(ArtifactCatalogReader) error) error {
	return r.transaction(ctx, org, workload, true, func(s *session) error { return run(s) })
}
func (r *Repository) WriteArtifactCatalog(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, run func(ArtifactCatalogWriter) error) error {
	return r.transaction(ctx, org, workload, false, func(s *session) error { return run(s) })
}
func (s *session) CatalogUpload(ctx context.Context, arg CatalogUploadInput) (CatalogUpload, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return CatalogUpload{}, err
	}

	row, err := s.queries.CatalogUpload(ctx, arg)

	return row, mapError(ctx, err)
}
func (s *session) CatalogUploadByKey(ctx context.Context, arg CatalogUploadByKeyInput) (CatalogUpload, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return CatalogUpload{}, err
	}

	row, err := s.queries.CatalogUploadByKey(ctx, arg)

	return row, mapError(ctx, err)
}
func (s *session) CatalogObject(ctx context.Context, arg CatalogObjectInput) (CatalogObject, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return CatalogObject{}, err
	}

	row, err := s.queries.CatalogObject(ctx, arg)

	return row, mapError(ctx, err)
}
func (s *session) CatalogManifest(ctx context.Context, arg CatalogManifestInput) (CatalogManifest, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return CatalogManifest{}, err
	}

	row, err := s.queries.CatalogManifest(ctx, arg)

	return row, mapError(ctx, err)
}
func (s *session) CatalogPublication(ctx context.Context, arg CatalogPublicationInput) (CatalogPublication, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return CatalogPublication{}, err
	}

	row, err := s.queries.CatalogPublication(ctx, arg)

	return row, mapError(ctx, err)
}
func (s *session) CatalogObjects(ctx context.Context, arg CatalogObjectsInput) ([]CatalogObject, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return nil, err
	}
	if arg.Limit < 1 || arg.Limit > 1024 {
		return nil, corestore.ErrInvalidPage
	}
	row, err := s.queries.CatalogObjects(ctx, arg)

	return row, mapError(ctx, err)
}
func (s *session) PendingCatalogPublications(ctx context.Context, arg PendingCatalogPublicationsInput) ([]CatalogPublication, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return nil, err
	}
	if err := checkPage(int(arg.Limit)); err != nil {
		return nil, err
	}
	row, err := s.queries.PendingCatalogPublications(ctx, arg)

	return row, mapError(ctx, err)
}
func (s *session) CatalogRetentionCandidates(ctx context.Context, arg CatalogRetentionCandidatesInput) ([]CatalogUpload, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return nil, err
	}
	if err := checkPage(int(arg.PageLimit)); err != nil {
		return nil, err
	}
	rows, err := s.queries.CatalogRetentionCandidates(ctx, arg)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	result := make([]CatalogUpload, len(rows))
	for i := range rows {
		result[i] = CatalogUpload(rows[i])
	}
	return result, nil
}
func (s *session) CatalogTrafficSettings(ctx context.Context, arg CatalogTrafficSettingsInput) (CatalogTrafficSettings, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return CatalogTrafficSettings{}, err
	}

	row, err := s.queries.CatalogTrafficSettings(ctx, arg)

	return row, mapError(ctx, err)
}
func (s *session) CatalogTraffic(ctx context.Context, arg CatalogTrafficInput) ([]CatalogTrafficBucket, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return nil, err
	}
	if err := checkPage(int(arg.Limit)); err != nil {
		return nil, err
	}
	if !arg.Minute_2.After(arg.Minute) {
		return nil, corestore.ErrInvalidPage
	}
	row, err := s.queries.CatalogTraffic(ctx, arg)

	return row, mapError(ctx, err)
}

type CatalogScope struct {
	OrganizationID tracker.OrganizationID
	ProjectID      tracker.ProjectID
	ServiceID      string
	UploadID       string
}
type CatalogReservation struct {
	Upload        CatalogUpload
	RetainedLimit int64
	ReservedLimit int64
}
type CatalogDeletion struct {
	CatalogScope
	At               time.Time
	ReceiptObjectKey string
}
type CatalogPublicationResult = pgquery.RecordCatalogPublicationParams
type CatalogTrafficReport = pgquery.CreateCatalogTrafficReceiptParams
type CatalogTrafficPolicy = pgquery.PutCatalogTrafficSettingsParams

func (s *session) CatalogUsage(ctx context.Context) (CatalogUsage, error) {
	row, err := s.queries.CatalogUsage(ctx, s.organization)
	if errors.Is(err, pgx.ErrNoRows) {
		return CatalogUsage{OrganizationID: s.organization}, nil
	}
	return row, mapError(ctx, err)
}
func (s *session) lockCatalogUsage(ctx context.Context) (CatalogUsage, error) {
	if err := s.queries.EnsureCatalogUsage(ctx, s.organization); err != nil {
		return CatalogUsage{}, mapError(ctx, err)
	}
	row, err := s.queries.LockCatalogUsage(ctx, s.organization)
	return row, mapError(ctx, err)
}
func (s *session) lockCatalogUpload(ctx context.Context, scope CatalogScope) (CatalogUpload, error) {
	if err := s.checkOrganization(scope.OrganizationID); err != nil {
		return CatalogUpload{}, err
	}
	if _, err := s.lockCatalogUsage(ctx); err != nil {
		return CatalogUpload{}, err
	}
	row, err := s.queries.LockCatalogUpload(ctx, pgquery.LockCatalogUploadParams{OrganizationID: s.organization, ProjectID: scope.ProjectID, ServiceID: scope.ServiceID, ID: scope.UploadID})
	return row, mapError(ctx, err)
}
func (s *session) adjustCatalogUsage(ctx context.Context, reserved, retained int64) error {
	return mapError(ctx, s.queries.AdjustCatalogUsage(ctx, pgquery.AdjustCatalogUsageParams{OrganizationID: s.organization, ReservedBytes: reserved, RetainedBytes: retained}))
}
