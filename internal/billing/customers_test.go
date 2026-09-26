package billing

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type customerFixture struct {
	mu       sync.Mutex
	live     bool
	account  string
	search   string
	created  string
	creates  int
	keys     []string
	metadata []string
}

func newCustomerProvider(t *testing.T, mode string, f *customerFixture) CustomerProvider {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		live := strconv.FormatBool(f.live)
		switch {
		case r.URL.Path == "/v1/account":
			fmt.Fprintf(w, `{"id":%q}`, f.account)
		case r.URL.Path == "/v1/customers/search":
			if r.URL.Query().Get("query") != "metadata['detent_organization_id']:'org_a'" {
				t.Errorf("customer search query = %q", r.URL.Query().Get("query"))
			}
			fmt.Fprintf(w, `{"data":[%s]}`, strings.ReplaceAll(f.search, "LIVE", live))
		case r.URL.Path == "/v1/customers" && r.Method == http.MethodPost:
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			f.creates++
			f.keys = append(f.keys, r.Header.Get("Idempotency-Key"))
			f.metadata = append(f.metadata, r.PostForm.Get("metadata[detent_organization_id]"))
			fmt.Fprint(w, strings.ReplaceAll(f.created, "LIVE", live))
		case r.URL.Path == "/v1/customers/cus_a":
			fmt.Fprint(w, strings.ReplaceAll(`{"id":"cus_a","livemode":LIVE,"metadata":{"detent_organization_id":"org_a"}}`, "LIVE", live))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	base, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(transport.CloseIdleConnections)
	provider, err := NewStripe(StripeConfig{APIKey: "sk_" + mode + "_fixture_2343", Mode: mode, Client: &http.Client{Transport: stripeFixtureTransport{t: t, base: base, client: transport}}})
	if err != nil {
		t.Fatal(err)
	}
	return provider.(CustomerProvider)
}

func TestNewStripeModes(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		mode, key string
		ok        bool
	}{
		{"", "sk_test_0123456789", true},
		{"test", "rk_test_0123456789", true},
		{"live", "sk_live_0123456789", true},
		{"live", "sk_test_0123456789", false},
		{"test", "sk_live_0123456789", false},
		{"production", "sk_live_0123456789", false},
		{"live", "sk_live_", false},
	} {
		if _, err := NewStripe(StripeConfig{APIKey: test.key, Mode: test.mode}); (err == nil) != test.ok {
			t.Errorf("NewStripe(%q, %q) error = %v, want ok %v", test.mode, test.key, err, test.ok)
		}
	}
}

func TestEnsureCustomer(t *testing.T) {
	t.Parallel()
	valid := `{"id":"cus_a","livemode":LIVE,"metadata":{"detent_organization_id":"org_a","detent_creation_key":"detent-customer-acct_a-test-org_a"}}`
	request := CustomerRequest{AccountID: "acct_a", OrganizationID: "org_a", IdempotencyKey: "detent-customer-acct_a-test-org_a"}
	for _, test := range []struct {
		name      string
		mode      string
		fixture   *customerFixture
		want      string
		creates   int
		wantError bool
	}{
		{name: "creates once with a stable key", mode: "test", fixture: &customerFixture{account: "acct_a", created: valid}, want: "cus_a", creates: 1},
		{name: "recovers an earlier creation", mode: "test", fixture: &customerFixture{account: "acct_a", search: valid}, want: "cus_a"},
		{name: "live mode", mode: "live", fixture: &customerFixture{live: true, account: "acct_a", created: valid}, want: "cus_a", creates: 1},
		{name: "test object in live mode", mode: "live", fixture: &customerFixture{account: "acct_a", created: valid}, creates: 1, wantError: true},
		{name: "duplicate customers", mode: "test", fixture: &customerFixture{account: "acct_a", search: valid + "," + strings.Replace(valid, "cus_a", "cus_b", 1)}, wantError: true},
		{name: "other organization metadata", mode: "test", fixture: &customerFixture{account: "acct_a", search: strings.Replace(valid, `"org_a"`, `"org_b"`, 1)}, wantError: true},
		{name: "metadata without creation proof", mode: "test", fixture: &customerFixture{account: "acct_a", search: `{"id":"cus_a","livemode":false,"metadata":{"detent_organization_id":"org_a"}}`}, wantError: true},
		{name: "wrong account", mode: "test", fixture: &customerFixture{account: "acct_other", created: valid}, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := test.fixture
			provider := newCustomerProvider(t, test.mode, fixture)
			got, err := provider.EnsureCustomer(t.Context(), request)
			if (err != nil) != test.wantError || got != test.want && !test.wantError {
				t.Fatalf("EnsureCustomer = %q, %v", got, err)
			}
			if fixture.creates != test.creates {
				t.Fatalf("creates = %d, want %d", fixture.creates, test.creates)
			}
			for i := range fixture.keys {
				if fixture.keys[i] != request.IdempotencyKey || fixture.metadata[i] != "org_a" {
					t.Fatalf("create used key %q metadata %q", fixture.keys[i], fixture.metadata[i])
				}
			}
		})
	}
	if _, err := newCustomerProvider(t, "test", &customerFixture{account: "acct_a"}).EnsureCustomer(t.Context(), CustomerRequest{AccountID: "acct_a", OrganizationID: "tenant"}); err == nil {
		t.Fatal("invalid request accepted")
	}
}

