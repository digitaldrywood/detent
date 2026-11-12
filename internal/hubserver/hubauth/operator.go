package hubauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Authenticator) Operator(options OperatorOptions) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			credential, status, err := s.AuthenticateRequest(c.Request().Context(), c)
			if err != nil && errors.Is(options.Classify(err), operatortool.ErrAccessDenied) && s.deps.Hosted != nil && !s.shared() && c.Request().Header.Get(echo.HeaderAuthorization) == "" {
				if _, hash, sessionErr := s.HostedSession(c.Request().Context(), c); sessionErr == nil {
					credential, err = options.AccountCredential(c.Request().Context(), hash)
				} else {
					err = sessionErr
				}
			}
			if err != nil {
				if !errors.Is(options.Classify(err), operatortool.ErrAccessDenied) {
					return options.Error(c, options.Classify(err))
				}
				return options.Refuse(c, status, "access_denied", operatortool.ErrAccessDenied.Error(), Response{Code: "access_denied", Message: operatortool.ErrAccessDenied.Error()})
			}
			organization := c.Param("organization")
			if organization == "" && s.deps.Hosted != nil {
				organization = s.deps.Hosted.OrganizationID
			}
			if selected := strings.TrimSpace(c.Request().Header.Get("X-Detent-Organization")); selected != "" && selected != organization {
				return options.Refuse(c, http.StatusForbidden, "access_denied", operatortool.ErrAccessDenied.Error(), Response{Code: "access_denied", Message: operatortool.ErrAccessDenied.Error()})
			}
			identity := Identity(credential, organization)
			var token string
			if c.Request().Header.Get(echo.HeaderAuthorization) != "" {
				token, err = BearerToken(c)
				if err != nil {
					return c.JSON(http.StatusUnauthorized, Response{Code: "access_denied", Message: operatortool.ErrAccessDenied.Error()})
				}
			}
			claims, shared := s.deps.SharedClaims(c)
			var entryOrigin string
			if credential.EntryKey != nil && claims.Key != nil && len(claims.Key.CredentialBinding) == 64 && options.ResolveEntryKey {
				encoded, err := json.Marshal(entryauthority.Origin{KeyID: claims.Key.ID, CredentialBinding: claims.Key.CredentialBinding, Subject: claims.Subject, Issuer: claims.Issuer, Generation: claims.Generation})
				if err != nil {
					return err
				}
				entryOrigin = string(encoded)
			}
			resolve := func(ctx context.Context) (Credential, error) {
				if current, ok := s.deps.BoardCredential(ctx); ok {
					return current, nil
				}
				current := credential
				if credential.EntryKey != nil {
					var err error
					if entryOrigin != "" {
						current, err = options.EntryOrigin(ctx, entryOrigin)
					} else {
						current, _, err = s.EntryKeyCredential(ctx, claims)
					}
					if err != nil {
						return Credential{}, options.Classify(err)
					}
				} else if token != "" {
					var err error
					if s.deps.Repository != nil {
						current, _, err = s.AuthenticateRepositoryAPIHash(ctx, s.deps.Repository, tracker.OrganizationID(organization), apikey.HashToken(token), "", "")
					} else {
						current, _, err = s.AuthenticateAccessHash(ctx, apikey.HashToken(token), "", "")
					}
					if err != nil {
						return Credential{}, options.Classify(err)
					}
				} else {
					session, err := s.StoredSession(ctx, credential.SessionHash, s.deps.Now())
					if err != nil {
						return Credential{}, options.Classify(err)
					}
					if session.Identity == nil {
						return Credential{}, operatortool.ErrAccessDenied
					}
					if shared {
						if claims.Kind != cloudassert.KindBrowser || !claims.AccessExpiresAt.After(options.AccessTime(ctx, s.deps.Now())) {
							return Credential{}, operatortool.ErrAccessDenied
						}
						current, _, err = s.SharedCredential(ctx, session, credential.SessionHash, claims.Role)
					} else {
						if credential.HostedRole == "account" {
							current, err = options.AccountCredential(ctx, credential.SessionHash)
						} else if _, err = options.CurrentSession(ctx, credential.SessionHash, s.deps.Now()); err == nil {
							current, _, err = s.SessionCredential(ctx, session, credential.SessionHash)
						}
					}
					if err != nil {
						return Credential{}, options.Classify(err)
					}
				}
				return current, nil
			}
			connection := operatortool.Connection{Identity: identity, Origin: entryOrigin, DashboardURL: options.DashboardURL, Resolve: func(ctx context.Context) (operatortool.Authority, error) {
				current, err := resolve(ctx)
				if err != nil {
					return operatortool.Authority{}, err
				}
				return options.Authority(ctx, current, organization)
			}}
			ctx := operatortool.WithConnection(c.Request().Context(), connection)
			if options.Bind != nil {
				ctx = options.Bind(ctx, organization, entryOrigin, credential, resolve)
			}
			ctx = WithResolver(ctx, func(ctx context.Context) (Input, error) {
				current, err := resolve(ctx)
				return Input{Organization: tracker.OrganizationID(organization), Credential: current}, err
			})
			authority, err := options.Authority(ctx, credential, organization)
			if err != nil {
				return options.Error(c, options.Classify(err))
			}
			if !identity.Valid() || authority.Identity != identity || authority.Check == nil {
				return options.Refuse(c, http.StatusForbidden, "access_denied", operatortool.ErrAccessDenied.Error(), Response{Code: "access_denied", Message: operatortool.ErrAccessDenied.Error()})
			}
			if err := authority.Check(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, OrganizationID: organization}); err != nil {
				return options.Error(c, options.Classify(err))
			}
			ctx = options.Catalog(ctx, credential, organization)
			request := c.Request().WithContext(ctx)
			if s.shared() {
				public, err := url.Parse(options.PublicURL)
				if err != nil || public.Host == "" {
					return options.Refuse(c, http.StatusForbidden, "access_denied", operatortool.ErrAccessDenied.Error(), Response{Code: "access_denied", Message: operatortool.ErrAccessDenied.Error()})
				}
				request.Host = public.Host
			}
			c.SetRequest(request)
			s.deps.StoreCredential(c, credential)
			return next(c)
		}
	}
}
