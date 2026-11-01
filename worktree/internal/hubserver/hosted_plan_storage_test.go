package hubserver

import (
	"encoding/json"
	"testing"
	"testing/fstest"
	"time"

	"github.com/pressly/goose/v3"
)

func TestHostedPlanStorageMigration(t *testing.T) {
	t.Parallel()
	f, provider := newHostedBillingFixture(t)
	d := f.service.database
	plans := capacityHostedPlans()
	for i := range plans.Plans {
		plan := &plans.Plans[i]
		if hostedCapacityPlan(plan.PlanReference) {
			plan.Allowances["collaboration_bytes"] = 1 << 30
		}
		if plan.ID == "scale" {
			plan.Allowances["collaboration_bytes"] = 500 << 30
		}
	}
	cfg := *f.service.config.Hosted
	cfg.Plans = &plans
	if err := d.configureHostedPlans(t.Context(), &cfg); err != nil {
		t.Fatal(err)
	}
	billingConfig := *cfg.Billing
	billingConfig.Prices = []HostedBillingPrice{{PriceID: "price_team", Plan: PlanReference{ID: "team", Version: 2}}}
	cfg.Billing = &billingConfig
	if err := d.configureHostedBilling(t.Context(), &cfg); err != nil {
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
	before, err := d.hostedPlanUsage(t.Context(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if before.Source != "subscription" || before.EffectiveBase != (PlanReference{ID: "team", Version: 2}) || before.Allowances["collaboration_bytes"] != 1<<30 {
		t.Fatalf("existing subscriber = %#v", before)
	}
	state, err := d.readHostedBilling(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	legacy := make(map[PlanReference]string)
	for _, plan := range plans.Plans {
		if plan.ID == "pilot_free" || plan.ID == "comp_team" || plan.ID == "scale" || plan.ID == "growth" && plan.Version == 1 {
			var raw string
			if err := d.db.QueryRowContext(t.Context(), "SELECT record_json FROM hosted_plans WHERE id=? AND version=?", plan.ID, plan.Version).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			legacy[plan.PlanReference] = raw
		}
	}
	const file = "20261021130000_hosted_plan_storage.sql"
	data, err := migrationFiles.ReadFile("migrations/" + file)
	if err != nil {
		t.Fatal(err)
	}
	migration, err := goose.NewProvider(goose.DialectSQLite3, d.db, fstest.MapFS{file: &fstest.MapFile{Data: data}}, goose.WithDisableGlobalRegistry(true), goose.WithTableName("storage_test_version"), goose.WithSlog(discardLogger()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migration.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
	current := capacityHostedPlans()
	if err := d.configureHostedPlans(t.Context(), &HostedConfig{Plans: &current}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		ref     PlanReference
		storage int64
	}{
		{PlanReference{ID: "free", Version: 1}, 1 << 30},
		{PlanReference{ID: "starter", Version: 1}, 25 << 30},
		{PlanReference{ID: "team", Version: 2}, 100 << 30},
		{PlanReference{ID: "growth", Version: 2}, 250 << 30},
	} {
		t.Run(test.ref.ID, func(t *testing.T) {
			plan, err := d.readHostedPlan(t.Context(), d.reader, test.ref)
			if err != nil {
				t.Fatal(err)
			}
			if plan.Allowances["collaboration_bytes"] != test.storage || plan.Allowances["history_records"] != 2000000 {
				t.Fatalf("updated catalog = %#v", plan)
			}
		})
	}
	for ref, before := range legacy {
		var after string
		if err := d.db.QueryRowContext(t.Context(), "SELECT record_json FROM hosted_plans WHERE id=? AND version=?", ref.ID, ref.Version).Scan(&after); err != nil {
			t.Fatal(err)
		}
		if after != before {
			t.Fatalf("legacy/custom plan changed: %v", ref)
		}
	}
	after, err := d.hostedPlanUsage(t.Context(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if after.EffectiveBase != before.EffectiveBase || after.Allowances["collaboration_bytes"] != 100<<30 {
		t.Fatalf("subscriber after migration = %#v", after)
	}
	retained, err := d.readHostedBilling(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if retained != state || len(provider.checkouts) != 0 {
		t.Fatal("storage migration changed subscription or required checkout")
	}
	custom := HostedPlan{PlanReference: PlanReference{ID: "custom_storage", Version: 1}, Allowances: map[string]int64{"collaboration_bytes": 750 << 30}}
	if err := d.applyHostedPlanCommand(t.Context(), bootstrapTokenID, hostedPlanCommand{ID: "custom-storage-plan", Action: "create_plan", ExpectedRevision: after.Revision, Plan: custom.PlanReference, Definition: &custom, Reason: "Custom organization storage"}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.db.ExecContext(t.Context(), "UPDATE hosted_plan_assignments SET base_id='scale',base_version=2"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.db.ExecContext(t.Context(), "DELETE FROM hosted_billing_accounts"); err != nil {
		t.Fatal(err)
	}
	entitlement, err := d.hostedPlanUsage(t.Context(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := d.applyHostedPlanCommand(t.Context(), bootstrapTokenID, hostedPlanCommand{ID: "custom-storage-grant", Action: "grant", ExpectedRevision: entitlement.Revision, GrantID: "custom-storage", Plan: custom.PlanReference, Scope: []string{"collaboration_bytes"}, Reason: "Custom organization storage"}); err != nil {
		t.Fatal(err)
	}
	entitlement, err = d.hostedPlanUsage(t.Context(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if entitlement.Allowances["collaboration_bytes"] != 750<<30 {
		t.Fatalf("custom Scale storage = %#v", entitlement)
	}
	if _, err := d.db.ExecContext(t.Context(), "UPDATE hosted_plans SET record_json=json_set(record_json,'$.allowances.collaboration_bytes',?) WHERE id='team' AND version=2", 200<<30); err != nil {
		t.Fatal(err)
	}
	if _, err := migration.Down(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := migration.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := d.db.QueryRowContext(t.Context(), "SELECT record_json FROM hosted_plans WHERE id='team' AND version=2").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var plan HostedPlan
	if err := json.Unmarshal([]byte(raw), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Allowances["collaboration_bytes"] != 200<<30 {
		t.Fatal("migration lowered custom paid allowance")
	}
}
