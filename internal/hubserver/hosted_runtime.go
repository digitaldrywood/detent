package hubserver

import (
	"log/slog"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
)

func (s *Service) hostedBillingConfig() *hostedplans.HostedBillingConfig {
	if s.config.Hosted == nil {
		return nil
	}
	return s.database.plansService().BillingConfig(s.config.Hosted)
}

func (s *Service) hostedPlansHTTPDeps() hostedplans.HTTPDeps {
	return hostedplans.HTTPDeps{
		Plans:          func() *hostedplans.Service { return s.database.plansService() },
		Hosted:         func() *hostedplans.HostedConfig { return s.config.Hosted },
		Logger:         func() *slog.Logger { return s.config.Logger },
		Now:            func() time.Time { return s.config.now() },
		RuntimeMu:      &s.runtimeMu,
		Error:          func(c echo.Context, err error) error { return s.nativeAPIError(c, err) },
		InvalidRequest: s.invalidAPIRequest,
		JSONError:      s.hostedJSONError,
		Administrator: func(c echo.Context) (string, error) {
			credential, err := s.hostedAdministrator(c)
			return credential.HostedRole, err
		},
		Render:    s.renderHosted,
		PageError: s.hostedError,
	}
}
