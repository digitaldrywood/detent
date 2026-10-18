package hubserver

import (
	"encoding/json"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestHostedPlanVersionCreatedAndGrantedWithoutRestart(t *testing.T) {
	t.Parallel()
	f := newHostedSecurityFixture(t)
	config := hostedTestPlans(t, f.service, nil)
	path := "/api/v2/organizations/org_security/entitlements"
	plan := HostedPlan{PlanReference: PlanReference{ID: "capacity_extension", Version: 1}, Name: "Capacity extension", Features: []string{"collaboration"}, Allowances: map[string]int64{"collaboration_bytes": 1 << 34}}
	create := hostedPlanCommand{ID: "runtime-create", Action: "create_plan", ExpectedRevision: 1, Plan: plan.PlanReference, Definition: &plan, Reason: "runtime capacity"}
	for _, test := range []struct {
		name, token string
		command     hostedPlanCommand
		want        int
	}{
		{"Hub admin cannot create", testHubAdminToken, create, http.StatusForbidden},
		{"create", testHostedPlanAdminToken, create, http.StatusNoContent},
		{"replay", testHostedPlanAdminToken, create, http.StatusNoContent},
		{"changed replay", testHostedPlanAdminToken, func() hostedPlanCommand { changed := create; changed.Reason = "changed reason"; return changed }(), http.StatusConflict},
		{"immutable version", testHostedPlanAdminToken, func() hostedPlanCommand {
			changed := create
			changed.ID = "overwrite"
			changed.ExpectedRevision = 2
			return changed
		}(), http.StatusConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, test.token, test.command), test.want)
		})
	}
	reportResponse := performHubAPIRequest(t, f.service, http.MethodGet, path, testHostedPlanAdminToken, nil)
	requireNativeStatus(t, reportResponse, http.StatusOK)
	var report hostedEntitlementReport
	if err := json.Unmarshal(reportResponse.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Revision != 2 || report.Base != config.Base || !slices.ContainsFunc(report.Plans, func(p HostedPlan) bool { return p.PlanReference == plan.PlanReference }) {
		t.Fatalf("runtime catalog or existing assignment changed: %+v", report)
	}
	grant := hostedPlanCommand{ID: "runtime-grant", Action: "grant", ExpectedRevision: report.Revision, GrantID: "capacity_grant", Plan: plan.PlanReference, Scope: []string{"collaboration_bytes"}, Reason: "runtime capacity"}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, testHostedPlanAdminToken, grant), http.StatusNoContent)
	usage, err := f.service.database.hostedPlanUsage(t.Context(), time.Now())
	if err != nil || usage.Allowances["collaboration_bytes"] != 1<<34 || usage.Base != config.Base {
		t.Fatalf("allowance did not apply: %+v %v", usage, err)
	}
	var audits int
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM hosted_plan_audit WHERE actor_id='test-operator' AND reason='runtime capacity' AND action IN ('create_plan','grant')").Scan(&audits); err != nil || audits != 2 {
		t.Fatalf("creation/grant audit=%d %v", audits, err)
	}
}

