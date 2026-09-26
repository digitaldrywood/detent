package cloudentry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

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
	transport := &http.Transport{MaxIdleConnsPerHost: 16, IdleConnTimeout: time.Minute, ResponseHeaderTimeout: 30 * time.Second, DisableCompression: true}
	if path, ok := strings.CutPrefix(organization.Endpoint, "unix:"); ok {
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", path)
		}
	} else {
		address := strings.TrimPrefix(organization.Endpoint, "http://")
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "tcp", address)
		}
	}
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

func (s *Service) browserDenied(c echo.Context, organization string, status int) error {
	request := c.Request()
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

func (s *Service) browserClaims(c echo.Context, organization Organization, claims *cloudassert.Claims) (int, error) {
	ctx := c.Request().Context()
	session, err := s.session(c)
	if err != nil {
		return http.StatusUnauthorized, err
	}
	method := c.Request().Method
	if method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions && !s.sameOrigin(c) {
		return http.StatusForbidden, errors.New("cross-origin browser mutation")
	}
	authorized, err := s.auth.authorization(ctx, session, organization.ID)
	if err != nil {
		return http.StatusUnauthorized, err
	}
	current, err := s.config.Provider.CurrentSession(ctx, authorized.Identity)
	if err != nil || current.Subject != session.Subject || current.OrganizationID != organization.ProviderID || current.SessionID != authorized.Identity.SessionID || !current.ExpiresAt.After(s.config.now()) {
		s.dropAuthorization(ctx, authorized)
		return http.StatusUnauthorized, errNoSession
	}
	memberships, err := s.config.Provider.Memberships(ctx, session.Subject, organization.ProviderID)
	if err != nil {
		return http.StatusServiceUnavailable, err
	}
	active := false
	for _, membership := range memberships {
		active = active || membership.UserID == session.Subject && membership.OrganizationID == organization.ProviderID && membership.Status == "active"
	}
	if !active {
		s.dropAuthorization(ctx, authorized)
		return http.StatusForbidden, errNoSession
	}
	identity := authorized.Identity
	if current.ExpiresAt.Before(identity.ExpiresAt) {
		identity.ExpiresAt = current.ExpiresAt
	}
	claims.Kind = cloudassert.KindBrowser
	claims.Subject, claims.Email, claims.ProviderOrganization, claims.ProviderSession = session.Subject, session.Email, organization.ProviderID, identity.SessionID
	claims.SessionCreatedAt, claims.SessionExpiresAt = identity.CreatedAt, identity.ExpiresAt
	claims.Binding, claims.CSRF = authorized.Binding, cloudassert.CSRFToken(session.Hash, organization.ID)
	return http.StatusOK, nil
}

func (s *Service) dropAuthorization(ctx context.Context, item authorization) {
	if err := s.auth.revokeAuthorization(ctx, item.Binding); err != nil {
		s.config.Logger.Warn("shared entry could not revoke an authorization")
	}
	s.revokeAtTenants(ctx, []authorization{item})
}

func (s *Service) proxy(c echo.Context) error {
	request := c.Request()
	ctx := request.Context()
	organization, err := s.readyOrganization(ctx, c.Param("organization"))
	if err != nil {
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
	if request.Header.Get(echo.HeaderAuthorization) == "" {
		if status, err := s.browserClaims(c, organization, &claims); err != nil {
			return s.browserDenied(c, organization.ID, status)
		}
	}
	assertion, err := cloudassert.Sign(s.config.SigningKey, claims)
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"code": "unavailable", "message": "Service is temporarily unavailable"})
	}
	transport, err := s.config.transport(organization)
	if err != nil {
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
			response.Header.Del("Set-Cookie")
			response.Header.Set("Cache-Control", "no-store")
			response.Header.Set("Content-Security-Policy", contentSecurity)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			w.Header().Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"code":"tenant_unavailable","message":"The organization is temporarily unavailable"}`))
		},
	}
	proxy.ServeHTTP(c.Response(), request)
	return nil
}

func (s *Service) serviceRequest(ctx context.Context, organization Organization, path string, payload any, identity func(*cloudassert.Claims)) (int, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}
	claims, err := s.claims(organization, cloudassert.KindService, http.MethodPost, path, body)
	if err != nil {
		return 0, err
	}
	if identity != nil {
		identity(&claims)
	}
	assertion, err := cloudassert.Sign(s.config.SigningKey, claims)
	if err != nil {
		return 0, err
	}
	transport, err := s.config.transport(organization)
	if err != nil {
		return 0, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://tenant"+path, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(cloudassert.Header, assertion)
	response, err := (&http.Client{Transport: transport, Timeout: 15 * time.Second}).Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10)); err != nil {
		return 0, err
	}
	return response.StatusCode, nil
}

func (s *Service) acceptInvitation(ctx context.Context, organization Organization, subject, email, providerSession, token string) error {
	if s.staff(email) {
		return errors.New("staff accounts cannot accept customer invitations")
	}
	status, err := s.serviceRequest(ctx, organization, "/internal/v1/invitations/accept", map[string]string{"token": token}, func(claims *cloudassert.Claims) {
		claims.Subject, claims.Email, claims.ProviderSession = subject, email, providerSession
	})
	if err != nil || status != http.StatusNoContent {
		return errors.Join(errors.New("tenant rejected the invitation"), err)
	}
	return nil
}

func (s *Service) revokeAtTenants(ctx context.Context, items []authorization) {
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
