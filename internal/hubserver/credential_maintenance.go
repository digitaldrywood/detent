package hubserver

import (
	"context"
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudorigin"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tenantauth"
	"github.com/digitaldrywood/detent/internal/tracker"
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
		if credential.Scope != apiScopeAdmin || credential.NativeOnly || credential.Hosted != nil || credential.Runner.RunnerID != "" || credential.HostedKeyKind != "service" || credential.HostedProjectAccess != hostedProjectsAll {
			return s.hostedJSONError(c, http.StatusForbidden, "A non-native instance administrator credential is required")
		}
		var member bool
		err = s.accessRead(readCtx, s.accessOrganization(), func(r tenantstore.Reader) error {
			var err error
			member, err = r.AccessCredentialHasMember(readCtx, credential.ID)
			return err
		})
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		if member {
			return s.hostedJSONError(c, http.StatusForbidden, "Hosted member credentials cannot maintain instance credentials")
		}
		identity := &auth.HostedIdentity{Subject: credential.ID, CreatedAt: s.config.now()}
		route := c.Request().Method + " " + c.Request().URL.Path
		if err := s.hostedAudit(c.Request().Context(), identity, "credential_maintenance_attempt", route, "", 0); err != nil {
			return s.nativeAPIError(c, err)
		}
		c.Set("hub_api_credential", credential)
		ctx := context.WithValue(c.Request().Context(), credentialMaintenanceAuthorityKey{}, accessstore.Authority{CredentialID: credential.ID, Hash: credential.Hash, Admin: true, Write: true})
		c.SetRequest(c.Request().WithContext(ctx))
		if err := next(c); err != nil {
			c.Error(err)
		}
		return s.hostedAudit(c.Request().Context(), identity, "credential_maintenance", route, "", c.Response().Status)
	}
}

type credentialMaintenanceAuthorityKey struct{}

func (s *Service) accessMaintenanceBinding(tenant accessstore.Tenant) error {
	cfg := s.config.Hosted
	if cfg == nil || tenant.OrganizationID != s.accessOrganization() || tenant.ProviderID != cfg.WorkOSOrganizationID || tenant.BootstrapSubject != cfg.BootstrapSubject {
		return ErrHostedDatabaseBinding
	}
	deployment, generation := cfg.deployment()
	if tenant.Deployment != deployment || tenant.AllocationGeneration != generation {
		return ErrHostedDatabaseBinding
	}
	if tenant.PublicURL != cfg.PublicURL && (tenant.Deployment != "shared" || cfg.SharedEntry == nil || !cloudorigin.Aliases(tenant.PublicURL, cfg.PublicURL)) {
		return ErrHostedDatabaseBinding
	}
	return nil
}

func (s *Service) validateAccessMaintenanceBinding(ctx context.Context) error {
	return s.accessRead(ctx, s.accessOrganization(), func(r tenantstore.Reader) error {
		tenant, err := r.AccessTenant(ctx)
		if errors.Is(err, tenantauth.ErrUnavailable) {
			return err
		}
		if errors.Is(err, corestore.ErrNotFound) {
			return ErrHostedDatabaseBinding
		}
		if err != nil {
			return err
		}
		return s.accessMaintenanceBinding(tenant)
	})
}

func (s *Service) authorizeCredentialMaintenanceWrite(ctx context.Context, w tenantstore.Writer, organization tracker.OrganizationID) error {
	if !s.config.CredentialMaintenance {
		return nil
	}
	authority, ok := ctx.Value(credentialMaintenanceAuthorityKey{}).(accessstore.Authority)
	if !ok || authority.CredentialID == "" || authority.Hash == "" || organization != s.accessOrganization() {
		return accessstore.ErrDenied
	}
	if err := w.LockAccessOrganization(ctx); err != nil {
		return err
	}
	tenant, err := w.AccessTenant(ctx)
	if err != nil {
		return err
	}
	if err := s.accessMaintenanceBinding(tenant); err != nil {
		return err
	}
	if _, err := w.AuthorizeAccess(ctx, authority, s.config.now()); err != nil {
		return err
	}
	credential, err := w.AccessCredential(ctx, authority.CredentialID)
	if err != nil {
		return err
	}
	if credential.Scope != string(apiScopeAdmin) || credential.Kind != "service" || credential.ProjectAccess != string(hostedProjectsAll) || credential.NativeOnly || credential.UserID != "" || credential.MembershipID != "" {
		return accessstore.ErrDenied
	}
	member, err := w.AccessCredentialHasMember(ctx, credential.ID)
	if err != nil {
		return err
	}
	if member {
		return accessstore.ErrDenied
	}
	return nil
}
