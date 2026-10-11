package hubserver

import (
	"context"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

func (s *Service) adminRequestMetrics(ctx context.Context) (requestMetricsReport, bool, error) {
	if s.requestMetrics == nil {
		return requestMetricsReport{}, false, nil
	}
	if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeAdmin, OrganizationID: string(s.requestMetrics.organization), OrganizationWide: true}); err != nil {
		return requestMetricsReport{}, false, nil
	}
	credential, err := currentHubOperator(ctx)
	if err != nil || credential.HostedKeyScope != "" && credential.HostedProjectAccess != hostedProjectsAll {
		return requestMetricsReport{}, false, nil
	}
	report, err := s.requestMetrics.report(ctx, s.config.now())
	return report, true, err
}

func (s *Service) hostedRequestMetrics(c echo.Context) error {
	report, allowed, err := s.adminRequestMetrics(c.Request().Context())
	if err != nil {
		return s.hostedJSONError(c, http.StatusServiceUnavailable, "Request metrics are temporarily unavailable")
	}
	if !allowed {
		return c.NoContent(http.StatusForbidden)
	}
	return c.JSON(http.StatusOK, report)
}
