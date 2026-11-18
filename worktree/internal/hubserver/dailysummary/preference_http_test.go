package dailysummary

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresDailySummaryPreferences(t *testing.T) {
	repository := hubtest.Repository(t)
	for _, organization := range []tracker.OrganizationID{"org_security", "org_summary_other"} {
		t.Run(string(organization), func(t *testing.T) {
			now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
			f, summary := summaryHTTPFixture(t, repository, organization, now)
			first := f.MemberSession(t, hubtest.MemberOptions{Role: "member", Email: "first@example.test"})
			second := f.MemberSession(t, hubtest.MemberOptions{Role: "member", Email: "second@example.test"})
			summary.deps.EmailSender = summaryEmailSender(func(context.Context, auth.EmailMessage) error { t.Fatal("unexpected summary delivery"); return nil })
			if err := summary.Deliver(t.Context(), now); err != nil {
				t.Fatal(err)
			}
			path := "/api/v2/organizations/" + string(organization) + "/daily-summary-preference"
			for _, user := range []hubtest.HTTPActor{first, second} {
				response := summaryRequest(f, t, user, http.MethodGet, path, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var preference Preference
				decodeSummaryResponse(t, response, &preference)
				if preference.Configured || preference.Recipient != user.Member.Member.Email {
					t.Fatalf("default=%+v", preference)
				}
				response = summaryRequest(f, t, user, http.MethodPut, path, map[string]any{"enabled": true, "send_time": "06:30", "time_zone": "America/Chicago", "days": "weekdays"})
				requireNativeStatus(t, response, http.StatusOK)
				decodeSummaryResponse(t, response, &preference)
				if !preference.Configured || !preference.Enabled || preference.SendTime != "06:30" || preference.Days != "weekdays" || preference.NextSend == nil || !preference.NextSend.After(now) {
					t.Fatalf("saved=%+v", preference)
				}
			}
			requireNativeStatus(t, summaryRequest(f, t, first, http.MethodPut, path, map[string]any{"enabled": true, "send_time": "06:15", "time_zone": "UTC", "days": "every_day"}), http.StatusUnprocessableEntity)
			requireNativeStatus(t, summaryRequest(f, t, first, http.MethodPut, path, map[string]any{"enabled": false, "send_time": "06:30", "time_zone": "UTC", "days": "every_day"}), http.StatusOK)
			var preference Preference
			response := summaryRequest(f, t, second, http.MethodGet, path, nil)
			requireNativeStatus(t, response, http.StatusOK)
			decodeSummaryResponse(t, response, &preference)
			if !preference.Enabled {
				t.Fatal("one member disabled another member's summary")
			}
			var member accessstore.Member
			if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				member, err = r.AccessMember(t.Context(), second.Member.Member.UserID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			saved, err := repository.DailySummaryPreference(t.Context(), organization, member.UserID, member.MembershipID, member.PrincipalID)
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := repository.UnsubscribeDailySummary(t.Context(), organization, saved.Token); err != nil {
					t.Fatal(err)
				}
			}
			response = summaryRequest(f, t, second, http.MethodGet, path, nil)
			requireNativeStatus(t, response, http.StatusOK)
			decodeSummaryResponse(t, response, &preference)
			if preference.Enabled || preference.NextSend != nil {
				t.Fatalf("unsubscribed=%+v", preference)
			}
			if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				return w.RevokeAccessMember(t.Context(), first.Member.Member.UserID, now)
			}); err != nil {
				t.Fatal(err)
			}
			requireNativeStatus(t, summaryRequest(f, t, first, http.MethodPut, path, map[string]any{"enabled": true, "send_time": "06:30", "time_zone": "UTC", "days": "every_day"}), http.StatusForbidden)

		})
	}
}
