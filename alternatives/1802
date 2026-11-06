package hubserver

import (
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHostedGitHubRepositoriesPostgres(t *testing.T) {
	admin := postgresStartupTemplate.Database(t)
	connection := startupConnection(t, admin, "detent_runtime_v2", "")
	key := hostedSharedKey(97)
	config := startupConfig(connection)
	config.Hosted.SharedEntry = &HostedSharedEntry{Issuer: "entry", PublicKeys: []ed25519.PublicKey{key.Public().(ed25519.PublicKey)}, Generation: 1}
	database, err := openDatabase(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	seedStartupOrganization(t, database.postgres, "org_startup")
	now := time.Now().UTC()
	type projectBinding struct {
		id                 tracker.ProjectID
		profile            string
		repository         string
		checkoutRepository string
	}
	for _, organization := range []tracker.OrganizationID{"org_startup", "org_other"} {
		if organization != "org_startup" {
			if err := database.postgres.WriteTenant(t.Context(), organization, corestore.Serving, func(writer tenantstore.Writer) error {
				if err := writer.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: "Other example", CreatedAt: now}); err != nil {
					return err
				}
				return writer.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: organization, ProviderID: "provider_other"})
			}); err != nil {
				t.Fatal(err)
			}
		}
		projects := []projectBinding{}
		if organization == "org_startup" {
			projects = []projectBinding{
				{id: "project_bound", profile: "native", repository: "Example-Org/Repo-One", checkoutRepository: "Example-Org/Checkout-One"},
				{id: "project_checkout", profile: "native", checkoutRepository: "example-org/checkout-one"},
				{id: "project_compatible", profile: "github_compatible", repository: "example-org/hidden"},
			}
		} else {
			projects = []projectBinding{{id: "project_other", profile: "native", repository: "other-org/other-repo"}}
		}
		if err := database.postgres.WriteTenant(t.Context(), organization, corestore.Serving, func(writer tenantstore.Writer) error {
			for _, project := range projects {
				record, err := writer.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: project.id, Name: string(project.id), Profile: project.profile}, States: []tracker.NativeState{{Name: "Todo", Dispatchable: true}}})
				if err != nil {
					return err
				}
				record.Settings.Repository = project.repository
				record.Settings.CheckoutRepository = project.checkoutRepository
				if _, err := writer.UpdateProjectRecord(t.Context(), record); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		organization tracker.OrganizationID
		want         []string
	}{
		{organization: "org_startup", want: []string{"example-org/checkout-one", "example-org/repo-one"}},
		{organization: "org_other", want: []string{"other-org/other-repo"}},
	} {
		t.Run(string(test.organization), func(t *testing.T) {
			config := config
			hosted := *config.Hosted
			hosted.OrganizationID = string(test.organization)
			config.Hosted = &hosted
			service, err := Open(t.Context(), config)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := service.Close(); err != nil {
					t.Error(err)
				}
			})
			path := "/internal/v1/github/repositories"
			body := []byte("{}")
			issuedAt := time.Now().UTC()
			id, err := cloudassert.NewID()
			if err != nil {
				t.Fatal(err)
			}
			assertion, err := cloudassert.Sign(key, cloudassert.Claims{Issuer: "entry", Audience: string(test.organization), Generation: 1, Kind: cloudassert.KindService, Method: http.MethodPost, Path: path, BodyDigest: cloudassert.BodyDigest(body), IssuedAt: issuedAt, ExpiresAt: issuedAt.Add(30 * time.Second), ID: id, Subject: "service_example", Email: "service@example.test"})
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
			request.Header.Set(cloudassert.Header, assertion)
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			service.Handler().ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("signed request status=%d", response.Code)
			}
			var repositories []string
			if err := json.Unmarshal(response.Body.Bytes(), &repositories); err != nil {
				t.Fatal(err)
			}
			if len(repositories) != len(test.want) {
				t.Fatalf("repositories=%v want=%v", repositories, test.want)
			}
			for index := range test.want {
				if repositories[index] != test.want[index] {
					t.Fatalf("repositories=%v want=%v", repositories, test.want)
				}
			}
		})
	}
}
