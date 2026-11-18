package web

import (
	"bytes"
	"encoding/base64"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
)

func TestNewServerConfiguresHTTPTimeouts(t *testing.T) {
	t.Parallel()

	server := newPrivateDashboardTestServer(t, Config{})
	httpServer := server.echo.Server
	if httpServer.ReadHeaderTimeout <= 0 {
		t.Fatalf("ReadHeaderTimeout = %v, want positive duration", httpServer.ReadHeaderTimeout)
	}
	if httpServer.IdleTimeout <= 0 {
		t.Fatalf("IdleTimeout = %v, want positive duration", httpServer.IdleTimeout)
	}
}

func TestServerRegistersOnlyPublicOpenAPIRoute(t *testing.T) {
	t.Parallel()

	server, err := NewServer(Config{Mode: ModeOnboarding}, Dependencies{})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	var apiRoutes []string
	for _, route := range server.echo.Routes() {
		if strings.HasPrefix(route.Path, "/api/v1/") {
			apiRoutes = append(apiRoutes, route.Method+" "+route.Path)
		}
	}
	if !slices.Equal(apiRoutes, []string{http.MethodGet + " " + openAPIPath}) {
		t.Fatalf("API routes = %v, want only public OpenAPI route", apiRoutes)
	}
}

func TestPrivateDashboardAccessEstablishesSession(t *testing.T) {
	t.Parallel()

	token := privateDashboardTestToken(1)
	server := newPrivateDashboardTestServer(t, Config{
		GlobalConfig: globalconfig.Config{DashboardAccess: globalconfig.DashboardAccess{
			Mode:  globalconfig.DashboardAccessModePrivateToken,
			Token: token,
		}},
	})

	requestPrivateDashboard(t, server, http.MethodGet, "/test-read", nil, http.StatusNotFound)
	requestPrivateDashboard(t, server, http.MethodGet, "/health", nil, http.StatusOK)
	requestPrivateDashboard(t, server, http.MethodGet, "/test-read?token=wrong", nil, http.StatusNotFound)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "https://detent.example/?view=board&token="+url.QueryEscape(token), nil)
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("token entry status = %d, want %d; body = %s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	if location := rec.Header().Get("Location"); location != "/?view=board" {
		t.Fatalf("Location = %q, want clean URL", location)
	}
	cookie := privateDashboardCookie(t, rec.Result().Cookies())
	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" {
		t.Fatalf("session cookie = %#v, want HttpOnly Secure SameSite=Lax Path=/", cookie)
	}
	if strings.Contains(cookie.Value, token) {
		t.Fatal("session cookie contains URL token")
	}

	requestPrivateDashboard(t, server, http.MethodGet, "/test-read", cookie, http.StatusNoContent)
}

func TestPrivateDashboardAccessEnforcesReadOnlyMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		allowWrite bool
		want       int
	}{
		{name: "read only by default", want: http.StatusForbidden},
		{name: "explicit write access", allowWrite: true, want: http.StatusNoContent},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			token := privateDashboardTestToken(byte(len(tt.name)))
			server := newPrivateDashboardTestServer(t, Config{
				GlobalConfig: globalconfig.Config{DashboardAccess: globalconfig.DashboardAccess{
					Mode:       globalconfig.DashboardAccessModePrivateToken,
					Token:      token,
					AllowWrite: tt.allowWrite,
				}},
			})
			cookie := establishPrivateDashboardSession(t, server, token)
			requestPrivateDashboard(t, server, http.MethodPost, "/test-mutation", cookie, tt.want)
		})
	}
}

