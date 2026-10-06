package cloudentry

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver"
)

func TestSharedEntryGitHubWebhookUnavailable(t *testing.T) {
	t.Parallel()
	f := newEntryFixture(t)
	response := httptest.NewRecorder()
	f.service.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", strings.NewReader(`{}`)))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusServiceUnavailable, response.Body)
	}
}

func TestSharedEntryGitHubWebhookRouting(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name          string
		binding       string
		unsigned      bool
		redelivery    bool
		secondTenant  bool
		failedTenant  bool
		wrongSecret   bool
		afterMutation bool
		wantStatus    int
		wantAlpha     int
		wantBeta      int
	}{
		{name: "checkout binding", binding: "checkout", wantStatus: http.StatusAccepted, wantAlpha: 1},
		{name: "repository binding", binding: "repository", wantStatus: http.StatusAccepted, wantAlpha: 1},
		{name: "unsigned", binding: "checkout", unsigned: true, wantStatus: http.StatusUnauthorized},
		{name: "wrong signature", binding: "checkout", wrongSecret: true, wantStatus: http.StatusUnauthorized},
		{name: "unbound", wantStatus: http.StatusAccepted},
		{name: "redelivery", binding: "checkout", redelivery: true, wantStatus: http.StatusAccepted, wantAlpha: 1},
		{name: "two matching tenants", binding: "checkout", secondTenant: true, wantStatus: http.StatusAccepted, wantAlpha: 1, wantBeta: 1},
		{name: "failed matching tenant", binding: "checkout", failedTenant: true, wantStatus: http.StatusServiceUnavailable},
		{name: "partial tenant failure", binding: "checkout", secondTenant: true, failedTenant: true, wantStatus: http.StatusServiceUnavailable, wantBeta: 1},
		{name: "new binding after mutation", binding: "checkout", afterMutation: true, wantStatus: http.StatusAccepted, wantAlpha: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newEntryFixture(t)
			t.Cleanup(func() {
				for _, fixture := range f.fixtures {
					_ = fixture.service.Close()
				}
			})
			withDatabase := func(id string, operation func(*sql.DB)) {
				t.Helper()
				fixture := f.fixtures[id]
				if err := fixture.service.Close(); err != nil {
					t.Fatal(err)
				}
				db, err := sql.Open("sqlite", fixture.path+"?_pragma=foreign_keys(1)")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = db.Close() })
				operation(db)
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
				fixture.service, err = hubserver.Open(t.Context(), fixture.config)
				if err != nil {
					t.Fatal(err)
				}
				f.fixtures[id] = fixture
				f.tenants[testSocketEndpoint(strings.TrimPrefix(id, "org_")+".sock")] = fixture.service.Handler()
			}
			bind := func(id, binding string) {
				t.Helper()
				if binding == "" {
					return
				}
				withDatabase(id, func(db *sql.DB) {
					switch binding {
					case "repository":
						if _, err := db.ExecContext(t.Context(), `INSERT INTO repositories(github_node_id, github_owner, github_name, created_at, updated_at) VALUES ('R_repo', 'Acme', 'Orders', '2026-10-06T12:00:00Z', '2026-10-06T12:00:00Z')`); err != nil {
							t.Fatal(err)
						}
						if _, err := db.ExecContext(t.Context(), "DELETE FROM projects WHERE repository_id = (SELECT id FROM repositories WHERE github_node_id = 'R_repo')"); err != nil {
							t.Fatal(err)
						}
						if _, err := db.ExecContext(t.Context(), "UPDATE projects SET repository_id = (SELECT id FROM repositories WHERE github_node_id = 'R_repo') WHERE profile = 'native'"); err != nil {
							t.Fatal(err)
						}
					case "checkout":
						if _, err := db.ExecContext(t.Context(), "UPDATE projects SET checkout_repository = 'Acme/Orders' WHERE profile = 'native'"); err != nil {
							t.Fatal(err)
						}
					}
				})
			}
			if !test.afterMutation {
				bind("org_alpha", test.binding)
			}
			if test.secondTenant {
				bind("org_beta", "checkout")
			}
			cfg := f.service.config
			cfg.GitHubWebhookSecret = []byte("product-webhook-secret")
			calls := map[string]int{}
			bindingCalls := map[string]int{}
			cfg.transport = func(organization Organization) (http.RoundTripper, error) {
				handler := f.tenants[organization.Endpoint]
				return handlerTransport{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/internal/v1/github/repositories" {
						bindingCalls[organization.ID]++
					}
					if r.URL.Path == "/internal/v1/github/webhook" {
						calls[organization.ID]++
						if test.failedTenant && organization.ID == "org_alpha" {
							w.WriteHeader(http.StatusServiceUnavailable)
							return
						}
					}
					if r.URL.Path == "/api/v2/organizations/org_alpha/projects/prj_test/integration/repository" {
						bind("org_alpha", test.binding)
						w.WriteHeader(http.StatusOK)
						return
					}
					handler.ServeHTTP(w, r)
				})}, nil
			}
			if err := f.service.Close(); err != nil {
				t.Fatal(err)
			}
			service, err := Open(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = service.Close() })
			if test.afterMutation {
				request := httptest.NewRequest(http.MethodPost, "/api/v2/organizations/org_alpha/projects/prj_test/integration/repository", strings.NewReader(`{}`))
				request.Header.Set("Authorization", "Bearer "+testAdminKey)
				response := httptest.NewRecorder()
				service.Handler().ServeHTTP(response, request)
				if response.Code != http.StatusOK || bindingCalls["org_alpha"] != 2 || bindingCalls["org_beta"] != 1 {
					t.Fatalf("mutation = %d, binding reads = %v: %s", response.Code, bindingCalls, response.Body)
				}
			}
			payload := `{
 "action":"opened",
 "repository":{"node_id":"R_repo","name":"orders","full_name":"acme/orders","owner":{"login":"acme"}},
 "issue":{"node_id":"I_report","number":12,"title":"Hosted report","body":"Report body","html_url":"https://github.com/acme/orders/issues/12","state":"open","user":{"login":"reporter"},"labels":[],"assignees":[],"created_at":"2026-10-06T12:00:00Z","updated_at":"2026-10-06T12:00:00Z"}
}`
			attempts := 1
			if test.redelivery {
				attempts = 2
			}
			for attempt := range attempts {
				request := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", strings.NewReader(payload))
				request.Header.Set("X-GitHub-Delivery", "hosted-delivery")
				request.Header.Set("X-GitHub-Event", "issues")
				if !test.unsigned {
					secret := cfg.GitHubWebhookSecret
					if test.wrongSecret {
						secret = []byte("wrong-secret")
					}
					mac := hmac.New(sha256.New, secret)
					if _, err := mac.Write([]byte(payload)); err != nil {
						t.Fatal(err)
					}
					request.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
				}
				response := httptest.NewRecorder()
				service.Handler().ServeHTTP(response, request)
				if response.Code != test.wantStatus {
					t.Fatalf("status = %d, want %d: %s", response.Code, test.wantStatus, response.Body)
				}
				if test.redelivery {
					var receipt struct{ Duplicate bool }
					if err := json.Unmarshal(response.Body.Bytes(), &receipt); err != nil || receipt.Duplicate != (attempt == 1) {
						t.Fatalf("redelivery response = %s: %v", response.Body, err)
					}
				}
			}
			for id, want := range map[string]int{"org_alpha": test.wantAlpha, "org_beta": test.wantBeta} {
				var inbox, issues, redeliveries int
				withDatabase(id, func(db *sql.DB) {
					if err := db.QueryRowContext(t.Context(), `SELECT
 (SELECT count(*) FROM github_webhook_inbox),
 (SELECT count(*) FROM issues WHERE native_id IS NOT NULL),
 COALESCE((SELECT max(redelivery_count) FROM github_webhook_inbox), 0)`).Scan(&inbox, &issues, &redeliveries); err != nil {
						t.Fatal(err)
					}
				})
				if inbox != want || issues != want {
					t.Fatalf("%s inbox = %d, issues = %d, want %d", id, inbox, issues, want)
				}
				if want > 0 && (calls[id] != attempts || redeliveries != attempts-1) {
					t.Fatalf("%s calls = %d, redeliveries = %d, want %d, %d", id, calls[id], redeliveries, attempts, attempts-1)
				}
				if want == 0 && calls[id] != 0 && (!test.failedTenant || id != "org_alpha") {
					t.Fatalf("unrelated tenant %s received %d deliveries", id, calls[id])
				}
				wantBindingCalls := 1
				if test.afterMutation && id == "org_alpha" {
					wantBindingCalls++
				}
				if bindingCalls[id] != wantBindingCalls {
					t.Fatalf("delivery queried tenant bindings again: %v", bindingCalls)
				}
			}
		})
	}
}
