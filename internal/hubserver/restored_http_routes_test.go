package hubserver

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func newLocalHTTPService(t *testing.T) *Service {
	t.Helper()
	admin := postgresStartupTemplate.Database(t)
	cfg := startupConfig(startupConnection(t, admin, "detent_runtime_v2", ""))
	cfg.Hosted = nil
	cfg.InitialAdminToken = []byte(testHubAdminToken)
	cfg.SecretKeys = secretTestKeys(t, "1", "1")
	cfg.Logger = discardLogger()
	cfg.Content = (&changeObjectFixture{}).call
	service, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	projectRecordDatabases.Store(service.database.postgres, admin)
	t.Cleanup(func() { projectRecordDatabases.Delete(service.database.postgres) })
	organization := service.accessOrganization()
	var identity accessstore.HubIdentity
	if err := service.accessRead(t.Context(), organization, func(r tenantstore.Reader) error {
		var err error
		identity, err = r.HubIdentity(t.Context())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	envelope, err := cfg.SecretKeys.Seal(identity.CursorKey, accessstore.SecretAAD(organization, "", accessstore.NativeCursorKeyKind))
	if err != nil {
		t.Fatal(err)
	}
	if err := service.accessWrite(t.Context(), organization, func(w tenantstore.Writer) error {
		return w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: organization, Kind: accessstore.NativeCursorKeyKind, Envelope: envelope, UpdatedAt: service.config.now()}, accessstore.SecretAudit{OrganizationID: organization, Kind: accessstore.NativeCursorKeyKind, Actor: "fixture", Event: "import", KeyVersion: envelope.Version, RecordedAt: service.config.now()})
	}); err != nil {
		t.Fatal(err)
	}
	return service
}

