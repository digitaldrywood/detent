package hubserver

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func TestPostgresDailySummaryPreferences(t *testing.T) {
	repository := projectRecordPostgres(t)
	f := newHostedSecurityFixture(t, func(cfg *Config) { cfg.AccessRepository = repository; cfg.TenantRepository = repository })
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	f.service.config.Clock = func() time.Time { return now }
	if err := repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: "org_security", Name: "Example", CreatedAt: now}); err != nil {
			return err
		}
		return w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: "org_security", ProviderID: "org_provider"})
	}); err != nil {
		t.Fatal(err)
	}
	first := f.user(t, "first", "member", "first@example.test", "", "")
	second := f.user(t, "second", "member", "second@example.test", "", "")
	disableLegacyPostgresTestHandles(t, f.service, repository)
	f.service.config.Hosted.EmailSender = healthEmailSenderFunc(func(context.Context, auth.EmailMessage) error { t.Fatal("unexpected summary delivery"); return nil })
	if err := f.service.deliverDailySummaries(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	path := "/api/v2/organizations/org_security/daily-summary-preference"
	for _, user := range []hostedSecurityUser{first, second} {
		response := f.request(t, user, http.MethodGet, path, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var preference dailySummaryPreference
		decodeHubResponse(t, response, &preference)
		if preference.Configured || preference.Recipient != user.identity.Email {
			t.Fatalf("default=%+v", preference)
		}
		response = f.request(t, user, http.MethodPut, path, map[string]any{"enabled": true, "send_time": "06:30", "time_zone": "America/Chicago", "days": "weekdays"})
		requireNativeStatus(t, response, http.StatusOK)
		decodeHubResponse(t, response, &preference)
		if !preference.Configured || !preference.Enabled || preference.SendTime != "06:30" || preference.Days != "weekdays" || preference.NextSend == nil || !preference.NextSend.After(now) {
			t.Fatalf("saved=%+v", preference)
		}
	}
	requireNativeStatus(t, f.request(t, first, http.MethodPut, path, map[string]any{"enabled": true, "send_time": "06:15", "time_zone": "UTC", "days": "every_day"}), http.StatusUnprocessableEntity)
	requireNativeStatus(t, f.request(t, first, http.MethodPut, path, map[string]any{"enabled": false, "send_time": "06:30", "time_zone": "UTC", "days": "every_day"}), http.StatusOK)
	var preference dailySummaryPreference
	response := f.request(t, second, http.MethodGet, path, nil)
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &preference)
	if !preference.Enabled {
		t.Fatal("one member disabled another member's summary")
	}
	var member accessstore.Member
	if err := repository.ReadTenant(t.Context(), "org_security", corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		member, err = r.AccessMember(t.Context(), second.identity.Subject)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	saved, err := repository.DailySummaryPreference(t.Context(), "org_security", member.UserID, member.MembershipID, member.PrincipalID)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := repository.UnsubscribeDailySummary(t.Context(), "org_security", saved.Token); err != nil {
			t.Fatal(err)
		}
	}
	response = f.request(t, second, http.MethodGet, path, nil)
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &preference)
	if preference.Enabled || preference.NextSend != nil {
		t.Fatalf("unsubscribed=%+v", preference)
	}
	if err := repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
		return w.RevokeAccessMember(t.Context(), first.identity.Subject, now)
	}); err != nil {
		t.Fatal(err)
	}
	requireNativeStatus(t, f.request(t, first, http.MethodPut, path, map[string]any{"enabled": true, "send_time": "06:30", "time_zone": "UTC", "days": "every_day"}), http.StatusForbidden)
}
