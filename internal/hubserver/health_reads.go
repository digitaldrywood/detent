package hubserver

import (
	"context"
	"net/http"
	"strconv"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/healthsignals"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type healthFindingsPage = healthsignals.FindingsPage

func (s *Service) getHealthFindings(c echo.Context) error {
	limit := 100
	if raw := c.QueryParam("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 100 {
			return s.nativeAPIError(c, hubapi.NativePublicInvalid("limit must be between 1 and 100"))
		}
		limit = value
	}
	page, err := s.readHealthFindings(c.Request().Context(), hubscope.NativeRequestScope(c), c.QueryParam("state"), c.QueryParam("since"), c.QueryParam("cursor"), limit)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, page)
}

func (s *Service) readHealthFindings(ctx context.Context, scope hubscope.NativeScope, state, since, cursor string, limit int) (healthFindingsPage, error) {
	deps := healthsignals.FindingsDeps{ReadEvidence: s.readHealthEvidence}
	if s.config.TenantRepository != nil {
		deps.ReadSnapshot = s.readHealthSnapshot
	}
	return healthsignals.ReadFindings(ctx, deps, scope, state, since, cursor, limit)
}

func (s *Service) readHealthSnapshot(ctx context.Context, organization tracker.OrganizationID, read func(tenantstore.Reader) error) error {
	repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return hubapi.NativeWorkItemStorageUnavailable()
	}
	return repository.ReadTenantSnapshot(ctx, organization, corestore.Serving, read)
}

func (s *Service) readHealthEvidence(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	return s.runDetailStore().ReadDetail(ctx, organization, ref)
}
