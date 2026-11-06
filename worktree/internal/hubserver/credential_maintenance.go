package hubserver

import (
	"context"
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func validateCredentialMaintenance(cfg Config) error {
	if !cfg.CredentialMaintenance {
		return nil
	}
	if cfg.Hosted == nil || cfg.Hosted.WorkOSOrganizationID == "" || !listenerAddressLoopback(cfg.ListenAddress) || cfg.TrustedProxy {
		return errors.New("credential maintenance requires hosted configuration with an explicit provider organization and private loopback without a trusted proxy")
	}
	return nil
}

func (s *Service) registerCredentialMaintenanceRoutes(e *echo.Echo) {
	admin := s.requireCredentialMaintenance
	e.POST("/api/v1/tokens", s.createAPIToken, admin)
	e.POST("/api/v1/tokens/:id/rotate", s.rotateAPIToken, admin)
	e.DELETE("/api/v1/tokens/:id", s.revokeAPIToken, admin)
	e.POST("/api/v2/tokens/:id/grants", s.grantNativeToken, admin)
}

func (s *Service) requireCredentialMaintenance(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		readCtx, cancelRead := context.WithTimeout(c.Request().Context(), hubReadTimeout)
		defer cancelRead()
		c.Response().Header().Set("Cache-Control", "no-store")
		if _, err := apiBearerToken(c); err != nil || c.Request().Header.Get("Origin") != "" {
			return c.NoContent(http.StatusUnauthorized)
		}
		credential, status, err := s.authenticateAPIRequest(c)
		if err != nil {
			if status == http.StatusForbidden {
				return s.hostedJSONError(c, status, "Instance administrator credential is not authorized")
			}
			return c.NoContent(status)
		}
		if credential.Scope != apiScopeAdmin || credential.NativeOnly || credential.Hosted != nil || credential.Runner.RunnerID != "" {
			return s.hostedJSONError(c, http.StatusForbidden, "A non-native instance administrator credential is required")
		}
		var member bool
		err = s.accessRead(readCtx, s.accessOrganization(), func(r tenantstore.Reader) error {
			var err error
			member, err = r.AccessCredentialHasMember(readCtx, credential.ID)
			return err
		})
		if err != nil || member {
			return s.hostedJSONError(c, http.StatusForbidden, "Hosted member credentials cannot maintain instance credentials")
		}
		identity := &auth.HostedIdentity{Subject: credential.ID, CreatedAt: s.config.now()}
		route := c.Request().Method + " " + c.Request().URL.Path
		if err := s.hostedAudit(c.Request().Context(), identity, "credential_maintenance_attempt", route, "", 0); err != nil {
			return s.nativeAPIError(c, err)
		}
		c.Set("hub_api_credential", credential)
		if err := next(c); err != nil {
			c.Error(err)
		}
		return s.hostedAudit(c.Request().Context(), identity, "credential_maintenance", route, "", c.Response().Status)
	}
}
