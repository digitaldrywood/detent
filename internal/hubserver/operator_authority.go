package hubserver

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/mcp"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) registerOperatorTools(e *echo.Echo) {
	// Hosted administration reuses application commands; telemetry/explainer
	// services remain unavailable here.
	executor := s.operatorAdministration()
	s.administration = executor
	s.mcpHTTP = mcp.NewHTTPHandler(executor, s.config.Version, mcp.HTTPConfig{
		Principal: func(request *http.Request) operatortool.Identity {
			return operatortool.ConnectionIdentity(request.Context())
		},
	})
	e.Any("/api/v2/organizations/:organization/mcp", echo.WrapHandler(s.mcpHTTP), s.operatorAuthority)
	if s.config.Hosted != nil {
		e.GET("/chat/approval", s.operatorAdministrationApproval, s.operatorAuthority)
		e.POST("/chat/approval", s.operatorAdministrationApproval, s.operatorAuthority)
		e.Any("/mcp", echo.WrapHandler(s.mcpHTTP), s.operatorAuthority)
	}
}

func (s *Service) operatorAuthority(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		credential, status, err := s.authenticateAPIRequest(c)
		if err != nil && s.config.Hosted != nil && !s.hostedShared() && c.Request().Header.Get(echo.HeaderAuthorization) == "" {
			if _, hash, sessionErr := s.hostedSession(c); sessionErr == nil {
				credential, err = s.hostedAccountCredential(c.Request().Context(), hash)
			}
		}
		if err != nil {
			return c.JSON(status, apiErrorResponse{Code: "access_denied", Message: operatortool.ErrAccessDenied.Error()})
		}
		organization := c.Param("organization")
		if organization == "" && s.config.Hosted != nil {
			organization = s.config.Hosted.OrganizationID
		}
		if selected := strings.TrimSpace(c.Request().Header.Get("X-Detent-Organization")); selected != "" && selected != organization {
			return c.JSON(http.StatusForbidden, apiErrorResponse{Code: "access_denied", Message: operatortool.ErrAccessDenied.Error()})
		}
		identity := operatorIdentity(credential, organization)
		var token string
		if c.Request().Header.Get(echo.HeaderAuthorization) != "" {
			token, err = apiBearerToken(c)
			if err != nil {
				return c.JSON(http.StatusUnauthorized, apiErrorResponse{Code: "access_denied", Message: operatortool.ErrAccessDenied.Error()})
			}
		}
		claims, shared := hostedSharedClaims(c)
		connection := operatortool.Connection{DashboardURL: func() string {
			if s.config.Hosted != nil {
				return s.config.Hosted.PublicURL
			}
			return ""
		}(), Identity: identity, Resolve: func(ctx context.Context) (operatortool.Authority, error) {
			current := credential
			if token != "" {
				var err error
				current, _, err = s.authenticateAPIToken(ctx, token, "", "")
				if err != nil {
					return operatortool.Authority{}, operatortool.ErrAccessDenied
				}
			} else {
				session, err := s.storedWebSession(ctx, credential.SessionHash, s.config.now())
				if err != nil || session.Identity == nil {
					return operatortool.Authority{}, operatortool.ErrAccessDenied
				}
				if shared {
					if claims.Kind != cloudassert.KindBrowser || !claims.AccessExpiresAt.After(s.config.now()) {
						return operatortool.Authority{}, operatortool.ErrAccessDenied
					}
					current, _, err = s.hostedSharedCredential(ctx, session, credential.SessionHash, claims.Role)
				} else {
					if credential.HostedRole == "account" {
						current, err = s.hostedAccountCredential(ctx, credential.SessionHash)
					} else if _, err = s.WebSession(ctx, credential.SessionHash, s.config.now()); err == nil {
						current, _, err = s.hostedSessionCredential(ctx, session, credential.SessionHash)
					}
				}
				if err != nil {
					return operatortool.Authority{}, operatortool.ErrAccessDenied
				}
			}
			return s.operatorCurrentAuthority(ctx, current, organization)
		}}
		ctx := operatortool.WithConnection(c.Request().Context(), connection)
		if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead}); err != nil {
			return c.JSON(http.StatusForbidden, apiErrorResponse{Code: "access_denied", Message: operatortool.ErrAccessDenied.Error()})
		}
		request := c.Request().WithContext(ctx)
		if s.hostedShared() {
			// The verified entry rewrites Host to its private tenant destination.
			// MCP's Origin check uses the configured public origin, never a
			// caller-supplied Forwarded/X-Forwarded-Host permission switch.
			public, err := url.Parse(s.config.Hosted.PublicURL)
			if err != nil || public.Host == "" {
				return c.JSON(http.StatusForbidden, apiErrorResponse{Code: "access_denied", Message: operatortool.ErrAccessDenied.Error()})
			}
			request.Host = public.Host
		}
		c.SetRequest(request)
		return next(c)
	}
}