func TestPrivateDashboardCookieIsSecureBehindTLSProxy(t *testing.T) {
	t.Parallel()

	token := privateDashboardTestToken(7)
	server := newPrivateDashboardTestServer(t, Config{
		GlobalConfig: globalconfig.Config{DashboardAccess: globalconfig.DashboardAccess{
			Mode:  globalconfig.DashboardAccessModePrivateToken,
			Token: token,
		}},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://detent.internal/?token="+url.QueryEscape(token), nil)
	server.Handler().ServeHTTP(rec, req)
	cookie := privateDashboardCookie(t, rec.Result().Cookies())
	if !cookie.Secure {
		t.Fatal("private dashboard cookie Secure = false behind HTTP TLS proxy hop")
	}
}

func TestPrivateDashboardAccessRotationInvalidatesSession(t *testing.T) {
	oldToken := privateDashboardTestToken(2)
	newToken := privateDashboardTestToken(3)
	cfg := globalconfig.Config{DashboardAccess: globalconfig.DashboardAccess{
		Mode:  globalconfig.DashboardAccessModePrivateToken,
		Token: oldToken,
	}}
	server := newPrivateDashboardTestServer(t, Config{
		GlobalConfig:       cfg,
		GlobalConfigSource: func() globalconfig.Config { return cfg },
	})
	oldCookie := establishPrivateDashboardSession(t, server, oldToken)
	requestPrivateDashboard(t, server, http.MethodGet, "/test-read", oldCookie, http.StatusNoContent)

	cfg.DashboardAccess.Token = newToken
	requestPrivateDashboard(t, server, http.MethodGet, "/test-read", oldCookie, http.StatusNotFound)
	newCookie := establishPrivateDashboardSession(t, server, newToken)
	requestPrivateDashboard(t, server, http.MethodGet, "/test-read", newCookie, http.StatusNoContent)
}

func TestPrivateDashboardAccessDoesNotGateAPICredentials(t *testing.T) {
	t.Parallel()

	server := newPrivateDashboardTestServer(t, Config{
		GlobalConfig: globalconfig.Config{
			APIToken: "detent_api_token",
			DashboardAccess: globalconfig.DashboardAccess{
				Mode:  globalconfig.DashboardAccessModePrivateToken,
				Token: privateDashboardTestToken(4),
			},
		},
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/test-read", nil)
	req.Header.Set("Authorization", "Bearer detent_api_token")
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("API token status = %d, want %d; body = %s", rec.Code, http.StatusNoContent, rec.Body.String())
	}
	requestPrivateDashboard(t, server, http.MethodGet, "/api/v1/test-read", nil, http.StatusNotFound)
}

func TestPrivateDashboardTokenDoesNotLeakToLogs(t *testing.T) {
	t.Parallel()

	token := privateDashboardTestToken(5)
	var logs bytes.Buffer
	server := newPrivateDashboardTestServer(t, Config{
		Logger: slog.New(slog.NewTextHandler(&logs, nil)),
		GlobalConfig: globalconfig.Config{DashboardAccess: globalconfig.DashboardAccess{
			Mode:  globalconfig.DashboardAccessModePrivateToken,
			Token: token,
		}},
	})
	requestPrivateDashboard(t, server, http.MethodGet, "/?token="+url.QueryEscape(token+"wrong"), nil, http.StatusNotFound)
	if strings.Contains(logs.String(), token) {
		t.Fatalf("logs leaked dashboard token: %s", logs.String())
	}
}

func newPrivateDashboardTestServer(t *testing.T, cfg Config) *Server {
	t.Helper()
	cfg.Mode = ModeOnboarding
	cfg.LookupEnv = func(string) string { return "" }
	server, err := NewServer(cfg, Dependencies{})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	noContent := func(c echo.Context) error { return c.NoContent(http.StatusNoContent) }
	server.echo.GET("/test-read", noContent)
	server.echo.GET("/api/v1/test-read", noContent)
	server.echo.POST("/test-mutation", noContent)
	return server
}

func establishPrivateDashboardSession(t *testing.T, server *Server, token string) *http.Cookie {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/?token="+url.QueryEscape(token), nil)
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("token entry status = %d, want %d; body = %s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	return privateDashboardCookie(t, rec.Result().Cookies())
}

func privateDashboardCookie(t *testing.T, cookies []*http.Cookie) *http.Cookie {
	t.Helper()
	for _, cookie := range cookies {
		if cookie.Name == privateDashboardCookieName {
			return cookie
		}
	}
	t.Fatalf("private dashboard cookie missing: %#v", cookies)
	return nil
}

func requestPrivateDashboard(t *testing.T, server *Server, method string, path string, cookie *http.Cookie, want int) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != want {
		t.Fatalf("%s %s status = %d, want %d; body = %s", method, path, rec.Code, want, rec.Body.String())
	}
}

func privateDashboardTestToken(fill byte) string {
	return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{fill}, 32))
}
