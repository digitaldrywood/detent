package hubserver

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresHostedArtifactAllowanceBoundary(t *testing.T) {
	admin := postgresStartupTemplate.Database(t)
	for _, suffix := range []string{"a", "b"} {
		t.Run(suffix, func(t *testing.T) {
			organization := tracker.OrganizationID("org_" + strings.Repeat(suffix, 32))
			project := tracker.ProjectID("prj_" + strings.Repeat("c", 32))
			cfg := startupConfig(startupConnection(t, admin, "detent_runtime_v2", ""))
			now := time.Now().UTC().Truncate(time.Microsecond)
			cfg.Clock = func() time.Time { return now }
			cfg.Hosted.OrganizationID = string(organization)
			service, err := Open(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := service.Close(); err != nil {
					t.Error(err)
				}
			})
			service.database.hostedRuntime.Store(&hostedRuntimeConfig{Plans: HostedPlansConfig{WindowSeconds: 60, RetentionWindows: 10}})
			repository := service.config.TenantRepository
			seedStartupOrganization(t, repository, organization)
			publisherID, publisher := "credential_publisher", "publisher_"+suffix
			operator := "operator_" + suffix
			binding := artifact.Binding{ServiceID: artifact.NewID("service"), Origin: "https://artifacts.example.test", Mode: "customer", PublisherTokenID: publisherID}
			err = repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: project, Name: "Example project", Slug: "example-project", Profile: "native"}, States: nativeFixtureStates()}); err != nil {
					return err
				}
				if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: publisherID, Name: "Example publisher", Hash: apikey.HashToken(publisher), Scope: "worker", NativeOnly: true, ProjectAccess: "selected", Kind: "service", CreatedAt: now, UpdatedAt: now}); err != nil {
					return err
				}
				if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: publisherID, ProjectID: project}); err != nil {
					return err
				}
				if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: "credential_operator", Name: "Example operator", Hash: apikey.HashToken(operator), Scope: "operator", NativeOnly: true, ProjectAccess: "selected", Kind: "service", CreatedAt: now, UpdatedAt: now}); err != nil {
					return err
				}
				if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: "credential_operator", ProjectID: project}); err != nil {
					return err
				}
				plan := billingstore.PlanReference{ID: "artifact_usage", Version: 1}
				if err := w.CreateBillingPlan(t.Context(), billingstore.Plan{OrganizationID: organization, PlanReference: plan, Name: "Artifact usage", Allowances: map[string]int64{}}); err != nil {
					return err
				}
				if err := w.SetBillingAssignment(t.Context(), billingstore.Assignment{OrganizationID: organization, Base: plan}, 0); err != nil {
					return err
				}
				return w.BindArtifactService(t.Context(), project, binding)
			})
			if err != nil {
				t.Fatal(err)
			}
			path := "/api/v2/organizations/" + string(organization) + "/artifact-allowances/" + binding.ServiceID
			usage := artifact.Usage{RetainedBytes: 123, ReservedBytes: 456, RelayBytes: 789, StorageRequests: 3, ObservedAt: now, WindowStart: now.Truncate(time.Minute)}
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, publisher, usage), http.StatusForbidden)
			binding.Mode, binding.HostedOptIn = "hosted", true
			if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error { return w.BindArtifactService(t.Context(), project, binding) }); err != nil {
				t.Fatal(err)
			}
			response := performHubAPIRequest(t, service, http.MethodPost, path, publisher, usage)
			requireNativeStatus(t, response, http.StatusOK)
			var limits artifact.Limits
			decodeHubResponse(t, response, &limits)
			if limits.RetainedBytes != 0 || limits.UploadBytes != 0 || limits.WindowSeconds != 60 {
				t.Fatalf("limits = %+v", limits)
			}
			for _, tc := range []struct {
				name, token, path string
				status            int
			}{
				{"operator cannot impersonate publisher", operator, path, http.StatusForbidden},
				{"invalid publisher", "invalid", path, http.StatusForbidden},
				{"unknown service", publisher, "/api/v2/organizations/" + string(organization) + "/artifact-allowances/" + artifact.NewID("service"), http.StatusForbidden},
				{"foreign organization", publisher, "/api/v2/organizations/org_foreign/artifact-allowances/" + binding.ServiceID, http.StatusNotFound},
			} {
				t.Run(tc.name, func(t *testing.T) {
					requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, tc.path, tc.token, usage), tc.status)
				})
			}
			for _, tc := range []struct {
				name   string
				mutate func(*artifact.Usage)
				status int
			}{
				{"replay", func(*artifact.Usage) {}, http.StatusOK},
				{"older observation", func(u *artifact.Usage) {
					u.ObservedAt = now.Add(-time.Second)
					u.RetainedBytes = 999
					u.RelayBytes = 999
				}, http.StatusOK},
				{"negative usage", func(u *artifact.Usage) { u.ReservedBytes = -1 }, http.StatusUnprocessableEntity},
				{"future observation", func(u *artifact.Usage) { u.ObservedAt = now.Add(2 * time.Minute) }, http.StatusUnprocessableEntity},
			} {
				t.Run(tc.name, func(t *testing.T) {
					request := usage
					tc.mutate(&request)
					requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, publisher, request), tc.status)
				})
			}
			latest := usage
			latest.ObservedAt = now.Add(time.Second)
			latest.RetainedBytes, latest.RelayBytes = 321, 100
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, publisher, latest), http.StatusOK)
			var retained, reserved int64
			if err := admin.QueryRowContext(t.Context(), "SELECT retained_bytes,reserved_bytes FROM hosted_artifact_usage WHERE organization_id=$1", organization).Scan(&retained, &reserved); err != nil {
				t.Fatal(err)
			}
			if retained != 321 || reserved != 456 {
				t.Fatalf("usage = %d/%d", retained, reserved)
			}
			if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
				amount, err := r.BillingUsage(t.Context(), usage.WindowStart, "relay_bytes")
				if err == nil && amount != 789 {
					t.Fatalf("relay usage = %d", amount)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			binding.ServiceID = artifact.NewID("service")
			if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error { return w.BindArtifactService(t.Context(), project, binding) }); err != nil {
				t.Fatal(err)
			}
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, "/api/v2/organizations/"+string(organization)+"/artifact-allowances/"+binding.ServiceID, publisher, usage), http.StatusTooManyRequests)
			if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error { return w.RevokeAccessCredential(t.Context(), publisherID, now) }); err != nil {
				t.Fatal(err)
			}
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, publisher, usage), http.StatusForbidden)
		})
	}
}
