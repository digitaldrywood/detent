package hostedplans

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/testenv"
)

func TestRuntimeRefreshRequestsOverlap(t *testing.T) {
	release := make(chan struct{})
	probe := &runtimeReadProbe{release: release}
	s := runtimeProbeService(probe)
	e := echo.New()
	e.GET("/api/v2/organizations/:organization/entitlements/runtime", s.readHostedRuntime, s.refreshHostedRuntime)
	var wg sync.WaitGroup
	defer func() { close(release); wg.Wait() }()
	for range 2 {
		wg.Go(func() {
			req := httptest.NewRequest(http.MethodGet, "/api/v2/organizations/org_example/entitlements/runtime", nil)
			req.Header.Set("Authorization", "Bearer "+testHostedPlanAdminToken)
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("runtime status=%d", rec.Code)
			}
		})
	}
	testenv.WaitUntil(t, t.Context(), func() error {
		if got := probe.calls.Load(); got != 2 {
			return fmt.Errorf("concurrent requests entered %d runtime reads; want 2", got)
		}
		return nil
	})
}

func TestRuntimeRefreshRetainsSnapshotOnError(t *testing.T) {
	probe := &runtimeReadProbe{err: errors.New("runtime storage unavailable")}
	s := runtimeProbeService(probe)
	before := s.database.plansService().RuntimeState()
	e := echo.New()
	e.GET("/api/v2/organizations/:organization/entitlements/runtime", s.readHostedRuntime, s.refreshHostedRuntime)
	req := httptest.NewRequest(http.MethodGet, "/api/v2/organizations/org_example/entitlements/runtime", nil)
	req.Header.Set("Authorization", "Bearer "+testHostedPlanAdminToken)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || s.database.plansService().RuntimeState() != before {
		t.Fatalf("last good snapshot lost: status=%d", rec.Code)
	}
}

func TestRuntimeRefreshRejectsInvalidPublishedSnapshot(t *testing.T) {
	s := runtimeProbeService(&runtimeReadProbe{revision: "invalid"})
	before := s.database.plansService().RuntimeState()
	plans := PilotPlans()
	plans.WindowSeconds = 61
	raw, err := json.Marshal(HostedRuntimeUpdate{Plans: &plans})
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	close(release)
	s.database.billingAuditStore = &runtimeObjectProbe{release: release, raw: raw}
	if err := hubapi.FromFeatureError(s.database.plansService().RefreshRuntime(t.Context())); err == nil || s.database.plansService().RuntimeState() != before {
		t.Fatalf("invalid publication replaced valid snapshot: %v", err)
	}
}

