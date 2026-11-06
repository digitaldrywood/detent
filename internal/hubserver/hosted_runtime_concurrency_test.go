package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

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
	for _, plan := range pilotHostedPlans().Plans {
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
func runtimeProbeService(repo tenantstore.Repository) *Service {
	d := &database{billingRepository: repo, postgres: &postgres.Repository{}, logger: discardLogger(), now: time.Now}
	d.hostedRuntime.Store(&hostedRuntimeConfig{Revision: "loaded", Plans: pilotHostedPlans()})
	return &Service{database: d, config: Config{Logger: discardLogger(), now: time.Now, Hosted: &HostedConfig{OrganizationID: "org_example", EntitlementAdminToken: []byte(testHostedPlanAdminToken)}}}
}
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
func TestRuntimeRefreshExcludesUnrelatedRoutes(t *testing.T) {
	probe := &runtimeReadProbe{err: errors.New("runtime storage unavailable")}
	s := runtimeProbeService(probe)
	e := echo.New()
	s.registerPostgresRoutes(e)
	for _, path := range []string{"/health", "/static/missing.css"} {
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if got := probe.calls.Load(); got != 0 {
			t.Errorf("%s performed %d runtime reads", path, got)
		}
	}
}
func TestRuntimeRefreshRetainsSnapshotOnError(t *testing.T) {
	probe := &runtimeReadProbe{err: errors.New("runtime storage unavailable")}
	s := runtimeProbeService(probe)
	before := s.database.runtimeState()
	e := echo.New()
	e.GET("/api/v2/organizations/:organization/entitlements/runtime", s.readHostedRuntime, s.refreshHostedRuntime)
	req := httptest.NewRequest(http.MethodGet, "/api/v2/organizations/org_example/entitlements/runtime", nil)
	req.Header.Set("Authorization", "Bearer "+testHostedPlanAdminToken)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || s.database.runtimeState() != before {
		t.Fatalf("last good snapshot lost: status=%d", rec.Code)
	}
}

func TestRuntimeRefreshRejectsInvalidPublishedSnapshot(t *testing.T) {
	s := runtimeProbeService(&runtimeReadProbe{revision: "invalid"})
	before := s.database.runtimeState()
	plans := pilotHostedPlans()
	plans.WindowSeconds = 61
	raw, err := json.Marshal(HostedRuntimeUpdate{Plans: &plans})
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	close(release)
	s.database.billingAuditStore = &runtimeObjectProbe{release: release, raw: raw}
	if err := s.database.refreshRuntime(t.Context()); err == nil || s.database.runtimeState() != before {
		t.Fatalf("invalid publication replaced valid snapshot: %v", err)
	}
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
func TestRuntimeRefreshSharesChangedObjectLoad(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprint(failed), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				probe := &runtimeReadProbe{revision: "changed"}
				s := runtimeProbeService(probe)
				original := s.database.runtimeState()
				plans := pilotHostedPlans()
				raw, err := json.Marshal(HostedRuntimeUpdate{Plans: &plans})
				if err != nil {
					t.Fatal(err)
				}
				release := make(chan struct{})
				objects := &runtimeObjectProbe{release: release, raw: raw}
				if failed {
					objects.err = errors.New("object unavailable")
				}
				s.database.billingAuditStore = objects
				e := echo.New()
				s.registerPostgresRoutes(e)
				var wg sync.WaitGroup
				var once sync.Once
				unblock := func() { once.Do(func() { close(release) }); wg.Wait() }
				defer unblock()
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
				synctest.Wait()
				if probe.calls.Load() != 2 || objects.calls.Load() != 1 {
					t.Fatalf("reads=%d objects=%d", probe.calls.Load(), objects.calls.Load())
				}

				rec := httptest.NewRecorder()
				e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
				if probe.calls.Load() != 2 {
					t.Error("health refreshed runtime during object load")
				}
				unblock()
				if objects.calls.Load() != 1 {
					t.Errorf("object loaded %d times", objects.calls.Load())
				}
				if failed {
					if s.database.runtimeState() != original {
						t.Error("failed object read replaced snapshot")
					}
				} else if s.database.runtimeState().Revision != "changed" {
					t.Error("changed revision not applied")
				}
			})
		})
	}
}
