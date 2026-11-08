package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type CatalogManifestIntent = pgquery.ArtifactCatalogManifestIntent
type CatalogManifestIntentInput = pgquery.CatalogManifestIntentParams
type CatalogManifestIntentsInput = pgquery.CatalogManifestIntentsParams

type CatalogUpload = pgquery.ArtifactCatalogUpload
type CatalogObject = pgquery.ArtifactCatalogObject
type CatalogManifest = pgquery.ArtifactCatalogManifest
type CatalogPublication = pgquery.ArtifactCatalogPublication
type CatalogTrafficReceipt = pgquery.ArtifactCatalogTrafficReceipt
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
	ScopedCatalogPolicies(context.Context, ScopedCatalogPoliciesInput) ([]CatalogBinding, error)
	CatalogServiceUsage(context.Context, CatalogServiceUsageInput) (CatalogUsage, error)
	CatalogPolicy(context.Context, CatalogPolicyInput) (CatalogBinding, error)
	ScopedCatalogPublications(context.Context, ScopedCatalogPublicationsInput) ([]CatalogPublication, error)
	ScopedCatalogTrafficBuckets(context.Context, ScopedCatalogTrafficBucketsInput) ([]CatalogTrafficBucket, error)
	ScopedCatalogTrafficReceipts(context.Context, ScopedCatalogTrafficReceiptsInput) ([]CatalogTrafficReceipt, error)
	ScopedCatalogSettings(context.Context, ScopedCatalogSettingsInput) ([]CatalogTrafficSettings, error)

	CatalogServiceUploads(context.Context, CatalogServiceUploadsInput) ([]CatalogUpload, error)
	CatalogServiceBinding(context.Context, CatalogServiceBindingInput) (CatalogBinding, error)
	CatalogServiceTraffic(context.Context, CatalogServiceTrafficInput) (CatalogTrafficUsage, error)
	ScopedCatalogUploads(context.Context, ScopedCatalogUploadsInput) ([]CatalogUpload, error)
	ScopedPendingCatalogManifests(context.Context, ScopedPendingCatalogManifestsInput) ([]CatalogManifest, error)
	ScopedCatalogRetention(context.Context, ScopedCatalogRetentionInput) ([]CatalogUpload, error)
	ScopedCatalogObjects(context.Context, ScopedCatalogObjectsInput) ([]CatalogObject, error)
	ScopedCatalogManifests(context.Context, ScopedCatalogManifestsInput) ([]CatalogManifest, error)
	ScopedCatalogIntents(context.Context, ScopedCatalogIntentsInput) ([]CatalogManifestIntent, error)

	CatalogManifestIntent(context.Context, CatalogManifestIntentInput) (CatalogManifestIntent, error)
	CatalogManifestIntents(context.Context, CatalogManifestIntentsInput) ([]CatalogManifestIntent, error)
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
	SetCatalogPolicy(context.Context, tracker.ProjectID, string, artifact.Policy) (CatalogBinding, error)
	CatalogExecutionAuthority(context.Context, CatalogExecutionAuthorityInput) error
	PruneScopedCatalogTraffic(context.Context, ScopedCatalogTrafficPrune) error
	RecordCatalogTrafficNow(context.Context, CatalogTrafficReport, time.Time) error
	PrepareCatalogManifestIntent(context.Context, CatalogManifestIntent, time.Time) (CatalogManifestIntent, error)
	TransitionCatalogManifestIntent(context.Context, CatalogIntentTransition) (CatalogManifestIntent, error)
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
	Upload               CatalogUpload
	RetainedLimit        int64
	ReservedLimit        int64
	ServiceRetainedLimit int64
	ServiceReservedLimit int64
	EnforceRelay         bool
	RelayLimit           int64
	RelayWindowStart     time.Time
	At                   time.Time
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
		active, err := s.queries.CatalogOrganizationUsageActive(ctx, s.organization)
		if err != nil {
			return CatalogUsage{}, mapError(ctx, err)
		}
		if active {
			return CatalogUsage{}, artifact.ErrIntegrity
		}
		return CatalogUsage{OrganizationID: s.organization}, nil
	}
	return row, mapError(ctx, err)
}
func (s *session) lockCatalogUsage(ctx context.Context) (CatalogUsage, error) {
	if _, err := s.CatalogUsage(ctx); err != nil {
		return CatalogUsage{}, err
	}
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
func (s *session) adjustCatalogUsage(ctx context.Context, service, mode string, reserved, retained int64) error {
	if err := s.queries.AdjustCatalogUsage(ctx, pgquery.AdjustCatalogUsageParams{OrganizationID: s.organization, ReservedBytes: reserved, RetainedBytes: retained}); err != nil {
		return mapError(ctx, err)
	}
	services := []string{service}
	if mode == "hosted" {
		services = append(services, "")
	}
	for _, id := range services {
		count, err := s.queries.AdjustCatalogServiceUsage(ctx, pgquery.AdjustCatalogServiceUsageParams{OrganizationID: s.organization, ServiceID: id, ReservedBytes: reserved, RetainedBytes: retained})
		if err := affected(ctx, count, err); err != nil {
			return err
		}
	}
	return nil
}
