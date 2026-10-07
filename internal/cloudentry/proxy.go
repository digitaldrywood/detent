package cloudentry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
)

var strippedRequestHeaders = []string{
	"Cookie", "Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Forwarded-Port",
	"X-Real-Ip", "X-Original-Url", "X-Rewrite-Url", "X-Original-Forwarded-For", cloudassert.Header,
}

func (s *Service) tenantTransport(organization Organization) (http.RoundTripper, error) {
	if cached, ok := s.transports.Load(organization.Endpoint); ok {
		if transport, ok := cached.(http.RoundTripper); ok {
			return transport, nil
		}
	}
	if !ValidEndpoint(organization.Endpoint) {
		return nil, errors.New("tenant endpoint is invalid")
	}
	path := strings.TrimPrefix(organization.Endpoint, "unix:")
	transport := &http.Transport{MaxIdleConnsPerHost: 16, IdleConnTimeout: time.Minute, ResponseHeaderTimeout: 30 * time.Second, DisableCompression: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			if err := verifyPrivateSocket(path); err != nil {
				return nil, err
			}
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", path)
		}}
	actual, _ := s.transports.LoadOrStore(organization.Endpoint, transport)
	if stored, ok := actual.(http.RoundTripper); ok {
		return stored, nil
	}
	return transport, nil
}

func (s *Service) claims(organization Organization, kind, method, path string, body []byte) (cloudassert.Claims, error) {
	id, err := cloudassert.NewID()
	if err != nil {
		return cloudassert.Claims{}, err
	}
	now := s.config.now()
	return cloudassert.Claims{
		Issuer: s.config.Issuer, Audience: organization.ID, Generation: organization.Generation, Kind: kind,
		Method: method, Path: path, BodyDigest: cloudassert.BodyDigest(body), IssuedAt: now, ExpiresAt: now.Add(assertionLifetime), ID: id,
	}, nil
}

func (s *Service) browserFailure(c echo.Context, organization string, status int) error {
	request := c.Request()
	if status == http.StatusServiceUnavailable {
		if strings.HasPrefix(request.URL.Path, "/api/") || request.Header.Get("Accept") == "application/json" {
			return c.JSON(status, map[string]string{"code": "unavailable", "message": "Service is temporarily unavailable"})
		}
		return s.browserUnavailable(c, status)
	}
	if (request.Method == http.MethodGet || request.Method == http.MethodHead) && !strings.HasPrefix(request.URL.Path, "/api/") && status == http.StatusUnauthorized {
		query := url.Values{"organization": {organization}}
		if validReturnPath(request.URL.Path, organization) {
			query.Set("return", request.URL.Path)
		}
		return c.Redirect(http.StatusSeeOther, "/auth/oidc/start?"+query.Encode())
	}
	if strings.HasPrefix(request.URL.Path, "/api/") || request.Header.Get("Accept") == "application/json" {
		return c.JSON(status, map[string]string{"code": "unauthorized", "message": "Sign in to this organization to continue"})
	}
	if status == http.StatusUnauthorized {
		return s.denied(c, status, "Sign in again to continue")
	}
	return s.denied(c, status, "This organization is unavailable to your account. Choose another organization.")
}

func wantsHTMLNavigation(c echo.Context) bool {
	request := c.Request()
	if (request.Method != http.MethodGet && request.Method != http.MethodHead) || strings.Contains(request.URL.Path, "/api/") || request.Header.Get(echo.HeaderAuthorization) != "" || wantsJSON(c) {
		return false
	}
	for value := range strings.SplitSeq(request.Header.Get(echo.HeaderAccept), ",") {
		mediaType, params, err := mime.ParseMediaType(value)
		if err != nil || mediaType != echo.MIMETextHTML {
			continue
		}
		if quality, ok := params["q"]; ok {
			q, err := strconv.ParseFloat(quality, 64)
			if err != nil || !(q > 0 && q <= 1) {
				continue
			}
		}
		return true
	}
	return false
}

