package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/logging"
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
	cfg.Logger = slog.New(logging.NewHandler(&output, slog.LevelInfo, false, logging.SourceSetting{}))
	s := openTestService(t, cfg)
	s.config.Logger.Warn("hosted diagnostic", "title", "tenant content sentinel", "secret", "attribute-sentinel", "at", time.Now())
	logged := output.String()
	if strings.Contains(logged, "tenant content sentinel") || strings.Contains(logged, "attribute-sentinel") || !strings.Contains(logged, "hosted diagnostic") || !strings.Contains(logged, `"source":`) {
		t.Fatalf("hosted service log was not redacted: %s", logged)
	}

	failure := fmt.Errorf("get change: load version: %w", &json.UnmarshalTypeError{Value: "array", Type: reflect.TypeFor[string]()})
	for _, tt := range []struct {
		name  string
		attrs []any
		want  map[string]any
	}{
		{"diagnostic fields kept", []any{"tool", operatortool.GetChange, "correlation_id", "6f1c1c55-6c0d-4b8e-9a43-0d6c2b7a9e10", "err", failure, "organization_id", "org_test", "project_id", "prj_test", "issue_id", "wi_test", "duration", 3}, map[string]any{"tool": operatortool.GetChange, "correlation_id": "6f1c1c55-6c0d-4b8e-9a43-0d6c2b7a9e10", "error": failure.Error(), "error_class": "*json.UnmarshalTypeError", "organization_id": "org_test", "project_id": "prj_test", "issue_id": "wi_test", "duration": float64(3)}},
		{"unknown tool dropped", []any{"tool", "tenant-tool-sentinel", "error_class", "sqlite_5"}, map[string]any{"error_class": "sqlite_5"}},
		{"free text class dropped", []any{"error_class", "tenant error sentinel", "correlation_id", "tenant-correlation-sentinel"}, map[string]any{}},
		{"content and secrets dropped", []any{"issue_title", "tenant content sentinel", "body", "tenant content sentinel", "comment", "tenant content sentinel", "prompt", "tenant content sentinel", "conversation_text", "tenant content sentinel", "token", "secret-sentinel", "error", fmt.Errorf("load version: token=secret-sentinel: %w", context.DeadlineExceeded)}, map[string]any{"error": "load version: [redacted] context deadline exceeded", "error_class": "deadline_exceeded"}},
		{"nested content dropped", []any{slog.Group("resource", "id", "wi_test", "body", "tenant content sentinel"), "audit", map[string]any{"project_id": "prj_test", "title": "tenant content sentinel", "token": "secret-sentinel"}}, map[string]any{"resource": map[string]any{"id": "wi_test"}, "audit": map[string]any{"project_id": "prj_test"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var output hostedLogBuffer
			logger := slog.New(hostedLogHandler{output: logging.NewHandler(&output, slog.LevelInfo, false, logging.SourceSetting{})}).With("title", "tenant content sentinel", "secret", "attribute-sentinel", "component", "operator")
			logger.Error("operator tool failed", tt.attrs...)
			var record map[string]any
			if err := json.Unmarshal([]byte(output.String()), &record); err != nil {
				t.Fatal(err)
			}
			source, ok := record["source"].(map[string]any)
			if !ok || !strings.HasSuffix(source["file"].(string), "hosted_login_log_test.go") || source["line"].(float64) <= 0 {
				t.Fatal(output.String())
			}
			if record["msg"] != "operator tool failed" || record["component"] != "operator" {
				t.Fatal(output.String())
			}
			for _, key := range []string{"time", "level", "msg", "source", "component"} {
				delete(record, key)
			}
			if !reflect.DeepEqual(record, tt.want) || strings.Contains(output.String(), "sentinel") {
				t.Fatalf("attrs=%v want=%v: %s", record, tt.want, output.String())
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
		{"other messages keep safe diagnostics", "lease renewed", []any{"duration_ms", int64(10), "route", "/api/v2/x"},
			map[string]any{"msg": "lease renewed", "duration_ms": float64(10), "route": "/api/v2/x"}},
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
