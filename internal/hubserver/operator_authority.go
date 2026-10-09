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
	// Hubs expose native work commands and hosted billing/usage operations.
	s.operatorChat = chat.NewService(nil, nil, hostedOperatorExecutor{s}, chat.WithClock(s.config.now), chat.WithSessionStore(operatorChatStore{s.database}, s.resolveOperatorChatAuthority))
	s.administration = s.operatorAdministration()
	s.administration.Chat = s.operatorChat
	executor := hostedOperatorExecutor{s}
	s.mcpHTTP = mcp.NewHTTPHandler(executor, s.config.Version, mcp.HTTPConfig{
		Logger: s.config.Logger,
		Principal: func(request *http.Request) operatortool.Identity {
			return operatortool.ConnectionIdentity(request.Context())
		},
	})
	e.Any("/api/v2/organizations/:organization/mcp", echo.WrapHandler(s.mcpHTTP), s.operatorAuthority)
	if s.config.Hosted != nil {
		e.Any("/mcp", echo.WrapHandler(s.mcpHTTP), s.operatorAuthority)
	}
}

func (s *Service) operatorAuthority(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		credential, status, err := s.authenticateAPIRequest(c)
		if err != nil && s.config.Hosted != nil && !s.hostedShared() && c.Request().Header.Get(echo.HeaderAuthorization) == "" {
			if _, hash, sessionErr := s.hostedSession(c.Request().Context(), c); sessionErr == nil {
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
		resolve := func(ctx context.Context) (apiCredential, error) {
			current := credential
			if token != "" {
				var err error
				current, _, err = s.authenticateAPIToken(ctx, token, "", "")
				if err != nil {
					return apiCredential{}, operatortool.ErrAccessDenied
				}
			} else {
				session, err := s.storedWebSession(ctx, credential.SessionHash, s.config.now())
				if err != nil || session.Identity == nil {
					return apiCredential{}, operatortool.ErrAccessDenied
				}
				if shared {
					if claims.Kind != cloudassert.KindBrowser || !claims.AccessExpiresAt.After(s.config.now()) {
						return apiCredential{}, operatortool.ErrAccessDenied
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
					return apiCredential{}, operatortool.ErrAccessDenied
				}
			}
			return current, nil
		}
		connection := operatortool.Connection{Identity: identity, DashboardURL: s.operatorDashboardURL(), Resolve: func(ctx context.Context) (operatortool.Authority, error) {
			current, err := resolve(ctx)
			if err != nil {
				return operatortool.Authority{}, err
			}
			return s.operatorCurrentAuthority(ctx, current, organization)
		}}
		ctx := operatortool.WithConnection(context.WithValue(c.Request().Context(), operatorCredentialKey{}, billingAuthorization(resolve)), connection)
		ctx = context.WithValue(ctx, hubOperatorResolverKey{}, resolve)
		ctx = context.WithValue(ctx, nativeOperatorScopeKey{}, func(ctx context.Context) (nativeScope, error) {
			current, err := resolve(ctx)
			if err != nil {
				return nativeScope{}, err
			}
			return nativeScope{organization: tracker.OrganizationID(organization), credential: current}, nil
		})
		authority, err := s.operatorCurrentAuthority(ctx, credential, organization)
		if err != nil || !identity.Valid() || authority.Identity != identity || authority.Check(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, OrganizationID: organization}) != nil {
			return c.JSON(http.StatusForbidden, apiErrorResponse{Code: "access_denied", Message: operatortool.ErrAccessDenied.Error()})
		}
		ctx = s.withOperatorCatalog(ctx, credential, organization)
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
		c.Set("hub_api_credential", credential)
		return next(c)
	}
}

func operatorIdentity(credential apiCredential, organization string) operatortool.Identity {
	principal := credential.ID
	if credential.HostedPrincipal != "" {
		principal = credential.HostedPrincipal
	}
	return operatortool.Identity{PrincipalID: principal, OrganizationID: organization, CredentialID: credential.Hash, SessionID: credential.SessionHash}
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
	return operatortool.Authority{Account: account, Identity: operatorIdentity(credential, organization), Explainer: operatorWorkReads{service: s, scope: scope}, WorkReads: operatorWorkReads{service: s, scope: scope}, Changes: hubChangeApplication{service: s, scope: scope}, BindContext: func(ctx context.Context) context.Context {
		ctx = context.WithValue(ctx, operatorScopeKey{}, scope)
		ctx = context.WithValue(ctx, hubOperatorResolverKey{}, func(context.Context) (apiCredential, error) { return credential, nil })
		ctx = context.WithValue(ctx, nativeOperatorScopeKey{}, func(context.Context) (nativeScope, error) { return scope, nil })
		return context.WithValue(ctx, operatorCredentialKey{}, billingAuthorization(func(context.Context) (apiCredential, error) { return credential, nil }))
	}, Check: func(ctx context.Context, requirement operatortool.Requirement) error {
		if requirement.ResourceKind == "credentials" && s.config.Hosted != nil {
			if requirement.ProjectID != "" || requirement.ResourceID != "" {
				return operatortool.ErrAccessDenied
			}
			_, err := s.hostedKeyCredentialFor(ctx, credential)
			return err
		}
		if requirement.ResourceKind == "credentials" {
			requirement.ResourceKind = ""
		}
		if credential.HostedKeyScope != "" && !hostedKeyAllows(credential.HostedKeyScope, requirement.Scope) {
			return operatortool.ErrAccessDenied
		}
		if requirement.OrganizationWide && credential.Hosted == nil && credential.NativeOnly {
			return operatortool.ErrAccessDenied
		}
		if requirement.ResourceKind == "runners" {
			if credential.NativeOnly && credential.Hosted == nil || credential.Runner.RunnerID != "" {
				return operatortool.ErrAccessDenied
			}
			if credential.Hosted != nil {
				return s.requireHostedRunnerAdministration(ctx, s.database.db, credential)
			}
			if credential.Scope != apiScopeAdmin {
				return operatortool.ErrAccessDenied
			}
			return nil
		}
		if requirement.ResourceKind == "billing" || requirement.ResourceKind == "plan" {
			if credential.Hosted == nil || requirement.ResourceID != "" || s.config.Hosted == nil || organization != s.config.Hosted.OrganizationID {
				return operatortool.ErrAccessDenied
			}
			if requirement.ResourceKind == "billing" && (credential.HostedRole != "owner" || credential.Hosted.SupportActor != "") {
				return operatortool.ErrAccessDenied
			}
			if requirement.ResourceKind == "plan" && credential.HostedRole != "owner" && credential.HostedRole != "admin" {
				return operatortool.ErrAccessDenied
			}
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
				if credential.HostedRole == "viewer" || requirement.Scope == apikey.ScopeAdmin && credential.HostedRole != "owner" && credential.HostedRole != "admin" {
					return operatortool.ErrAccessDenied
				}
				checkScope.requireHostedAdmin = requirement.Scope == apikey.ScopeAdmin
				tx, err := s.database.db.BeginTx(ctx, nil)
				if err != nil {
					return err
				}
				defer tx.Rollback()
				if err := s.recheckHostedMutation(ctx, tx, checkScope); err != nil {
					return err
				}
				if requirement.ResourceKind == "billing" || requirement.ResourceKind == "plan" {
					return nil
				}
				return s.checkOperatorResource(ctx, tx, checkScope, requirement)
			}
			if requirement.Scope == apikey.ScopeAdmin && credential.Scope != apiScopeAdmin {
				return operatortool.ErrAccessDenied
			}
		}
		if requirement.ResourceKind == "billing" || requirement.ResourceKind == "plan" {
			return nil
		}
		return s.checkOperatorResource(ctx, s.database.db, checkScope, requirement)
	}}, nil
}
