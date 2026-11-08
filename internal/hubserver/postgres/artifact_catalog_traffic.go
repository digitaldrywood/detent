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

func (s *session) RecordCatalogTraffic(ctx context.Context, arg CatalogTrafficReport) error {
	return s.recordCatalogTraffic(ctx, arg, false)
}
func (s *session) RecordCatalogTrafficNow(ctx context.Context, arg CatalogTrafficReport, at time.Time) error {
	arg.Minute = at.UTC().Truncate(time.Minute)
	return s.recordCatalogTraffic(ctx, arg, true)
}
func (s *session) recordCatalogTraffic(ctx context.Context, arg CatalogTrafficReport, authoritativeTime bool) error {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return err
	}
	if arg.ID == "" || len(arg.ID) > 200 || arg.Minute.IsZero() || !arg.Minute.Equal(arg.Minute.UTC().Truncate(time.Minute)) || arg.UploadBytes < 0 || arg.DownloadBytes < 0 || arg.Requests < 0 {
		return artifact.ErrInvalid
	}
	if _, err := s.lockCatalogUsage(ctx); err != nil {
		return err
	}
	existing, err := s.queries.CatalogTrafficReceipt(ctx, pgquery.CatalogTrafficReceiptParams{OrganizationID: s.organization, ProjectID: arg.ProjectID, ServiceID: arg.ServiceID, ID: arg.ID})
	if err == nil {
		if authoritativeTime {
			arg.Minute = existing.Minute
		}
		if !existing.Minute.Equal(arg.Minute) || existing.UploadBytes != arg.UploadBytes || existing.DownloadBytes != arg.DownloadBytes || existing.Requests != arg.Requests {
			return corestore.ErrRevisionConflict
		}
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return mapError(ctx, err)
	}
	if err := s.queries.CreateCatalogTrafficReceipt(ctx, arg); err != nil {
		return mapError(ctx, err)
	}
	if err := s.queries.AddCatalogTraffic(ctx, pgquery.AddCatalogTrafficParams{OrganizationID: s.organization, ProjectID: arg.ProjectID, ServiceID: arg.ServiceID, Minute: arg.Minute, UploadBytes: arg.UploadBytes, DownloadBytes: arg.DownloadBytes, Requests: arg.Requests}); err != nil {
		return mapError(ctx, err)
	}
	for _, service := range []string{arg.ServiceID, ""} {
		if err := s.queries.AddCatalogServiceTraffic(ctx, pgquery.AddCatalogServiceTrafficParams{OrganizationID: s.organization, ServiceID: service, Minute: arg.Minute, UploadBytes: arg.UploadBytes, DownloadBytes: arg.DownloadBytes, Requests: arg.Requests}); err != nil {
			return mapError(ctx, err)
		}
	}
	return nil
}
func (s *session) PutCatalogTrafficSettings(ctx context.Context, arg CatalogTrafficPolicy) (CatalogTrafficSettings, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return CatalogTrafficSettings{}, err
	}
	if arg.WindowSeconds < 1 || arg.RetentionSeconds < arg.WindowSeconds {
		return CatalogTrafficSettings{}, artifact.ErrInvalid
	}
	if _, err := s.lockCatalogUsage(ctx); err != nil {
		return CatalogTrafficSettings{}, err
	}
	row, err := s.queries.PutCatalogTrafficSettings(ctx, arg)
	return row, mapError(ctx, err)
}
func (s *session) PruneCatalogTraffic(ctx context.Context, before time.Time, limit int) error {
	if before.IsZero() {
		return artifact.ErrInvalid
	}
	if err := checkPage(limit); err != nil {
		return err
	}
	if _, err := s.queries.PruneCatalogTraffic(ctx, pgquery.PruneCatalogTrafficParams{OrganizationID: s.organization, Minute: before, Limit: int32(limit)}); err != nil {
		return mapError(ctx, err)
	}
	_, err := s.queries.PruneCatalogTrafficReceipts(ctx, pgquery.PruneCatalogTrafficReceiptsParams{OrganizationID: s.organization, Minute: before, Limit: int32(limit)})
	return mapError(ctx, err)
}
