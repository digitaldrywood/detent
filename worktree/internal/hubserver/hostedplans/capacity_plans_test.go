package hostedplans

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestCapacityLegacyAssignmentsAndGrants(t *testing.T) {
	f := newEntitlementFixture(t)
	d := f.service.database
	base := PlanReference{ID: "pilot_free", Version: 1}
	readPlan := func() billingstore.Plan {
		t.Helper()
		var plan billingstore.Plan
		err := hubapi.FromFeatureError(d.plansService().ReadBillingTenant(t.Context(), func(r tenantstore.Reader) error {
			var err error
			plan, err = r.BillingPlan(t.Context(), billingstore.PlanReference{ID: base.ID, Version: base.Version})
			return err
		}))
		if err != nil {
			t.Fatal(err)
		}
		return plan
	}
	readGrant := func() billingstore.Grant {
		t.Helper()
		var grant billingstore.Grant
		err := hubapi.FromFeatureError(d.plansService().ReadBillingTenant(t.Context(), func(r tenantstore.Reader) error {
			var err error
			grant, err = r.BillingGrant(t.Context(), "legacy")
			return err
		}))
		if err != nil {
			t.Fatal(err)
		}
		return grant
	}
	original := readPlan()
	for _, test := range []struct {
		ref     PlanReference
		limit   int64
		history int64
	}{
		{PlanReference{ID: "pilot_free", Version: 1}, 10, 10000},
		{PlanReference{ID: "comp_team", Version: 1}, 20, 20000},
	} {
		t.Run(test.ref.ID+" before capacity publication", func(t *testing.T) {
			plan, err := hubapi.FeatureResult(d.plansService().ReadPlan(t.Context(), nil, test.ref))
			if err != nil {
				t.Fatal(err)
			}
			if plan.Name != "" || plan.Allowances["projects"] != test.limit || plan.Allowances["history_records"] != test.history {
				t.Fatalf("missing starter changed legacy plan: %+v", plan)
			}
		})
	}
	if err := hubapi.FromFeatureError(d.plansService().ApplyPlanCommand(t.Context(), bootstrapTokenID, PlanCommand{ID: "legacy-grant", Action: "grant", ExpectedRevision: 1, GrantID: "legacy", Plan: PlanReference{ID: "comp_team", Version: 1}, Scope: []string{"projects", "native_execution", "collaboration_bytes"}, Reason: "existing complimentary access"})); err != nil {
		t.Fatal(err)
	}
	originalGrant := readGrant()

	plans := CapacityPlans()
	if err := hubapi.FromFeatureError(d.plansService().ConfigurePlans(t.Context(), PlansInput(&HostedConfig{Plans: &plans}))); err != nil {
		t.Fatal(err)
	}
	retained := readPlan()
	if retainedGrant := readGrant(); !reflect.DeepEqual(retainedGrant, originalGrant) {
		t.Fatalf("capacity seeding rewrote the legacy grant: before=%+v after=%+v", originalGrant, retainedGrant)
	}

	entitlement, err := hubapi.FeatureResult(d.plansService().Entitlement(t.Context(), nil, time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	if state := d.plansService().RuntimeState(); state == nil || retained.RuntimeID != state.Revision {
		t.Fatalf("capacity seeding did not bind the retained legacy plan to its runtime: %+v", retained)
	}
	original.RuntimeID = retained.RuntimeID
	if !reflect.DeepEqual(retained, original) {
		t.Fatalf("capacity seeding rewrote the legacy plan: before=%+v after=%+v", original, retained)
	}
	if entitlement.Base != base || len(entitlement.Grants) != 1 || entitlement.Allowances["unarchived_issues"] != 10000 || !slices.Contains(entitlement.Features, "native_execution") {
		t.Fatalf("legacy migration = %#v", entitlement)
	}
	if entitlement.Allowances["collaboration_bytes"] != 128<<20 {
		t.Fatalf("legacy storage changed: %d", entitlement.Allowances["collaboration_bytes"])
	}
	for _, test := range []struct {
		ref   PlanReference
		bytes int64
	}{
		{PlanReference{ID: "pilot_free", Version: 1}, 64 << 20}, {PlanReference{ID: "comp_team", Version: 1}, 128 << 20},
		{PlanReference{ID: "free", Version: 1}, 1 << 30}, {PlanReference{ID: "starter", Version: 1}, 25 << 30}, {PlanReference{ID: "team", Version: 2}, 100 << 30}, {PlanReference{ID: "growth", Version: 2}, 250 << 30}, {PlanReference{ID: "scale", Version: 2}, 1 << 30},
	} {
		t.Run(test.ref.ID, func(t *testing.T) {
			plan, err := hubapi.FeatureResult(d.plansService().ReadPlan(t.Context(), nil, test.ref))
			if err != nil {
				t.Fatal(err)
			}
			if plan.Allowances["collaboration_bytes"] != test.bytes {
				t.Fatalf("storage=%d, want %d", plan.Allowances["collaboration_bytes"], test.bytes)
			}
			if test.ref.ID == "pilot_free" || test.ref.ID == "comp_team" {
				name := "Legacy complimentary pilot"
				if test.ref.ID == "comp_team" {
					name = "Legacy Team"
				}
				if plan.Name != name || plan.Allowances["unarchived_issues"] != 10000 || plan.Allowances["history_records"] != 2000000 {
					t.Fatalf("legacy read normalization=%+v, want name=%q and current floors", plan, name)
				}
				for _, resource := range []string{"members", "projects", "repositories", "registered_runners", "connected_runners", "concurrent_work"} {
					if _, limited := plan.Allowances[resource]; limited {
						t.Fatalf("legacy read still limits %s", resource)
					}
				}
			}
		})
	}

	for _, resource := range []string{"members", "projects", "concurrent_work"} {
		if _, limited := entitlement.Allowances[resource]; limited {
			t.Fatalf("legacy still limits %s", resource)
		}
	}
	t.Run("comp_team base", func(t *testing.T) {
		if err := hubapi.FromFeatureError(d.plansService().ApplyPlanCommand(t.Context(), bootstrapTokenID, PlanCommand{ID: "legacy-comp-base", Action: "base", ExpectedRevision: entitlement.Revision, Plan: PlanReference{ID: "comp_team", Version: 1}, Reason: "existing Team access"})); err != nil {
			t.Fatal(err)
		}
		team, err := hubapi.FeatureResult(d.plansService().Entitlement(t.Context(), nil, time.Now()))
		if err != nil {
			t.Fatal(err)
		}
		if team.Base != (PlanReference{ID: "comp_team", Version: 1}) || team.Name != "Legacy Team" || team.Allowances["history_records"] != 2000000 || team.Allowances["collaboration_bytes"] != 128<<20 || !slices.Contains(team.Features, "native_execution") {
			t.Fatalf("legacy Team base normalization=%+v", team)
		}
		for _, resource := range []string{"members", "projects", "repositories", "registered_runners", "connected_runners", "concurrent_work"} {
			if _, limited := team.Allowances[resource]; limited {
				t.Fatalf("legacy Team base still limits %s", resource)
			}
		}
		if retained := readPlan(); !reflect.DeepEqual(retained, original) {
			t.Fatalf("base change rewrote immutable pilot plan: before=%+v after=%+v", original, retained)
		}
		if retained := readGrant(); !reflect.DeepEqual(retained, originalGrant) {
			t.Fatalf("base change rewrote legacy grant: before=%+v after=%+v", originalGrant, retained)
		}
	})
	t.Run("unpublished starter retains legacy limits", func(t *testing.T) {
		f := newEntitlementFixture(t)
		d := f.service.database
		const runtimeID = "runtime_unpublished_normalization"
		if err := hubapi.FromFeatureError(d.plansService().WriteBillingTenant(t.Context(), func(w tenantstore.Writer) error {
			if err := w.StageRuntime(t.Context(), billingstore.RuntimeRevision{ID: runtimeID}); err != nil {
				return err
			}
			return w.CreateBillingPlan(t.Context(), billingstore.Plan{OrganizationID: d.hostedOrganization, PlanReference: billingstore.PlanReference{ID: "starter", Version: 1}, RuntimeID: runtimeID, Name: "Example unpublished starter", Allowances: map[string]int64{"projects": 1}})
		})); err != nil {
			t.Fatal(err)
		}
		if err := hubapi.FromFeatureError(d.plansService().ReadBillingTenant(t.Context(), func(r tenantstore.Reader) error {
			visible, err := r.RuntimePublished(t.Context(), runtimeID)
			if visible {
				t.Fatal("staged starter is already published")
			}
			return err
		})); err != nil {
			t.Fatal(err)
		}
		for _, test := range []struct {
			ref               PlanReference
			projects, history int64
		}{
			{PlanReference{ID: "pilot_free", Version: 1}, 10, 10000},
			{PlanReference{ID: "comp_team", Version: 1}, 20, 20000},
		} {
			plan, err := hubapi.FeatureResult(d.plansService().ReadPlan(t.Context(), nil, test.ref))
			if err != nil {
				t.Fatal(err)
			}
			if plan.Name != "" || plan.Allowances["projects"] != test.projects || plan.Allowances["history_records"] != test.history {
				t.Fatalf("unpublished starter normalized legacy plan: %+v", plan)
			}
		}
	})
	t.Run("capacity tier read preserves stored plan", func(t *testing.T) {
		f := newEntitlementFixture(t, func(cfg *hubConfig) {
			plans := CapacityPlans()
			for index := range plans.Plans {
				plan := &plans.Plans[index]
				if plan.PlanReference == (PlanReference{ID: "free", Version: 1}) || plan.PlanReference == (PlanReference{ID: "team", Version: 2}) {
					plan.Allowances["unarchived_issues"], plan.Allowances["concurrent_work"] = 31, 61
					for _, resource := range []string{"members", "projects", "repositories", "registered_runners", "connected_runners"} {
						plan.Allowances[resource] = 3
					}
				}
			}
			cfg.Hosted.Plans = &plans
		})
		d := f.service.database
		for _, test := range []struct {
			ref        PlanReference
			concurrent int64
		}{
			{PlanReference{ID: "free", Version: 1}, 2},
			{PlanReference{ID: "team", Version: 2}, 20},
		} {
			var raw billingstore.Plan
			if err := hubapi.FromFeatureError(d.plansService().ReadBillingTenant(t.Context(), func(r tenantstore.Reader) error {
				var err error
				raw, err = r.BillingPlan(t.Context(), billingstore.PlanReference{ID: test.ref.ID, Version: test.ref.Version})
				return err
			})); err != nil {
				t.Fatal(err)
			}
			if raw.Allowances["unarchived_issues"] != 31 || raw.Allowances["concurrent_work"] != 61 || raw.Allowances["projects"] != 3 {
				t.Fatalf("stored older capacity premise missing: %+v", raw)
			}
			plan, err := hubapi.FeatureResult(d.plansService().ReadPlan(t.Context(), nil, test.ref))
			if err != nil {
				t.Fatal(err)
			}
			if plan.Allowances["unarchived_issues"] != 10000 || plan.Allowances["concurrent_work"] != test.concurrent || !slices.Contains(plan.Features, "native_execution") {
				t.Fatalf("capacity tier read=%+v, want concurrent=%d", plan, test.concurrent)
			}
			for _, resource := range []string{"members", "projects", "repositories", "registered_runners", "connected_runners"} {
				if _, limited := plan.Allowances[resource]; limited {
					t.Fatalf("capacity tier read still limits %s", resource)
				}
			}
			if err := hubapi.FromFeatureError(d.plansService().ReadBillingTenant(t.Context(), func(r tenantstore.Reader) error {
				stored, err := r.BillingPlan(t.Context(), billingstore.PlanReference{ID: test.ref.ID, Version: test.ref.Version})
				if err == nil && !reflect.DeepEqual(stored, raw) {
					t.Fatalf("read normalization rewrote stored plan: before=%+v after=%+v", raw, stored)
				}
				return err
			})); err != nil {
				t.Fatal(err)
			}
		}
	})
}

func TestCapacityGrantPreservesUnrestrictedResources(t *testing.T) {
	f := newEntitlementFixture(t)
	config := CapacityPlans()
	grant := HostedPlan{PlanReference: PlanReference{ID: "capacity_grant", Version: 1}, Allowances: map[string]int64{"concurrent_work": 9, "projects": 1, "repositories": 1, "registered_runners": 1, "connected_runners": 1}}
	config.Plans = append(config.Plans, grant)
	d := f.service.database
	if err := hubapi.FromFeatureError(d.plansService().ConfigurePlans(t.Context(), PlansInput(&HostedConfig{Plans: &config}))); err != nil {
		t.Fatal(err)
	}
	if err := hubapi.FromFeatureError(d.plansService().ApplyPlanCommand(t.Context(), bootstrapTokenID, PlanCommand{ID: "capacity-base", Action: "base", ExpectedRevision: 1, Plan: config.Base, Reason: "capacity access"})); err != nil {
		t.Fatal(err)
	}
	if err := hubapi.FromFeatureError(d.plansService().ApplyPlanCommand(t.Context(), bootstrapTokenID, PlanCommand{ID: "capacity-grant", Action: "grant", ExpectedRevision: 2, GrantID: "capacity", Plan: grant.PlanReference, Scope: []string{"concurrent_work", "projects", "repositories", "registered_runners", "connected_runners"}, Reason: "additional capacity"})); err != nil {
		t.Fatal(err)
	}
	entitlement, err := hubapi.FeatureResult(d.plansService().PlanUsage(t.Context(), time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	if entitlement.Allowances["concurrent_work"] != 9 {
		t.Fatalf("concurrent work grant = %#v", entitlement.Allowances)
	}
	for _, resource := range []string{"projects", "repositories", "registered_runners", "connected_runners"} {
		if _, limited := entitlement.Allowances[resource]; limited {
			t.Fatalf("grant introduced a %s restriction", resource)
		}
	}
	if err := hubapi.FromFeatureError(d.plansService().ApplyPlanCommand(t.Context(), bootstrapTokenID, PlanCommand{ID: "tier-grant", Action: "grant", ExpectedRevision: 3, GrantID: "tier", Plan: PlanReference{ID: "team", Version: 2}, Scope: []string{"projects"}, Reason: "Team access"})); err != nil {
		t.Fatal(err)
	}
	entitlement, err = hubapi.FeatureResult(d.plansService().PlanUsage(t.Context(), time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	if entitlement.Allowances["concurrent_work"] != 20 {
		t.Fatalf("tier grant concurrent work = %d", entitlement.Allowances["concurrent_work"])
	}
}

func TestCapacityLegacyMetadataKeepsImmutablePlan(t *testing.T) {
	f := newEntitlementFixture(t)
	cfg := *f.service.config.Hosted
	cfg.Plans = nil
	cfg.PlanID = "pilot_free"
	cfg.StorageQuotaBytes = 64 << 20
	cfg.EventQuota = 10000
	if err := hubapi.FromFeatureError(f.service.database.plansService().ConfigurePlans(t.Context(), PlansInput(&cfg))); err != nil {
		t.Fatal(err)
	}
	entitlement, err := hubapi.FeatureResult(f.service.database.plansService().PlanUsage(t.Context(), time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	if entitlement.Base.ID != "pilot_free" || entitlement.Allowances["projects"] != 10 || !slices.Contains(entitlement.Features, "native_execution") {
		t.Fatalf("legacy metadata lost access: %#v", entitlement)
	}
}

func TestUnconfiguredHostedPlansResolveToFreeDefault(t *testing.T) {
	repo, _ := postgresWithAdmin(t, 4)
	open := func(org tracker.OrganizationID, plans *HostedPlansConfig) *hubService {
		t.Helper()
		if err := repo.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: "Example", CreatedAt: time.Now()})
		}); err != nil {
			t.Fatal(err)
		}
		d := &hubDatabase{billingRepository: repo, hostedOrganization: org, billingAuditStore: hubtest.NewDetails(), now: time.Now, logger: discardLogger()}
		s := &hubService{database: d, config: hubConfig{Hosted: &HostedConfig{OrganizationID: string(org), Plans: plans}, now: time.Now, Logger: discardLogger()}}
		if err := hubapi.FromFeatureError(d.plansService().ConfigurePlans(t.Context(), PlansInput(s.config.Hosted))); err != nil {
			t.Fatal(err)
		}
		if err := hubapi.FromFeatureError(d.plansService().ConfigureBilling(t.Context(), PlansInput(s.config.Hosted))); err != nil {
			t.Fatal(err)
		}
		return s
	}
	free := Tiers()[0].PlanReference
	catalog := CapacityPlans()
	now := time.Now()
	reference, err := hubapi.FeatureResult(open("org_free_reference", &catalog).database.plansService().PlanUsage(t.Context(), now))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name  string
		plans *HostedPlansConfig
	}{
		{name: "absent"},
		{name: "empty mapping", plans: &HostedPlansConfig{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, suffix := range []string{"first", "second"} {
				org := tracker.OrganizationID("org_free_" + strings.ReplaceAll(test.name, " ", "_") + "_" + suffix)
				plans := open(org, test.plans).database.plansService()
				if plans.Plans() == nil || plans.Plans().Base != free {
					t.Fatalf("%s runtime catalog=%+v, want base %v", org, plans.Plans(), free)
				}
				got, err := hubapi.FeatureResult(plans.PlanUsage(t.Context(), now))
				if err != nil {
					t.Fatalf("%s plan usage=%v", org, err)
				}
				if got.OrganizationID != string(org) || got.Base != free || got.EffectiveBase != free || got.Name != "Free" || got.MonthlyUSDCents == nil || *got.MonthlyUSDCents != 0 {
					t.Fatalf("%s entitlement=%+v, want the free plan", org, got)
				}
				if !reflect.DeepEqual(got.Allowances, reference.Allowances) || !reflect.DeepEqual(got.Features, reference.Features) || got.Allowances["concurrent_work"] != 2 || got.Allowances["collaboration_bytes"] != 1<<30 {
					t.Fatalf("%s allowances=%v features=%v, want free plan %v %v", org, got.Allowances, got.Features, reference.Allowances, reference.Features)
				}
				if _, shown := got.Allowances["artifact_retained_bytes"]; shown || slices.Contains(got.Features, "hosted_artifacts") {
					t.Fatalf("%s free plan shows hosted artifact storage: %+v", org, got)
				}
				err = hubapi.FromFeatureError(plans.ReadBillingTenant(t.Context(), func(r tenantstore.Reader) error {
					return plans.RequireFeatureRecord(t.Context(), r, "hosted_artifacts", now)
				}))
				var limit *LimitError
				if !errors.As(err, &limit) || limit.Resource != "hosted_artifacts" {
					t.Fatalf("%s hosted artifacts outside the free plan=%v", org, err)
				}
				billing, err := hubapi.FeatureResult(plans.ReadBilling(t.Context()))
				if err != nil || billing.Status != "free" {
					t.Fatalf("%s billing=%+v err=%v", org, billing, err)
				}
			}
		})
	}
	t.Run("plan-less organizations on one hosted Hub", func(t *testing.T) {
		plans := open("org_free_hub", nil).database.plansService()
		for _, org := range []tracker.OrganizationID{"org_free_planless_first", "org_free_planless_second"} {
			if err := repo.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: "Example", CreatedAt: time.Now()})
			}); err != nil {
				t.Fatal(err)
			}
			if err := repo.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				if _, err := r.BillingAssignment(t.Context()); !errors.Is(err, corestore.ErrNotFound) {
					return fmt.Errorf("fixture organization already has a plan: %v", err)
				}
				got, err := hubapi.FeatureResult(plans.EntitlementRecord(t.Context(), r, now))
				if err != nil {
					return fmt.Errorf("plan-less entitlement: %w", err)
				}
				got = UsageView(got)
				if got.Base != free || got.EffectiveBase != free || got.Name != "Free" || !reflect.DeepEqual(got.Allowances, reference.Allowances) || !reflect.DeepEqual(got.Features, reference.Features) {
					return fmt.Errorf("plan-less entitlement=%+v, want the free plan %+v", got, reference)
				}
				for _, feature := range []string{"collaboration", "native_execution", "github_integration"} {
					if err := hubapi.FromFeatureError(plans.RequireFeatureRecord(t.Context(), r, feature, now)); err != nil {
						return fmt.Errorf("free feature %s refused: %w", feature, err)
					}
				}
				var limit *LimitError
				if err := hubapi.FromFeatureError(plans.RequireFeatureRecord(t.Context(), r, "hosted_artifacts", now)); !errors.As(err, &limit) || limit.Resource != "hosted_artifacts" {
					return fmt.Errorf("hosted artifacts outside the free plan: %v", err)
				}
				return nil
			}); err != nil {
				t.Fatalf("%s: %v", org, err)
			}
		}
	})
}

