package hubserver

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/internal/hubdeps"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresAccountPreferencesUseCurrentMember(t *testing.T) {
	t.Parallel()
	repository := projectRecordPostgres(t)
	f := accountPreferencesPostgresFixture(t, repository)
	now := time.Now().UTC()
	if err := repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: "org_security", Name: "Example organization", CreatedAt: now}); err != nil {
			return err
		}
		return w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: "org_security", ProviderID: "org_provider"})
	}); err != nil {
		t.Fatal(err)
	}
	user := f.user(t, "preferences", "member", "preferences@example.test", "", "")
	other := f.user(t, "other_preferences", "member", "other-preferences@example.test", "", "")
	f.service.config.Hosted.Provider = accountWidgetProvider{f.provider}
	requireNativeStatus(t, f.request(t, user, http.MethodPost, "/api/v2/organizations/org_security/widget-token", map[string]any{}), http.StatusOK)
	var member accessstore.Member
	var err error
	var response *httptest.ResponseRecorder
	f.service.releaseNotes = testReleaseBuilds()
	f.service.config.Version = "1.2.1"
	releasePath := "/api/v2/organizations/org_security/release-notes"
	response = f.request(t, user, http.MethodPut, releasePath+"/seen", nil)
	requireNativeStatus(t, response, http.StatusOK)
	response = f.request(t, other, http.MethodPut, releasePath+"/seen", nil)
	requireNativeStatus(t, response, http.StatusOK)
	err = repository.ReadTenant(t.Context(), "org_security", corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		member, err = r.AccessMember(t.Context(), user.identity.Subject)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	release, err := repository.ReleasePreference(t.Context(), "org_security", user.identity.Subject, member.MembershipID, member.PrincipalID)
	if err != nil || release.Version != "v1.2.1" {
		t.Fatalf("release preference=%+v error=%v", release, err)
	}
	response = f.request(t, user, http.MethodGet, releasePath, nil)
	requireNativeStatus(t, response, http.StatusOK)

	if err := repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
		return w.RevokeAccessMember(t.Context(), user.identity.Subject, now.Add(time.Second))
	}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{releasePath} {
		requireNativeStatus(t, f.request(t, user, http.MethodGet, path, nil), http.StatusForbidden)
	}
}

type accountWidgetProvider struct {
	*hostedSecurityProvider
}

func (accountWidgetProvider) WidgetToken(context.Context, string) (string, error) {
	return "example-widget-token", nil
}

func TestPostgresHostedCurrentSessionReadSurfacesStoreFailure(t *testing.T) {
	t.Parallel()
	repository := projectRecordPostgres(t)
	f := accountPreferencesPostgresFixture(t, repository)
	now := time.Now().UTC()
	if err := repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: "org_security", Name: "Example organization", CreatedAt: now}); err != nil {
			return err
		}
		return w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: "org_security", ProviderID: "org_provider"})
	}); err != nil {
		t.Fatal(err)
	}
	user := f.user(t, "read_failure", "member", "read-failure@example.test", "", "")
	f.service.config.AccessRepository = &hostedSessionReadFailureRepository{Repository: repository, err: errors.New("temporary tenant read failure")}
	for _, path := range []string{
		"/api/v2/organizations/org_security/widget-token",
		"/api/v2/organizations/org_security/issue-notification-preference",
		"/api/v2/organizations/org_security/daily-summary-preference",
	} {
		method := http.MethodGet
		var body any
		if strings.HasSuffix(path, "/widget-token") {
			method = http.MethodPost
			body = map[string]any{}
		}
		response := f.request(t, user, method, path, body)
		if response.Code != http.StatusInternalServerError && response.Code != http.StatusServiceUnavailable {
			t.Errorf("%s status=%d body=%s, want server failure", path, response.Code, response.Body.String())
		}
		if strings.Contains(response.Body.String(), "temporary tenant read failure") {
			t.Errorf("%s exposed storage error: %s", path, response.Body.String())
		}
	}
}

type hostedSessionReadFailureRepository struct {
	tenantstore.Repository
	err error
}

func (r *hostedSessionReadFailureRepository) ReadTenant(context.Context, tracker.OrganizationID, corestore.Workload, func(tenantstore.Reader) error) error {
	return r.err
}

func accountPreferencesPostgresFixture(t *testing.T, repository *postgres.Repository) hostedSecurityFixture {
	t.Helper()
	provider := newHostedSecurityProvider()
	service := &Service{echo: echo.New(), secretRewrapReady: make(chan struct{}), database: &database{postgres: repository, billingRepository: repository, hostedOrganization: "org_security", now: time.Now, logger: discardLogger()}, config: WithDeps(Config{Logger: discardLogger(), Clock: time.Now, AccessRepository: repository, TenantRepository: repository, RunDetails: &memoryRunDetails{}, Hosted: &hostedplans.HostedConfig{OrganizationID: "org_security", WorkOSOrganizationID: "org_provider", PublicURL: "http://example.test", Provider: provider}}, hubdeps.Deps{NewLeaseID: func() string { return hubapi.NewID("lease") }, GenerateToken: apikey.GenerateToken})}
	sessions, err := auth.NewSessionService(auth.SessionConfig{SessionTTL: 24 * time.Hour, PublicURL: service.config.Hosted.PublicURL}, service)
	if err != nil {
		t.Fatal(err)
	}
	service.hostedSessions = sessions
	service.registerRoutes(service.echo)
	return hostedSecurityFixture{service: service, provider: provider}
}