const (
	verificationToken     = "token"
	verificationRefreshed = "refreshed"
)

// browserClaims authorizes a proxied browser request from the stored
// session and the organization's WorkOS access token, verified locally
// against the cached JWKS. The provider is called only to refresh an expired
// access token, so revocation and membership removal take effect at the next
// refresh, one access-token lifetime at most.
func (s *Service) browserClaims(c echo.Context, organization Organization, claims *cloudassert.Claims) (int, string, error) {
	ctx := c.Request().Context()
	session, err := s.storedSession(c)
	if err != nil {
		return http.StatusUnauthorized, "", err
	}
	method := c.Request().Method
	if method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions && !s.sameOrigin(c) {
		return http.StatusForbidden, "", errors.New("cross-origin browser mutation")
	}
	authorized, err := s.auth.authorization(ctx, session, organization.ID)
	if err != nil {
		return http.StatusUnauthorized, "", err
	}
	subject, email := session.Subject, session.Email
	if authorized.Support {
		if !s.supportActor(ctx, session.Email) || authorized.EffectiveEmail == "" {
			s.dropAuthorization(ctx, authorized)
			return http.StatusForbidden, "", errNoSession
		}
		subject, email = authorized.Identity.Subject, authorized.EffectiveEmail
	}
	access, verification, status, err := s.verifiedAccess(ctx, authorized)
	if err != nil {
		return status, verification, err
	}
	switch {
	case access.Subject != subject || access.SessionID != authorized.Identity.SessionID || !strings.EqualFold(access.SupportActor, authorized.Identity.SupportActor):
		s.dropAuthorization(ctx, authorized)
		return http.StatusUnauthorized, verification, errNoSession
	case access.OrganizationID != organization.ProviderID || !auth.ValidOrganizationRole(access.Role):
		s.dropAuthorization(ctx, authorized)
		return http.StatusForbidden, verification, errNoSession
	}
	identity := authorized.Identity
	claims.Kind = cloudassert.KindBrowser
	claims.Subject, claims.Email, claims.ProviderOrganization, claims.ProviderSession = subject, email, organization.ProviderID, identity.SessionID
	claims.SupportActor, claims.SupportReason = identity.SupportActor, identity.SupportReason
	claims.SessionCreatedAt, claims.SessionExpiresAt = identity.CreatedAt, identity.ExpiresAt
	claims.Role, claims.AccessExpiresAt = access.Role, access.ExpiresAt
	if !authorized.Support {
		claims.PlatformRole = s.platformRole(ctx, email)
	}
	claims.Binding, claims.CSRF = authorized.Binding, cloudassert.CSRFToken(session.CSRFSecret, organization.ID)
	return http.StatusOK, verification, nil
}

