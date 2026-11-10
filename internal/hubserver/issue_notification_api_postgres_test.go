package hubserver

import (
	"net/http"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/accountstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func TestPostgresIssueNotificationPreferenceReadWrite(t *testing.T) {
	f := newHostedSecurityFixture(t)
	repository := f.service.database.postgres
	now := time.Now().UTC()
	user := f.user(t, "preference", "member", "preference@example.test", "", "")
	other := f.user(t, "other_preference", "member", "other-preference@example.test", "", "")
	if err := f.service.collectIssueEmails(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, page, _, err := f.service.claimIssueEmail(t.Context(), now, accountstore.NotificationCursor{}, nil); err != nil || page.Found {
		t.Fatalf("empty delivery found=%v error=%v", page.Found, err)
	}
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
