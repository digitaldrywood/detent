package hubserver

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type concurrentRuntimeRepository struct{ tenantstore.Repository }

func (r concurrentRuntimeRepository) WriteTenant(ctx context.Context, organization tracker.OrganizationID, _ corestore.Workload, write func(tenantstore.Writer) error) error {
	return r.Repository.WriteTenant(ctx, organization, corestore.Serving, write)
}

func TestPostgresRuntimeFirstAdoptionRejectsConcurrentHead(t *testing.T) {
	for _, pauseAfter := range []string{"MissingConfiguredBillingPlan", "InsertRuntimeRevision"} {
		t.Run(pauseAfter, func(t *testing.T) {
			repo, admin := projectRecordPostgresWithAdmin(t, 4)
			tenants := []hubmaintenance.Tenant{{Organization: "org_first", Generation: 1}, {Organization: "org_second", Generation: 1}}
			for _, tenant := range tenants {
				org := tracker.OrganizationID(tenant.Organization)
				if err := repo.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: "Example", CreatedAt: time.Now()})
				}); err != nil {
					t.Fatal(err)
				}
			}
			older, newer := uuid.NewString(), uuid.NewString()
			if _, err := admin.ExecContext(t.Context(), "INSERT INTO hosted_runtime_publications(id) VALUES($1),($2)", older, newer); err != nil {
				t.Fatal(err)
			}
			oldSnapshots, err := NewHostedRuntimeSnapshots(concurrentRuntimeRepository{repo}, &memoryRunDetails{})
			if err != nil {
				t.Fatal(err)
			}
			newSnapshots, err := NewHostedRuntimeSnapshots(repo, &memoryRunDetails{})
			if err != nil {
				t.Fatal(err)
			}
			paused, resume, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var release sync.Once
			var olderErr error
			ctx := postgres.WithTransactionTiming(t.Context(), nil, func(_ time.Time, query string) {
				if strings.Contains(query, "-- name: "+pauseAfter+" ") {
					close(paused)
					<-resume
				}
			})
			oldPlans := pilotHostedPlans()
			go func() {
				defer close(done)
				olderErr = oldSnapshots.Adopt(ctx, tenants[0], "", HostedRuntimeUpdate{Plans: &oldPlans, Reason: "older publication", StageID: older})
			}()
			t.Cleanup(func() {
				release.Do(func() { close(resume) })
				<-done
			})
			select {
			case <-paused:
			case <-done:
				t.Fatalf("older adoption did not reach barrier: %v", olderErr)
			}
			for i, tenant := range tenants {
				plans := pilotHostedPlans()
				plans.Plans[0].Allowances["concurrent_work"] = int64(17 + i)
				if err := newSnapshots.Adopt(t.Context(), tenant, "", HostedRuntimeUpdate{Plans: &plans, Reason: "newer publication", StageID: newer}); err != nil {
					t.Fatal(err)
				}
			}
			release.Do(func() { close(resume) })
			<-done
			if !errors.Is(olderErr, corestore.ErrRevisionConflict) {
				t.Errorf("older first adoption error = %v; want revision conflict", olderErr)
			}
			for i, tenant := range tenants {
				if err := repo.ReadTenant(t.Context(), tracker.OrganizationID(tenant.Organization), corestore.Serving, func(r tenantstore.Reader) error {
					head, err := r.CurrentRuntime(t.Context())
					if err != nil {
						return err
					}
					plan, err := r.BillingPlan(t.Context(), billingstore.PlanReference{ID: oldPlans.Plans[0].ID, Version: oldPlans.Plans[0].Version})
					if err == nil && (head.ID != newer || plan.RuntimeID != newer || plan.Allowances["concurrent_work"] != int64(17+i)) {
						t.Errorf("%s head=%s plan runtime=%s allowance=%d; want newer head/catalog and allowance=%d", tenant.Organization, head.ID, plan.RuntimeID, plan.Allowances["concurrent_work"], 17+i)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestPostgresRuntimeAdoptionCommitsPlansWithHead(t *testing.T) {
	repo, admin := projectRecordPostgresWithAdmin(t, 4)
	plans := pilotHostedPlans()
	services := configuredPlanPostgresServices(t, plans, repo)
	revision := uuid.NewString()
	if _, err := admin.ExecContext(t.Context(), "INSERT INTO hosted_runtime_publications(id) VALUES($1)", revision); err != nil {
		t.Fatal(err)
	}
	plans.Plans[0].Allowances["concurrent_work"] = 17
	for _, s := range services {
		d := s.database
		if err := d.applyRuntimeRecords(t.Context(), plans, nil, "configuration", "published adoption", revision, false); err != nil {
			t.Fatal(err)
		}
		if err := repo.ReadTenant(t.Context(), d.hostedOrganization, corestore.Serving, func(r tenantstore.Reader) error {
			head, err := r.CurrentRuntime(t.Context())
			if err != nil {
				return err
			}
			plan, err := r.BillingPlan(t.Context(), billingstore.PlanReference{ID: plans.Plans[0].ID, Version: plans.Plans[0].Version})
			if err == nil && (head.ID != revision || plan.Allowances["concurrent_work"] != 17) {
				t.Errorf("%s head=%s allowance=%d; want published head and allowance together", d.hostedOrganization, head.ID, plan.Allowances["concurrent_work"])
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPostgresConfiguredPlanReplacement(t *testing.T) {
	for _, mode := range []string{"startup", "reload", "publication", "audit refusal"} {
		t.Run(mode, func(t *testing.T) {
			plans := pilotHostedPlans()
			plans.Plans[0].ID = "free"
			plans.Base = plans.Plans[0].PlanReference
			plans.Plans[0].Allowances["api_mutations"] = 0
			plans.Plans[1].ID = "starter"
			plans.Plans[1].Features = []string{"collaboration"}
			plans.Plans[1].Allowances["api_mutations"] = 0
			repo, admin := projectRecordPostgresWithAdmin(t, 4)
			services := configuredPlanPostgresServices(t, plans, repo)
			for _, s := range services {
				if err := s.database.writeBillingTenant(t.Context(), func(w tenantstore.Writer) error {
					return w.CreateBillingGrant(t.Context(), billingstore.Grant{OrganizationID: s.database.hostedOrganization, ID: "grant_" + string(s.database.hostedOrganization), Plan: billingstore.PlanReference{ID: plans.Plans[1].ID, Version: 1}, Scope: []string{"api_mutations", "ingested_events"}, StartsAt: time.Now().Add(-time.Hour)})
				}); err != nil {
					t.Fatal(err)
				}
			}
			denied := errors.New("usage refused")
			request := func(s *Service, id string) error {
				return s.database.writeBillingTenant(t.Context(), func(w tenantstore.Writer) error {
					entitlement, err := s.database.hostedEntitlementRecord(t.Context(), w, time.Now())
					if err != nil {
						return err
					}
					usage, err := w.ApplyBillingUsage(t.Context(), billingstore.UsageReceipt{OrganizationID: s.database.hostedOrganization, ID: id, WindowStart: time.Now().UTC().Truncate(time.Hour), Metric: "api_mutations", Amount: 1}, entitlement.Allowances["api_mutations"])
					if err == nil && !usage.Allowed {
						return denied
					}
					return err
				})
			}
			for _, s := range services {
				if err := request(s, "before"); !errors.Is(err, denied) {
					t.Fatalf("old allowance error = %v", err)
				}
			}
			plans.Plans[0].Name = "Configured plan"
			plans.Plans[0].MonthlyUSDCents = new(int64(1234))
			plans.Plans[0].Features = []string{"collaboration", "model_choice"}
			plans.Plans[0].Allowances["api_mutations"] = 1000000
			plans.Plans[0].Allowances["ingested_events"] = 1
			plans.Plans[0].Allowances["concurrent_work"] = 17
			stage := "00000000-0000-4000-8000-000000000010"
			if mode == "audit refusal" {
				if _, err := admin.ExecContext(t.Context(), "ALTER TABLE hosted_plan_audit ADD CONSTRAINT refuse_update CHECK (action <> 'update_plan')"); err != nil {
					t.Fatal(err)
				}
			}
			for _, s := range services {
				if mode == "startup" {
					if err := s.database.configureHostedPlans(t.Context(), s.config.Hosted); err != nil {
						t.Fatal(err)
					}
				} else {
					update := HostedRuntimeUpdate{Plans: &plans, Reason: "configuration edit"}
					if mode == "publication" {
						s.config.Hosted.SharedEntry = &HostedSharedEntry{}
						update.StageID = stage
					}
					before := s.database.runtimeState()
					validation := update
					validation.ValidateOnly = true
					if err := s.applyHostedRuntime(t.Context(), &validation); err != nil {
						t.Fatal(err)
					}
					if err := request(s, "validation"); !errors.Is(err, denied) || s.database.runtimeState() != before {
						t.Fatalf("validation changed active definition: %v", err)
					}
					err := s.applyHostedRuntime(t.Context(), &update)
					if mode == "audit refusal" {
						if err == nil || s.database.runtimeState() != before {
							t.Fatalf("audit refusal activated runtime: %v", err)
						}
						if err := request(s, "refused"); !errors.Is(err, denied) {
							t.Fatalf("audit refusal committed allowance: %v", err)
						}
					} else if err != nil {
						t.Fatal(err)
					}
				}
			}
			if mode == "audit refusal" {
				if _, err := admin.ExecContext(t.Context(), "ALTER TABLE hosted_plan_audit DROP CONSTRAINT refuse_update"); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "audit refusal" {
				for _, s := range services {
					update := HostedRuntimeUpdate{Plans: &plans, Reason: "retry configuration edit"}
					if err := s.applyHostedRuntime(t.Context(), &update); err != nil {
						t.Fatal(err)
					}
				}
			}
			if mode == "publication" {
				for _, s := range services {
					if err := request(s, "prepared"); !errors.Is(err, denied) {
						t.Fatalf("preparation changed allowance: %v", err)
					}
				}
				if _, err := admin.ExecContext(t.Context(), "INSERT INTO hosted_runtime_publications(id) VALUES($1)", stage); err != nil {
					t.Fatal(err)
				}
				for _, s := range services {
					if err := s.database.refreshRuntime(t.Context()); err != nil {
						t.Fatal(err)
					}
				}
			}
			for _, s := range services {
				if err := s.database.refreshRuntime(t.Context()); err != nil {
					t.Fatal(err)
				}
				if err := s.database.readBillingTenant(t.Context(), func(r tenantstore.Reader) error {
					entitlement, err := s.database.hostedEntitlementRecord(t.Context(), r, time.Now())
					if err != nil {
						return err
					}
					if entitlement.Allowances["concurrent_work"] != 17 || entitlement.Allowances["api_mutations"] != 1000000 || entitlement.Allowances["ingested_events"] != plans.Plans[1].Allowances["ingested_events"] || entitlement.Name != "Configured plan" || entitlement.MonthlyUSDCents == nil || *entitlement.MonthlyUSDCents != 1234 || !slices.Contains(entitlement.Features, "model_choice") || slices.Contains(entitlement.Features, "native_execution") {
						return fmt.Errorf("configured entitlement = %+v", entitlement)
					}
					audits, err := r.BillingPlanAudits(t.Context(), 0, 100)
					if err != nil {
						return err
					}
					updates := 0
					for _, audit := range audits {
						if audit.Action == "update_plan" && audit.ActorID == "configuration" {
							updates++
						}
					}
					if updates != 1 {
						return fmt.Errorf("configuration update audits = %d", updates)
					}
					var limited *hostedLimitError
					if err := s.database.requireHostedFeatureRecord(t.Context(), r, "native_execution", time.Now()); !errors.As(err, &limited) {
						return fmt.Errorf("removed feature allowed: %v", err)
					}
					return s.database.requireHostedFeatureRecord(t.Context(), r, "model_choice", time.Now())
				}); err != nil {
					t.Fatal(err)
				}
				if err := request(s, "after"); err != nil {
					t.Fatalf("new allowance refused request: %v", err)
				}
			}
		})
	}
}

func TestPostgresRuntimeValidationDoesNotPublish(t *testing.T) {
	repo, admin := projectRecordPostgresWithAdmin(t, 4)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repo.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: "Example", CreatedAt: time.Now()})
		}); err != nil {
			t.Fatal(err)
		}
		d := &database{billingRepository: repo, hostedOrganization: org, billingAuditStore: &memoryRunDetails{}, now: time.Now, logger: discardLogger()}
		s := &Service{database: d, config: Config{Hosted: &HostedConfig{OrganizationID: string(org), EntitlementAdministrator: "actor"}, now: time.Now, Logger: discardLogger()}}
		plans := pilotHostedPlans()
		update := HostedRuntimeUpdate{Plans: &plans, Reason: "Test runtime", ValidateOnly: true}
		if err := s.applyHostedRuntime(t.Context(), &update); err != nil {
			t.Fatal(err)
		}
		if d.plans() != nil {
			t.Fatal("validation published runtime")
		}
		if err := repo.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			rows, err := r.BillingPlans(t.Context(), billingstore.PlanReference{}, 100)
			if len(rows) != 0 {
				t.Error("validation committed plans")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		update.ValidateOnly = false
		if org == "org_second" {
			s.config.Hosted.SharedEntry = &HostedSharedEntry{}
			update.StageID = "00000000-0000-4000-8000-000000000003"
		}
		if err := s.applyHostedRuntime(t.Context(), &update); err != nil {
			t.Fatal(err)
		}
		if update.StageID != "" {
			if d.plans() != nil {
				t.Fatal("first preparation was visible before publication")
			}
			if _, err := admin.ExecContext(t.Context(), "INSERT INTO hosted_runtime_publications(id) VALUES($1)", update.StageID); err != nil {
				t.Fatal(err)
			}
			if err := d.refreshRuntime(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		if d.plans() == nil {
			t.Fatal("runtime not published")
		}
		if err := repo.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			rows, err := r.BillingPlans(t.Context(), billingstore.PlanReference{}, 100)
			if len(rows) != len(plans.Plans) {
				t.Errorf("plans=%d", len(rows))
			}
			audits, err2 := r.BillingPlanAudits(t.Context(), 0, 100)
			if len(audits) != len(plans.Plans)+1 {
				t.Errorf("audits=%d", len(audits))
			}
			if err != nil {
				return err
			}
			return err2
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func configuredPlanPostgresServices(t *testing.T, plans HostedPlansConfig, repo *postgres.Repository) []*Service {
	t.Helper()
	var services []*Service
	suffix := sha256.Sum256([]byte(t.Name()))
	for _, name := range []string{"first", "second"} {
		org := tracker.OrganizationID(fmt.Sprintf("org_%x_%s", suffix[:6], name))
		if err := repo.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: "Example", CreatedAt: time.Now()})
		}); err != nil {
			t.Fatal(err)
		}
		d := &database{postgres: repo, billingRepository: repo, hostedOrganization: org, billingAuditStore: &memoryRunDetails{}, now: time.Now, logger: discardLogger()}
		s := &Service{database: d, config: Config{Hosted: &HostedConfig{OrganizationID: string(org), Plans: &plans, EntitlementAdministrator: "operator"}, now: time.Now, Logger: discardLogger()}}
		if err := d.configureHostedPlans(t.Context(), s.config.Hosted); err != nil {
			t.Fatal(err)
		}
		services = append(services, s)
	}
	return services
}

func TestSharedRuntimeSnapshotsTenantIsolation(t *testing.T) {
	repo, admin := projectRecordPostgresWithAdmin(t, 4)
	objects := &memoryRunDetails{}
	snapshots, err := NewHostedRuntimeSnapshots(repo, objects)
	if err != nil {
		t.Fatal(err)
	}
	revision := uuid.NewString()
	if _, err := admin.ExecContext(t.Context(), "INSERT INTO hosted_runtime_publications(id) VALUES($1)", revision); err != nil {
		t.Fatal(err)
	}
	tenants := []hubmaintenance.Tenant{{Organization: "org_first", Generation: 1}, {Organization: "org_second", Generation: 1}}
	for i, tenant := range tenants {
		if err := repo.WriteTenant(t.Context(), tracker.OrganizationID(tenant.Organization), corestore.Serving, func(w tenantstore.Writer) error {
			return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: tracker.OrganizationID(tenant.Organization), Name: "Example", CreatedAt: time.Now()})
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := snapshots.Load(t.Context(), tenant); !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("unconfigured tenant served runtime: %v", err)
		}
		plans := pilotHostedPlans()
		plans.WindowSeconds = int64(120 * (i + 1))
		update := HostedRuntimeUpdate{Plans: &plans, Reason: "adopt published configuration", StageID: revision}
		if err := snapshots.Adopt(t.Context(), tenant, "", update); err != nil {
			t.Fatal(err)
		}
		if err := snapshots.Adopt(t.Context(), tenant, "", update); err != nil {
			t.Fatalf("idempotent adoption: %v", err)
		}
		if err := repo.WriteTenant(t.Context(), tracker.OrganizationID(tenant.Organization), corestore.Serving, func(w tenantstore.Writer) error {
			return w.SetBillingAssignment(t.Context(), billingstore.Assignment{OrganizationID: tracker.OrganizationID(tenant.Organization), Base: billingstore.PlanReference{ID: plans.Base.ID, Version: plans.Base.Version}}, 0)
		}); err != nil {
			t.Fatal(err)
		}
	}
	for i, tenant := range tenants {
		update, err := snapshots.Load(t.Context(), tenant)
		if err != nil || update.StageID != revision || update.Plans.WindowSeconds != int64(120*(i+1)) {
			t.Fatalf("%s snapshot=%+v error=%v", tenant.Organization, update, err)
		}
		update.Plans.WindowSeconds = 600
		update.Plans.Plans[0].Allowances["concurrent_work"] = 999
		cached, err := snapshots.Load(t.Context(), tenant)
		if err != nil || cached.Plans.WindowSeconds != int64(120*(i+1)) || cached.Plans.Plans[0].Allowances["concurrent_work"] == 999 {
			t.Fatalf("caller mutated cached configuration: %v", err)
		}
		before := objects.reads
		tenant.Generation++
		if _, err := snapshots.Load(t.Context(), tenant); err != nil || objects.reads != before+1 {
			t.Fatalf("generation reused old cache: reads=%d before=%d error=%v", objects.reads, before, err)
		}
	}
	restarted, err := NewHostedRuntimeSnapshots(repo, objects)
	if err != nil {
		t.Fatal(err)
	}
	for _, tenant := range tenants {
		before, err := restarted.Load(t.Context(), tenant)
		if err != nil {
			t.Fatal(err)
		}
		plans := *before.Plans
		plans.Plans = plans.Plans[1:]
		plans.Base = plans.Plans[0].PlanReference
		update := HostedRuntimeUpdate{Plans: &plans, Reason: "omit referenced plan", StageID: uuid.NewString()}
		if err := restarted.Adopt(t.Context(), tenant, revision, update); err == nil {
			t.Fatal("referenced missing plan was adopted")
		}
		update.Plans = before.Plans
		if err := restarted.Adopt(t.Context(), tenant, "stale", update); !errors.Is(err, corestore.ErrRevisionConflict) {
			t.Fatalf("stale adoption error=%v", err)
		}
		if err := restarted.Adopt(t.Context(), tenant, revision, update); !errors.Is(err, corestore.ErrRevisionConflict) {
			t.Fatalf("unpublished adoption error=%v", err)
		}
		after, err := restarted.Load(t.Context(), tenant)
		if err != nil || after.StageID != revision || after.Plans.WindowSeconds != before.Plans.WindowSeconds {
			t.Fatalf("failed adoption lost valid revision: %v", err)
		}
		abandoned := uuid.NewString()
		if err := repo.WriteTenant(t.Context(), tracker.OrganizationID(tenant.Organization), corestore.Serving, func(w tenantstore.Writer) error {
			return w.StageRuntime(t.Context(), billingstore.RuntimeRevision{ID: abandoned})
		}); err != nil {
			t.Fatal(err)
		}
		update.StageID = uuid.NewString()
		if _, err := admin.ExecContext(t.Context(), "INSERT INTO hosted_runtime_publications(id) VALUES($1)", update.StageID); err != nil {
			t.Fatal(err)
		}
		price := HostedBillingPrice{PriceID: "price_shared", Label: "Example paid plan", Plan: before.Plans.Plans[1].PlanReference}
		update.Prices = []HostedBillingPrice{price}
		if err := restarted.Adopt(t.Context(), tenant, abandoned, update); err != nil {
			t.Fatalf("abandoned preparation blocked adoption: %v", err)
		}
		applied := update.StageID
		update.StageID = uuid.NewString()
		if _, err := admin.ExecContext(t.Context(), "INSERT INTO hosted_runtime_publications(id) VALUES($1)", update.StageID); err != nil {
			t.Fatal(err)
		}
		other := update.Plans.Plans[1]
		other.ID = "other_paid"
		update.Plans.Plans = append(update.Plans.Plans, other)
		update.Prices[0].Plan = other.PlanReference
		if err := restarted.Adopt(t.Context(), tenant, applied, update); err == nil {
			t.Fatal("immutable price mapping changed")
		}
		update.Prices[0] = price
		update.Plans.Plans[0].Name = "Changed plan"
		if _, err := admin.ExecContext(t.Context(), "ALTER TABLE hosted_plan_audit ADD CONSTRAINT runtime_audit_refusal CHECK (action <> 'update_plan')"); err != nil {
			t.Fatal(err)
		}
		if err := restarted.Adopt(t.Context(), tenant, applied, update); err == nil {
			t.Fatal("audit refusal committed runtime adoption")
		}
		if _, err := admin.ExecContext(t.Context(), "ALTER TABLE hosted_plan_audit DROP CONSTRAINT runtime_audit_refusal"); err != nil {
			t.Fatal(err)
		}
		retained, err := restarted.Load(t.Context(), tenant)
		if err != nil || retained.StageID != applied || retained.Plans.Plans[0].Name == "Changed plan" {
			t.Fatalf("price or audit refusal lost applied configuration: %v", err)
		}
	}
}
