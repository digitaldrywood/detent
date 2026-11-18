package billingcosts_test

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestSpriteUsageAPI(t *testing.T) {
	t.Parallel()
	from := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	to, now := from.Add(time.Hour), from.Add(2*time.Hour)
	f := newDefaultNativeFixture(t, Config{Clock: func() time.Time { return now }})
	f.service.costRoutes = true
	runner := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat)
	runner.enroll(t)
	membership := runnerstore.SpriteMembership{OrganizationID: f.project.OrganizationID, TokenProjectID: f.project.ID, Name: "worker", ProviderOrganization: "customer", EnrollmentID: runner.enrollment.ID, State: "enrolled", IdleSince: from, CreatedAt: from}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		return w.CreateRunnerSpriteMembership(t.Context(), membership)
	}); err != nil {
		t.Fatal(err)
	}
	admin := usagePostgresAdmin(t, f.service)
	other := newNativeFixture(t, f.service, f.project.OrganizationID, "other")
	request := spriteUsageRequest{Mutation: tracker.Mutation{IdempotencyKey: "usage-import"}, Observations: []costObservation{costTestObservation("cpu", from, to), costTestObservation("storage_cold", from, to)}}
	path := f.base + "/usage/sprites"
	var unmetered monthlyCostReport
	response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/usage/monthly?month=2026-02", f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &unmetered)
	if len(unmetered.Gaps) != 4 || unmetered.Coverage != "unknown" || len(unmetered.Totals) != 0 {
		t.Fatalf("unmetered pool invented costs: %+v", unmetered)
	}
	response = performHubAPIRequest(t, f.service, http.MethodPost, path, testHubAdminToken, request)
	requireNativeStatus(t, response, http.StatusOK)
	replay := performHubAPIRequest(t, f.service, http.MethodPost, path, testHubAdminToken, request)
	requireNativeStatus(t, replay, http.StatusOK)
	if response.Body.String() != replay.Body.String() {
		t.Fatal("business replay changed receipt")
	}
	request.IdempotencyKey = "new-import-key"
	response = performHubAPIRequest(t, f.service, http.MethodPost, path, testHubAdminToken, request)
	requireNativeStatus(t, response, http.StatusOK)
	var receipt struct {
		Recorded int `json:"recorded"`
	}
	decodeHubResponse(t, response, &receipt)
	if receipt.Recorded != 0 {
		t.Fatalf("source retry added cost: %s", response.Body.String())
	}

	args, err := json.Marshal(operatortool.ProjectRequest[operatortool.SpriteUsageInput]{ProjectID: string(f.project.ID), RequestID: "mcp-source-retry", Input: operatortool.SpriteUsageInput{Observations: request.Observations}})
	if err != nil {
		t.Fatal(err)
	}
	action := projectAction(t, f, operatortool.Call{Name: "import_sprite_usage", Arguments: args})
	if action.Status != "succeeded" || action.Result != "{\"recorded\":0}" {
		t.Fatalf("MCP importer did not share API deduplication: %+v", action)
	}
	result, err := callTool(t, f, operatortool.Call{Name: "monthly_usage_costs", Arguments: json.RawMessage(`{"scope":"organization","month":"2026-02"}`)})
	if err != nil {
		t.Fatal(err)
	}
	var toolReport monthlyCostReport
	if err := json.Unmarshal(result.Content, &toolReport); err != nil || *toolReport.Totals[0].KnownMicros != 40_000 {
		t.Fatalf("MCP monthly total: %s %v", result.Content, err)
	}
	for _, test := range []struct {
		name, token, path string
		status            int
		want              int64
	}{
		{name: "own project", token: f.token, path: f.base + "/usage/monthly?month=2026-02", status: http.StatusOK, want: 40_000},
		{name: "runner budget consumer reads project", token: runner.redemption.Credential, path: f.base + "/usage/monthly?month=2026-02", status: http.StatusOK, want: 40_000},
		{name: "other project", token: other.token, path: other.base + "/usage/monthly?month=2026-02", status: http.StatusOK},
		{name: "project grant cannot read sibling", token: other.token, path: f.base + "/usage/monthly?month=2026-02", status: http.StatusNotFound},
		{name: "org total counts project once", token: testHubAdminToken, path: "/api/v2/organizations/" + string(f.project.OrganizationID) + "/usage/monthly?month=2026-02", status: http.StatusOK, want: 40_000},
		{name: "operator cannot read org total", token: f.token, path: "/api/v2/organizations/" + string(f.project.OrganizationID) + "/usage/monthly?month=2026-02", status: http.StatusForbidden},
		{name: "bad month", token: f.token, path: f.base + "/usage/monthly?month=2026-13", status: http.StatusUnprocessableEntity},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := performHubAPIRequest(t, f.service, http.MethodGet, test.path, test.token, nil)
			requireNativeStatus(t, response, test.status)
			if test.status != http.StatusOK {
				return
			}
			var report monthlyCostReport
			decodeHubResponse(t, response, &report)
			if test.want == 0 {
				if len(report.Totals) != 0 || report.Coverage != "unknown" {
					t.Fatalf("empty scope invented usage: %+v", report)
				}
				return
			}
			if *report.Totals[0].KnownMicros != test.want || len(report.ByProject) != 1 || report.Period.From != from {
				t.Fatalf("wrong monthly total: %+v", report)
			}
		})
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.token, request), http.StatusForbidden)
	request.IdempotencyKey = "atomic-import"
	next := costTestObservation("ram", from, to)
	conflict := costTestObservation("cpu", from, to)
	conflict.Coverage = "partial"
	request.Observations = []costObservation{next, conflict}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, testHubAdminToken, request), http.StatusConflict)
	var count int
	if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM cost_sources WHERE organization_id=$1 AND metric='ram'", f.project.OrganizationID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed batch partially committed: count=%d error=%v", count, err)
	}
	membership.State = "deleted"
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		return w.UpdateRunnerSpriteMembership(t.Context(), membership)
	}); err != nil {
		t.Fatal(err)
	}
	window, _ := costMonth("2026-02", now)
	report, err := f.service.database.monthlyCosts(t.Context(), string(f.project.OrganizationID), "organization", nil, window, now)
	if err != nil || len(report.Sources) != 2 || report.Sources[0].ProjectID != string(f.project.ID) || *report.Totals[0].KnownMicros != 40_000 {
		t.Fatalf("deleted Sprite attribution was lost: %+v %v", report, err)
	}
	if _, err := admin.ExecContext(t.Context(), "DELETE FROM runner_sprite_memberships WHERE organization_id=$1", f.project.OrganizationID); err != nil {
		t.Fatal(err)
	}
	after, err := f.service.database.monthlyCosts(t.Context(), string(f.project.OrganizationID), "organization", nil, window, now)
	if err != nil || !reflect.DeepEqual(report, after) {
		t.Fatalf("inventory cleanup changed historical costs: %+v %v", after, err)
	}
}

