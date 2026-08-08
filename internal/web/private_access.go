package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
)

const privateDashboardCookieName = "detent_private_dashboard"
const privateDashboardSessionPayload = "detent-private-dashboard-session-v1"

func (s *Server) privateDashboardAccess(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		access := s.dashboardAccess()
		if access.Mode != globalconfig.DashboardAccessModePrivateToken || dashboardAccessPublicRequest(c.Request()) {
			return next(c)
		}

		if token := strings.TrimSpace(c.QueryParam("token")); token != "" {
			if !privateDashboardTokenEqual(token, access.Token) {
				return echo.NewHTTPError(http.StatusNotFound)
			}
			s.setPrivateDashboardCookie(c, access.Token)
			return c.Redirect(http.StatusSeeOther, cleanPrivateDashboardURL(c.Request()))
		}

		if !s.authorizePrivateDashboardSession(c) {
			return echo.NewHTTPError(http.StatusNotFound)
		}
		if requestMutates(c.Request()) && !access.AllowWrite {
			return c.JSON(http.StatusForbidden, errorResponse("dashboard_read_only", "Private dashboard access is read-only"))
		}
		return next(c)
	}
}

func (s *Server) dashboardAccess() globalconfig.DashboardAccess {
	if s == nil {
		return globalconfig.DashboardAccess{}
	}
	cfg := s.globalConfig
	if s.globalConfigSource != nil {
		cfg = s.globalConfigSource()
	}
	access := cfg.DashboardAccess
	access.Mode = strings.ToLower(strings.TrimSpace(access.Mode))
	access.Token = strings.TrimSpace(access.Token)
	return access
}

func dashboardAccessPublicRequest(req *http.Request) bool {
	if req == nil {
		return false
	}
	path := req.URL.Path
	if path == "/health" || path == openAPIPath || path == "/mcp" || strings.HasPrefix(path, "/static/") {
		return true
	}
	if path == "/api/v1/webhooks/github" || strings.HasPrefix(path, "/api/v1/intake/") {
		return true
	}
	return strings.HasPrefix(path, "/api/") && len(requestAPITokens(req)) > 0
}

func (s *Server) setPrivateDashboardCookie(c echo.Context, token string) {
	c.SetCookie(&http.Cookie{
		Name:     privateDashboardCookieName,
		Value:    privateDashboardSession(token),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   true,
	})
}

func (s *Server) authorizePrivateDashboardSession(c echo.Context) bool {
	if c == nil || c.Request() == nil {
		return false
	}
	access := s.dashboardAccess()
	if access.Mode != globalconfig.DashboardAccessModePrivateToken || access.Token == "" {
		return false
	}
	cookie, err := c.Cookie(privateDashboardCookieName)
	if err != nil {
		return false
	}
	return hmac.Equal([]byte(cookie.Value), []byte(privateDashboardSession(access.Token)))
}

func privateDashboardSession(token string) string {
	mac := hmac.New(sha256.New, []byte(strings.TrimSpace(token)))
	_, _ = mac.Write([]byte(privateDashboardSessionPayload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func privateDashboardTokenEqual(candidate string, configured string) bool {
	return hmac.Equal([]byte(strings.TrimSpace(candidate)), []byte(strings.TrimSpace(configured)))
}

func cleanPrivateDashboardURL(req *http.Request) string {
	if req == nil || req.URL == nil {
		return "/"
	}
	clean := *req.URL
	query := clean.Query()
	query.Del("token")
	clean.RawQuery = query.Encode()
	clean.Scheme = ""
	clean.Host = ""
	if clean.Path == "" {
		clean.Path = "/"
	}
	return clean.RequestURI()
}

func requestMutates(req *http.Request) bool {
	if req == nil {
		return false
	}
	switch req.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	default:
		return true
	}
}
