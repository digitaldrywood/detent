package hubserver

import (
	"context"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

func (s *Service) hostedDiagnostics(c echo.Context) error {
	ctx := c.Request().Context()
	credential, _, err := s.hostedCredential(ctx, c)
	if err != nil {
		return s.usageCredentialError(c, err)
	}
	window, err := usageRangeWindow(c.QueryParam("range"), s.config.now())
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	w := operatortool.AnalyticsWindow{From: window.From, To: window.To, Bucket: time.Hour}
	report, err := s.readCloudDiagnostics(ctx, credential, w)
	if err != nil {
		return s.hostedJSONError(c, http.StatusServiceUnavailable, "Diagnostics are temporarily unavailable")
	}
	return c.JSON(http.StatusOK, report)
}

func (s *Service) readCloudDiagnostics(ctx context.Context, credential hubapi.APICredential, w operatortool.AnalyticsWindow) (diagnosticsReport, error) {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	if s.config.TenantRepository == nil {
		return diagnosticsReport{}, hubapi.NativeWorkItemStorageUnavailable()
	}
	return s.readCloudDiagnosticsTenant(ctx, credential, w)
}

func (s *Service) diagnosticsRequestMetrics(ctx context.Context) (*requestMetricsReport, error) {
	requests, visible, err := s.adminRequestMetrics(ctx)
	if err != nil || !visible {
		return nil, err
	}
	return &requests, nil
}