func TestHostedMonthlyCostsAccess(t *testing.T) {
	t.Parallel()
	f := newUsageHostedFixture(t)
	now := f.service.config.Clock()
	window, err := costMonth("", now)
	if err != nil {
		t.Fatal(err)
	}
	for index, project := range []string{f.project, f.privateProject} {
		o := costTestObservation("storage_cold", window.From, now)
		o.ResourceID += project
		o.SourceID += project
		q := float64(index + 1)
		o.Quantity = &q
		if _, err := recordPostgresTestCost(t, f.service.database, f.service.config.Hosted.OrganizationID, project, o, now); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		account string
		want    int64
	}{
		{account: "owner", want: 30_000},
		{account: "viewer", want: 10_000},
	} {
		t.Run(test.account, func(t *testing.T) {
			var report usageReport
			response := f.usage(t, test.account, browserHostedOrganizationBase+"/usage?range=month:"+now.UTC().Format("2006-01"), http.StatusOK)
			if err := json.Unmarshal(response.Body.Bytes(), &report); err != nil {
				t.Fatal(err)
			}
			if report.MonthlyCosts == nil || *report.MonthlyCosts.Totals[0].KnownMicros != test.want || report.MonthlyCosts.Scope != "readable_projects" || report.MonthlyCosts.Period.From != window.From {
				t.Fatalf("monthly access report: %+v", report.MonthlyCosts)
			}
			if report.Total.Cost == float64(test.want)/1_000_000 {
				t.Fatal("infrastructure cost was folded into AI estimates")
			}
		})
	}
	var organization monthlyCostReport
	response := f.usage(t, "owner", browserHostedOrganizationBase+"/usage/monthly?month="+now.UTC().Format("2006-01"), http.StatusOK)
	decodeHubResponse(t, response, &organization)
	if organization.Scope != "organization" || *organization.Totals[0].KnownMicros != 30_000 {
		t.Fatalf("organization ledger: %+v", organization)
	}
	f.usage(t, "viewer", browserHostedOrganizationBase+"/usage/monthly", http.StatusNotFound)
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), tracker.OrganizationID(f.service.config.Hosted.OrganizationID), corestore.Serving, func(w tenantstore.Writer) error {
		return w.DeleteAccessProjectGrant(t.Context(), f.actors["owner"].Member.Member.UserID, tracker.ProjectID(f.privateProject))
	}); err != nil {
		t.Fatal(err)
	}
	f.usage(t, "owner", browserHostedOrganizationBase+"/usage/monthly", http.StatusOK)
}
