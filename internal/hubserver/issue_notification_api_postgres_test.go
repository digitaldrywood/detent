package hubserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresIssueNotificationPreferenceReadWrite(t *testing.T) {
	repository := projectRecordPostgres(t)
	f := newHostedSecurityFixture(t, func(cfg *Config) {
		cfg.AccessRepository = repository
		cfg.TenantRepository = repository
	})
	now := time.Now().UTC()
	if err := repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: "org_security", Name: "Example organization", CreatedAt: now}); err != nil {
			return err
		}
		return w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: "org_security", ProviderID: "org_provider"})
	}); err != nil {
		t.Fatal(err)
	}
	user := f.user(t, "preference", "member", "preference@example.test", "", "")
	other := f.user(t, "other_preference", "member", "other-preference@example.test", "", "")
	disableLegacyPostgresTestHandles(t, f.service, repository)
	path := "/api/v2/organizations/org_security/issue-notification-preference"
	var preference issueNotificationPreference
	for _, member := range []hostedSecurityUser{user, other} {
		response := f.request(t, member, http.MethodGet, path, nil)
		requireNativeStatus(t, response, http.StatusOK)
		decodeHubResponse(t, response, &preference)
		if !preference.Enabled {
			t.Fatalf("new member preference for %s was not enabled", member.identity.Subject)
		}
	}
	response := f.request(t, user, http.MethodPut, path, map[string]bool{"enabled": false})
	requireNativeStatus(t, response, http.StatusOK)
	response = f.request(t, user, http.MethodGet, path, nil)
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &preference)
	if preference.Enabled {
		t.Fatal("disabled member preference was not read back")
	}
	for _, test := range []struct {
		user string
		want bool
	}{{user.identity.Subject, false}, {other.identity.Subject, true}} {
		var member accessstore.Member
		if err := repository.ReadTenant(t.Context(), "org_security", corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			member, err = r.AccessMember(t.Context(), test.user)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if member.NotifyIssues != test.want {
			t.Fatalf("PostgreSQL notify_issues for %s=%t, want %t", test.user, member.NotifyIssues, test.want)
		}
	}

	legacyRepository := &issuePreferenceLegacyFallbackRepository{Repository: repository, service: f.service}
	previousRepository := f.service.config.AccessRepository
	f.service.config.AccessRepository = legacyRepository
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.AddCookie(&http.Cookie{Name: hostedCookie, Value: other.token})
	context := f.service.echo.NewContext(request, httptest.NewRecorder())
	context.SetParamNames("organization")
	context.SetParamValues("org_security")
	var legacyHandlerPanicked bool
	func() {
		defer func() { legacyHandlerPanicked = recover() != nil }()
		_ = f.service.issueNotificationPreference(context)
	}()
	f.service.config.AccessRepository = previousRepository
	if !legacyHandlerPanicked || legacyRepository.reads != 2 {
		t.Fatalf("original SQLite preference read did not fail at the nil-reader boundary: panicked=%t access_reads=%d", legacyHandlerPanicked, legacyRepository.reads)
	}

	if err := repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
		return w.RevokeAccessMember(t.Context(), user.identity.Subject, now.Add(time.Second))
	}); err != nil {
		t.Fatal(err)
	}
	requireNativeStatus(t, f.request(t, user, http.MethodPut, path, map[string]bool{"enabled": true}), http.StatusForbidden)
	var deactivated accessstore.Member
	if err := repository.ReadTenant(t.Context(), "org_security", corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		deactivated, err = r.AccessMember(t.Context(), user.identity.Subject)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if deactivated.Active || deactivated.NotifyIssues {
		t.Fatalf("deactivated member state=%+v", deactivated)
	}
}

func disableLegacyPostgresTestHandles(t *testing.T, service *Service, repository *postgres.Repository) {
	t.Helper()
	database := service.database
	db, reader, authReader, claimReader, previousRepository := database.db, database.reader, database.authReader, database.claimReader, database.postgres
	database.db, database.reader, database.authReader, database.claimReader, database.postgres = nil, nil, nil, nil, repository
	previousEcho := service.echo
	service.echo = echo.New()
	service.registerRoutes(service.echo)
	t.Cleanup(func() {
		service.echo = previousEcho
		database.db, database.reader, database.authReader, database.claimReader, database.postgres = db, reader, authReader, claimReader, previousRepository
	})
}

type issuePreferenceLegacyFallbackRepository struct {
	tenantstore.Repository
	service *Service
	reads   int
}

func (r *issuePreferenceLegacyFallbackRepository) ReadTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	err := r.Repository.ReadTenant(ctx, organization, workload, read)
	if err == nil {
		r.reads++
		if r.reads == 2 {
			r.service.config.AccessRepository = nil
		}
	}
	return err
}
