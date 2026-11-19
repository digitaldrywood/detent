package hubserver

import (
	"errors"
	"io/fs"
	"reflect"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func TestHostedPlanStorageMigration(t *testing.T) {
	t.Parallel()
	f, provider := newHostedBillingFixture(t)
	d := f.service.database
	readPlan := func(ref hostedplans.PlanReference) hostedplans.HostedPlan {
		t.Helper()
		var plan hostedplans.HostedPlan
		if err := hubapi.FromFeatureError(d.plansService().ReadBillingTenant(t.Context(), func(r tenantstore.Reader) error {
			var err error
			plan, err = hubapi.FeatureResult(d.plansService().ReadPlanRecord(t.Context(), r, ref))
			return err
		})); err != nil {
			t.Fatal(err)
		}
		return plan
	}
	readStoredPlan := func(ref hostedplans.PlanReference) billingstore.Plan {
		t.Helper()
		var plan billingstore.Plan
		if err := hubapi.FromFeatureError(d.plansService().ReadBillingTenant(t.Context(), func(r tenantstore.Reader) error {
			var err error
			plan, err = r.BillingPlan(t.Context(), billingstore.PlanReference{ID: ref.ID, Version: ref.Version})
			if err != nil {
				return err
			}
			published, err := r.RuntimePublished(t.Context(), plan.RuntimeID)
			if err != nil {
				return err
			}
			if plan.RuntimeID == "" || !published {
				return errors.New("stored plan runtime binding is unavailable")
			}
			return nil
		})); err != nil {
			t.Fatal(err)
		}
		plan.RuntimeID = ""
		return plan
	}
	fixturePlans := f.service.config.Hosted.Plans.Plans
	plans := hostedplans.CapacityPlans()
	for i := range plans.Plans {
		plan := &plans.Plans[i]
		if hostedplans.CapacityPlan(plan.PlanReference) {
			plan.Allowances["collaboration_bytes"] = 1 << 30
		}
		if plan.ID == "scale" {
			plan.Allowances["collaboration_bytes"] = 500 << 30
		}
	}
	plans.Plans = append(plans.Plans, fixturePlans...)
	cfg := *f.service.config.Hosted
	cfg.Plans = &plans
	if err := hubapi.FromFeatureError(d.plansService().ConfigurePlans(t.Context(), hostedplans.PlansInput(&cfg))); err != nil {
		t.Fatal(err)
	}
	billingConfig := *cfg.Billing
	billingConfig.Prices = []hostedplans.HostedBillingPrice{{PriceID: "price_team", Plan: hostedplans.PlanReference{ID: "team", Version: 2}}}
	cfg.Billing = &billingConfig
	if err := hubapi.FromFeatureError(d.plansService().ConfigureBilling(t.Context(), hostedplans.PlansInput(&cfg))); err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	provider.snapshot = activeBillingSnapshot(time.Now())
	provider.snapshot.PriceID = "price_team"
	provider.mu.Unlock()
	f.service.config.Hosted = &cfg
	if err := f.service.billing.reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	before, err := hubapi.FeatureResult(d.plansService().PlanUsage(t.Context(), time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	if before.Source != "subscription" || before.EffectiveBase != (hostedplans.PlanReference{ID: "team", Version: 2}) || before.Allowances["collaboration_bytes"] != 1<<30 {
		t.Fatalf("existing subscriber = %#v", before)
	}
	state, err := hubapi.FeatureResult(d.plansService().ReadBilling(t.Context()))
	if err != nil {
		t.Fatal(err)
	}
	legacy := make(map[hostedplans.PlanReference]billingstore.Plan)
	for _, plan := range plans.Plans {
		if plan.ID == "pilot_free" || plan.ID == "comp_team" || plan.ID == "scale" || plan.ID == "growth" && plan.Version == 1 {
			legacy[plan.PlanReference] = readStoredPlan(plan.PlanReference)
		}
	}
	const file = "20261112124958_historical_plan_storage_floors.sql"
	migrations, err := postgres.MigrationFiles()
	if err != nil {
		t.Fatal(err)
	}
	data, err := fs.ReadFile(migrations, file)
	if err != nil {
		t.Fatal(err)
	}
	connection := startupConnection(t, hostedPostgresAdmin(t, f.service), postgres.MigrationRoleName, "")
	poolConfig, err := connection.PoolConfig()
	if err != nil {
		t.Fatal(err)
	}
	poolConfig.ConnConfig.RuntimeParams["role"] = postgres.MigrationRoleName
	migrationDB := stdlib.OpenDB(*poolConfig.ConnConfig)
	t.Cleanup(func() {
		if err := migrationDB.Close(); err != nil {
			t.Error(err)
		}
	})
	var role string
	if err := migrationDB.QueryRowContext(t.Context(), "SELECT current_user").Scan(&role); err != nil || role != postgres.MigrationRoleName {
		t.Fatal("storage upgrade did not select the migration role", role, err)
	}
	migration, err := goose.NewProvider(goose.DialectPostgres, migrationDB, postgres.RoleMigrations(fstest.MapFS{file: &fstest.MapFile{Data: data}}), goose.WithDisableGlobalRegistry(true), goose.WithTableName("storage_test_version"), goose.WithSlog(discardLogger()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migration.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
	current := hostedplans.CapacityPlans()
	current.Plans = append(current.Plans, fixturePlans...)
	if err := hubapi.FromFeatureError(d.plansService().ConfigurePlans(t.Context(), hostedplans.PlansInput(&hostedplans.HostedConfig{Plans: &current}))); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		ref     hostedplans.PlanReference
		storage int64
	}{
		{hostedplans.PlanReference{ID: "free", Version: 1}, 1 << 30},
		{hostedplans.PlanReference{ID: "starter", Version: 1}, 25 << 30},
		{hostedplans.PlanReference{ID: "team", Version: 2}, 100 << 30},
		{hostedplans.PlanReference{ID: "growth", Version: 2}, 250 << 30},
	} {
		t.Run(test.ref.ID, func(t *testing.T) {
			plan := readPlan(test.ref)
			if plan.Allowances["collaboration_bytes"] != test.storage || plan.Allowances["history_records"] != 2000000 {
				t.Fatalf("updated catalog = %#v", plan)
			}
		})
	}
	for ref, before := range legacy {
		after := readStoredPlan(ref)
		if !reflect.DeepEqual(after, before) {
			t.Fatalf("legacy/custom plan changed: %v", ref)
		}
	}
	after, err := hubapi.FeatureResult(d.plansService().PlanUsage(t.Context(), time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	if after.EffectiveBase != before.EffectiveBase || after.Allowances["collaboration_bytes"] != 100<<30 {
		t.Fatalf("subscriber after migration = %#v", after)
	}
	retained, err := hubapi.FeatureResult(d.plansService().ReadBilling(t.Context()))
	if err != nil {
		t.Fatal(err)
	}
	if retained != state || len(provider.checkouts) != 0 {
		t.Fatal("storage migration changed subscription or required checkout")
	}
	custom := hostedplans.HostedPlan{PlanReference: hostedplans.PlanReference{ID: "custom_storage", Version: 1}, Allowances: map[string]int64{"collaboration_bytes": 750 << 30}}
	if err := hubapi.FromFeatureError(d.plansService().ApplyPlanCommand(t.Context(), bootstrapTokenID, hostedplans.PlanCommand{ID: "custom-storage-plan", Action: "create_plan", ExpectedRevision: after.Revision, Plan: custom.PlanReference, Definition: &custom, Reason: "Custom organization storage"})); err != nil {
		t.Fatal(err)
	}
	if err := hubapi.FromFeatureError(d.plansService().WriteBillingTenant(t.Context(), func(w tenantstore.Writer) error {
		assignment, err := w.BillingAssignment(t.Context())
		if err != nil {
			return err
		}
		assignment.Base = billingstore.PlanReference{ID: "scale", Version: 2}
		if err := w.SetBillingAssignment(t.Context(), assignment, assignment.Revision); err != nil {
			return err
		}
		account, err := w.BillingAccount(t.Context())
		if err != nil {
			return err
		}
		return w.DeleteBillingAccountIfMatches(t.Context(), account.Binding.AccountID, account.Binding.CustomerID, account.Mode)
	})); err != nil {
		t.Fatal(err)
	}
	entitlement, err := hubapi.FeatureResult(d.plansService().PlanUsage(t.Context(), time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	if err := hubapi.FromFeatureError(d.plansService().ApplyPlanCommand(t.Context(), bootstrapTokenID, hostedplans.PlanCommand{ID: "custom-storage-grant", Action: "grant", ExpectedRevision: entitlement.Revision, GrantID: "custom-storage", Plan: custom.PlanReference, Scope: []string{"collaboration_bytes"}, Reason: "Custom organization storage"})); err != nil {
		t.Fatal(err)
	}
	entitlement, err = hubapi.FeatureResult(d.plansService().PlanUsage(t.Context(), time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	if entitlement.Allowances["collaboration_bytes"] != 750<<30 {
		t.Fatalf("custom Scale storage = %#v", entitlement)
	}
	if err := hubapi.FromFeatureError(d.plansService().WriteBillingTenant(t.Context(), func(w tenantstore.Writer) error {
		plan, err := w.BillingPlan(t.Context(), billingstore.PlanReference{ID: "team", Version: 2})
		if err != nil {
			return err
		}
		plan.Allowances["collaboration_bytes"] = 200 << 30
		return w.ReplaceBillingPlan(t.Context(), plan)
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := migration.Down(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := migration.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
	plan := readPlan(hostedplans.PlanReference{ID: "team", Version: 2})
	if plan.Allowances["collaboration_bytes"] != 200<<30 {
		t.Fatal("migration lowered custom paid allowance")
	}
}