type planResolutionReader struct {
	tenantstore.Reader
	assignment    *billingstore.Assignment
	assignmentErr error
	planErr       error
}

func (r planResolutionReader) BillingAssignment(ctx context.Context) (billingstore.Assignment, error) {
	if r.assignmentErr != nil {
		return billingstore.Assignment{}, r.assignmentErr
	}
	if r.assignment != nil {
		return *r.assignment, nil
	}
	return r.Reader.BillingAssignment(ctx)
}

func (r planResolutionReader) BillingPlan(ctx context.Context, ref billingstore.PlanReference) (billingstore.Plan, error) {
	if r.planErr != nil {
		return billingstore.Plan{}, r.planErr
	}
	return r.Reader.BillingPlan(ctx, ref)
}

func TestPlanResolutionDefaultsOnlyADeliberateAbsence(t *testing.T) {
	repo, _ := postgresWithAdmin(t, 4)
	hub := tracker.OrganizationID("org_resolution_hub")
	if err := repo.WriteTenant(t.Context(), hub, corestore.Serving, func(w tenantstore.Writer) error {
		return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: hub, Name: "Example", CreatedAt: time.Now()})
	}); err != nil {
		t.Fatal(err)
	}
	d := &hubDatabase{billingRepository: repo, hostedOrganization: hub, billingAuditStore: hubtest.NewDetails(), now: time.Now, logger: discardLogger()}
	plans := d.plansService()
	if err := hubapi.FromFeatureError(plans.ConfigurePlans(t.Context(), PlansInput(&HostedConfig{OrganizationID: string(hub)}))); err != nil {
		t.Fatal(err)
	}
	unavailable := errors.New("example plan store unavailable")
	free := Tiers()[0].PlanReference
	for _, test := range []struct {
		name    string
		org     tracker.OrganizationID
		reader  func(tenantstore.Reader) planResolutionReader
		wantErr error
	}{
		{name: "absent plan resolves to free", org: "org_resolution_planless", reader: func(r tenantstore.Reader) planResolutionReader { return planResolutionReader{Reader: r} }},
		{name: "assignment read error fails closed", org: "org_resolution_read_error", reader: func(r tenantstore.Reader) planResolutionReader {
			return planResolutionReader{Reader: r, assignmentErr: unavailable}
		}, wantErr: unavailable},
		{name: "assignment naming an unknown plan fails closed", org: hub, reader: func(r tenantstore.Reader) planResolutionReader {
			return planResolutionReader{Reader: r, assignment: &billingstore.Assignment{OrganizationID: hub, Base: billingstore.PlanReference{ID: "missing_paid", Version: 1}, Revision: 1}}
		}, wantErr: sql.ErrNoRows},
		{name: "plan store read error fails closed", org: hub, reader: func(r tenantstore.Reader) planResolutionReader {
			return planResolutionReader{Reader: r, planErr: unavailable}
		}, wantErr: unavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.org != hub {
				if err := repo.WriteTenant(t.Context(), test.org, corestore.Serving, func(w tenantstore.Writer) error {
					return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: test.org, Name: "Example", CreatedAt: time.Now()})
				}); err != nil {
					t.Fatal(err)
				}
			}
			if err := repo.ReadTenant(t.Context(), test.org, corestore.Serving, func(r tenantstore.Reader) error {
				reader := test.reader(r)
				entitlement, err := plans.EntitlementRecord(t.Context(), reader, time.Now())
				featureErr := plans.RequireFeatureRecord(t.Context(), reader, "hosted_artifacts", time.Now())
				if test.wantErr == nil {
					var limit *LimitError
					if err != nil || entitlement.EffectiveBase != free || !errors.As(featureErr, &limit) {
						return fmt.Errorf("entitlement=%+v err=%v feature=%v, want free", entitlement, err, featureErr)
					}
					return nil
				}
				if !errors.Is(err, test.wantErr) || len(entitlement.Allowances) != 0 {
					return fmt.Errorf("entitlement=%+v err=%v, want %v", entitlement, err, test.wantErr)
				}
				if featureErr == nil || test.org != hub && !errors.Is(featureErr, test.wantErr) {
					return fmt.Errorf("feature check=%v, want a refusal or %v", featureErr, test.wantErr)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
