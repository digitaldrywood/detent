package hostedplans

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
)

func TestHostedPlanReportRequiresEntitlementAdministrator(t *testing.T) {
	t.Parallel()
	f := newEntitlementSessionFixture(t)
	config := hostedTestPlans(t, f, nil)
	owner := f.user(t, "owner", "owner", "owner@example.test", "", "")
	path := "/api/v2/organizations/org_security/entitlements"
	until := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	grant := PlanCommand{ID: "report-grant", Action: "grant", ExpectedRevision: 1, GrantID: "comp_report", Plan: config.Plans[1].PlanReference, Scope: []string{"projects", "hosted_artifacts"}, ExpiresAt: &until, Reason: "design partner"}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, testHostedPlanAdminToken, grant), http.StatusNoContent)
	for _, test := range []struct {
		name     string
		response func() int
		want     int
	}{
		{"owner session", func() int { return f.request(t, owner, http.MethodGet, path, nil).Code }, http.StatusForbidden},
		{"hub admin token", func() int {
			return performHubAPIRequest(t, f.service, http.MethodGet, path, testHubAdminToken, nil).Code
		}, http.StatusForbidden},
		{"entitlement administrator", func() int {
			return performHubAPIRequest(t, f.service, http.MethodGet, path, testHostedPlanAdminToken, nil).Code
		}, http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := test.response(); got != test.want {
				t.Fatalf("status = %d, want %d", got, test.want)
			}
		})
	}
	response := performHubAPIRequest(t, f.service, http.MethodGet, path, testHostedPlanAdminToken, nil)
	var report EntitlementReport
	if err := json.Unmarshal(response.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Revision != 2 || report.Base != config.Base || report.EffectiveBase != config.Base || !slices.ContainsFunc(report.Plans, func(plan HostedPlan) bool { return plan.PlanReference == config.Plans[1].PlanReference }) || len(report.Grants) != 1 {
		t.Fatalf("report = %+v", report)
	}
	recorded := report.Grants[0]
	if recorded.ID != "comp_report" || recorded.Reason != "design partner" || recorded.GrantedBy != "test-operator" || recorded.GrantedAt == nil || recorded.ExpiresAt == nil || !recorded.ExpiresAt.Equal(until) || recorded.Plan != config.Plans[1].PlanReference {
		t.Fatalf("grant = %+v", recorded)
	}
}

func TestHostedPlanPages(t *testing.T) {
	t.Parallel()
	f := newEntitlementSessionFixture(t)
	hostedTestPlans(t, f, map[string]int64{"projects": 0})
	owner := f.user(t, "owner", "owner", "owner@example.test", "", "")
	viewer := f.user(t, "viewer", "viewer", "viewer@example.test", "", "")
	for _, test := range []struct {
		name string
		user hubtest.HTTPActor
		want int
	}{
		{"owner sees downgrade", owner, http.StatusOK},
		{"viewer cannot inspect organization allowances", viewer, http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := f.request(t, test.user, http.MethodGet, "/organization/plan", nil)
			requireNativeStatus(t, response, test.want)
			if test.want == http.StatusOK {
				for _, expected := range []string{"Above allowance", "Maximum:", "separate from charges by your model providers", "Billing usage export"} {
					if !strings.Contains(response.Body.String(), expected) {
						t.Errorf("plan page omitted %q", expected)
					}
				}
			}
		})
	}
}
