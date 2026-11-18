package hostedplans

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
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
			repo, admin := postgresWithAdmin(t, 4)
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
			oldSnapshots, err := NewHostedRuntimeSnapshots(concurrentRuntimeRepository{repo}, hubtest.NewDetails())
			if err != nil {
				t.Fatal(err)
			}
			newSnapshots, err := NewHostedRuntimeSnapshots(repo, hubtest.NewDetails())
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
			oldPlans := PilotPlans()
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
				plans := PilotPlans()
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
	repo, admin := postgresWithAdmin(t, 4)
	plans := PilotPlans()
	services := configuredPlanServices(t, plans, repo)
	stagedPlans := PilotPlans()
	stagedVersion := stagedPlans.Plans[0]
	stagedVersion.Version++
	stagedVersion.Allowances = maps.Clone(stagedVersion.Allowances)
	stagedPlans.Plans = append(stagedPlans.Plans, stagedVersion)
	prepared := uuid.NewString()
	for _, s := range services {
		if err := s.ApplyRuntimeRecords(t.Context(), stagedPlans, nil, "configuration", "superseded preparation", prepared, false); err != nil {
			t.Fatal(err)
		}
	}
	plans = stagedPlans
	revision := uuid.NewString()
	if _, err := admin.ExecContext(t.Context(), "INSERT INTO hosted_runtime_publications(id) VALUES($1)", revision); err != nil {
		t.Fatal(err)
	}
	plans.Plans[0].Allowances["concurrent_work"] = 17
	plans.Plans[len(plans.Plans)-1].Allowances["concurrent_work"] = 19
	for _, s := range services {
		d := s
		if err := d.ApplyRuntimeRecords(t.Context(), plans, nil, "configuration", "published adoption", revision, false); err != nil {
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
			if err != nil {
				return err
			}
			retained, err := r.BillingPlan(t.Context(), billingstore.PlanReference{ID: stagedVersion.ID, Version: stagedVersion.Version})
			if err == nil && (retained.RuntimeID != revision || retained.Allowances["concurrent_work"] != 19) {
				t.Errorf("%s superseded staged version runtime=%s allowance=%d; want current publication and allowance19", d.hostedOrganization, retained.RuntimeID, retained.Allowances["concurrent_work"])
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSharedRuntimeSnapshotsTenantIsolation(t *testing.T) {
	repo, admin := postgresWithAdmin(t, 4)
	objects := &countingDetails{DetailStore: hubtest.NewDetails()}
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
		plans := PilotPlans()
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
		before := objects.count()
		tenant.Generation++
		if _, err := snapshots.Load(t.Context(), tenant); err != nil || objects.count() != before+1 {
			t.Fatalf("generation reused old cache: reads=%d before=%d error=%v", objects.count(), before, err)
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

func configuredPlanServices(t *testing.T, plans HostedPlansConfig, repo *postgres.Repository) []*Service {
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
		s := New(Deps{Repository: repo, Organization: org, Details: hubtest.NewDetails(), Now: time.Now, Logger: slog.New(slog.DiscardHandler)})
		if err := s.ConfigurePlans(t.Context(), &Hosted{Plans: &plans}); err != nil {
			t.Fatal(err)
		}
		services = append(services, s)
	}
	return services
}

type countingDetails struct {
	attachment.DetailStore
	mu    sync.Mutex
	reads int
}

func (d *countingDetails) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	d.mu.Lock()
	d.reads++
	d.mu.Unlock()
	return d.DetailStore.ReadDetail(ctx, organization, ref)
}

func (d *countingDetails) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.reads
}
