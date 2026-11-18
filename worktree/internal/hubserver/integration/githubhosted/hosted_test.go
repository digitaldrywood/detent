package githubhosted_test

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHostedGitHubRepositoriesPostgres(t *testing.T) {
	admin := hubtest.Database(t)
	connection := hubtest.PostgresConfig(t, admin)
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{97}, ed25519.SeedSize))
	config := hubserver.Config{DatabaseBackend: hubserver.DatabasePostgres, Postgres: &connection, RunDetails: hubtest.NewDetails(), Hosted: &hostedplans.HostedConfig{OrganizationID: "org_startup", WorkOSOrganizationID: "provider_org_startup", PublicURL: "https://hub.example.test", Provider: sharedProvider{}}}
	config.Hosted.SharedEntry = &hostedplans.HostedSharedEntry{Issuer: "entry", PublicKeys: []ed25519.PublicKey{key.Public().(ed25519.PublicKey)}, Generation: 1}
	repository := hubtest.RepositoryOn(t, admin)
	nowSeed := time.Now().UTC()
	if err := repository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: "org_startup", Name: "Example", Slug: "example-startup", CreatedAt: nowSeed}); err != nil {
			return err
		}
		return w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: "org_startup", ProviderID: config.Hosted.WorkOSOrganizationID, Deployment: "shared", PublicURL: config.Hosted.PublicURL, AllocationGeneration: 1})
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	type projectBinding struct {
		id                 tracker.ProjectID
		profile            string
		repository         string
		checkoutRepository string
	}
	for _, organization := range []tracker.OrganizationID{"org_startup", "org_other"} {
		if organization != "org_startup" {
			if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(writer tenantstore.Writer) error {
				if err := writer.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: "Other example", CreatedAt: now}); err != nil {
					return err
				}
				deployment, generation := "shared", int64(1)
				return writer.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: organization, ProviderID: "provider_other", BootstrapSubject: config.Hosted.BootstrapSubject, Deployment: deployment, PublicURL: config.Hosted.PublicURL, AllocationGeneration: generation})
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
				{id: "project_tail", profile: "native", repository: "Z-Org/Final", checkoutRepository: "z-org/final"},
			}
			for index := range corestore.MaxPageSize + 1 {
				projects = append(projects, projectBinding{id: tracker.ProjectID(fmt.Sprintf("project_blank_%03d", index)), profile: "native"})
			}
		} else {
			projects = []projectBinding{
				{id: "project_other", profile: "native", repository: "other-org/other-repo"},
				{id: "project_other_copy", profile: "native", checkoutRepository: "OTHER-ORG/OTHER-REPO"},
			}
		}
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(writer tenantstore.Writer) error {
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

	for _, test := range []struct {
		organization tracker.OrganizationID
		want         []string
	}{
		{organization: "org_startup", want: []string{"example-org/checkout-one", "example-org/repo-one", "z-org/final"}},
		{organization: "org_other", want: []string{"other-org/other-repo"}},
	} {
		t.Run(string(test.organization), func(t *testing.T) {
			config := config
			hosted := *config.Hosted
			hosted.OrganizationID = string(test.organization)
			if test.organization == "org_other" {
				hosted.WorkOSOrganizationID = "provider_other"
			}
			config.Hosted = &hosted
			firstReceipt := now.Add(time.Hour).Truncate(time.Microsecond)
			if test.organization == "org_other" {
				firstReceipt = now.Add(time.Minute).Truncate(time.Microsecond)
			}
			var receiptClock atomic.Int64
			receiptClock.Store(firstReceipt.UnixNano())
			config.Clock = func() time.Time { return time.Unix(0, receiptClock.Load()).UTC() }
			service, err := hubserver.Open(t.Context(), config)
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
			sign := func(t *testing.T, audience tracker.OrganizationID) string {
				t.Helper()
				issuedAt := time.Now().UTC()
				id, err := cloudassert.NewID()
				if err != nil {
					t.Fatal(err)
				}
				assertion, err := cloudassert.Sign(key, cloudassert.Claims{Issuer: "entry", Audience: string(audience), Generation: 1, Kind: cloudassert.KindService, Method: http.MethodPost, Path: path, BodyDigest: cloudassert.BodyDigest(body), IssuedAt: issuedAt, ExpiresAt: issuedAt.Add(30 * time.Second), ID: id, Subject: "service_example", Email: "service@example.test"})
				if err != nil {
					t.Fatal(err)
				}
				return assertion
			}
			request := func(t *testing.T, assertion string) *httptest.ResponseRecorder {
				t.Helper()
				request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
				request.Header.Set(cloudassert.Header, assertion)
				request.Header.Set("Content-Type", "application/json")
				response := httptest.NewRecorder()
				service.Handler().ServeHTTP(response, request)
				return response
			}
			if response := request(t, ""); response.Code != http.StatusUnauthorized {
				t.Fatalf("unsigned request status=%d", response.Code)
			}
			foreign := tracker.OrganizationID("org_startup")
			if foreign == test.organization {
				foreign = "org_other"
			}
			if response := request(t, sign(t, foreign)); response.Code != http.StatusUnauthorized {
				t.Fatalf("foreign audience request status=%d", response.Code)
			}
			response := request(t, sign(t, test.organization))
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
			path = "/internal/v1/github/receipt"
			if response := request(t, ""); response.Code != http.StatusUnauthorized {
				t.Fatalf("unsigned receipt status=%d", response.Code)
			}
			if response := request(t, sign(t, foreign)); response.Code != http.StatusUnauthorized {
				t.Fatalf("foreign receipt audience status=%d", response.Code)
			}
			receipt := func(t *testing.T, want *time.Time) {
				t.Helper()
				path, body = "/internal/v1/github/receipt", []byte("{}")
				response := request(t, sign(t, test.organization))
				if response.Code != http.StatusOK {
					t.Fatalf("receipt status=%d body=%s", response.Code, response.Body.String())
				}
				wire := "{\"received_at\":null}\n"
				if want != nil {
					wire = fmt.Sprintf("{\"received_at\":%q}\n", want.UTC().Format(time.RFC3339Nano))
				}
				if response.Body.String() != wire {
					t.Fatalf("receipt wire=%q want=%q", response.Body.String(), wire)
				}
				var decoded struct {
					ReceivedAt *time.Time `json:"received_at"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
					t.Fatal(err)
				}
				if (decoded.ReceivedAt == nil) != (want == nil) || want != nil && !decoded.ReceivedAt.Equal(*want) {
					t.Fatalf("decoded receipt=%v want=%v", decoded.ReceivedAt, want)
				}
			}
			receipt(t, nil)
			firstPayload := []byte(`{"zen":"example first","repository":{"full_name":"example-org/receipt-only"}}`)
			secondPayload := []byte(`{"zen":"example second","repository":{"full_name":"example-org/receipt-only"}}`)
			for _, delivery := range []struct {
				name       string
				id         string
				payload    []byte
				observed   time.Time
				wantLatest time.Time
				status     int
				duplicate  bool
			}{
				{name: "first", id: "example-first", payload: firstPayload, observed: firstReceipt, wantLatest: firstReceipt, status: http.StatusAccepted},
				{name: "second", id: "example-second", payload: secondPayload, observed: firstReceipt.Add(2 * time.Second), wantLatest: firstReceipt.Add(2 * time.Second), status: http.StatusAccepted},
				{name: "valid duplicate", id: "example-first", payload: firstPayload, observed: firstReceipt.Add(4 * time.Second), wantLatest: firstReceipt.Add(4 * time.Second), status: http.StatusAccepted, duplicate: true},
				{name: "older duplicate", id: "example-first", payload: firstPayload, observed: firstReceipt.Add(3 * time.Second), wantLatest: firstReceipt.Add(4 * time.Second), status: http.StatusAccepted, duplicate: true},
				{name: "equal duplicate", id: "example-first", payload: firstPayload, observed: firstReceipt.Add(4 * time.Second), wantLatest: firstReceipt.Add(4 * time.Second), status: http.StatusAccepted, duplicate: true},
				{name: "conflicting duplicate", id: "example-first", payload: secondPayload, observed: firstReceipt.Add(8 * time.Second), wantLatest: firstReceipt.Add(4 * time.Second), status: http.StatusConflict},
			} {
				t.Run("receipt "+delivery.name, func(t *testing.T) {
					receiptClock.Store(delivery.observed.UnixNano())
					path, body = "/internal/v1/github/webhook?delivery_id="+delivery.id+"&event_type=ping", delivery.payload
					response := request(t, sign(t, test.organization))
					if response.Code != delivery.status {
						t.Fatalf("webhook status=%d want=%d body=%s", response.Code, delivery.status, response.Body.String())
					}
					if response.Code == http.StatusAccepted {
						var result struct {
							DeliveryID string `json:"delivery_id"`
							Duplicate  bool   `json:"duplicate"`
						}
						if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
							t.Fatal(err)
						}
						if result.DeliveryID != delivery.id || result.Duplicate != delivery.duplicate {
							t.Fatalf("webhook receipt=%+v", result)
						}
					}
					receipt(t, &delivery.wantLatest)
				})
			}
			if err := repository.ReadTenant(t.Context(), test.organization, corestore.Serving, func(reader tenantstore.Reader) error {
				owner := reader.(projectstore.GitHubIntakeReader)
				for _, delivery := range []struct {
					id   string
					want time.Time
				}{{id: "example-first", want: firstReceipt}, {id: "example-second", want: firstReceipt.Add(2 * time.Second)}} {
					row, err := owner.GitHubWebhook(t.Context(), delivery.id)
					if err != nil {
						return err
					}
					if !row.ReceivedAt.Equal(delivery.want) || row.Payload.PayloadObjectKey == "" {
						t.Fatalf("first receipt/payload changed: %+v", row)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