func TestRestoredOrganizationHTTPRoutes(t *testing.T) {
	for _, mode := range []string{"local", "hosted", "shared"} {
		t.Run(mode, func(t *testing.T) {
			var service *Service
			var base, project string
			var request func(*testing.T, bool, string, string, string) *httptest.ResponseRecorder
			var internalRequest func(*testing.T, bool, string) *httptest.ResponseRecorder
			switch mode {
			case "local":
				f := newNativeFixture(t, newLocalHTTPService(t), "", "Example project")
				service, project = f.service, f.base
				base = "/api/v2/organizations/" + string(f.project.OrganizationID)
				request = func(t *testing.T, authenticated bool, method, path, body string) *httptest.ResponseRecorder {
					t.Helper()
					req := httptest.NewRequest(method, path, strings.NewReader(body))
					req.Header.Set("Content-Type", "application/json")
					if authenticated {
						req.SetBasicAuth("operator", testHubAdminToken)
						req.Header.Set("Origin", "http://example.com")
					}
					rec := httptest.NewRecorder()
					service.Handler().ServeHTTP(rec, req)
					return rec
				}
			case "hosted":
				f := newHostedSecurityFixture(t, func(cfg *Config) { cfg.SecretKeys = secretTestKeys(t, "1", "1") })
				owner := f.user(t, "owner", "owner", "owner@example.test", "write", "")
				service, project = f.service, f.base
				base = "/api/v2/organizations/org_security"
				request = func(t *testing.T, authenticated bool, method, path, body string) *httptest.ResponseRecorder {
					t.Helper()
					user := hostedSecurityUser{}
					if authenticated {
						user = owner
					}
					var input any
					if body != "" {
						input = json.RawMessage(body)
					}
					return f.request(t, user, method, path, input)
				}
			case "shared":
				f := newHostedSharedFixture(t, func(cfg *Config) { cfg.SecretKeys = secretTestKeys(t, "1", "1") })
				owner := f.member(t, "owner", "owner", "write")
				service, project = f.service, f.base
				base = "/api/v2/organizations/org_security"
				request = func(t *testing.T, authenticated bool, method, path, body string) *httptest.ResponseRecorder {
					t.Helper()
					if !strings.HasPrefix(path, "/api/v2/") {
						path = "/organizations/org_security" + path
					}
					req := hostedSharedRequest{method: method, target: path, body: body}
					if authenticated {
						req.user = &owner
						req.csrf = cloudassert.CSRFToken("shared-user_owner", "org_security")
					}
					return f.serve(t, req)
				}
				internalRequest = func(t *testing.T, authenticated bool, body string) *httptest.ResponseRecorder {
					t.Helper()
					return f.serve(t, hostedSharedRequest{kind: cloudassert.KindService, method: http.MethodPost, target: "/internal/v1/issue-notifications/unsubscribe", body: body, unsigned: !authenticated})
				}
			}
			service.config.SlackHTTPClient = &http.Client{Transport: spritesTestTransport(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
			})}
			if err := service.scaleSpritePool(t.Context(), nativeScope{organization: service.accessOrganization()}, true); err != nil {
				t.Fatalf("unconfigured controller authority: %v", err)
			}
			for _, test := range []struct{ method, path, body string }{
				{http.MethodGet, base + "/integrations/slack", ""},
				{http.MethodPut, base + "/integrations/slack", `{"webhook":"https://hooks.slack.com/services/T/B/EXAMPLE","channel_name":"#example"}`},
				{http.MethodPost, base + "/integrations/slack/test", `{}`},
				{http.MethodGet, base + "/sprite-pool", ""},
				{http.MethodPut, base + "/sprite-pool", `{"revision":0,"min_runners":0,"max_runners":0}`},
				{http.MethodGet, project + "/sprite-pool", ""},
				{http.MethodPut, project + "/sprite-pool", `{"revision":1,"min_runners":0,"max_runners":0}`},
				{http.MethodGet, project + "/sprite-pool/allocation", ""},
				{http.MethodPut, project + "/sprite-pool/allocation", `{"idempotency_key":"allocation","expected_revision":"0","desired_count":0}`},
			} {
				t.Run(test.method+" "+test.path, func(t *testing.T) {
					denied := request(t, false, test.method, test.path, test.body)
					if denied.Code != http.StatusUnauthorized && denied.Code != http.StatusForbidden {
						t.Fatalf("unauthenticated status=%d body=%s", denied.Code, denied.Body.String())
					}
					requireNativeStatus(t, request(t, true, test.method, test.path, test.body), http.StatusOK)
				})
			}
			if mode != "local" {
				for _, method := range []string{http.MethodGet, http.MethodPost} {
					if method == http.MethodPost && mode != "shared" {
						continue
					}
					t.Run(method+" issue unsubscribe", func(t *testing.T) {
						body, err := json.Marshal(tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "unsubscribe-" + method}, Title: "Example issue", Body: nativeContractTestBody, State: "Todo"})
						if err != nil {
							t.Fatal(err)
						}
						created := request(t, true, http.MethodPost, project+"/work-items", string(body))
						requireNativeStatus(t, created, http.StatusOK)
						var issue tracker.NativeIssue
						decodeHubResponse(t, created, &issue)
						token := hubapi.NewID("unsub")
						admin := hostedPostgresAdmin(t, service)
						if _, err := admin.ExecContext(t.Context(), `INSERT INTO issue_subscriptions(organization_id,work_item_id,user_id,token) VALUES($1,$2,'user_owner',$3)`, service.accessOrganization(), issue.WorkItemID, token); err != nil {
							t.Fatal(err)
						}
						if method == http.MethodGet {
							shared := mode == "shared"
							if shared {
								requireNativeStatus(t, request(t, false, method, "/issue-notifications/unsubscribe?token="+token, ""), http.StatusUnauthorized)
							}
							requireNativeStatus(t, request(t, shared, method, "/issue-notifications/unsubscribe?token=invalid", ""), http.StatusNotFound)
							requireNativeStatus(t, request(t, shared, method, "/issue-notifications/unsubscribe?token="+token, ""), http.StatusOK)
						} else {
							body := `{"token":"` + token + `"}`
							requireNativeStatus(t, internalRequest(t, false, body), http.StatusUnauthorized)
							requireNativeStatus(t, internalRequest(t, true, body), http.StatusNoContent)
						}
						var unsubscribed bool
						if err := admin.QueryRowContext(t.Context(), `SELECT unsubscribed FROM issue_subscriptions WHERE organization_id=$1 AND token=$2`, service.accessOrganization(), token).Scan(&unsubscribed); err != nil || !unsubscribed {
							t.Fatalf("unsubscribed=%v error=%v", unsubscribed, err)
						}
					})
				}
			}
			if mode == "shared" {
				for _, path := range []string{"/organization/plan", "/organization/billing", "/api/cloud/billing/subscription"} {
					t.Run(path, func(t *testing.T) {
						denied := request(t, false, http.MethodGet, path, "")
						if denied.Code != http.StatusUnauthorized && denied.Code != http.StatusSeeOther {
							t.Fatalf("unauthenticated status=%d body=%s", denied.Code, denied.Body.String())
						}
						requireNativeStatus(t, request(t, true, http.MethodGet, path, ""), http.StatusOK)
					})
				}
			}
			for _, test := range []struct{ method, path string }{
				{http.MethodPost, "/api/v1/tokens"},
				{http.MethodDelete, "/api/v1/tokens/example"},
				{http.MethodPost, "/api/v1/tokens/example/rotate"},
			} {
				t.Run("private token administration "+test.method+" "+test.path, func(t *testing.T) {
					requireNativeStatus(t, request(t, true, test.method, test.path, `{}`), http.StatusNotFound)
				})
			}
		})
	}
}