// verifiedAccess returns the authorization's current access claims. A valid
// stored token costs no provider call; an expired one is refreshed once,
// serialized per authorization because WorkOS refresh tokens are single use.
// A rejected refresh means the provider session ended, so the authorization
// is dropped; an unreachable provider is a transient 503.
func (s *Service) verifiedAccess(ctx context.Context, authorized authorization) (auth.HostedAccess, string, int, error) {
	tokens, err := s.auth.tokens(authorized)
	if err != nil || tokens.AccessToken == "" || tokens.RefreshToken == "" {
		s.dropAuthorization(ctx, authorized)
		return auth.HostedAccess{}, verificationToken, http.StatusUnauthorized, errNoSession
	}
	access, err := s.config.Provider.VerifyAccess(ctx, tokens.AccessToken)
	if err == nil {
		return access, verificationToken, http.StatusOK, nil
	}
	if auth.HostedIdentityReason(err) == auth.HostedReasonProviderUnavailable {
		return auth.HostedAccess{}, verificationToken, http.StatusServiceUnavailable, err
	}
	if !errors.Is(err, auth.ErrAccessExpired) {
		s.dropAuthorization(ctx, authorized)
		return auth.HostedAccess{}, verificationToken, http.StatusUnauthorized, errNoSession
	}
	unlock := s.refreshes.lock(authorized.Binding)
	defer unlock()
	current, err := s.auth.authorizationByBinding(ctx, authorized.Binding)
	if err != nil {
		return auth.HostedAccess{}, verificationRefreshed, http.StatusUnauthorized, errNoSession
	}
	if tokens, err = s.auth.tokens(current); err != nil {
		s.dropAuthorization(ctx, authorized)
		return auth.HostedAccess{}, verificationRefreshed, http.StatusUnauthorized, errNoSession
	}
	if access, err := s.config.Provider.VerifyAccess(ctx, tokens.AccessToken); err == nil {
		return access, verificationRefreshed, http.StatusOK, nil
	}
	// The redeemed refresh token is spent, so the rotated pair is persisted
	// even if the browser gives up on this request.
	detached, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	access, rotated, err := s.config.Provider.RefreshAccess(detached, tokens.RefreshToken)
	if auth.HostedIdentityReason(err) == auth.HostedReasonProviderUnavailable {
		if rotated.RefreshToken != "" {
			if err := s.auth.storeTokens(detached, authorized.Binding, rotated); err != nil {
				s.config.Logger.WarnContext(ctx, "shared entry could not store rotated tokens", "organization", authorized.Organization)
			}
		}
		return auth.HostedAccess{}, verificationRefreshed, http.StatusServiceUnavailable, err
	}
	if err != nil {
		if latest, lookupErr := s.auth.authorizationByBinding(detached, authorized.Binding); lookupErr == nil && latest.sealedRefresh != current.sealedRefresh {
			if latestTokens, openErr := s.auth.tokens(latest); openErr == nil {
				if access, verifyErr := s.config.Provider.VerifyAccess(detached, latestTokens.AccessToken); verifyErr == nil {
					return access, verificationRefreshed, http.StatusOK, nil
				}
			}
		}
		s.config.Logger.InfoContext(ctx, "shared entry token refresh rejected", "organization", authorized.Organization, "reason", auth.HostedIdentityReason(err))
		s.dropAuthorization(detached, authorized)
		return auth.HostedAccess{}, verificationRefreshed, http.StatusUnauthorized, errNoSession
	}
	if err := s.auth.storeTokens(detached, authorized.Binding, rotated); err != nil {
		return auth.HostedAccess{}, verificationRefreshed, http.StatusUnauthorized, errNoSession
	}
	return access, verificationRefreshed, http.StatusOK, nil
}

func (s *Service) dropAuthorization(ctx context.Context, item authorization) {
	if err := s.auth.revokeAuthorization(ctx, item.Binding); err != nil {
		s.config.Logger.Warn("shared entry could not revoke an authorization")
	}
	s.revokeAtTenants(ctx, []authorization{item})
}

// logProxied records one proxied request's latency and how its browser
// identity was verified. It logs the path without its query and never
// tokens, cookies or email addresses.
func (s *Service) logProxied(c echo.Context, organization, verification string, started time.Time, extra ...any) {
	request := c.Request()
	fields := append([]any{"organization", organization, "method", request.Method, "path", request.URL.Path, "status", c.Response().Status, "duration_ms", time.Since(started).Milliseconds(), "verification", verification}, extra...)
	s.config.Logger.InfoContext(request.Context(), "shared entry proxied request", fields...)
}

