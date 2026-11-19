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
