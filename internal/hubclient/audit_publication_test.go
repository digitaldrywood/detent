package hubclient

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestAuditPublicationRetainsSafeFailureMeasurements(t *testing.T) {
	for _, test := range []struct {
		name     string
		status   int
		response string
		field    string
	}{
		{"pass", 200, `{}`, ""},
		{"payload rejected before decoding", 413, `private-audit-secret`, ""},
		{"audit authority rejection", 422, `{"code":"invalid_request","message":"private-audit-secret","details":{"rejection":{"category":"security_audit","field":"session_id"}}}`, "session_id"},
		{"session conflict", 409, `{"code":"conflict","message":"private-audit-secret"}`, ""},
		{"server failure", 503, `{"code":"unavailable","message":"private-audit-secret"}`, ""},
		{"invalid success response", 200, `private-audit-secret`, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			var received int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				payload, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				received = int64(len(payload))
				var request tracker.ReviewChange
				if err := json.Unmarshal(payload, &request); err != nil || request.SecurityAudit == nil || request.SecurityAudit.SessionID != 6102 || request.SecurityAudit.Verdict != "pass" {
					t.Errorf("audit identity changed: %+v, %v", request.SecurityAudit, err)
				}
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.response)
			}))
			defer server.Close()
			client, err := New(Config{URL: server.URL, TokenSource: func() string { return "private-auth-secret" }, HTTPClient: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			native, err := client.Native("org_example", "prj_example")
			if err != nil {
				t.Fatal(err)
			}
			request := tracker.ReviewChange{Decision: "approved", Body: "private-audit-secret é", SecurityAudit: &tracker.SecurityAuditReview{SessionID: 6102, Verdict: "pass", Run: securityaudit.Run{ID: 6102, Verdict: "pass", Summary: "private-audit-secret", OutputBytes: 123}}}
			_, err = native.ReviewChange(t.Context(), "wi_example", "change_example", "version_example", request)
			if test.name == "pass" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var carrier interface {
				NativeTerminalFailure() tracker.NativeTerminalFailure
			}
			if !errors.As(err, &carrier) {
				t.Fatalf("missing typed failure: %v", err)
			}
			failure := carrier.NativeTerminalFailure().Public()
			want := tracker.PublicationFailure{Operation: "security_audit/review", Code: "http_rejection", HTTPStatus: test.status, RequestBytes: received, BodyBytes: 23, OutputBytes: 123}
			if test.status == 200 {
				want.Code, want.HTTPStatus = "request_failed", 0
			}
			if failure.Publication == nil || *failure.Publication != want || failure.Stage != "security_audit_publication" {
				t.Fatalf("failure = %+v publication=%+v; want %+v", failure, failure.Publication, want)
			}
			if test.field != "" && (failure.Rejection == nil || failure.Rejection.Field != test.field) {
				t.Fatalf("lost audit authority rejection: %+v", failure)
			}
			raw, err := json.Marshal(failure)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), "private-") {
				t.Fatalf("private content in receipt: %s", raw)
			}
			var replay tracker.NativeTerminalFailure
			if err := json.Unmarshal(raw, &replay); err != nil {
				t.Fatal(err)
			}
			if replay = replay.Public(); replay.Publication == nil || *replay.Publication != want {
				t.Fatalf("receipt lost measurements: %+v", replay)
			}
		})
	}
}
