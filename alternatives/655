package cloudentry

import (
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/cloudorigin"
)

// Legacy aliases belong only to the shared hosted product. A customer-operated
// entry keeps its configured origin and never inherits the hosted aliases.
func (s *Service) legacyHostedRequest(request *http.Request) bool {
	legacy := cloudorigin.Legacy(s.config.PublicURL)
	return legacy != "" && "https://"+request.Host == legacy
}

func (s *Service) redirectLegacyNavigation(c echo.Context) bool {
	r := c.Request()
	if !s.legacyHostedRequest(r) || r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	// Integration requests retain their method, credentials, signed body, query,
	// and streaming connection. The TLS ingress forwards both hosts unchanged.
	if r.Header.Get("Authorization") != "" || r.Header.Get("Upgrade") != "" ||
		strings.HasPrefix(r.URL.Path, "/api/") || strings.Contains(r.URL.Path, "/api/") ||
		strings.HasPrefix(r.URL.Path, "/webhooks/") || r.URL.Path == "/auth/oidc/callback" {
		return false
	}
	// A new login must create its host-only transaction cookie at the canonical
	// origin, even when an older account session is still present at the alias.
	if r.URL.Path == "/auth/oidc/start" || r.URL.Path == "/invite" {
		return true
	}
	// Existing sessions and callbacks finish at the alias without copying cookies
	// between origins. Their normal expiry or sign-out moves navigation to Cloud.
	if cookie, err := c.Cookie(s.cookieName("session")); err == nil && cookie.Value != "" {
		return false
	}
	return true
}
