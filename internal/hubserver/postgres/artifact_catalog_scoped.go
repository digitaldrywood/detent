package postgres

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
)

type CatalogBinding = pgquery.ArtifactService
type CatalogTrafficUsage = pgquery.CatalogServiceTrafficRow
type CatalogServiceUploadsInput = pgquery.CatalogServiceUploadsParams

func (s *session) CatalogServiceUploads(ctx context.Context, arg CatalogServiceUploadsInput) ([]CatalogUpload, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return nil, err
	}
	rows, err := s.queries.CatalogServiceUploads(ctx, arg)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	return rows, nil
}

type CatalogServiceBindingInput = pgquery.CatalogServiceBindingParams

func (s *session) CatalogServiceBinding(ctx context.Context, arg CatalogServiceBindingInput) (CatalogBinding, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return CatalogBinding{}, err
	}
	row, err := s.queries.CatalogServiceBinding(ctx, arg)
	return row, mapError(ctx, err)
}

type CatalogServiceTrafficInput = pgquery.CatalogServiceTrafficParams

func (s *session) CatalogServiceTraffic(ctx context.Context, arg CatalogServiceTrafficInput) (CatalogTrafficUsage, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return CatalogTrafficUsage{}, err
	}
	if !arg.Minute_2.After(arg.Minute) || arg.Minute_2.Sub(arg.Minute) > 24*time.Hour+time.Minute {
		return CatalogTrafficUsage{}, corestore.ErrInvalidPage
	}
	row, err := s.queries.CatalogServiceTraffic(ctx, arg)
	return row, mapError(ctx, err)
}

type ScopedCatalogUploadsInput = pgquery.ScopedCatalogUploadsParams

func (s *session) ScopedCatalogUploads(ctx context.Context, arg ScopedCatalogUploadsInput) ([]CatalogUpload, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return nil, err
	}
	if arg.PageLimit < 1 || arg.PageLimit > 32 {
		return nil, corestore.ErrInvalidPage
	}
	rows, err := s.queries.ScopedCatalogUploads(ctx, arg)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	return rows, nil
}

type ScopedPendingCatalogManifestsInput = pgquery.ScopedPendingCatalogManifestsParams

func (s *session) ScopedPendingCatalogManifests(ctx context.Context, arg ScopedPendingCatalogManifestsInput) ([]CatalogManifest, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return nil, err
	}
	if arg.PageLimit < 1 || arg.PageLimit > 32 {
		return nil, corestore.ErrInvalidPage
	}
	rows, err := s.queries.ScopedPendingCatalogManifests(ctx, arg)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	return rows, nil
}

type ScopedCatalogRetentionInput = pgquery.ScopedCatalogRetentionParams

func (s *session) ScopedCatalogRetention(ctx context.Context, arg ScopedCatalogRetentionInput) ([]CatalogUpload, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return nil, err
	}
	if arg.PageLimit < 1 || arg.PageLimit > 32 {
		return nil, corestore.ErrInvalidPage
	}
	if arg.Category < 0 || arg.Category > 3 {
		return nil, corestore.ErrInvalidPage
	}
	rows, err := s.queries.ScopedCatalogRetention(ctx, arg)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	return rows, nil
}

type ScopedCatalogObjectsInput = pgquery.ScopedCatalogObjectsParams

func (s *session) ScopedCatalogObjects(ctx context.Context, arg ScopedCatalogObjectsInput) ([]CatalogObject, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return nil, err
	}
	if arg.PageLimit < 1 || arg.PageLimit > 32 {
		return nil, corestore.ErrInvalidPage
	}
	rows, err := s.queries.ScopedCatalogObjects(ctx, arg)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	return rows, nil
}

type ScopedCatalogManifestsInput = pgquery.ScopedCatalogManifestsParams

func (s *session) ScopedCatalogManifests(ctx context.Context, arg ScopedCatalogManifestsInput) ([]CatalogManifest, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return nil, err
	}
	if arg.PageLimit < 1 || arg.PageLimit > 32 {
		return nil, corestore.ErrInvalidPage
	}
	rows, err := s.queries.ScopedCatalogManifests(ctx, arg)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	return rows, nil
}

type ScopedCatalogIntentsInput = pgquery.ScopedCatalogIntentsParams

func (s *session) ScopedCatalogIntents(ctx context.Context, arg ScopedCatalogIntentsInput) ([]CatalogManifestIntent, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return nil, err
	}
	if arg.PageLimit < 1 || arg.PageLimit > 32 {
		return nil, corestore.ErrInvalidPage
	}
	rows, err := s.queries.ScopedCatalogIntents(ctx, arg)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	return rows, nil
}

