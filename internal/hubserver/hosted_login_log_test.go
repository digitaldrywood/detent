package hubserver

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

type hostedLogBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *hostedLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

func (b *hostedLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

func TestHostedLoginDenialLogging(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		wantStatus int
		wantReason string
		wantFields map[string]any
	}{
		{name: "wrong state", wantStatus: http.StatusUnauthorized, wantReason: "state_mismatch"},
		{name: "missing cookie", wantStatus: http.StatusUnauthorized, wantReason: "transaction_missing"},
		{name: "missing verifier", wantStatus: http.StatusUnauthorized, wantReason: "pkce_missing"},
		{name: "provider error callback", wantStatus: http.StatusUnauthorized, wantReason: "provider_error", wantFields: map[string]any{"provider_error": "access_denied"}},
		{name: "issuer mismatch", wantStatus: http.StatusUnauthorized, wantReason: "issuer_mismatch", wantFields: map[string]any{"token_issuer": "https://api.workos.com", "flow": "login_callback"}},
		{name: "exchange failed", wantStatus: http.StatusUnauthorized, wantReason: "exchange_failed", wantFields: map[string]any{"provider_status": float64(http.StatusBadRequest)}},
		{name: "wrong organization", wantStatus: http.StatusForbidden, wantReason: "organization_mismatch", wantFields: map[string]any{"email_domain": "example.test"}},
		{name: "unverified email", wantStatus: http.StatusUnauthorized, wantReason: "email_unverified"},
		{name: "support without start", wantStatus: http.StatusForbidden, wantReason: "support_actor_invalid"},
		{name: "invitation missing token", wantStatus: http.StatusForbidden, wantReason: "invitation_invalid", wantFields: map[string]any{"flow": "invitation_start"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var output hostedLogBuffer
			p := newHostedLoginProvider()
			cfg := hostedLoginConfig(t, p, true)
			cfg.Logger = slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))
			s := openTestService(t, cfg)
			transaction, state := hostedLoginStart(t, s, false)
			query := url.Values{"code": {"callback-code-sentinel"}, "state": {state}}
			cookies := []*http.Cookie{transaction}
			target := "/auth/oidc/callback?"
			switch tt.name {
			case "wrong state":
				query.Set("state", "another_browser")
			case "missing cookie":
				cookies = nil
			case "missing verifier":
				hostedLoginExec(t, s, "UPDATE hosted_transactions SET verifier = ''")
			case "provider error callback":
				query.Set("error", "access_denied")
			case "issuer mismatch":
				p.exchangeErr = &auth.HostedIdentityError{Reason: auth.HostedReasonIssuerMismatch, TokenIssuer: "https://api.workos.com"}
			case "exchange failed":
				p.exchangeErr = &auth.HostedIdentityError{Reason: auth.HostedReasonExchangeFailed, Status: http.StatusBadRequest}
			case "wrong organization":
				p.identity.Hosted.OrganizationID = "org_other_provider"
			case "unverified email":
				p.identity.EmailVerified = false
			case "support without start":
				p.identity.Hosted.SupportActor, p.identity.Hosted.SupportReason = "support@example.test", "troubleshooting"
			case "invitation missing token":
				target, query = "/invite?", url.Values{}
			}
			recorder := hostedLoginRequest(s, http.MethodGet, target+query.Encode(), "", nil, false, cookies...)
			if recorder.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", recorder.Code, tt.wantStatus)
			}
			logged := output.String()
			var denial map[string]any
			for line := range strings.SplitSeq(strings.TrimSpace(logged), "\n") {
				var record map[string]any
				if err := json.Unmarshal([]byte(line), &record); err != nil {
					t.Fatalf("log line is not JSON: %q", line)
				}
				if record["msg"] == "hosted sign-in denied" {
					if denial != nil {
						t.Fatalf("multiple denial records:\n%s", logged)
					}
					denial = record
				}
			}
			if denial == nil || denial["level"] != "WARN" || denial["reason"] != tt.wantReason {
				t.Fatalf("denial record = %v, want reason %s:\n%s", denial, tt.wantReason, logged)
			}
			if id, _ := denial["request_id"].(string); id == "" || recorder.Header().Get("X-Request-Id") != id {
				t.Fatalf("request_id = %v, header = %q", denial["request_id"], recorder.Header().Get("X-Request-Id"))
			}
			for key, want := range tt.wantFields {
				if denial[key] != want {
					t.Errorf("%s = %v, want %v", key, denial[key], want)
				}
			}
			for _, secret := range []string{"callback-code-sentinel", state, transaction.Value, "customer@example.test", "support@example.test"} {
				if strings.Contains(logged, secret) {
					t.Errorf("log exposed %q:\n%s", secret, logged)
				}
			}
		})
	}
}

