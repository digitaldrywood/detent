package origin

import (
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/cloudorigin"
)

// Legacy aliases belong only to the shared hosted product. A customer-operated
// entry keeps its configured origin and never inherits the hosted aliases.
func (s *Service) LegacyHostedRequest(request *http.Request) bool {
	legacy := cloudorigin.Legacy(s.PublicURL)
	return legacy != "" && "https://"+request.Host == legacy
}

func (s *Service) RedirectLegacyNavigation(c echo.Context) bool {
	r := c.Request()
	if !s.LegacyHostedRequest(r) || r.Method != http.MethodGet && r.Method != http.MethodHead {
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
	if cookie, err := c.Cookie(s.CookieName("session")); err == nil && cookie.Value != "" {
		return false
	}
	return true
}

const ContentSecurity = "default-src 'self'; script-src 'self' 'wasm-unsafe-eval'; worker-src 'self' blob:; style-src 'self'; style-src-attr 'unsafe-inline'; img-src 'self' data: https://workoscdn.com; font-src 'self'; connect-src 'self' https://huggingface.co https://us.aws.cdn.hf.co https://api.workos.com; object-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'self' https://checkout.stripe.com https://billing.stripe.com https://detent.build"

type Deps struct {
	PublicURL string
	Secure    bool
}

type Service struct {
	Deps
}

func New(deps Deps) *Service { return &Service{Deps: deps} }

func (s *Service) Boundary(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		header := c.Response().Header()
		header.Set("Cache-Control", "no-store")
		header.Set("Referrer-Policy", "no-referrer")
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("Content-Security-Policy", ContentSecurity)
		if s.Secure {
			header.Set("Strict-Transport-Security", "max-age=31536000")
		}
		if !cloudassert.CanonicalPath(c.Request().URL) {
			return c.JSON(http.StatusNotFound, map[string]string{"code": "not_found", "message": "Resource was not found"})
		}
		if s.RedirectLegacyNavigation(c) {
			return c.Redirect(http.StatusTemporaryRedirect, s.PublicURL+c.Request().URL.RequestURI())
		}
		return next(c)
	}
}

func (s *Service) CookieName(name string) string {
	if s.Secure {
		return "__Host-detent_" + name
	}
	return "detent_entry_" + name
}

func (s *Service) SameOrigin(c echo.Context) bool {
	header := c.Request().Header
	origin := header.Get("Origin")
	if origin == "null" {
		return header.Get("Sec-Fetch-Site") == "same-origin"
	}
	return origin != "" && (origin == strings.TrimRight(s.PublicURL, "/") ||
		s.LegacyHostedRequest(c.Request()) && origin == "https://"+c.Request().Host)
}
