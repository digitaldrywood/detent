package web_test

import (
	"bytes"
	"encoding/base64"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/web"
)

func TestPrivateDashboardAccessEstablishesSession(t *testing.T) {
	t.Parallel()

	token := privateDashboardTestToken(1)
	server, err := web.NewServer(web.Config{
		GlobalConfig: globalconfig.Config{DashboardAccess: globalconfig.DashboardAccess{
			Mode:  globalconfig.DashboardAccessModePrivateToken,
			Token: token,
		}},
	}, testDeps(t))
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	requestPrivateDashboard(t, server, http.MethodGet, "/", nil, http.StatusNotFound)
	requestPrivateDashboard(t, server, http.MethodGet, "/health", nil, http.StatusOK)
	requestPrivateDashboard(t, server, http.MethodGet, "/?token=wrong", nil, http.StatusNotFound)

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

	requestPrivateDashboard(t, server, http.MethodGet, "/", cookie, http.StatusOK)
	requestPrivateDashboard(t, server, http.MethodGet, "/api/v1/state", cookie, http.StatusOK)
}

func TestPrivateDashboardAccessComposesWithMagicLinkAuth(t *testing.T) {
	t.Parallel()

	token := privateDashboardTestToken(7)
	deps := testDeps(t)
	deps.Store = openWebTestStore(t)
	deps.MagicLinkSender = &webAuthSender{}
	server, err := web.NewServer(web.Config{
		DashboardURL: "http://detent.test",
		GlobalConfig: globalconfig.Config{
			DashboardAccess: globalconfig.DashboardAccess{
				Mode:  globalconfig.DashboardAccessModePrivateToken,
				Token: token,
			},
			Auth: globalconfig.Auth{
				Mode:          globalconfig.AuthModeMagicLink,
				AllowedEmails: []string{"operator@example.com"},
				LinkTTL:       "15m",
				SessionTTL:    "1h",
			},
		},
	}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	requestPrivateDashboard(t, server, http.MethodGet, "/login", nil, http.StatusNotFound)
	privateCookie := establishPrivateDashboardSession(t, server, token)
	protected := performWebAuthRequest(t, server, http.MethodGet, "/", "", privateCookie)
	if protected.Code != http.StatusSeeOther || protected.Header().Get("Location") != "/login" {
		t.Fatalf("protected response = %d Location %q, want redirect to login", protected.Code, protected.Header().Get("Location"))
	}
	requestPrivateDashboard(t, server, http.MethodGet, "/login", privateCookie, http.StatusOK)
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
			server, err := web.NewServer(web.Config{
				GlobalConfig: globalconfig.Config{DashboardAccess: globalconfig.DashboardAccess{
					Mode:       globalconfig.DashboardAccessModePrivateToken,
					Token:      token,
					AllowWrite: tt.allowWrite,
				}},
			}, testDeps(t))
			if err != nil {
				t.Fatalf("NewServer() error = %v", err)
			}
			server.Echo().POST("/test-mutation", func(c echo.Context) error {
				return c.NoContent(http.StatusNoContent)
			})
			cookie := establishPrivateDashboardSession(t, server, token)
			requestPrivateDashboard(t, server, http.MethodPost, "/test-mutation", cookie, tt.want)
		})
	}
}

func TestPrivateDashboardCookieIsSecureBehindTLSProxy(t *testing.T) {
	t.Parallel()

	token := privateDashboardTestToken(7)
	server, err := web.NewServer(web.Config{
		GlobalConfig: globalconfig.Config{DashboardAccess: globalconfig.DashboardAccess{
			Mode:  globalconfig.DashboardAccessModePrivateToken,
			Token: token,
		}},
	}, testDeps(t))
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
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
	server, err := web.NewServer(web.Config{
		GlobalConfig:       cfg,
		GlobalConfigSource: func() globalconfig.Config { return cfg },
	}, testDeps(t))
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	oldCookie := establishPrivateDashboardSession(t, server, oldToken)
	requestPrivateDashboard(t, server, http.MethodGet, "/reports", oldCookie, http.StatusOK)

	cfg.DashboardAccess.Token = newToken
	requestPrivateDashboard(t, server, http.MethodGet, "/reports", oldCookie, http.StatusNotFound)
	newCookie := establishPrivateDashboardSession(t, server, newToken)
	requestPrivateDashboard(t, server, http.MethodGet, "/reports", newCookie, http.StatusOK)
}

func TestPrivateDashboardWriteAccessAuthorizesDashboardAPI(t *testing.T) {
	t.Parallel()

	token := privateDashboardTestToken(6)
	probe := &refreshProbe{}
	deps := testDeps(t)
	deps.Refresher = probe
	server, err := web.NewServer(web.Config{
		GlobalConfig: globalconfig.Config{DashboardAccess: globalconfig.DashboardAccess{
			Mode:       globalconfig.DashboardAccessModePrivateToken,
			Token:      token,
			AllowWrite: true,
		}},
	}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	cookie := establishPrivateDashboardSession(t, server, token)
	requestPrivateDashboard(t, server, http.MethodPost, "/api/v1/refresh", cookie, http.StatusAccepted)
	if probe.calls != 1 {
		t.Fatalf("RequestRefresh() calls = %d, want 1", probe.calls)
	}
}

func TestPrivateDashboardAccessDoesNotReplaceAPIAuthentication(t *testing.T) {
	t.Parallel()

	server, err := web.NewServer(web.Config{
		GlobalConfig: globalconfig.Config{
			APIToken: "detent_api_token",
			DashboardAccess: globalconfig.DashboardAccess{
				Mode:  globalconfig.DashboardAccessModePrivateToken,
				Token: privateDashboardTestToken(4),
			},
		},
	}, testDeps(t))
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/state", nil)
	req.Header.Set("Authorization", "Bearer detent_api_token")
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("API token status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
}

func TestPrivateDashboardTokenDoesNotLeakToLogs(t *testing.T) {
	t.Parallel()

	token := privateDashboardTestToken(5)
	var logs bytes.Buffer
	server, err := web.NewServer(web.Config{
		Logger: slog.New(slog.NewTextHandler(&logs, nil)),
		GlobalConfig: globalconfig.Config{DashboardAccess: globalconfig.DashboardAccess{
			Mode:  globalconfig.DashboardAccessModePrivateToken,
			Token: token,
		}},
	}, testDeps(t))
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	requestPrivateDashboard(t, server, http.MethodGet, "/?token="+url.QueryEscape(token+"wrong"), nil, http.StatusNotFound)
	if strings.Contains(logs.String(), token) {
		t.Fatalf("logs leaked dashboard token: %s", logs.String())
	}
}

func establishPrivateDashboardSession(t *testing.T, server *web.Server, token string) *http.Cookie {
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
		if cookie.Name == "detent_private_dashboard" {
			return cookie
		}
	}
	t.Fatalf("private dashboard cookie missing: %#v", cookies)
	return nil
}

func requestPrivateDashboard(t *testing.T, server *web.Server, method string, path string, cookie *http.Cookie, want int) {
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
