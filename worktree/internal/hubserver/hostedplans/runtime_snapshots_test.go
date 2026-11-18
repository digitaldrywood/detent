package hostedplans

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestSharedRuntimeSnapshotCacheBoundsAndFlights(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		plans := PilotPlans()
		raw, err := json.Marshal(HostedRuntimeUpdate{Plans: &plans})
		if err != nil {
			t.Fatal(err)
		}
		release := make(chan struct{})
		objects := &runtimeObjectProbe{release: release, raw: raw}
		snapshots, err := NewHostedRuntimeSnapshots(&runtimeReadProbe{}, objects)
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var once sync.Once
		unblock := func() { once.Do(func() { close(release) }); wg.Wait() }
		defer unblock()
		for _, organization := range []string{"org_first", "org_first", "org_second"} {
			wg.Go(func() {
				if _, err := snapshots.Load(t.Context(), hubmaintenance.Tenant{Organization: organization, Generation: 1}); err != nil {
					t.Error(err)
				}
			})
		}
		synctest.Wait()
		if objects.calls.Load() != 2 {
			t.Fatalf("tenant-scoped flights loaded %d objects; want 2", objects.calls.Load())
		}
		unblock()
		for generation := range int64(150) {
			if _, err := snapshots.Load(t.Context(), hubmaintenance.Tenant{Organization: "org_first", Generation: generation + 2}); err != nil {
				t.Fatal(err)
			}
		}
		if len(snapshots.cache) != 128 {
			t.Fatalf("snapshot cache grew to %d entries", len(snapshots.cache))
		}
	})
}

type runtimeReadProbe struct {
	tenantstore.Repository
	revision string
	calls    atomic.Int32
	release  <-chan struct{}
	err      error
}

func (p *runtimeReadProbe) ReadTenant(ctx context.Context, _ tracker.OrganizationID, _ corestore.Workload, read func(tenantstore.Reader) error) error {
	p.calls.Add(1)
	if p.release != nil {
		select {
		case <-p.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if p.err != nil {
		return p.err
	}
	return read(runtimeRevisionProbe{id: p.revision})
}

func (p *runtimeReadProbe) WriteTenant(ctx context.Context, _ tracker.OrganizationID, _ corestore.Workload, write func(tenantstore.Writer) error) error {
	return write(runtimeWriteProbe{runtimeReadProbe: p})
}

type runtimeWriteProbe struct {
	tenantstore.Writer
	*runtimeReadProbe
}

func (p runtimeWriteProbe) CurrentRuntime(ctx context.Context) (billingstore.RuntimeRevision, error) {
	return runtimeRevisionProbe{id: p.revision}.CurrentRuntime(ctx)
}

func (p runtimeWriteProbe) MissingConfiguredBillingPlan(context.Context, []billingstore.PlanReference, time.Time) (billingstore.PlanReference, error) {
	return billingstore.PlanReference{}, corestore.ErrNotFound
}

func (p runtimeWriteProbe) BillingPlan(_ context.Context, ref billingstore.PlanReference) (billingstore.Plan, error) {
	for _, plan := range PilotPlans().Plans {
		if plan.ID == ref.ID && plan.Version == ref.Version {
			return billingstore.Plan{PlanReference: ref, RuntimeID: p.revision, Features: plan.Features, Allowances: plan.Allowances}, nil
		}
	}
	return billingstore.Plan{}, corestore.ErrNotFound
}

type runtimeRevisionProbe struct {
	tenantstore.Reader
	id string
}

func (p runtimeRevisionProbe) CurrentRuntime(context.Context) (billingstore.RuntimeRevision, error) {
	id := p.id
	if id == "" {
		id = "loaded"
	}
	return billingstore.RuntimeRevision{ID: id}, nil
}

func (p runtimeRevisionProbe) MissingConfiguredBillingPlan(context.Context, []billingstore.PlanReference, time.Time) (billingstore.PlanReference, error) {
	return billingstore.PlanReference{}, corestore.ErrNotFound
}

type runtimeObjectProbe struct {
	attachment.DetailStore
	calls   atomic.Int32
	release <-chan struct{}
	raw     []byte
	err     error
}

func (p *runtimeObjectProbe) ReadDetail(ctx context.Context, _ string, _ attachment.ArchiveReference) ([]byte, error) {
	p.calls.Add(1)
	select {
	case <-p.release:
		return p.raw, p.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
