package hubserver

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/onboarding"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestOnboardingPageFencesPublicationAndAccess(t *testing.T) {
	service := newPostgresNativeService(t, Config{})
	repository := service.config.TenantRepository
	for _, organization := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		t.Run(string(organization), func(t *testing.T) {
			fixture := newNativeFixture(t, service, organization, "Example")
			key := strings.Repeat("a", 64)
			write := func(fn func(projectstore.OnboardingPageWriter) error) error {
				return repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error { return fn(w.(projectstore.OnboardingPageWriter)) })
			}
			if err := write(func(w projectstore.OnboardingPageWriter) error {
				return w.RequestOnboardingPage(t.Context(), fixture.project.ID, key, projectstore.OnboardingViewer{ID: "example", Scope: "operator"})
			}); err != nil {
				t.Fatal(err)
			}
			claim := func() projectstore.OnboardingCandidate {
				var candidate projectstore.OnboardingCandidate
				if err := write(func(w projectstore.OnboardingPageWriter) error {
					var err error
					candidate, err = w.ClaimOnboardingPage(t.Context())
					return err
				}); err != nil {
					t.Fatal(err)
				}
				return candidate
			}
			first, second := claim(), claim()
			setupDeclared := false
			now := service.config.now().UTC().Truncate(time.Microsecond)
			value := onboarding.Project{
				Repository: "owner/repository",
				Progress:   onboarding.Progress{Revision: 2, Repository: "existing", Artifacts: "local"},
				Policy:     &policy.Approval{Policy: hubTestPolicy()},
				Runners: []runnerauth.Eligibility{{
					Runner: runnerauth.Runner{
						OrganizationID: organization,
						Binding:        runnerauth.Binding{RunnerID: "example"},
						Routing: runnerauth.Routing{
							DisplayName: "Example", ProjectIDs: []tracker.ProjectID{fixture.project.ID},
							ProjectRanks:          map[tracker.ProjectID]int{fixture.project.ID: 3},
							ProjectCapacityLimits: map[tracker.ProjectID]int{}, Tags: []string{"example"},
							Availability: runnerauth.Availability{Windows: []string{}, WindowSlots: map[string]int{"second": 2, "first": 1}},
						},
						BackendIsolation: map[string][]string{"example": {"native", "sandbox"}},
						Problems:         []runnerauth.Problem{{Code: "example", Message: "Example", ReportedAt: now}},
						CapacityConfig:   &runnerauth.CapacityConfig{Revision: "example", LocalLimit: 2, Manageable: true, ObservedAt: now},
					},
					Exclusions:     []runnerauth.Exclusion{},
					LocalChecks:    &runnerauth.LocalChecks{Checkout: "passed", Doctor: "passed", Provider: "passed", RunnerSetupDeclared: &setupDeclared},
					RepositoryList: &runnerauth.RepositoryList{RequestID: "example", Repositories: []runnerauth.Repository{{Owner: "owner", Name: "repository", Private: true}}},
				}},
			}
			value.Evaluate()
			if err := write(func(w projectstore.OnboardingPageWriter) error {
				return w.PublishOnboardingPage(t.Context(), first, value, service.config.now())
			}); !errors.Is(err, corestore.ErrRevisionConflict) {
				t.Fatalf("stale publication: %v", err)
			}
			if err := write(func(w projectstore.OnboardingPageWriter) error {
				return w.PublishOnboardingPage(t.Context(), second, value, service.config.now())
			}); err != nil {
				t.Fatal(err)
			}
			changed := value
			changed.Repository = "owner/changed"
			if err := write(func(w projectstore.OnboardingPageWriter) error {
				return w.PublishOnboardingPage(t.Context(), second, changed, service.config.now())
			}); err != nil {
				t.Fatal(err)
			}
			read := func(ctx context.Context, org tracker.OrganizationID, viewer string) (onboarding.Project, error) {
				var result onboarding.Project
				err := repository.ReadTenant(ctx, org, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					result, err = r.(projectstore.OnboardingPageReader).OnboardingPage(ctx, fixture.project.ID, viewer)
					return err
				})
				return result, err
			}
			got, err := read(t.Context(), organization, key)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, value) {
				t.Fatalf("typed page round trip changed the display:\ngot=%#v\nwant=%#v", got, value)
			}
			if _, err := read(t.Context(), organization, strings.Repeat("b", 64)); !errors.Is(err, corestore.ErrNotFound) {
				t.Fatalf("foreign viewer read: %v", err)
			}
			other := tracker.OrganizationID("org_first")
			if other == organization {
				other = "org_first_second"
			}
			if _, err := read(t.Context(), other, key); !errors.Is(err, corestore.ErrNotFound) {
				t.Fatalf("foreign tenant read: %v", err)
			}
			third := claim()
			newNativeFixture(t, service, organization, "Another example")
			if _, err := read(t.Context(), organization, key); !errors.Is(err, corestore.ErrNotFound) {
				t.Fatalf("access change did not invalidate readiness: %v", err)
			}
			if err := write(func(w projectstore.OnboardingPageWriter) error {
				return w.PublishOnboardingPage(t.Context(), third, value, service.config.now())
			}); !errors.Is(err, corestore.ErrRevisionConflict) {
				t.Fatalf("publication crossed access revision: %v", err)
			}
			current := claim()
			if err := write(func(w projectstore.OnboardingPageWriter) error {
				return w.PublishOnboardingPage(t.Context(), current, value, service.config.now())
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := read(t.Context(), organization, key); err != nil {
				t.Fatalf("current publication unavailable: %v", err)
			}
		})
	}
}