func TestHostedLoginLogsStayRedactedOutsideDenials(t *testing.T) {
	t.Parallel()
	var output hostedLogBuffer
	cfg := hostedLoginConfig(t, newHostedLoginProvider(), true)
	cfg.Logger = slog.New(slog.NewJSONHandler(&output, nil))
	s := openTestService(t, cfg)
	s.config.Logger.Warn("tenant content sentinel", "secret", "attribute-sentinel", "at", time.Now())
	logged := output.String()
	if strings.Contains(logged, "tenant content sentinel") || strings.Contains(logged, "attribute-sentinel") || !strings.Contains(logged, "hosted service event") {
		t.Fatalf("hosted service log was not redacted:\n%s", logged)
	}

	for _, tt := range []struct {
		name  string
		attrs []any
		want  map[string]any
	}{
		{"diagnostic fields kept", []any{"tool", operatortool.FileIssue, "correlation_id", "6f1c1c55-6c0d-4b8e-9a43-0d6c2b7a9e10", "error_class", "sqlite_5", "error", "tenant-error-sentinel"}, map[string]any{"tool": operatortool.FileIssue, "correlation_id": "6f1c1c55-6c0d-4b8e-9a43-0d6c2b7a9e10", "error_class": "sqlite_5"}},
		{"unknown tool dropped", []any{"tool", "tenant-tool-sentinel", "error_class", "sqlite_5"}, map[string]any{"error_class": "sqlite_5"}},
		{"free text class dropped", []any{"error_class", "tenant error sentinel", "correlation_id", "tenant-correlation-sentinel"}, map[string]any{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var output hostedLogBuffer
			logger := slog.New(hostedLogHandler{output: slog.NewJSONHandler(&output, nil)})
			logger.Error("operator tool failed", tt.attrs...)
			var record map[string]any
			if err := json.Unmarshal([]byte(output.String()), &record); err != nil {
				t.Fatalf("log = %s: %v", output.String(), err)
			}
			delete(record, "time")
			delete(record, "level")
			if record["msg"] != "hosted service event" {
				t.Fatalf("msg = %v", record["msg"])
			}
			delete(record, "msg")
			if !reflect.DeepEqual(record, tt.want) {
				t.Fatalf("attrs = %v, want %v", record, tt.want)
			}
		})
	}
}

func TestHostedLogsKeepWriterTiming(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		message string
		attrs   []any
		want    map[string]any
	}{
		{"request timing kept", "hub request timing", []any{"method", "POST", "route", "/api/v2/organizations/:organization/projects/:project/leases/:lease/renew", "status", 200, "duration_ms", int64(5200), "writer_hold_ms", int64(4900), "writer_txs", int64(2)},
			map[string]any{"msg": "hub request timing", "method": "POST", "route": "/api/v2/organizations/:organization/projects/:project/leases/:lease/renew", "status": float64(200), "duration_ms": float64(5200), "writer_hold_ms": float64(4900), "writer_txs": float64(2)}},
		{"writer waits kept", "hub writer waits", []any{"waits", int64(40), "wait_ms", int64(9000), "in_use", 1, "interval_s", int64(60)},
			map[string]any{"msg": "hub writer waits", "waits": float64(40), "wait_ms": float64(9000), "in_use": float64(1), "interval_s": float64(60)}},
		{"tenant content dropped from timing", "hub request timing", []any{"route", "/work/tenant title sentinel", "method", "tenant method sentinel", "secret", "attribute-sentinel", "duration_ms", "9"},
			map[string]any{"msg": "hub request timing"}},
		{"other messages stay redacted", "lease renewed", []any{"duration_ms", int64(10), "route", "/api/v2/x"},
			map[string]any{"msg": "hosted service event"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var output hostedLogBuffer
			logger := slog.New(hostedLogHandler{output: slog.NewJSONHandler(&output, nil)})
			logger.Info(tt.message, tt.attrs...)
			var record map[string]any
			if err := json.Unmarshal([]byte(output.String()), &record); err != nil {
				t.Fatalf("log = %s: %v", output.String(), err)
			}
			delete(record, "time")
			delete(record, "level")
			if !reflect.DeepEqual(record, tt.want) {
				t.Fatalf("record = %v, want %v", record, tt.want)
			}
		})
	}
}
