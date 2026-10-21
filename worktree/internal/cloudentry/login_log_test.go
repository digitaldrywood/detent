package cloudentry

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
)

type logBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

func (b *logBuffer) take() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	value := b.buffer.String()
	b.buffer.Reset()
	return value
}

func deniedRecords(t *testing.T, output string) []map[string]any {
	t.Helper()
	var records []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(output), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		if record["msg"] == "hosted sign-in denied" {
			records = append(records, record)
		}
	}
	return records
}

func TestSharedEntryDenialLogging(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	var output logBuffer
	f := newEntryFixtureWithLogger(t, slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug})))
	tests := []struct {
		name       string
		run        func(t *testing.T) (page, []string)
		wantStatus int
		wantReason string
		wantFields map[string]any
	}{
		{
			name: "malformed state",
			run: func(t *testing.T) (page, []string) {
				browser := newBrowser(t, f.service.Handler())
				return browser.do(http.MethodGet, "/auth/oidc/callback?"+url.Values{"code": {"code-sentinel-1"}, "state": {"not-hex.state-sentinel"}}.Encode(), nil, nil), []string{"code-sentinel-1", "state-sentinel"}
			},
			wantStatus: http.StatusUnauthorized, wantReason: "state_invalid",
		},
		{
			name: "transaction cookie missing",
			run: func(t *testing.T) (page, []string) {
				owner := newBrowser(t, f.service.Handler())
				start, _ := owner.get("/auth/oidc/start")
				state := stateOf(t, start)
				other := newBrowser(t, f.service.Handler())
				return other.do(http.MethodGet, "/auth/oidc/callback?"+url.Values{"code": {"user_alice:"}, "state": {state}}.Encode(), nil, nil), []string{state}
			},
			wantStatus: http.StatusUnauthorized, wantReason: "transaction_cookie_missing",
		},
		{
			name: "state mismatch",
			run: func(t *testing.T) (page, []string) {
				browser := newBrowser(t, f.service.Handler())
				start, _ := browser.get("/auth/oidc/start")
				state := stateOf(t, start)
				id, _, _ := strings.Cut(state, ".")
				cookies := cookieValues(browser)
				return browser.do(http.MethodGet, "/auth/oidc/callback?"+url.Values{"code": {"user_alice:"}, "state": {id + ".tampered"}}.Encode(), nil, nil), append(cookies, state)
			},
			wantStatus: http.StatusUnauthorized, wantReason: "state_mismatch",
		},
		{
			name: "provider error",
			run: func(t *testing.T) (page, []string) {
				browser := newBrowser(t, f.service.Handler())
				start, _ := browser.get("/auth/oidc/start")
				state := stateOf(t, start)
				return browser.do(http.MethodGet, "/auth/oidc/callback?"+url.Values{"error": {"access_denied"}, "state": {state}}.Encode(), nil, nil), append(cookieValues(browser), state)
			},
			wantStatus: http.StatusUnauthorized, wantReason: "provider_error", wantFields: map[string]any{"provider_error": "access_denied"},
		},
		{
			name: "issuer mismatch from provider",
			run: func(t *testing.T) (page, []string) {
				browser := newBrowser(t, f.service.Handler())
				start, _ := browser.get("/auth/oidc/start")
				state := stateOf(t, start)
				return browser.do(http.MethodGet, "/auth/oidc/callback?"+url.Values{"code": {"deny:issuer_mismatch"}, "state": {state}}.Encode(), nil, nil), append(cookieValues(browser), state, "deny:issuer_mismatch")
			},
			wantStatus: http.StatusUnauthorized, wantReason: "issuer_mismatch", wantFields: map[string]any{"token_issuer": "https://api.workos.com", "flow": "login_callback"},
		},
		{
			name: "organization mismatch",
			run: func(t *testing.T) (page, []string) {
				browser := newBrowser(t, f.service.Handler())
				start, _ := browser.get("/auth/oidc/start?organization=org_alpha")
				state := stateOf(t, start)
				return browser.do(http.MethodGet, "/auth/oidc/callback?"+url.Values{"code": {"user_alice:porg_beta"}, "state": {state}}.Encode(), nil, nil), append(cookieValues(browser), state, "alice@example.test", "user_alice:porg_beta")
			},
			wantStatus: http.StatusForbidden, wantReason: "organization_mismatch", wantFields: map[string]any{"email_domain": "example.test"},
		},
		{
			name: "invitation invalid",
			run: func(t *testing.T) (page, []string) {
				browser := newBrowser(t, f.service.Handler())
				return browser.do(http.MethodGet, "/invite?invitation_token=inv_sentinel_unknown", nil, nil), []string{"inv_sentinel_unknown"}
			},
			wantStatus: http.StatusForbidden, wantReason: "invitation_invalid", wantFields: map[string]any{"flow": "invitation_start"},
		},
		{
			name: "support without session",
			run: func(t *testing.T) (page, []string) {
				browser := newBrowser(t, f.service.Handler())
				return browser.do(http.MethodPost, "/support/start", url.Values{"organization": {"org_alpha"}}, nil), nil
			},
			wantStatus: http.StatusForbidden, wantReason: "session_not_found", wantFields: map[string]any{"flow": "support_start"},
		},
		{
			name: "proxy request id is reused",
			run: func(t *testing.T) (page, []string) {
				browser := newBrowser(t, f.service.Handler())
				return browser.do(http.MethodGet, "/auth/oidc/callback?state=bad", nil, map[string]string{"X-Request-Id": "proxy-request-1"}), nil
			},
			wantStatus: http.StatusUnauthorized, wantReason: "state_invalid", wantFields: map[string]any{"request_id": "proxy-request-1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			output.take()
			response, secrets := tt.run(t)
			logged := output.take()
			if response.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d", response.StatusCode, tt.wantStatus)
			}
			records := deniedRecords(t, logged)
			if len(records) != 1 {
				t.Fatalf("denial records = %d, want 1:\n%s", len(records), logged)
			}
			record := records[0]
			if record["level"] != "WARN" || record["reason"] != tt.wantReason {
				t.Fatalf("record level = %v reason = %v, want WARN %s", record["level"], record["reason"], tt.wantReason)
			}
			requestID, _ := record["request_id"].(string)
			if requestID == "" || response.Header.Get("X-Request-Id") != requestID {
				t.Fatalf("request_id = %q, response header = %q", requestID, response.Header.Get("X-Request-Id"))
			}
			for key, want := range tt.wantFields {
				if record[key] != want {
					t.Errorf("%s = %v, want %v", key, record[key], want)
				}
			}
			for _, secret := range secrets {
				if secret != "" && strings.Contains(logged, secret) {
					t.Errorf("log exposed %q:\n%s", secret, logged)
				}
			}
		})
	}
}

func cookieValues(b *browser) []string {
	base, _ := url.Parse(testPublicURL)
	var values []string
	for _, cookie := range b.jar.Cookies(base) {
		values = append(values, cookie.Value)
	}
	return values
}
