package hostedplans

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestHostedPlanReportPaginatesGrantHistory(t *testing.T) {
	t.Parallel()
	f := newEntitlementFixture(t)
	config := hostedTestPlans(t, f, nil)
	path := "/api/v2/organizations/org_security/entitlements"
	until := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	for i := 1; i <= 3; i++ {
		grant := PlanCommand{ID: fmt.Sprintf("history-grant-%d", i), Action: "grant", ExpectedRevision: int64(i), GrantID: fmt.Sprintf("history-%d", i), Plan: config.Plans[1].PlanReference, Scope: []string{"projects"}, ExpiresAt: &until, Reason: "history example"}
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, testHostedPlanAdminToken, grant), http.StatusNoContent)
	}
	after := ""
	for i := 1; i <= 3; i++ {
		target := path + "?history_limit=1"
		if after != "" {
			target += "&history_after=" + after
		}
		response := performHubAPIRequest(t, f.service, http.MethodGet, target, testHostedPlanAdminToken, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var report EntitlementReport
		if err := json.Unmarshal(response.Body.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		if len(report.GrantHistory) != 1 || report.GrantHistory[0].ID != fmt.Sprintf("history-%d", i) {
			t.Fatalf("grant history page %d = %+v", i, report.GrantHistory)
		}
		if i < 3 && report.NextGrantHistory == "" {
			t.Fatalf("grant history page %d has no continuation", i)
		}
		if i == 3 && report.NextGrantHistory != "" {
			t.Fatalf("final grant history page has continuation %q", report.NextGrantHistory)
		}
		after = report.NextGrantHistory
	}
}

func TestHostedPlanReportListsOnlyActiveGrants(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		after func(t *testing.T, f *entitlementFixture, path string, clock *leaseTestClock)
	}{
		{name: "revoked", after: func(t *testing.T, f *entitlementFixture, path string, _ *leaseTestClock) {
			revoke := PlanCommand{ID: "report-revoke", Action: "revoke", ExpectedRevision: 2, GrantID: "comp_report", Reason: "partnership ended"}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, testHostedPlanAdminToken, revoke), http.StatusNoContent)
		}},
		{name: "expired", after: func(_ *testing.T, _ *entitlementFixture, _ string, clock *leaseTestClock) {
			clock.value = clock.value.Add(2 * time.Hour)
		}},
		{name: "not started", after: func(_ *testing.T, _ *entitlementFixture, _ string, clock *leaseTestClock) {
			clock.value = clock.value.Add(-time.Hour)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newEntitlementFixture(t)
			config := hostedTestPlans(t, f, nil)
			clock := &leaseTestClock{value: time.Now().UTC()}
			f.service.database.now = clock.Now
			path := "/api/v2/organizations/org_security/entitlements"
			until := clock.value.Add(time.Hour).Truncate(time.Second)
			grant := PlanCommand{ID: "report-grant", Action: "grant", ExpectedRevision: 1, GrantID: "comp_report", Plan: config.Plans[1].PlanReference, Scope: []string{"projects"}, ExpiresAt: &until, Reason: "design partner"}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, testHostedPlanAdminToken, grant), http.StatusNoContent)
			test.after(t, &f, path, clock)
			response := performHubAPIRequest(t, f.service, http.MethodGet, path, testHostedPlanAdminToken, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var report EntitlementReport
			if err := json.Unmarshal(response.Body.Bytes(), &report); err != nil {
				t.Fatal(err)
			}
			if len(report.Grants) != 0 {
				t.Fatalf("report grants = %+v, want none", report.Grants)
			}
		})
	}
}
