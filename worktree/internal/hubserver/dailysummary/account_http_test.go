package dailysummary

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func TestPostgresAccountPreferencesUseCurrentMember(t *testing.T) {
	repository := hubtest.Repository(t)
	now := time.Now().UTC()
	f, _ := summaryHTTPFixture(t, repository, "org_security", now)
	user := f.MemberSession(t, hubtest.MemberOptions{Role: "member", Email: "preferences@example.test"})
	other := f.MemberSession(t, hubtest.MemberOptions{Role: "member", Email: "other-preferences@example.test"})
	dailyPath := "/api/v2/organizations/org_security/daily-summary-preference"
	response := summaryRequest(f, t, user, http.MethodGet, dailyPath, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var daily Preference
	decodeSummaryResponse(t, response, &daily)
	if daily.Configured || daily.Enabled || daily.Recipient != "preferences@example.test" {
		t.Fatalf("default daily preference=%+v", daily)
	}
	response = summaryRequest(f, t, user, http.MethodPut, dailyPath, map[string]any{"enabled": true, "send_time": "06:30", "time_zone": "America/Chicago", "days": "weekdays"})
	requireNativeStatus(t, response, http.StatusOK)
	response = summaryRequest(f, t, user, http.MethodGet, dailyPath, nil)
	requireNativeStatus(t, response, http.StatusOK)
	decodeSummaryResponse(t, response, &daily)
	if !daily.Configured || !daily.Enabled || daily.SendTime != "06:30" || daily.TimeZone != "America/Chicago" || daily.Days != "weekdays" {
		t.Fatalf("saved daily preference=%+v", daily)
	}
	var member accessstore.Member
	if err := repository.ReadTenant(t.Context(), "org_security", corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		member, err = r.AccessMember(t.Context(), user.Member.Member.UserID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	preference, err := repository.DailySummaryPreference(t.Context(), "org_security", user.Member.Member.UserID, member.MembershipID, member.PrincipalID)
	if err != nil || preference.Token == "" {
		t.Fatalf("daily unsubscribe token=%q error=%v", preference.Token, err)
	}
	response = summaryRequest(f, t, user, http.MethodPut, dailyPath, map[string]any{"enabled": true, "send_time": "07:00", "time_zone": "UTC", "days": "every_day"})
	requireNativeStatus(t, response, http.StatusOK)
	response = summaryRequest(f, t, other, http.MethodPut, dailyPath, map[string]any{"enabled": true, "send_time": "08:00", "time_zone": "UTC", "days": "every_day"})
	requireNativeStatus(t, response, http.StatusOK)
	request := httptest.NewRequest(http.MethodGet, "/daily-summary/unsubscribe?token="+url.QueryEscape(preference.Token), nil)
	recorder := httptest.NewRecorder()
	f.Echo.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("unsubscribe status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	response = summaryRequest(f, t, user, http.MethodGet, dailyPath, nil)
	requireNativeStatus(t, response, http.StatusOK)
	decodeSummaryResponse(t, response, &daily)
	if daily.Enabled {
		t.Fatal("daily summary unsubscribe left preference enabled")
	}
	response = summaryRequest(f, t, other, http.MethodGet, dailyPath, nil)
	requireNativeStatus(t, response, http.StatusOK)
	decodeSummaryResponse(t, response, &daily)
	if !daily.Enabled || daily.SendTime != "08:00" || daily.Recipient != "other-preferences@example.test" {
		t.Fatalf("other member daily preference=%+v", daily)
	}

	if err := repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
		return w.RevokeAccessMember(t.Context(), user.Member.Member.UserID, now.Add(time.Second))
	}); err != nil {
		t.Fatal(err)
	}
	requireNativeStatus(t, summaryRequest(f, t, user, http.MethodGet, dailyPath, nil), http.StatusForbidden)
}