func TestHostedPlanVersionCreatedAndGrantedWithoutRestart(t *testing.T) {
	t.Parallel()
	f := newEntitlementFixture(t)
	config := hostedTestPlans(t, f, nil)
	path := "/api/v2/organizations/org_security/entitlements"
	plan := HostedPlan{PlanReference: PlanReference{ID: "capacity_extension", Version: 1}, Name: "Capacity extension", Features: []string{"collaboration"}, Allowances: map[string]int64{"collaboration_bytes": 1 << 34}}
	create := PlanCommand{ID: "runtime-create", Action: "create_plan", ExpectedRevision: 1, Plan: plan.PlanReference, Definition: &plan, Reason: "runtime capacity"}
	for _, test := range []struct {
		name, token string
		command     PlanCommand
		want        int
	}{
		{"Hub admin cannot create", testHubAdminToken, create, http.StatusForbidden},
		{"create", testHostedPlanAdminToken, create, http.StatusNoContent},
		{"replay", testHostedPlanAdminToken, create, http.StatusNoContent},
		{"changed replay", testHostedPlanAdminToken, func() PlanCommand { changed := create; changed.Reason = "changed reason"; return changed }(), http.StatusConflict},
		{"immutable version", testHostedPlanAdminToken, func() PlanCommand {
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
	var report EntitlementReport
	if err := json.Unmarshal(reportResponse.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Revision != 2 || report.Base != config.Base || !slices.ContainsFunc(report.Plans, func(p HostedPlan) bool { return p.PlanReference == plan.PlanReference }) {
		t.Fatalf("runtime catalog or existing assignment changed: %+v", report)
	}
	grant := PlanCommand{ID: "runtime-grant", Action: "grant", ExpectedRevision: report.Revision, GrantID: "capacity_grant", Plan: plan.PlanReference, Scope: []string{"collaboration_bytes"}, Reason: "runtime capacity"}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, testHostedPlanAdminToken, grant), http.StatusNoContent)
	usage, err := hubapi.FeatureResult(f.service.database.plansService().PlanUsage(t.Context(), time.Now()))
	if err != nil || usage.Allowances["collaboration_bytes"] != 1<<34 || usage.Base != config.Base {
		t.Fatalf("allowance did not apply: %+v %v", usage, err)
	}
	var rows []billingstore.PlanAudit
	if err := hubapi.FromFeatureError(f.service.database.plansService().ReadBillingTenant(t.Context(), func(r tenantstore.Reader) error {
		var err error
		rows, err = r.BillingPlanAudits(t.Context(), 0, 100)
		return err
	})); err != nil {
		t.Fatal(err)
	}
	if len(rows) == 100 {
		t.Fatal("creation/grant audit population was clipped")
	}
	var audits int
	for _, row := range rows {
		if row.ActorID != "test-operator" || row.Action != "create_plan" && row.Action != "grant" {
			continue
		}
		raw, err := f.service.database.billingAuditStore.ReadDetail(t.Context(), string(f.service.accessOrganization()), row.Reference.ArchiveReference())
		if err != nil {
			t.Fatal(err)
		}
		var command PlanCommand
		if err := json.Unmarshal(raw, &command); err != nil {
			t.Fatal(err)
		}
		if command.Reason == "runtime capacity" {
			audits++
		}
	}
	if audits != 2 {
		t.Fatalf("creation/grant audit=%d %v", audits, nil)
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
			service, admin := newRuntimePostgresService(t, PilotPlans(), true)
			f := &entitlementFixture{service: service}
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
			if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM hosted_plans WHERE organization_id=$1 AND id=$2 AND version=$3", f.service.config.Hosted.OrganizationID, plan.ID, plan.Version).Scan(&stored); err != nil {
				t.Fatal(err)
			}
			audits = runtimeAuditCount(t, f.service, "reload", "")

			if !applied {
				if f.service.database.hostedRuntime.Load() != before || stored != 0 || audits != 0 || len(f.service.hostedBillingConfig().Prices) != 1 {
					t.Fatal("failed or validation-only reload changed persisted or runtime state")
				}
				return
			}
			readPlan := func() error {
				return hubapi.FromFeatureError(f.service.database.plansService().ReadBillingTenant(t.Context(), func(r tenantstore.Reader) error {
					_, err := hubapi.FeatureResult(f.service.database.plansService().ReadPlanRecord(t.Context(), r, plan.PlanReference))
					return err
				}))
			}
			if test.prepare {
				if f.service.database.hostedRuntime.Load() != before || len(f.service.hostedBillingConfig().Prices) != 1 {
					t.Fatal("preparation exposed policy or prices")
				}
				if err := readPlan(); err == nil {
					t.Fatal("preparation exposed a plan version")
				}
				if _, err := admin.ExecContext(t.Context(), "INSERT INTO hosted_runtime_publications(id) VALUES($1)", update.StageID); err != nil {
					t.Fatal(err)
				}
				if err := hubapi.FromFeatureError(f.service.database.plansService().RefreshRuntime(t.Context())); err != nil {
					t.Fatal(err)
				}

				if err := readPlan(); err != nil {
					t.Fatal(err)
				}
				audits = runtimeAuditCount(t, f.service, "prepare_reload", update.Reason)

			}
			if stored != 1 || audits != 1 || f.service.database.plansService().Plans().WindowSeconds != 120 || !slices.ContainsFunc(f.service.hostedBillingConfig().Prices, func(price HostedBillingPrice) bool { return price.PriceID == "price_runtime" }) {
				t.Fatal("valid reload did not publish plan and prices")
			}
			command := PlanCommand{ID: "assign-reloaded", Action: "base", ExpectedRevision: 1, Plan: plan.PlanReference, Reason: "reloaded capacity"}
			if err := hubapi.FromFeatureError(f.service.database.plansService().ApplyPlanCommand(t.Context(), "test-operator", command)); err != nil {
				t.Fatal(err)
			}
			var entitlement HostedEntitlement
			err := hubapi.FromFeatureError(f.service.database.plansService().ReadBillingTenant(t.Context(), func(r tenantstore.Reader) error {
				var err error
				entitlement, err = hubapi.FeatureResult(f.service.database.plansService().EntitlementRecord(t.Context(), r, time.Now()))
				return err
			}))
			if err != nil || entitlement.Allowances["collaboration_bytes"] != 1<<34 {
				t.Fatalf("reloaded allowance=%+v %v", entitlement, err)
			}
			command.ID, command.Action, command.ExpectedRevision = "stripe-ownership", "subscription", 2
			if err := hubapi.FromFeatureError(f.service.database.plansService().ApplyPlanCommand(t.Context(), "test-operator", command)); err == nil {
				t.Fatal("reload bypassed Stripe subscription ownership")
			}
			previous.Plans.Plans = append(previous.Plans.Plans, plan)
			previous.Reason = "SIGHUP reload failed; restore previous runtime settings"
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, testHostedPlanAdminToken, previous), http.StatusNoContent)
			if f.service.database.plansService().Plans().WindowSeconds != before.Plans.WindowSeconds || !slices.Equal(f.service.hostedBillingConfig().Prices, previous.Prices) {
				t.Fatal("rollback did not restore the prior policy and active price list")
			}
			err = hubapi.FromFeatureError(f.service.database.plansService().ReadBillingTenant(t.Context(), func(r tenantstore.Reader) error {
				var err error
				entitlement, err = hubapi.FeatureResult(f.service.database.plansService().EntitlementRecord(t.Context(), r, time.Now()))
				return err
			}))
			if err != nil || entitlement.Base != plan.PlanReference || entitlement.Allowances["collaboration_bytes"] != 1<<34 {
				t.Fatalf("rollback changed an independently assigned version: %+v %v", entitlement, err)
			}
			if audits = runtimeAuditCount(t, f.service, "reload", previous.Reason); audits != 1 {
				t.Fatalf("rollback audit=%d", audits)
			}

		})
	}
}