func TestHostedRuntimeReloadAtomicPolicyAndPrices(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name                              string
		invalid, remap, validate, prepare bool
	}{
		{name: "new version and price"},
		{name: "prepared policy and prices publish together", prepare: true},
		{name: "invalid policy", invalid: true},
		{name: "immutable price mapping", remap: true},
		{name: "validation changes nothing", validate: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f, _ := newHostedBillingFixture(t)
			before := f.service.database.hostedRuntime.Load()
			plans := *f.service.config.Hosted.Plans
			plan := plans.Plans[1]
			plan.Version++
			plan.Allowances = maps.Clone(plan.Allowances)
			plan.Allowances["collaboration_bytes"] = 1 << 34
			plans.Plans = append(slices.Clone(plans.Plans), plan)
			plans.WindowSeconds = 120
			if test.invalid {
				plans.ConnectedSeconds = 0
			}
			prices := append(slices.Clone(f.service.hostedBillingConfig().Prices), HostedBillingPrice{PriceID: "price_runtime", Label: "Extended capacity", Plan: plan.PlanReference})
			if test.remap {
				prices[0].Plan = plan.PlanReference
			}
			update := HostedRuntimeUpdate{Plans: &plans, Prices: prices, Reason: "SIGHUP regression", ValidateOnly: test.validate}
			if test.prepare {
				f.service.config.Hosted.SharedEntry = &HostedSharedEntry{}
				f.service.database.configureRuntimePublication(f.service.config)
				update.StageID = "00000000-0000-4000-8000-000000000001"
			}
			path := "/api/v2/organizations/org_browser_preview/entitlements/runtime"
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, path, testHubAdminToken, nil), http.StatusForbidden)
			snapshot := performHubAPIRequest(t, f.service, http.MethodGet, path, testHostedPlanAdminToken, nil)
			requireNativeStatus(t, snapshot, http.StatusOK)
			var previous HostedRuntimeUpdate
			if err := json.Unmarshal(snapshot.Body.Bytes(), &previous); err != nil {
				t.Fatal(err)
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, testHubAdminToken, update), http.StatusForbidden)
			response := performHubAPIRequest(t, f.service, http.MethodPost, path, testHostedPlanAdminToken, update)
			applied := !test.invalid && !test.remap && !test.validate
			if !test.invalid && !test.remap {
				want := http.StatusNoContent
				if test.prepare {
					want = http.StatusOK
				}
				requireNativeStatus(t, response, want)
			} else if response.Code < 400 {
				t.Fatalf("invalid reload accepted: %d", response.Code)
			}
			var stored, audits int
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT (SELECT count(*) FROM hosted_plans WHERE id=? AND version=?), (SELECT count(*) FROM hosted_plan_audit WHERE action='reload')", plan.ID, plan.Version).Scan(&stored, &audits); err != nil {
				t.Fatal(err)
			}
			if !applied {
				if f.service.database.hostedRuntime.Load() != before || stored != 0 || audits != 0 || len(f.service.hostedBillingConfig().Prices) != 1 {
					t.Fatal("failed or validation-only reload changed persisted or runtime state")
				}
				return
			}
			if test.prepare {
				if f.service.database.hostedRuntime.Load() != before || len(f.service.hostedBillingConfig().Prices) != 1 {
					t.Fatal("preparation exposed policy or prices")
				}
				if _, err := f.service.database.readHostedPlan(t.Context(), f.service.database.db, plan.PlanReference); err == nil {
					t.Fatal("preparation exposed a plan version")
				}
				publication := HostedRuntimePublication{Published: []string{update.StageID}, Update: update, Tenants: map[string]HostedRuntimeUpdate{"org_browser_preview": update}}
				raw, err := json.Marshal(publication)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(filepath.Dir(f.service.database.path), HostedRuntimeFilename), raw, 0o600); err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.readHostedPlan(t.Context(), f.service.database.db, plan.PlanReference); err != nil {
					t.Fatal(err)
				}
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM hosted_plan_audit WHERE action='prepare_reload' AND actor_id='test-operator' AND reason=?", update.Reason).Scan(&audits); err != nil {
					t.Fatal(err)
				}
			}
			if stored != 1 || audits != 1 || f.service.database.plans().WindowSeconds != 120 || !slices.ContainsFunc(f.service.hostedBillingConfig().Prices, func(price HostedBillingPrice) bool { return price.PriceID == "price_runtime" }) {
				t.Fatal("valid reload did not publish plan and prices")
			}
			command := hostedPlanCommand{ID: "assign-reloaded", Action: "base", ExpectedRevision: 1, Plan: plan.PlanReference, Reason: "reloaded capacity"}
			if err := f.service.database.applyHostedPlanCommand(t.Context(), "test-operator", command); err != nil {
				t.Fatal(err)
			}
			entitlement, err := f.service.database.hostedEntitlement(t.Context(), f.service.database.db, time.Now())
			if err != nil || entitlement.Allowances["collaboration_bytes"] != 1<<34 {
				t.Fatalf("reloaded allowance=%+v %v", entitlement, err)
			}
			command.ID, command.Action, command.ExpectedRevision = "stripe-ownership", "subscription", 2
			if err := f.service.database.applyHostedPlanCommand(t.Context(), "test-operator", command); err == nil {
				t.Fatal("reload bypassed Stripe subscription ownership")
			}
			previous.Reason = "SIGHUP reload failed; restore previous runtime settings"
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, testHostedPlanAdminToken, previous), http.StatusNoContent)
			if f.service.database.plans().WindowSeconds != before.Plans.WindowSeconds || !slices.Equal(f.service.hostedBillingConfig().Prices, previous.Prices) {
				t.Fatal("rollback did not restore the prior policy and active price list")
			}
			entitlement, err = f.service.database.hostedEntitlement(t.Context(), f.service.database.db, time.Now())
			if err != nil || entitlement.Base != plan.PlanReference || entitlement.Allowances["collaboration_bytes"] != 1<<34 {
				t.Fatalf("rollback changed an independently assigned immutable version: %+v %v", entitlement, err)
			}
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM hosted_plan_audit WHERE action='reload' AND actor_id='test-operator' AND reason=?", previous.Reason).Scan(&audits); err != nil || audits != 1 {
				t.Fatalf("rollback audit=%d %v", audits, err)
			}
		})
	}
}
