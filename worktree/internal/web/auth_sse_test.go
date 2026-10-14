package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/store"
)

func TestRequestRemoteAddrLoopback(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		remoteAddr string
		want       bool
	}{
		{name: "IPv4", remoteAddr: "127.0.0.1:49152", want: true},
		{name: "IPv4 loopback range", remoteAddr: "127.255.255.254:1", want: true},
		{name: "IPv6", remoteAddr: "[::1]:49152", want: true},
		{name: "IPv4-mapped IPv6", remoteAddr: "[::ffff:127.0.0.1]:49152", want: true},
		{name: "non-loopback IPv4", remoteAddr: "192.0.2.10:49152"},
		{name: "non-loopback IPv6", remoteAddr: "[2001:db8::10]:49152"},
		{name: "empty"},
		{name: "missing port", remoteAddr: "127.0.0.1"},
		{name: "hostname", remoteAddr: "localhost:49152"},
		{name: "invalid port", remoteAddr: "127.0.0.1:not-a-port"},
		{name: "leading whitespace", remoteAddr: " 127.0.0.1:49152"},
		{name: "unbracketed IPv6", remoteAddr: "::1:49152"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := requestRemoteAddrLoopback(tt.remoteAddr); got != tt.want {
				t.Fatalf("requestRemoteAddrLoopback(%q) = %t, want %t", tt.remoteAddr, got, tt.want)
			}
		})
	}
}

func TestLoopbackPeerReadTrustPreservesRateLimits(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		configure func(*Server)
	}{
		{
			name: "pre-auth IP limiter",
			configure: func(server *Server) {
				server.ipLimiter = newAPIRateLimiter(1, 1)
			},
		},
		{
			name: "credential limiter",
			configure: func(server *Server) {
				server.keyLimiter = newAPIRateLimiter(1, 1)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := globalconfig.Config{TrustLoopbackPeerRead: true}
			server := &Server{
				globalConfig:       cfg,
				globalConfigSource: func() globalconfig.Config { return cfg },
				lookupEnv:          func(string) string { return "" },
				serverAddr:         "0.0.0.0:0",
				now:                time.Now,
			}
			tt.configure(server)
			t.Cleanup(func() {
				if server.ipLimiter != nil {
					server.ipLimiter.Stop()
				}
				if server.keyLimiter != nil {
					server.keyLimiter.Stop()
				}
			})

			e := echo.New()
			handler := server.apiAuth(false)(func(c echo.Context) error {
				return c.NoContent(http.StatusNoContent)
			})
			for index, want := range []int{http.StatusNoContent, http.StatusTooManyRequests} {
				req := httptest.NewRequest(http.MethodGet, "/api/v1/state", nil)
				req.RemoteAddr = "127.0.0.1:49152"
				rec := httptest.NewRecorder()
				err := handler(e.NewContext(req, rec))
				if index == 0 && err != nil {
					t.Fatalf("request %d error = %v", index+1, err)
				}
				if rec.Code != want {
					t.Fatalf("request %d status = %d, want %d", index+1, rec.Code, want)
				}
				if index == 1 && (rec.Header().Get("X-RateLimit-Limit") == "" || rec.Header().Get("Retry-After") == "") {
					t.Fatalf("rate limit headers = %#v", rec.Header())
				}
			}
		})
	}
}

func TestLoopbackPeerReadTrustSkipsPersistentAPIKeyUsage(t *testing.T) {
	t.Parallel()

	usageStore := &loopbackPeerReadUsageStore{}
	server := &Server{store: usageStore}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/state", nil)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	credential := apikey.LoopbackPeerReadCredential()

	server.markAPIKeyLastUsed(credential)
	server.recordAPIUsage(c, credential, time.Now(), nil)
	if usageStore.lastUsedCalls != 0 || usageStore.usageCalls != 0 {
		t.Fatalf("persistent usage calls = last-used %d usage %d, want 0", usageStore.lastUsedCalls, usageStore.usageCalls)
	}
}

type loopbackPeerReadUsageStore struct {
	store.Store
	lastUsedCalls int
	usageCalls    int
}

func (s *loopbackPeerReadUsageStore) MarkAPIKeyLastUsed(context.Context, string, time.Time) error {
	s.lastUsedCalls++
	return nil
}

func (s *loopbackPeerReadUsageStore) RecordAPIUsageLog(context.Context, store.APIUsageLog) error {
	s.usageCalls++
	return nil
}

func TestAuthorizeDashboardSSE(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		method     string
		host       string
		remoteAddr string
		accept     string
		referer    string
		want       bool
	}{
		{
			name:       "allows private same-origin event source",
			method:     http.MethodGet,
			host:       "dashboard.detent.test:4000",
			remoteAddr: "100.95.107.51:49152",
			accept:     "text/event-stream",
			referer:    "http://dashboard.detent.test:4000/",
			want:       true,
		},
		{
			name:       "allows local event source from loopback peer",
			method:     http.MethodGet,
			host:       "localhost:4000",
			remoteAddr: "127.0.0.1:49152",
			accept:     "text/event-stream",
			referer:    "http://localhost:4000/",
			want:       true,
		},
		{
			name:       "rejects cross-origin event source",
			method:     http.MethodGet,
			host:       "dashboard.detent.test:4000",
			remoteAddr: "100.95.107.51:49152",
			accept:     "text/event-stream",
			referer:    "http://attacker.test:4000/",
		},
		{
			name:       "rejects non-stream request",
			method:     http.MethodGet,
			host:       "dashboard.detent.test:4000",
			remoteAddr: "100.95.107.51:49152",
			accept:     "text/html",
			referer:    "http://dashboard.detent.test:4000/",
		},
		{
			name:       "rejects external peer spoofing localhost",
			method:     http.MethodGet,
			host:       "localhost:4000",
			remoteAddr: "203.0.113.10:49152",
			accept:     "text/event-stream",
			referer:    "http://localhost:4000/",
		},
		{
			name:       "rejects non-get stream request",
			method:     http.MethodPost,
			host:       "dashboard.detent.test:4000",
			remoteAddr: "100.95.107.51:49152",
			accept:     "text/event-stream",
			referer:    "http://dashboard.detent.test:4000/",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequest(tt.method, "http://"+tt.host+"/api/v1/board/activity/events", nil)
			req.Host = tt.host
			req.RemoteAddr = tt.remoteAddr
			req.Header.Set("Accept", tt.accept)
			req.Header.Set("Referer", tt.referer)
			ctx := echo.New().NewContext(req, httptest.NewRecorder())
			if got := authorizeDashboardSSE(ctx); got != tt.want {
				t.Fatalf("authorizeDashboardSSE() = %t, want %t", got, tt.want)
			}
		})
	}
}
