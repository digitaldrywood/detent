package hubserver

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHostedSecretStatusTenantPagesAndKeyChecks(t *testing.T) {
	f := newSecretPostgresFixture(t)
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		t.Run(string(organization), func(t *testing.T) {
			now := f.service.config.now()
			keys := secretTestKeys(t, "1", "1", "2")
			envelope, err := keys.Seal([]byte(spritesSecretSentinel), accessstore.SecretAAD(organization, "second-project", flySpritesToken))
			if err != nil {
				t.Fatal(err)
			}
			if err := f.repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: organization, ProviderID: "provider_" + string(organization)}); err != nil {
					return err
				}
				if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: "test_hub_admin", Name: "Example administrator", Hash: apikey.HashToken(testHubAdminToken), Scope: "admin", Kind: "service", KeyScope: "admin", ProjectAccess: "all", CreatedAt: now, UpdatedAt: now}); err != nil {
					return err
				}
				if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: "second-project", Name: "Second project", Profile: "native"}, States: nativeFixtureStates()}); err != nil {
					return err
				}
				return w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: organization, ProjectID: "second-project", Kind: flySpritesToken, Envelope: envelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: organization, ProjectID: "second-project", Kind: flySpritesToken, Actor: "fixture", Event: "set", KeyVersion: 1, RecordedAt: now})
			}); err != nil {
				t.Fatal(err)
			}
			service := openTestService(t, Config{TenantRepository: f.repository, SecretKeys: keys, Hosted: &HostedConfig{OrganizationID: string(organization), WorkOSOrganizationID: "provider_" + string(organization), PublicURL: "https://hub.example.test", Provider: newHostedSecurityProvider()}})
			service.workerCancel()
			<-service.workerDone
			<-service.secretRewrapDone
			service.config.SecretKeys = secretTestKeys(t, "2", "1", "2")
			path := "/api/v2/organizations/" + string(organization) + "/secrets/status"
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, path, "", nil), http.StatusUnauthorized)
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, "/api/v2/organizations/foreign/secrets/status", testHubAdminToken, nil), http.StatusNotFound)
			for _, limit := range []string{"0", "201", "invalid"} {
				requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, path+"?limit="+limit, testHubAdminToken, nil), http.StatusUnprocessableEntity)
			}
			project, kind := "", ""
			count := 0
			for page := 0; page < 4; page++ {
				response := performHubAPIRequest(t, service, http.MethodGet, path+"?limit=1&after_project="+url.QueryEscape(project)+"&after_kind="+url.QueryEscape(kind), testHubAdminToken, nil)
				requireNativeStatus(t, response, http.StatusOK)
				if strings.Contains(response.Body.String(), spritesSecretSentinel) || strings.Contains(response.Body.String(), "wrapped_data_key") {
					t.Fatal("secret status exposed encrypted or plaintext material")
				}
				var status HostedSecretStatus
				decodeHubResponse(t, response, &status)
				if status.OrganizationID != organization {
					t.Fatalf("status belongs to %q", status.OrganizationID)
				}
				for _, group := range status.Counts {
					count += group.Count
					if group.MasterKeyVersion != 1 || group.Count != 1 {
						t.Fatalf("version group=%+v", group)
					}
				}
				for _, problem := range status.Problems {
					if problem.OrganizationID != string(organization) || problem.Problem != "not_active_version" {
						t.Fatalf("rotation diagnostic=%+v", problem)
					}
				}
				if len(status.Problems) != len(status.Counts) {
					t.Fatal("old key diagnostic was lost")
				}
				project, kind = string(status.NextProjectID), status.NextKind
				if kind == "" {
					break
				}
			}
			if count != 3 {
				t.Fatalf("paged secrets=%d, want 3", count)
			}
			for _, test := range []struct {
				name, active, problem string
				versions              []string
			}{
				{name: "active", active: "1", versions: []string{"1"}},
				{name: "missing", active: "2", versions: []string{"2"}, problem: "missing_key"},
			} {
				t.Run(test.name, func(t *testing.T) {
					service.config.SecretKeys = secretTestKeys(t, test.active, test.versions...)
					response := performHubAPIRequest(t, service, http.MethodGet, path, testHubAdminToken, nil)
					requireNativeStatus(t, response, http.StatusOK)
					var status HostedSecretStatus
					decodeHubResponse(t, response, &status)
					want := 0
					if test.problem != "" {
						want = 3
					}
					if len(status.Problems) != want {
						t.Fatalf("diagnostics=%+v", status.Problems)
					}
					for _, problem := range status.Problems {
						if problem.Problem != test.problem {
							t.Fatalf("diagnostic=%+v", problem)
						}
					}
				})
			}
			cfg := service.config
			cfg.SecretKeys = keys
			if err := service.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := openTestService(t, cfg)
			response := performHubAPIRequest(t, reopened, http.MethodGet, path, testHubAdminToken, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var status HostedSecretStatus
			decodeHubResponse(t, response, &status)
			count = 0
			for _, group := range status.Counts {
				count += group.Count
			}
			if count != 3 || len(status.Problems) != 0 {
				t.Fatalf("reopened status=%+v", status)
			}

		})
	}
}
