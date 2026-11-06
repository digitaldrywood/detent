package hubserver

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresAccountPreferencesUseCurrentMember(t *testing.T) {
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
	dailyPath := "/api/v2/organizations/org_security/daily-summary-preference"
	response := f.request(t, user, http.MethodGet, dailyPath, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var daily dailySummaryPreference
	decodeHubResponse(t, response, &daily)
	if daily.Configured || daily.Enabled || daily.Recipient != "preferences@example.test" {
		t.Fatalf("default daily preference=%+v", daily)
	}
	response = f.request(t, user, http.MethodPut, dailyPath, map[string]any{"enabled": true, "send_time": "06:30", "time_zone": "America/Chicago", "days": "weekdays"})
	requireNativeStatus(t, response, http.StatusOK)
	response = f.request(t, user, http.MethodGet, dailyPath, nil)
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &daily)
	if !daily.Configured || !daily.Enabled || daily.SendTime != "06:30" || daily.TimeZone != "America/Chicago" || daily.Days != "weekdays" {
		t.Fatalf("saved daily preference=%+v", daily)
	}
	var member accessstore.Member
	if err := repository.ReadTenant(t.Context(), "org_security", corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		member, err = r.AccessMember(t.Context(), user.identity.Subject)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	preference, err := repository.DailySummaryPreference(t.Context(), "org_security", user.identity.Subject, member.MembershipID, member.PrincipalID)
	if err != nil || preference.Token == "" {
		t.Fatalf("daily unsubscribe token=%q error=%v", preference.Token, err)
	}
	response = f.request(t, user, http.MethodPut, dailyPath, map[string]any{"enabled": true, "send_time": "07:00", "time_zone": "UTC", "days": "every_day"})
	requireNativeStatus(t, response, http.StatusOK)
	response = f.request(t, other, http.MethodPut, dailyPath, map[string]any{"enabled": true, "send_time": "08:00", "time_zone": "UTC", "days": "every_day"})
	requireNativeStatus(t, response, http.StatusOK)
	request := httptest.NewRequest(http.MethodGet, "/daily-summary/unsubscribe?token="+url.QueryEscape(preference.Token), nil)
	recorder := httptest.NewRecorder()
	f.service.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("unsubscribe status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	response = f.request(t, user, http.MethodGet, dailyPath, nil)
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &daily)
	if daily.Enabled {
		t.Fatal("daily summary unsubscribe left preference enabled")
	}
	response = f.request(t, other, http.MethodGet, dailyPath, nil)
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &daily)
	if !daily.Enabled || daily.SendTime != "08:00" || daily.Recipient != "other-preferences@example.test" {
		t.Fatalf("other member daily preference=%+v", daily)
	}

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
	for _, path := range []string{dailyPath, releasePath} {
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
		if response.Code != http.StatusInternalServerError {
			t.Errorf("%s status=%d body=%s, want internal error", path, response.Code, response.Body.String())
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
	service := &Service{echo: echo.New(), secretRewrapReady: make(chan struct{}), database: &database{postgres: repository, billingRepository: repository, hostedOrganization: "org_security", now: time.Now, logger: discardLogger()}, config: Config{Logger: discardLogger(), now: time.Now, newLeaseID: func() string { return newNativeID("lease") }, generateToken: apikey.GenerateToken, AccessRepository: repository, TenantRepository: repository, RunDetails: &memoryRunDetails{}, Hosted: &HostedConfig{OrganizationID: "org_security", WorkOSOrganizationID: "org_provider", PublicURL: "http://example.test", Provider: provider}}}
	sessions, err := auth.NewSessionService(auth.SessionConfig{SessionTTL: 24 * time.Hour, PublicURL: service.config.Hosted.PublicURL}, service)
	if err != nil {
		t.Fatal(err)
	}
	service.hostedSessions = sessions
	service.registerRoutes(service.echo)
	return hostedSecurityFixture{service: service, provider: provider}
}
