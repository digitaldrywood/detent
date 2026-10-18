package hubserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/billing"
)

func TestHostedBillingCheckoutFailures(t *testing.T) {
	for _, test := range []struct {
		name, path string
		status     int
		transport  bool
		pending    bool
		wantStatus int
	}{
		{name: "rejected", path: "/v1/checkout/sessions", status: http.StatusBadRequest, wantStatus: http.StatusBadGateway},
		{name: "unauthorized", path: "/v1/checkout/sessions", status: http.StatusUnauthorized, wantStatus: http.StatusBadGateway},
		{name: "rate limited", path: "/v1/checkout/sessions", status: http.StatusTooManyRequests, wantStatus: http.StatusBadGateway},
		{name: "server failure", path: "/v1/checkout/sessions", status: http.StatusServiceUnavailable, pending: true, wantStatus: http.StatusServiceUnavailable},
		{name: "transport failure", path: "/v1/checkout/sessions", transport: true, pending: true, wantStatus: http.StatusServiceUnavailable},
		{name: "price rejected before session request", path: "/v1/prices/price_fixture", status: http.StatusBadRequest, pending: true, wantStatus: http.StatusBadGateway},
		{name: "account rejected during reconciliation", path: "/v1/account", status: http.StatusBadRequest, wantStatus: http.StatusBadGateway},
	} {
		for _, api := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/api=%t", test.name, api), func(t *testing.T) {
				f, _ := newHostedBillingFixture(t)
				var logs strings.Builder
				f.service.config.Logger = slog.New(hostedLogHandler{output: slog.NewJSONHandler(&logs, nil)})
				failed := true
				var keys, expirations []string
				provider, err := billing.NewStripe(billing.StripeConfig{APIKey: "sk_test_fixture_2196", Client: &http.Client{Transport: hostedBillingStripeTransport(func(request *http.Request) (*http.Response, error) {
					if request.URL.Host != "api.stripe.com" {
						t.Fatalf("unexpected provider host: %s", request.URL.Host)
					}
					var body string
					status := http.StatusOK
					if request.URL.Path == "/v1/checkout/sessions" {
						if err := request.ParseForm(); err != nil {
							t.Fatal(err)
						}
						keys = append(keys, request.Header.Get("Idempotency-Key"))
						expirations = append(expirations, request.Form.Get("expires_at"))
					}
					if failed && request.URL.Path == test.path {
						if test.transport {
							return nil, errors.New("fixture network response loss")
						}
						status = test.status
						body = `{"error":{"type":"invalid_request_error","code":"parameter_missing","param":"tax_code","message":"the product tax code is missing sk_test_fixture_2196","customer":"customer-content-sentinel"},"secret":"secret-sentinel"}`
					} else {
						switch request.URL.Path {
						case "/v1/account":
							body = `{"id":"acct_fixture"}`
						case "/v1/customers/cus_fixture":
							body = `{"id":"cus_fixture","livemode":false,"metadata":{"detent_organization_id":"org_browser_preview"}}`
						case "/v1/subscriptions":
							body = `{"has_more":false,"data":[]}`
						case "/v1/prices/price_fixture":
							body = `{"id":"price_fixture","livemode":false,"active":true,"type":"recurring","billing_scheme":"per_unit","recurring":{"usage_type":"licensed"}}`
						case "/v1/checkout/sessions":
							body = fmt.Sprintf(`{"id":"cs_test_fixture","customer":"cus_fixture","livemode":false,"url":"https://checkout.stripe.com/c/pay/test_fixture","expires_at":%s}`, request.Form.Get("expires_at"))
						default:
							t.Fatalf("unexpected Stripe path: %s", request.URL.Path)
						}
					}
					return &http.Response{StatusCode: status, Header: http.Header{"Request-Id": {"req_fixture"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
				})}})
				if err != nil {
					t.Fatal(err)
				}
				f.service.config.Hosted.Billing.Provider = provider
				checkout := func() *httptest.ResponseRecorder {
					if api {
						return f.billingAPI(t, "owner", http.MethodPost, "/billing/checkout", `{"price":"price_fixture","idempotency_key":"checkout_failure"}`)
					}
					return f.form(t, "owner", "/organization/billing/checkout", url.Values{"price": {"price_fixture"}})
				}
				response := checkout()
				requireNativeStatus(t, response, test.wantStatus)
				if test.wantStatus != http.StatusServiceUnavailable && (strings.Contains(response.Body.String(), "temporarily unavailable") || strings.Contains(response.Body.String(), "retry the same purchase")) {
					t.Fatalf("rejected checkout encouraged same-key retry: %s", response.Body.String())
				}
				if !test.transport {
					step := "create billing checkout"
					if test.path == "/v1/account" {
						step = "reconcile billing"
					}
					for _, detail := range []string{`"level":"WARN"`, `"stripe_type":"invalid_request_error"`, `"stripe_code":"parameter_missing"`, `"stripe_param":"tax_code"`, `"stripe_message":"the product tax code is missing [redacted]"`, `"stripe_request_id":"req_fixture"`, step} {
						if !strings.Contains(logs.String(), detail) {
							t.Fatalf("missing %q in logs: %s", detail, logs.String())
						}
					}
					if strings.Count(logs.String(), "req_fixture") != 1 {
						t.Fatalf("provider failure was logged more than once: %s", logs.String())
					}
				}
				for _, private := range []string{"sk_test_fixture_2196", "secret-sentinel", "customer-content-sentinel", "the product tax code is missing", "req_fixture"} {
					if strings.Contains(response.Body.String(), private) {
						t.Fatalf("checkout response exposed %q", private)
					}
					if private != "the product tax code is missing" && private != "req_fixture" && strings.Contains(logs.String(), private) {
						t.Fatalf("checkout logs exposed %q", private)
					}
				}
				view := f.billingAPI(t, "owner", http.MethodGet, "/billing?view=usage", "")
				requireNativeStatus(t, view, http.StatusOK)
				if !strings.Contains(view.Body.String(), `"checkout_pending":`+strconv.FormatBool(test.pending)) || !strings.Contains(view.Body.String(), `"can_checkout":true`) {
					t.Fatalf("plan controls after failure: %s", view.Body.String())
				}
				var before, after hostedCheckout
				readIntent := func(intent *hostedCheckout) {
					var raw string
					if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT checkout_json FROM hosted_billing_accounts WHERE organization_id=?", "org_browser_preview").Scan(&raw); err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal([]byte(raw), intent); err != nil {
						t.Fatal(err)
					}
				}
				readIntent(&before)
				failed = false
				want := http.StatusOK
				if !api {
					want = http.StatusSeeOther
				}
				requireNativeStatus(t, checkout(), want)
				readIntent(&after)
				if test.pending {
					if before.Key == "" || before.Key != after.Key || !before.ExpiresAt.Equal(after.ExpiresAt) {
						t.Fatalf("ambiguous retry changed intent: before=%+v after=%+v", before, after)
					}
				} else if before.Key != "" || test.path == "/v1/checkout/sessions" && (len(keys) != 2 || keys[0] == "" || keys[0] == keys[1]) {
					t.Fatalf("rejected key reused: keys=%v intent=%+v", keys, before)
				}
				if test.path == "/v1/checkout/sessions" && (len(keys) != 2 || (keys[0] == keys[1]) != test.pending || test.pending && expirations[0] != expirations[1]) {
					t.Fatalf("retry identity: keys=%v expirations=%v", keys, expirations)
				}
				requireNativeStatus(t, checkout(), want)
				wantCalls := 2
				if test.path != "/v1/checkout/sessions" {
					wantCalls = 1
				}
				if len(keys) != wantCalls {
					t.Fatal("completed checkout created a duplicate session")
				}
			})
		}
	}
}