func (s *Service) proxy(c echo.Context) error {
	started := time.Now()
	request := c.Request()
	ctx := request.Context()
	if strings.Contains(request.URL.Path, "/attachment-metadata") {
		return c.JSON(http.StatusNotFound, map[string]string{"code": "not_found", "message": "Resource was not found"})
	}
	organization, err := s.readyOrganization(ctx, c.Param("organization"))
	if err != nil {
		if pending, lookupErr := s.registry.Organization(ctx, c.Param("organization")); lookupErr == nil && pending.Managed && (pending.State == "requested" || pending.State == "allocating" || pending.State == "failed") && request.Method == http.MethodGet && !strings.HasPrefix(request.URL.Path, "/api/") {
			return c.Redirect(http.StatusSeeOther, "/organizations/"+pending.ID+"/provisioning")
		}
		return c.JSON(http.StatusNotFound, map[string]string{"code": "not_found", "message": "Resource was not found"})
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, cloudassert.MaxBodyBytes+1))
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"code": "invalid_request", "message": "Request body could not be read"})
	}
	if len(body) > cloudassert.MaxBodyBytes {
		return c.JSON(http.StatusRequestEntityTooLarge, map[string]string{"code": "payload_too_large", "message": "Request body is too large"})
	}
	claims, err := s.claims(organization, cloudassert.KindMachine, request.Method, request.URL.RequestURI(), body)
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"code": "unavailable", "message": "Service is temporarily unavailable"})
	}
	verification := "machine"
	if request.Header.Get(echo.HeaderAuthorization) == "" {
		status, verified, err := s.browserClaims(c, organization, &claims)
		verification = verified
		if err != nil {
			defer s.logProxied(c, organization.ID, verification, started)
			return s.browserFailure(c, organization.ID, status)
		}
	}
	authorized := time.Since(started)
	defer func() { s.logProxied(c, organization.ID, verification, started, "auth_ms", authorized.Milliseconds()) }()
	assertion, err := cloudassert.Sign(s.config.SigningKey, claims)
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"code": "unavailable", "message": "Service is temporarily unavailable"})
	}
	transport, err := s.config.transport(organization)
	if err != nil {
		s.config.Logger.WarnContext(ctx, "shared entry tenant transport unavailable", "organization", organization.ID, "error", err)
		if wantsHTMLNavigation(c) {
			return s.browserUnavailable(c, http.StatusBadGateway)
		}
		return c.JSON(http.StatusBadGateway, map[string]string{"code": "tenant_unavailable", "message": "The organization is temporarily unavailable"})
	}
	proxy := &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(r *httputil.ProxyRequest) {
			r.Out.URL.Scheme, r.Out.URL.Host, r.Out.Host = "http", "tenant", "tenant"
			r.Out.URL.Path, r.Out.URL.RawPath, r.Out.URL.RawQuery = request.URL.Path, request.URL.RawPath, request.URL.RawQuery
			for _, header := range strippedRequestHeaders {
				r.Out.Header.Del(header)
			}
			r.Out.Header.Set(cloudassert.Header, assertion)
			r.Out.Body = io.NopCloser(bytes.NewReader(body))
			r.Out.ContentLength = int64(len(body))
		},
		ModifyResponse: func(response *http.Response) error {
			if request.Method != http.MethodGet && request.Method != http.MethodHead && response.StatusCode < http.StatusBadRequest {
				if err := s.refreshGitHubRoutes(c.Request().Context(), organization); err != nil {
					return err
				}
			}
			if err := s.attachmentMCPResponse(c, body, response); err != nil {
				return err
			}
			response.Header.Del("Set-Cookie")
			response.Header.Set("Cache-Control", "no-store")
			response.Header.Set("Content-Security-Policy", contentSecurity)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			s.config.Logger.Warn("shared entry tenant proxy failed", "organization", organization.ID, "error", err)
			if wantsHTMLNavigation(c) {
				if err := s.browserUnavailable(c, http.StatusBadGateway); err != nil {
					c.Error(err)
				}
				return
			}
			w.Header().Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"code":"tenant_unavailable","message":"The organization is temporarily unavailable"}`))
		},
	}
	proxy.ServeHTTP(c.Response(), request)
	return nil
}

func (s *Service) serviceRequest(ctx context.Context, organization Organization, path string, payload any, identity func(*cloudassert.Claims)) (int, error) {
	status, _, err := s.serviceCall(ctx, organization, path, payload, identity)
	return status, err
}

func (s *Service) serviceCall(ctx context.Context, organization Organization, path string, payload any, identity func(*cloudassert.Claims)) (int, []byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, err
	}
	claims, err := s.claims(organization, cloudassert.KindService, http.MethodPost, path, body)
	if err != nil {
		return 0, nil, err
	}
	if identity != nil {
		identity(&claims)
	}
	return s.signedCall(ctx, organization, claims, body, "")
}

// tenantCallResponseLimit bounds a tenant answer the entry reads itself. An
// answer past it is an error, never a truncated body that fails to decode.
const tenantCallResponseLimit = 64 << 10

func (s *Service) machineCall(ctx context.Context, organization Organization, method, path string, body []byte, bearer string) (int, []byte, error) {
	claims, err := s.claims(organization, cloudassert.KindMachine, method, path, body)
	if err != nil {
		return 0, nil, err
	}
	return s.signedCall(ctx, organization, claims, body, bearer)
}

func (s *Service) signedCall(ctx context.Context, organization Organization, claims cloudassert.Claims, body []byte, bearer string) (int, []byte, error) {
	return s.signedCallCSRF(ctx, organization, claims, body, bearer, "")
}

func (s *Service) signedCallCSRF(ctx context.Context, organization Organization, claims cloudassert.Claims, body []byte, bearer, csrf string) (int, []byte, error) {
	assertion, err := cloudassert.Sign(s.config.SigningKey, claims)
	if err != nil {
		return 0, nil, err
	}
	transport, err := s.config.transport(organization)
	if err != nil {
		return 0, nil, err
	}
	request, err := http.NewRequestWithContext(ctx, claims.Method, "http://tenant"+claims.Path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		request.Header.Set(echo.HeaderAuthorization, "Bearer "+bearer)
	}
	request.Header.Set(cloudassert.Header, assertion)
	if csrf != "" {
		request.Header.Set("X-CSRF-Token", csrf)
	}
	response, err := (&http.Client{Transport: transport, Timeout: 15 * time.Second}).Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, tenantCallResponseLimit+1))
	if err != nil {
		return 0, nil, err
	}
	if len(raw) > tenantCallResponseLimit {
		return 0, nil, fmt.Errorf("tenant %s %s response exceeds %d bytes", request.Method, request.URL.Path, tenantCallResponseLimit)
	}
	return response.StatusCode, raw, nil
}

func (s *Service) acceptInvitation(ctx context.Context, organization Organization, subject, email, providerSession, token string) error {
	status, err := s.serviceRequest(ctx, organization, "/internal/v1/invitations/accept", map[string]string{"token": token}, func(claims *cloudassert.Claims) {
		claims.Subject, claims.Email, claims.ProviderSession = subject, email, providerSession
	})
	if err != nil || status != http.StatusNoContent {
		return errors.Join(errors.New("tenant rejected the invitation"), err)
	}
	return nil
}

func (s *Service) revokeAtTenants(parent context.Context, items []authorization) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 15*time.Second)
	defer cancel()
	byOrganization := make(map[string][]string)
	for _, item := range items {
		byOrganization[item.Organization] = append(byOrganization[item.Organization], item.Binding)
	}
	for id, bindings := range byOrganization {
		organization, err := s.registry.Organization(ctx, id)
		if err != nil {
			continue
		}
		status, err := s.serviceRequest(ctx, organization, "/internal/v1/sessions/revoke", map[string][]string{"bindings": bindings}, nil)
		if err != nil || status != http.StatusNoContent {
			s.config.Logger.Warn("shared entry could not confirm tenant session revocation", "organization", id)
		}
	}
}

func (s *Service) acceptInvitationID(ctx context.Context, organization Organization, subject, email, providerSession, id string) error {
	status, err := s.serviceRequest(ctx, organization, "/internal/v1/invitations/accept", map[string]string{"invitation_id": id}, func(claims *cloudassert.Claims) {
		claims.Subject, claims.Email, claims.ProviderSession = subject, email, providerSession
	})
	if err != nil || status != http.StatusNoContent {
		return errors.Join(errors.New("tenant rejected the invitation"), err)
	}
	return nil
}
