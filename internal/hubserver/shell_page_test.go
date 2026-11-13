package hubserver

import (
	"context"
	"database/sql"
	"net/http"
	"os"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/onboarding"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestOnboardingPageDoesNotReadObjects(t *testing.T) {
	details := &memoryRunDetails{}
	service := newPostgresNativeService(t, Config{RunDetails: details})
	original := service.database.postgres
	database, _ := projectRecordDatabases.Load(original)
	wire := &boardWire{pingEveryAcquire: true}
	samples := 1
	if os.Getenv("SHELLPERF_RTT_MS") == "12" {
		wire.delay = 12 * time.Millisecond
		samples = 20
	}
	measured := boardWireRepository(t, database.(*sql.DB), wire)
	t.Cleanup(func() { service.config.TenantRepository, service.config.AccessRepository = original, original })
	var expected [2]int
	for _, organization := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		service.config.TenantRepository, service.config.AccessRepository = original, original
		details.beforeRead = nil
		fixture := newNativeFixture(t, service, organization, "Example")
		seeded := 0
		for _, size := range []int{2, 50} {
			service.config.TenantRepository, service.config.AccessRepository = original, original
			details.beforeRead = nil
			for seeded < size {
				runner := prepareRunnerForScope(t, fixture, "projects", runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
				runner.enroll(t)
				seeded++
			}
			cold := performHubAPIRequest(t, service, http.MethodGet, fixture.base+"/onboarding", fixture.token, nil)
			requireNativeStatus(t, cold, http.StatusOK)
			previousHosted := service.config.Hosted
			service.config.Hosted = &hostedplans.HostedConfig{OrganizationID: string(organization)}
			scheduler, err := service.newReadModelMaintenance()
			if err != nil {
				t.Fatal(err)
			}
			for range 16 {
				if err := scheduler.Tick(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			service.config.Hosted = previousHosted
			objects := 0
			details.beforeRead = func(context.Context) error { objects++; return nil }
			service.config.TenantRepository, service.config.AccessRepository = measured, measured
			durations := make([]time.Duration, 0, samples)
			for range samples {
				started := time.Now()
				wire.reset()
				response := performHubAPIRequest(t, service, http.MethodGet, fixture.base+"/onboarding", fixture.token, nil)
				exchanges, statements := wire.stop()
				requireNativeStatus(t, response, http.StatusOK)
				var page onboarding.Project
				decodeHubResponse(t, response, &page)
				if len(page.Runners) != size || page.Policy == nil || page.Policy.Policy.ID == "" || len(page.Steps) != 4 {
					t.Errorf("tenant=%s size=%d wrong onboarding page: %+v", organization, size, page)
				}
				for _, runner := range page.Runners {
					if runner.Runner.OrganizationID != organization || !slices.Equal(runner.Runner.ProjectIDs, []tracker.ProjectID{fixture.project.ID}) {
						t.Error("runner crossed scope boundary")
					}
				}
				t.Logf("tenant=%s size=%d statements=%d exchanges=%d objects=%d", organization, size, statements, exchanges, objects)
				if statements != 14 || exchanges != 10 || objects != 0 {
					t.Errorf("onboarding request reads source data")
				}
				if expected != [2]int{} && expected != [2]int{statements, exchanges} {
					t.Errorf("request cost changed: %v -> %v", expected, [2]int{statements, exchanges})
				}
				expected = [2]int{statements, exchanges}
				durations = append(durations, time.Since(started))
			}
			slices.Sort(durations)
			p95 := durations[(len(durations)*95+99)/100-1]
			t.Logf("tenant=%s size=%d samples=%d rtt=%s p95=%s", organization, size, samples, wire.delay, p95)
			if wire.delay > 0 && p95 > time.Second {
				t.Errorf("onboarding p95=%s exceeds one second", p95)
			}
		}
	}
}

type shellSnapshotRepository struct {
	tenantstore.Repository
	tenantstore.RequestReader
	active atomic.Int32
}

func (r *shellSnapshotRepository) ReadRequest(ctx context.Context, organization tracker.OrganizationID, read func(context.Context) error) error {
	r.active.Add(1)
	defer r.active.Add(-1)
	return r.RequestReader.ReadRequest(ctx, organization, read)
}
