package web

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
)

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