func TestHostedRuntimeReloadCatalogTransitions(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		plans *HostedPlansConfig
	}{
		{name: "omitted catalog"},
		{name: "empty catalog", plans: &HostedPlansConfig{}},
		{name: "partial catalog", plans: &HostedPlansConfig{WindowSeconds: 120}},
	} {
		for _, prepare := range []bool{false, true} {
			name := test.name
			if prepare {
				name += "/prepared"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				s, _ := newRuntimePostgresService(t, CapacityPlans(), false)
				before := s.database.plansService().RuntimeState()
				update := HostedRuntimeUpdate{Plans: test.plans, Reason: "catalog transition"}
				if prepare {
					s.config.Hosted.SharedEntry = &HostedSharedEntry{}
					update.StageID = "00000000-0000-4000-8000-000000000002"
				}
				err := s.applyHostedRuntime(t.Context(), &update)
				if test.plans != nil && test.plans.WindowSeconds != 0 {
					if err == nil || s.database.plansService().RuntimeState() != before {
						t.Fatalf("invalid catalog changed runtime: %v", err)
					}
					return
				}
				if err != nil {
					t.Fatalf("catalog without plans did not default to the free plan: %v", err)
				}
				after := s.database.plansService().RuntimeState()
				if prepare {
					if after != before {
						t.Fatal("prepared default catalog changed the runtime before publication")
					}
					return
				}
				if after == before || after.Plans.Base != Tiers()[0].PlanReference || len(after.Plans.Plans) != len(CapacityPlans().Plans) {
					t.Fatalf("catalog without plans runtime=%+v, want the free default catalog", after)
				}
			})
		}
	}
}
