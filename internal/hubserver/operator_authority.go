package hubserver

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/mcp"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) registerOperatorTools(e *echo.Echo) {
	// Hosted hubs do not own the daemon's telemetry/explainer. An absent
	// application service produces an empty catalog and opaque unavailable
	// calls, rather than using the compatibility API or opening a runtime DB.
	executor := workspaceOperatorExecutor{server: s}
	s.operatorChat = chat.NewService(nil, nil, executor)
	s.mcpHTTP = mcp.NewHTTPHandler(executor, s.config.Version, mcp.HTTPConfig{
		Principal: func(request *http.Request) operatortool.Identity {
			return operatortool.ConnectionIdentity(request.Context())
		},
	})
	e.Any("/api/v2/organizations/:organization/mcp", echo.WrapHandler(s.mcpHTTP), s.operatorAuthority)
	if s.config.Hosted != nil {
		e.GET("/chat/approval", s.workspaceApprovalPage, s.workspaceApprovalAuth)
		e.POST("/chat/approval", s.workspaceApprovalDecision, s.workspaceApprovalAuth)
		e.Any("/mcp", echo.WrapHandler(s.mcpHTTP), s.operatorAuthority)
	}
}

func (s *Service) operatorAuthority(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		credential, status, err := s.authenticateAPIRequest(c)
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
		connection := operatortool.Connection{DashboardURL: s.workspaceApprovalURL(""), Identity: identity, Resolve: func(ctx context.Context) (operatortool.Authority, error) {
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
					if _, err = s.WebSession(ctx, credential.SessionHash, s.config.now()); err == nil {
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
	scope := nativeScope{organization: tracker.OrganizationID(organization), credential: credential}
	if err := s.authorizeConversationOrganization(ctx, scope); err != nil {
		return operatortool.Authority{}, operatortool.ErrAccessDenied
	}
	return operatortool.Authority{Identity: operatorIdentity(credential, organization), ApplicationContext: func(ctx context.Context) context.Context { return context.WithValue(ctx, operatorScopeKey{}, scope) }, Check: func(ctx context.Context, requirement operatortool.Requirement) error {
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
				if err := s.recheckHostedMutation(ctx, tx, checkScope); err != nil {
					return err
				}
				return s.checkOperatorResource(ctx, tx, checkScope, requirement)
			}
			if requirement.Scope == apikey.ScopeAdmin && credential.Hosted == nil && credential.Scope != apiScopeAdmin {
				return operatortool.ErrAccessDenied
			}
		}
		return s.checkOperatorResource(ctx, s.database.db, checkScope, requirement)
	}}, nil
}