func TestOnboardingPageKeepsViewerVisibilityAndRevocation(t *testing.T) {
	service := newPostgresNativeService(t, Config{})
	for _, organization := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		t.Run(string(organization), func(t *testing.T) {
			fixture := newNativeFixture(t, service, organization, "Example")
			sibling := newNativeFixture(t, service, organization, "Another example")
			runner := prepareRunnerForScope(t, sibling, "projects", runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
			runner.enroll(t)
			other := tokenResponse{ID: newNativeID("credential"), Token: newNativeID("token")}
			now := service.config.now().UTC().Truncate(time.Microsecond)
			if err := service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				return w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: other.ID, Name: other.ID, Hash: apikey.HashToken(other.Token), Scope: "admin", Kind: "service", ProjectAccess: "all", CreatedAt: now, UpdatedAt: now})
			}); err != nil {
				t.Fatal(err)
			}
			tenant := hubmaintenance.Tenant{Organization: string(organization), Generation: 1}
			for _, test := range []struct {
				name, token string
				runners     int
			}{
				{name: "project operator", token: fixture.token, runners: 0},
				{name: "owning administrator", token: testHubAdminToken, runners: 1},
				{name: "other administrator", token: other.Token, runners: 0},
			} {
				t.Run(test.name, func(t *testing.T) {
					cold := performHubAPIRequest(t, service, http.MethodGet, fixture.base+"/onboarding", test.token, nil)
					requireNativeStatus(t, cold, http.StatusOK)
					var expected onboarding.Project
					decodeHubResponse(t, cold, &expected)
					if len(expected.Runners) != test.runners {
						t.Fatalf("cold runner visibility=%d want=%d", len(expected.Runners), test.runners)
					}
					for range 3 {
						ctx := (hubmaintenance.Budget{Items: 1, Bytes: integrationPageMaintenanceBytes}).Context(t.Context(), tenant)
						if err := service.OnboardingPageMaintenanceFeature().Step(ctx, tenant, hubmaintenance.Budget{}); err != nil {
							t.Fatal(err)
						}
					}
					credential, _, err := service.authenticateTenantAPIHash(t.Context(), organization, apikey.HashToken(test.token), "", "")
					if err != nil {
						t.Fatal(err)
					}
					_, key, err := onboardingViewer(credential)
					if err != nil {
						t.Fatal(err)
					}
					if err := service.config.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
						_, err := r.(projectstore.OnboardingPageReader).OnboardingPage(t.Context(), fixture.project.ID, key)
						return err
					}); err != nil {
						t.Fatalf("viewer never became ready: %v", err)
					}
					warm := performHubAPIRequest(t, service, http.MethodGet, fixture.base+"/onboarding", test.token, nil)
					requireNativeStatus(t, warm, http.StatusOK)
					if warm.Body.String() != cold.Body.String() {
						t.Fatalf("projected visibility changed:\ncold=%s\nwarm=%s", cold.Body.String(), warm.Body.String())
					}
				})
			}
			credential, _, err := service.authenticateTenantAPIHash(t.Context(), organization, apikey.HashToken(fixture.token), "", "")
			if err != nil {
				t.Fatal(err)
			}
			if err := service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				return w.DeleteAccessTokenGrant(t.Context(), credential.ID, fixture.project.ID)
			}); err != nil {
				t.Fatal(err)
			}
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, fixture.base+"/onboarding", fixture.token, nil), http.StatusForbidden)
		})
	}
}

type onboardingQueueFailureRepository struct {
	tenantstore.Repository
	failure error
}

func (r onboardingQueueFailureRepository) WriteTenant(context.Context, tracker.OrganizationID, corestore.Workload, func(tenantstore.Writer) error) error {
	return r.failure
}

func TestOnboardingColdFallbackSurvivesPreparationFailure(t *testing.T) {
	fixture := newPostgresDefaultNativeFixture(t, Config{})
	service := fixture.service
	original := service.config.TenantRepository
	t.Cleanup(func() { service.config.TenantRepository = original })
	for _, test := range []struct {
		name    string
		failure error
		status  int
	}{
		{name: "preparation unavailable", failure: errors.New("example preparation failure"), status: http.StatusOK},
		{name: "access revoked", failure: accessstore.ErrDenied, status: http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			service.config.TenantRepository = onboardingQueueFailureRepository{Repository: original, failure: test.failure}
			response := performHubAPIRequest(t, service, http.MethodGet, fixture.base+"/onboarding", fixture.token, nil)
			requireNativeStatus(t, response, test.status)
			if test.status == http.StatusOK {
				var page onboarding.Project
				decodeHubResponse(t, response, &page)
				if page.Policy == nil || len(page.Steps) != 4 {
					t.Fatalf("cold fallback lost existing content: %+v", page)
				}
			}
		})
	}
}
