package hubauth

import (
	"errors"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

func (s *Authenticator) RequireAPI(options APIOptions, allowed ...TokenScope) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			credential, status, err := s.AuthenticateRequest(c.Request().Context(), c)
			if err != nil {
				if options.IsNotFound(err) {
					return options.Error(c, err)
				}
				if !errors.Is(options.Classify(err), operatortool.ErrAccessDenied) {
					return options.Error(c, options.Classify(err))
				}
				return options.Refuse(c, status, "unauthorized", "Valid scoped API token is required", Response{Code: "unauthorized", Message: "Valid scoped API token is required"})
			}
			if s.deps.Hosted != nil {
				if credential.Hosted != nil && c.Path() == runnerBase+"/:runner/routing" && c.Request().Method == http.MethodGet {
					return options.HostedAdministration(next)(c)
				}
				if credential.Hosted == nil && credential.Runner.RunnerID == "" && c.Path() != "/health" && !options.ArtifactPublisher(c, credential) && !options.ChangePrincipal(c, credential) {
					return options.Error(c, hubapi.NotFound())
				}
				hostedRoute := c.Path() == "/api/v2/capabilities" || strings.HasPrefix(c.Path(), nativeBase) || c.Path() == "/api/v2/organizations" && c.Request().Method == http.MethodGet || (c.Path() == nativeOrganizationIssuePath || c.Path() == workBoardPath) && ReadRequest(c)
				if options.ConfigurationRank(c) {
					hostedRoute = true
				}
				if credential.Hosted != nil && (!hostedRoute || strings.HasSuffix(c.Path(), "/checks")) {
					return options.Error(c, hubapi.NotFound())
				}
			}
			if credential.HostedKeyScope != "" && !KeyAllows(credential.HostedKeyScope, apikey.ScopeWrite) && !ReadRequest(c) {
				return options.Refuse(c, http.StatusForbidden, "insufficient_scope", "API key permission does not permit writes", nil)
			}
			if credential.Runner.RunnerID != "" && !RunnerOperationAllowed(c, credential.Runner.Operations) {
				return options.Refuse(c, http.StatusForbidden, "insufficient_scope", "Runner does not permit this operation", Response{Code: "insufficient_scope", Message: "Runner does not permit this operation"})
			}
			if strings.HasPrefix(c.Path(), "/api/v1/") {
				if credential.NativeOnly {
					return options.Refuse(c, http.StatusForbidden, "native_protocol_required", "Scoped tokens require the native protocol", Response{Code: "native_protocol_required", Message: "Scoped tokens require the native protocol"})
				}
				if err := options.CompatibilityResource(c); err != nil {
					return err
				}
			}
			for _, scope := range allowed {
				if credential.Scope == scope || credential.Scope == Admin {
					s.deps.StoreCredential(c, credential)
					if err := options.AdmitRunner(c, credential); err != nil || c.Response().Committed {
						return err
					}
					return next(c)
				}
			}
			return options.Refuse(c, http.StatusForbidden, "insufficient_scope", "API token scope does not permit this operation", Response{Code: "insufficient_scope", Message: "API token scope does not permit this operation"})
		}
	}
}
