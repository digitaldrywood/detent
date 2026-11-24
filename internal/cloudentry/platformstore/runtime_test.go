package platformstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRuntimePublicationPreservesTenantPagingAndAdoption(t *testing.T) {
	admin := template.Database(t)
	pool := rolePool(t, admin, postgres.PlatformRoleName, "")
	r, err := New(t.Context(), pool, testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	seedOrganizations(t, r)
	for _, size := range []int{4, 5} {
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO platform.organizations(id,name,state,endpoint,generation,created_at,updated_at)
SELECT 'org_scale_'||n::text,'Example','ready','unix:/example/scale.sock',1,now(),now() FROM generate_series(1,$1::integer) n ON CONFLICT DO NOTHING`, size-2); err != nil {
			t.Fatal(err)
		}
		revision := uuid.NewString()
		if err := r.PublishRuntime(t.Context(), revision, "entry", "example/payload", nil); err != nil {
			t.Fatal(err)
		}
		page, err := r.RuntimeTenantPage(t.Context(), "", 2)
		if err != nil || len(page) != 2 || page[0].Organization != "org_first" || page[1].Organization != "org_scale_1" {
			t.Fatalf("%d tenants page=%+v error=%v", size, page, err)
		}
		for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
			target, err := r.RuntimeAdoption(t.Context(), org, "entry")
			if err != nil || target.Generation != 1 || target.Publication.Revision != revision || target.PreparedRevision != "" {
				t.Fatalf("%s adoption=%+v error=%v", org, target, err)
			}
		}
	}
	if _, err := r.RuntimeTenantPage(t.Context(), "", 33); err == nil {
		t.Fatal("unbounded maintenance page accepted")
	}
}

type runtimeAdoptionObjects struct {
	pool *pgxpool.Pool
	data map[string][]byte
}

func (s *runtimeAdoptionObjects) StoreDetail(_ context.Context, organization string, raw []byte) (attachment.ArchiveReference, error) {
	if s.pool.Stat().AcquiredConns() != 0 {
		return attachment.ArchiveReference{}, errors.New("runtime held a connection during object I/O")
	}
	ref, _, err := attachment.PrepareArchive(organization, 1, bytes.NewReader(raw))
	s.data[ref.Key] = bytes.Clone(raw)
	return ref, err
}

func (s *runtimeAdoptionObjects) ReadDetail(_ context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	if err := ref.Validate(organization); err != nil {
		return nil, err
	}
	return bytes.Clone(s.data[ref.Key]), nil
}

func TestRuntimeAdoptionPublishesAndReplacesPlanCatalogs(t *testing.T) {
	admin := template.Database(t)
	platform := repository(t, admin, testConfig(t))
	pool := rolePool(t, admin, postgres.RuntimeRoleName, "")
	repo, err := postgres.NewRepository(t.Context(), pool, postgres.PoolConfig{MaxConnections: 3, ServingConnections: 1, BackgroundConnections: 1})
	if err != nil {
		t.Fatal(err)
	}
	snapshots, err := hubserver.NewHostedRuntimeSnapshots(repo, &runtimeAdoptionObjects{pool: pool, data: make(map[string][]byte)})
	if err != nil {
		t.Fatal(err)
	}
	for _, size := range []int{4, 5} {
		plans := hostedplans.HostedPlansConfig{WindowSeconds: 60, RetentionWindows: 1, ConnectedSeconds: 30, InvitationSeconds: 60}
		for i := range size {
			plans.Plans = append(plans.Plans, hostedplans.HostedPlan{PlanReference: hostedplans.PlanReference{ID: fmt.Sprintf("plan_%d", i), Version: 1}, Name: "Example", Features: []string{"collaboration"}, Allowances: map[string]int64{"api_mutations": 1000000, "ingested_events": 1000000}})
		}
		plans.Base = plans.Plans[0].PlanReference
		revision := uuid.NewString()
		if err := platform.PublishRuntime(t.Context(), revision, "entry", "example/payload", nil); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"first", "second"} {
			org := tracker.OrganizationID(fmt.Sprintf("org_plans_%d_%s", size, name))
			if err := repo.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: "Example", CreatedAt: time.Now()})
			}); err != nil {
				t.Fatal(err)
			}
			update := hostedplans.HostedRuntimeUpdate{Plans: &plans, StageID: revision, Reason: "catalog adoption"}
			if err := snapshots.Adopt(t.Context(), hubmaintenance.Tenant{Organization: string(org), Generation: 1}, "", update); err != nil {
				t.Fatal(err)
			}
			update.StageID = uuid.NewString()
			if err := platform.PublishRuntime(t.Context(), update.StageID, "entry", "example/next-payload", nil); err != nil {
				t.Fatal(err)
			}
			if err := snapshots.Adopt(t.Context(), hubmaintenance.Tenant{Organization: string(org), Generation: 1}, revision, update); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestBillingScheduleSelectsDueOrganizationsAndFencesClaims(t *testing.T) {
	db := template.Database(t)
	r := repository(t, db, testConfig(t))
	seedOrganizations(t, r)
	at := r.now()
	a := hubmaintenance.Tenant{Organization: "org_first", Generation: 1}
	b := hubmaintenance.Tenant{Organization: "org_second", Generation: 1}
	before := at.Add(-2 * time.Hour)
	for _, tenant := range []hubmaintenance.Tenant{a, b} {
		if _, err := db.ExecContext(t.Context(), `INSERT INTO platform.billing_customers (organization_id,mode,account_id,customer_id,created_at) VALUES ($1,'test','acct_example',$2,$3)`, tenant.Organization, "cus_"+tenant.Organization, before); err != nil {
			t.Fatal(err)
		}
	}
	var legacy BillingScheduleDiscovery
	for _, tenant := range []hubmaintenance.Tenant{a, b} {
		for range 2 {
			page, err := legacy.Page(t.Context(), r, before, 1)
			if err != nil || len(page) != 1 || page[0] != tenant {
				t.Fatalf("legacy mapping %v discovered %v, err %v", tenant, page, err)
			}
		}
		if ok, err := r.BeginBillingReconciliation(t.Context(), tenant, before); err != nil || !ok {
			t.Fatalf("legacy claim %v: %v %v", tenant, ok, err)
		}
		if ok, err := r.BeginBillingReconciliation(t.Context(), tenant, before); err != nil || ok {
			t.Fatalf("duplicate legacy claim %v: %v %v", tenant, ok, err)
		}
	}
	if err := r.ConfigureBillingSchedule(t.Context(), a, 120, before); err != nil {
		t.Fatal(err)
	}
	if err := r.ConfigureBillingSchedule(t.Context(), b, 0, before); err != nil {
		t.Fatal(err)
	}
	if page, err := legacy.Page(t.Context(), r, before.Add(119*time.Second), 1); err != nil || len(page) != 0 {
		t.Fatalf("legacy binding ran ahead of its interval: %v %v", page, err)
	}
	if page, err := legacy.Page(t.Context(), r, before.Add(120*time.Second), 1); err != nil || len(page) != 1 || page[0] != a {
		t.Fatalf("reconciled legacy customer missing at deadline: %v %v", page, err)
	}

	for _, entry := range []struct {
		tenant  hubmaintenance.Tenant
		seconds int64
	}{{a, 60}, {b, 180}} {
		if err := r.ConfigureBillingSchedule(t.Context(), entry.tenant, entry.seconds, at); err != nil {
			t.Fatal(err)
		}
	}
	stale := a
	stale.Generation++
	if ok, err := r.BeginBillingReconciliation(t.Context(), stale, at); err != nil || ok {
		t.Fatalf("foreign generation claimed=%v err=%v", ok, err)
	}
	for _, step := range []struct {
		after time.Duration
		want  *hubmaintenance.Tenant
	}{
		{0, &a}, {0, &b}, {0, nil}, {59 * time.Second, nil}, {60 * time.Second, &a}, {179 * time.Second, &a}, {180 * time.Second, &b},
	} {
		now := at.Add(step.after)
		page, err := r.DueBillingTenants(t.Context(), now, 1)
		if err != nil {
			t.Fatal(err)
		}
		if step.want == nil {
			if len(page) != 0 {
				t.Fatalf("due after %s: %v", step.after, page)
			}
			continue
		}
		if len(page) != 1 || page[0] != *step.want {
			t.Fatalf("after %s got %v want %v", step.after, page, *step.want)
		}
		if ok, err := r.BeginBillingReconciliation(t.Context(), page[0], now); err != nil || !ok {
			t.Fatalf("claim %v: %v %v", page[0], ok, err)
		}
		if ok, err := r.BeginBillingReconciliation(t.Context(), page[0], now); err != nil || ok {
			t.Fatalf("duplicate claim %v: %v %v", page[0], ok, err)
		}
		if err := r.ConfigureBillingSchedule(t.Context(), a, 60, now); err != nil {
			t.Fatal(err)
		}
	}
}