func TestCustomerOrganization(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		mode string
		live bool
		want string
	}{{"test", false, "org_a"}, {"live", true, "org_a"}, {"live", false, ""}, {"test", true, ""}} {
		provider := newCustomerProvider(t, test.mode, &customerFixture{live: test.live})
		got, err := provider.CustomerOrganization(t.Context(), "cus_a")
		if got != test.want || (err == nil) != (test.want != "") {
			t.Errorf("mode %s livemode %v = %q, %v", test.mode, test.live, got, err)
		}
	}
}

func TestVerifyModeEvent(t *testing.T) {
	t.Parallel()
	secret := []byte("whsec_fixture_secret_value")
	now := time.Unix(1_800_000_000, 0)
	sign := func(body string) string {
		mac := hmac.New(sha256.New, secret)
		mac.Write([]byte(strconv.FormatInt(now.Unix(), 10) + "." + body))
		return "t=" + strconv.FormatInt(now.Unix(), 10) + ",v1=" + hex.EncodeToString(mac.Sum(nil))
	}
	for _, test := range []struct {
		name     string
		body     string
		live     bool
		customer string
		ok       bool
	}{
		{"test subscription", `{"id":"evt_1","type":"customer.subscription.updated","livemode":false,"data":{"object":{"id":"sub_1","object":"subscription","customer":"cus_a"}}}`, false, "cus_a", true},
		{"live event on test endpoint", `{"id":"evt_2","type":"invoice.paid","livemode":true,"data":{"object":{"id":"in_1","object":"invoice","customer":"cus_a"}}}`, false, "", false},
		{"live event on live endpoint", `{"id":"evt_3","type":"invoice.paid","livemode":true,"data":{"object":{"id":"in_1","object":"invoice","customer":"cus_a"}}}`, true, "cus_a", true},
		{"test event on live endpoint", `{"id":"evt_4","type":"invoice.paid","livemode":false,"data":{"object":{"customer":"cus_a"}}}`, true, "", false},
		{"customer object", `{"id":"evt_5","type":"customer.updated","livemode":false,"data":{"object":{"id":"cus_z","object":"customer"}}}`, false, "cus_z", true},
		{"expanded customer ignored", `{"id":"evt_6","type":"invoice.paid","livemode":false,"data":{"object":{"customer":{"id":"cus_a"}}}}`, false, "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			event, err := VerifyModeEvent([]byte(test.body), sign(test.body), secret, now, test.live)
			if (err == nil) != test.ok || event.Customer != test.customer {
				t.Fatalf("event = %+v, %v", event, err)
			}
		})
	}
}

func TestSessionPrefixFollowsMode(t *testing.T) {
	t.Parallel()
	if (&stripeProvider{}).sessionPrefix() != "cs_test_" || (&stripeProvider{live: true}).sessionPrefix() != "cs_live_" {
		t.Fatal("checkout session prefix does not follow the configured mode")
	}
}