type ScopedCatalogTrafficPrune = pgquery.PruneScopedCatalogTrafficParams

func (s *session) PruneScopedCatalogTraffic(ctx context.Context, arg ScopedCatalogTrafficPrune) error {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return err
	}
	if arg.PageLimit < 1 || arg.PageLimit > 32 || arg.Before.IsZero() {
		return corestore.ErrInvalidPage
	}
	if err := s.queries.PruneScopedCatalogTraffic(ctx, arg); err != nil {
		return mapError(ctx, err)
	}
	if err := s.queries.PruneScopedCatalogTrafficReceipts(ctx, pgquery.PruneScopedCatalogTrafficReceiptsParams(arg)); err != nil {
		return mapError(ctx, err)
	}
	if err := s.queries.PruneCatalogServiceTraffic(ctx, pgquery.PruneCatalogServiceTrafficParams{OrganizationID: arg.OrganizationID, ServiceID: arg.ServiceID, Before: arg.Before, PageLimit: arg.PageLimit}); err != nil {
		return mapError(ctx, err)
	}
	return mapError(ctx, s.queries.PruneCatalogServiceTraffic(ctx, pgquery.PruneCatalogServiceTrafficParams{OrganizationID: arg.OrganizationID, ServiceID: "", Before: arg.Before.Add(-30 * 24 * time.Hour), PageLimit: arg.PageLimit}))
}

type ScopedCatalogPublicationsInput = pgquery.ScopedCatalogPublicationsParams

func (s *session) ScopedCatalogPublications(ctx context.Context, arg ScopedCatalogPublicationsInput) ([]CatalogPublication, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return nil, err
	}
	if arg.PageLimit < 1 || arg.PageLimit > 32 {
		return nil, corestore.ErrInvalidPage
	}
	rows, err := s.queries.ScopedCatalogPublications(ctx, arg)
	return rows, mapError(ctx, err)
}

type ScopedCatalogTrafficBucketsInput = pgquery.ScopedCatalogTrafficBucketsParams

func (s *session) ScopedCatalogTrafficBuckets(ctx context.Context, arg ScopedCatalogTrafficBucketsInput) ([]CatalogTrafficBucket, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return nil, err
	}
	if arg.PageLimit < 1 || arg.PageLimit > 32 {
		return nil, corestore.ErrInvalidPage
	}
	rows, err := s.queries.ScopedCatalogTrafficBuckets(ctx, arg)
	return rows, mapError(ctx, err)
}

type ScopedCatalogTrafficReceiptsInput = pgquery.ScopedCatalogTrafficReceiptsParams

func (s *session) ScopedCatalogTrafficReceipts(ctx context.Context, arg ScopedCatalogTrafficReceiptsInput) ([]CatalogTrafficReceipt, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return nil, err
	}
	if arg.PageLimit < 1 || arg.PageLimit > 32 {
		return nil, corestore.ErrInvalidPage
	}
	rows, err := s.queries.ScopedCatalogTrafficReceipts(ctx, arg)
	return rows, mapError(ctx, err)
}

type ScopedCatalogSettingsInput = pgquery.ScopedCatalogSettingsParams

func (s *session) ScopedCatalogSettings(ctx context.Context, arg ScopedCatalogSettingsInput) ([]CatalogTrafficSettings, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return nil, err
	}
	if arg.PageLimit < 1 || arg.PageLimit > 32 {
		return nil, corestore.ErrInvalidPage
	}
	rows, err := s.queries.ScopedCatalogSettings(ctx, arg)
	return rows, mapError(ctx, err)
}

type CatalogExecutionAuthorityInput = pgquery.CatalogExecutionAuthorityParams

func (s *session) CatalogExecutionAuthority(ctx context.Context, arg CatalogExecutionAuthorityInput) error {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return err
	}
	_, err := s.queries.CatalogExecutionAuthority(ctx, arg)
	return mapError(ctx, err)
}

type ScopedCatalogPoliciesInput = pgquery.ScopedCatalogPoliciesParams

func (s *session) ScopedCatalogPolicies(ctx context.Context, arg ScopedCatalogPoliciesInput) ([]CatalogBinding, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return nil, err
	}
	if arg.PageLimit < 1 || arg.PageLimit > 32 {
		return nil, corestore.ErrInvalidPage
	}
	rows, err := s.queries.ScopedCatalogPolicies(ctx, arg)
	return rows, mapError(ctx, err)
}
