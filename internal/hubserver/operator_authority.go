package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/mcp"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tenantauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type operatorEntryOriginKey struct{}

func operatorRequestCredential(c echo.Context) (apiCredential, error) {
	credential, ok := c.Get("hub_api_credential").(apiCredential)
	if !ok || credential.ID == "" {
		return apiCredential{}, operatortool.ErrAccessDenied
	}
	return credential, nil
}

type operatorAuthorizationFailure struct {
	cause error
	kind  error
}

func (e *operatorAuthorizationFailure) Error() string        { return e.kind.Error() }
func (e *operatorAuthorizationFailure) Unwrap() error        { return e.cause }
func (e *operatorAuthorizationFailure) Is(target error) bool { return errors.Is(e.kind, target) }

func operatorAuthorizationError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, tenantauth.ErrUnavailable) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || auth.HostedIdentityReason(err) == auth.HostedReasonProviderInvalid {
		return &operatorAuthorizationFailure{cause: err, kind: operatortool.ErrServiceUnavailable}
	}
	var failure *operatorAuthorizationFailure
	if errors.As(err, &failure) {
		return failure
	}
	var denied *operatortool.AuthorizationError
	if errors.As(err, &denied) && denied.Cause != nil {
		return operatorAuthorizationError(denied.Cause)
	}
	if auth.HostedIdentityReason(err) == auth.HostedReasonProviderUnavailable || errors.Is(err, operatortool.ErrProviderUnavailable) {
		return &operatorAuthorizationFailure{cause: err, kind: operatortool.ErrProviderUnavailable}
	}
	var refusal *apikey.Refusal
	var native *nativeError
	if errors.As(err, &refusal) || errors.As(err, &native) && (native.status == http.StatusForbidden || native.status == http.StatusNotFound) {
		return operatortool.ErrAccessDenied
	}
	for _, denial := range []error{operatortool.ErrAccessDenied, accessstore.ErrDenied, auth.ErrHostedIdentity, auth.ErrInvalidSession, auth.ErrAccessExpired, cloudassert.ErrInvalid, corestore.ErrNotFound, corestore.ErrTenantMismatch, sql.ErrNoRows} {
		if errors.Is(err, denial) {
			return operatortool.ErrAccessDenied
		}
	}
	return &operatorAuthorizationFailure{cause: err, kind: operatortool.ErrServiceUnavailable}
}

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

type operatorAccessTimeKey struct{}

func operatorAccessTime(ctx context.Context, now time.Time) time.Time {
	if accepted, ok := ctx.Value(operatorAccessTimeKey{}).(time.Time); ok {
		return accepted
	}
	return now
}