func operatorIdentity(credential apiCredential, organization string) operatortool.Identity {
	return operatortool.Identity{PrincipalID: credential.ID, OrganizationID: organization, CredentialID: credential.Hash, SessionID: credential.SessionHash}
}

func (s *Service) operatorCurrentAuthority(ctx context.Context, credential apiCredential, organization string) (operatortool.Authority, error) {
	if organization == "" || credential.Runner.RunnerID != "" || credential.Scope != apiScopeOperator && credential.Scope != apiScopeAdmin || s.config.Hosted != nil && credential.Hosted == nil {
		return operatortool.Authority{}, operatortool.ErrAccessDenied
	}
	if credential.HostedRole == "account" {
		if s.config.Hosted == nil || s.hostedShared() || organization != s.config.Hosted.OrganizationID {
			return operatortool.Authority{}, operatortool.ErrAccessDenied
		}
		return operatortool.Authority{Identity: operatorIdentity(credential, organization), Account: operatortool.Account{Subject: credential.Hosted.Subject, Role: "account"}, Check: func(_ context.Context, r operatortool.Requirement) error {
			if r.ProjectID != "" || r.ResourceID != "" || r.ResourceKind != "" {
				return operatortool.ErrAccessDenied
			}
			return nil
		}}, nil
	}

	scope := nativeScope{organization: tracker.OrganizationID(organization), credential: credential}
	if err := s.authorizeConversationOrganization(ctx, scope); err != nil {
		return operatortool.Authority{}, operatortool.ErrAccessDenied
	}
	account := operatortool.Account{}
	if credential.Hosted != nil {
		account = operatortool.Account{Subject: credential.Hosted.Subject, Role: credential.HostedRole, SupportActor: credential.Hosted.SupportActor}
	}
	return operatortool.Authority{Account: account, Identity: operatorIdentity(credential, organization), Check: func(ctx context.Context, requirement operatortool.Requirement) error {
		if requirement.OrganizationWide && credential.Hosted == nil && credential.NativeOnly || requirement.ResourceID != "" || requirement.ResourceKind != "" {
			// Resource-specific commands must use their application's ownership
			// check; this initial read adapter never grants an unknown resource.
			return operatortool.ErrAccessDenied
		}
		checkScope := scope
		if requirement.ProjectID != "" {
			checkScope.project = tracker.ProjectID(requirement.ProjectID)
			if err := s.requireHostedProject(ctx, s.database.db, checkScope, requirement.Scope != apikey.ScopeRead); err != nil {
				return err
			}
			if err := s.database.authorizeNativeProject(ctx, checkScope); err != nil {
				return err
			}
		}
		if requirement.Scope != apikey.ScopeRead {
			if credential.Hosted != nil {
				checkScope.requireHostedAdmin = requirement.Scope == apikey.ScopeAdmin
				tx, err := s.database.db.BeginTx(ctx, nil)
				if err != nil {
					return err
				}
				defer tx.Rollback()
				return s.recheckHostedMutation(ctx, tx, checkScope)
			}
			if requirement.Scope == apikey.ScopeAdmin && credential.Scope != apiScopeAdmin {
				return operatortool.ErrAccessDenied
			}
		}
		return nil
	}}, nil
}

func (s *Service) operatorAdministrationApproval(c echo.Context) error {
	if c.Request().Header.Get(echo.HeaderAuthorization) != "" {
		return c.NoContent(http.StatusForbidden)
	}
	session, hash, err := s.hostedSession(c)
	if err != nil || session.Identity == nil {
		return c.NoContent(http.StatusForbidden)
	}
	csrf := hostedCSRF(hash)
	// Dedicated CSRF derives from the cookie token rather than its stored hash.
	if cookie, err := c.Cookie(hostedCookie); err == nil {
		csrf = hostedCSRF(cookie.Value)
	}
	if s.hostedShared() {
		if claims, ok := hostedSharedClaims(c); ok {
			csrf = claims.CSRF
		} else {
			return c.NoContent(http.StatusForbidden)
		}
	}
	return s.administration.Approval(c, csrf)
}
