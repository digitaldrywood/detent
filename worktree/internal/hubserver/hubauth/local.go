package hubauth

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

func (s *Authenticator) LocalCredential(ctx context.Context, c echo.Context) (Credential, int, error) {
	if s.deps.Hosted != nil {
		return Credential{}, http.StatusUnauthorized, fmt.Errorf("%w: local authentication is unavailable", operatortool.ErrAccessDenied)
	}
	token, err := BearerToken(c)
	if username, password, ok := c.Request().BasicAuth(); ok && username == "operator" {
		token, err = password, nil
	}
	if err != nil || len(token) > maxBearerTokenBytes || token == "" {
		return Credential{}, http.StatusUnauthorized, fmt.Errorf("%w: local credentials are required", operatortool.ErrAccessDenied)
	}
	credential, status, err := s.AuthenticateAccessHash(ctx, apikey.HashToken(token), "", "")
	if err != nil {
		return Credential{}, status, err
	}
	if credential.Scope != Admin || credential.NativeOnly || credential.Runner.RunnerID != "" {
		return Credential{}, http.StatusForbidden, fmt.Errorf("%w: local browser access requires an administrator", operatortool.ErrAccessDenied)
	}
	if _, _, basic := c.Request().BasicAuth(); basic {
		if c.Request().Header.Get("Sec-Fetch-Site") == "cross-site" {
			return Credential{}, http.StatusForbidden, fmt.Errorf("%w: cross-site browser access is unavailable", operatortool.ErrAccessDenied)
		}
		if !ReadRequest(c) {
			origin, err := url.Parse(c.Request().Header.Get("Origin"))
			scheme := "http"
			if c.IsTLS() || s.deps.TrustedProxy && c.Scheme() == "https" {
				scheme = "https"
			}
			if err != nil || origin.Scheme != scheme || origin.Host != c.Request().Host || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" {
				return Credential{}, http.StatusForbidden, fmt.Errorf("%w: same-origin browser access is required", operatortool.ErrAccessDenied)
			}
		}
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	c.Response().Header().Set("Referrer-Policy", "no-referrer")
	c.Response().Header().Set("X-Content-Type-Options", "nosniff")
	credential.HostedRole = "admin"
	s.deps.StoreCredential(c, credential)
	return credential, http.StatusOK, nil
}