func (s *Service) operatorAuthority(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		credential, status, err := s.authenticateAPIRequest(c)
		if err != nil && errors.Is(operatorAuthorizationError(err), operatortool.ErrAccessDenied) && s.config.Hosted != nil && !s.hostedShared() && c.Request().Header.Get(echo.HeaderAuthorization) == "" {
			if _, hash, sessionErr := s.hostedSession(c.Request().Context(), c); sessionErr == nil {
				credential, err = s.hostedAccountCredential(c.Request().Context(), hash)
			} else {
				err = sessionErr
			}
		}
		if err != nil {
			if !errors.Is(operatorAuthorizationError(err), operatortool.ErrAccessDenied) {
				return s.nativeAPIError(c, operatorAuthorizationError(err))
			}
			return s.refusedAPIError(c, status, "access_denied", operatortool.ErrAccessDenied.Error(), apiErrorResponse{Code: "access_denied", Message: operatortool.ErrAccessDenied.Error()})
		}
		organization := c.Param("organization")
		if organization == "" && s.config.Hosted != nil {
			organization = s.config.Hosted.OrganizationID
		}
		if selected := strings.TrimSpace(c.Request().Header.Get("X-Detent-Organization")); selected != "" && selected != organization {
			return s.refusedAPIError(c, http.StatusForbidden, "access_denied", operatortool.ErrAccessDenied.Error(), apiErrorResponse{Code: "access_denied", Message: operatortool.ErrAccessDenied.Error()})
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
		var entryOrigin string
		if credential.EntryKey != nil && claims.Key != nil && len(claims.Key.CredentialBinding) == 64 && s.config.Hosted.SharedEntry.ResolveKey != nil {
			encoded, err := json.Marshal(entryauthority.Origin{KeyID: claims.Key.ID, CredentialBinding: claims.Key.CredentialBinding, Subject: claims.Subject, Issuer: claims.Issuer, Generation: claims.Generation})
			if err != nil {
				return err
			}
			entryOrigin = string(encoded)
		}
		resolve := func(ctx context.Context) (apiCredential, error) {
			if current, ok := entryBoardCredential(ctx); ok {
				return current, nil
			}
			current := credential
			if credential.EntryKey != nil {
				var err error
				if entryOrigin != "" {
					current, err = s.currentEntryOriginCredential(ctx, entryOrigin)
				} else {
					current, _, err = s.entryKeyCredential(ctx, claims)
				}
				if err != nil {
					return apiCredential{}, operatorAuthorizationError(err)
				}
			} else if token != "" {
				var err error
				if s.config.TenantRepository != nil {
					current, _, err = s.authenticateTenantAPIHash(ctx, tracker.OrganizationID(organization), apikey.HashToken(token), "", "")
				} else {
					current, _, err = s.authenticateAPIToken(ctx, token, "", "")
				}
				if err != nil {
					return apiCredential{}, operatorAuthorizationError(err)
				}
			} else {
				session, err := s.storedWebSession(ctx, credential.SessionHash, s.config.now())
				if err != nil {
					return apiCredential{}, operatorAuthorizationError(err)
				}
				if session.Identity == nil {
					return apiCredential{}, operatortool.ErrAccessDenied
				}
				if shared {
					if claims.Kind != cloudassert.KindBrowser || !claims.AccessExpiresAt.After(operatorAccessTime(ctx, s.config.now())) {
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
					return apiCredential{}, operatorAuthorizationError(err)
				}
			}
			return current, nil
		}
		connection := operatortool.Connection{Identity: identity, Origin: entryOrigin, DashboardURL: s.operatorDashboardURL(), Resolve: func(ctx context.Context) (operatortool.Authority, error) {
			current, err := resolve(ctx)
			if err != nil {
				return operatortool.Authority{}, err
			}
			return s.operatorCurrentAuthority(ctx, current, organization)
		}}
		ctx := operatortool.WithConnection(context.WithValue(c.Request().Context(), operatorCredentialKey{}, billingAuthorization(resolve)), connection)
		if credential.EntryKey != nil {
			ctx = context.WithValue(ctx, operatorEntryOriginKey{}, entryOrigin)
		}

		ctx = context.WithValue(ctx, hubOperatorResolverKey{}, resolve)
		ctx = context.WithValue(ctx, nativeOperatorScopeKey{}, func(ctx context.Context) (nativeScope, error) {
			current, err := resolve(ctx)
			if err != nil {
				return nativeScope{}, err
			}
			return nativeScope{organization: tracker.OrganizationID(organization), credential: current}, nil
		})
		authority, err := s.operatorCurrentAuthority(ctx, credential, organization)
		if err != nil {
			return s.nativeAPIError(c, operatorAuthorizationError(err))
		}
		if !identity.Valid() || authority.Identity != identity || authority.Check == nil {
			return s.refusedAPIError(c, http.StatusForbidden, "access_denied", operatortool.ErrAccessDenied.Error(), apiErrorResponse{Code: "access_denied", Message: operatortool.ErrAccessDenied.Error()})
		}
		if err := authority.Check(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, OrganizationID: organization}); err != nil {
			return s.nativeAPIError(c, operatorAuthorizationError(err))
		}
		ctx = s.withOperatorCatalog(ctx, credential, organization)
		request := c.Request().WithContext(ctx)
		if s.hostedShared() {
			// The verified entry rewrites Host to its private tenant destination.
			// MCP's Origin check uses the configured public origin, never a
			// caller-supplied Forwarded/X-Forwarded-Host permission switch.
			public, err := url.Parse(s.config.Hosted.PublicURL)
			if err != nil || public.Host == "" {
				return s.refusedAPIError(c, http.StatusForbidden, "access_denied", operatortool.ErrAccessDenied.Error(), apiErrorResponse{Code: "access_denied", Message: operatortool.ErrAccessDenied.Error()})
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
		return operatortool.Authority{}, operatorAuthorizationError(err)
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
	}, Check: func(ctx context.Context, requirement operatortool.Requirement) (err error) {
		defer func() { err = operatorAuthorizationError(err) }()
		ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
		defer cancel()
		if credential.HostedRole == "reporter" && !hostedRoleAllows(credential.HostedRole, requirement.Scope, requirement.Action) {
			return operatortool.ErrAccessDenied
		}
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
				return s.requireHostedRunnerAdministration(ctx, nil, credential)
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
		var query nativeQueryer
		checkScope := scope
		checkScope.action = requirement.Action
		if requirement.ProjectID != "" {
			checkScope.project = tracker.ProjectID(requirement.ProjectID)
			if err := s.authorizeProjectRecord(ctx, checkScope, requirement.Scope != apikey.ScopeRead); err != nil {
				return err
			}
			if _, err := s.readProjectRecord(ctx, checkScope); err != nil {
				return err
			}
		}
		if requirement.Scope != apikey.ScopeRead {
			if credential.Hosted != nil {
				if !hostedRoleAllows(credential.HostedRole, requirement.Scope, requirement.Action) {
					return operatortool.ErrAccessDenied
				}
				if requirement.ProjectID != "" {
					return s.checkOperatorResource(ctx, query, checkScope, requirement)
				}
				checkScope.requireHostedAdmin = requirement.Scope == apikey.ScopeAdmin
				if operatortool.IsThreadTool(requirement.Action) {
					return s.authorizeProjectRecord(ctx, checkScope, true)
				}
				if err := s.recheckProjectRecordMutation(ctx, checkScope); err != nil {
					return err
				}
				if requirement.ResourceKind == "billing" || requirement.ResourceKind == "plan" {
					return nil
				}
				return s.checkOperatorResource(ctx, query, checkScope, requirement)
			}
			if requirement.Scope == apikey.ScopeAdmin && credential.Scope != apiScopeAdmin {
				return operatortool.ErrAccessDenied
			}
		}
		if requirement.ResourceKind == "billing" || requirement.ResourceKind == "plan" {
			return nil
		}
		return s.checkOperatorResource(ctx, query, checkScope, requirement)
	}}, nil
}
