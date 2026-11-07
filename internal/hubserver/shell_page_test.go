package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/onboarding"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestIntegrationPageDoesNotReadObjects(t *testing.T) {
	details := &memoryRunDetails{}
	service := newPostgresNativeService(t, Config{RunDetails: hubmaintenance.LimitDetails(details)})
	original := service.database.postgres
	value, _ := projectRecordDatabases.Load(original)
	wire := &boardWire{pingEveryAcquire: true}
	samples := 1
	if os.Getenv("SHELLPERF_RTT_MS") == "12" {
		wire.delay = 12 * time.Millisecond
		samples = 20
	}
	measured := boardWireRepository(t, value.(*sql.DB), wire)
	t.Cleanup(func() { service.config.TenantRepository, service.config.AccessRepository = original, original })
	feature := service.IntegrationPageMaintenanceFeature()
	tenant := hubmaintenance.Tenant{Organization: "org_first", Generation: 1}
	budget := (hubmaintenance.Budget{Items: 1, Bytes: hubmaintenance.ByteLimit}).Context(t.Context(), tenant)
	tenant.Generation++
	if err := feature.Step(budget, tenant, hubmaintenance.Budget{}); err == nil {
		t.Fatal("accepted mismatched tenant generation")
	}
	var expected [2]int
	for _, organization := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		service.config.TenantRepository, service.config.AccessRepository = original, original
		details.beforeRead = nil
		fixture := newNativeFixture(t, service, organization, "Example")
		for _, size := range []int{2, 50} {
			service.config.TenantRepository, service.config.AccessRepository = original, original
			details.beforeRead = nil
			markdown := fmt.Sprintf("Workflow for %s with %d states", organization, size)
			raw, err := json.Marshal(markdown)
			if err != nil {
				t.Fatal(err)
			}
			payload, err := eventstore.NewContent(service.runDetailStore()).PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: organization}, raw)
			if err != nil {
				t.Fatal(err)
			}
			err = original.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				record, err := w.ProjectRecord(t.Context(), fixture.project.ID)
				if err != nil {
					return err
				}
				record.Settings.WorkflowKey, record.Settings.WorkflowSHA256 = payload.PayloadObjectKey, payload.PayloadSHA256
				record.Settings.WorkflowBytes, record.Settings.WorkflowContentBytes = payload.PayloadBytes, payload.PayloadContentBytes
				record.States = nil
				for index := range size {
					record.States = append(record.States, tracker.NativeState{Name: fmt.Sprintf("State %02d", index), Dispatchable: index == 0, Terminal: index == size-1, OperatorOnly: index == size-1, Transitions: []string{}})
					if index == 0 {
						record.States[index].Transitions = []string{"State 00", "State 01"}
					}
				}
				_, err = w.UpdateProjectRecord(t.Context(), record)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			if size == 2 {
				probe := &shellSnapshotRepository{Repository: original, RequestReader: original}
				service.config.TenantRepository = probe
				coldObjects := 0
				details.beforeRead = func(context.Context) error {
					coldObjects++
					if probe.active.Load() != 0 {
						t.Error("cold integration holds a request snapshot during object I/O")
					}
					return nil
				}
				requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, fixture.base+"/integration", fixture.token, nil), http.StatusOK)
				if coldObjects != 1 {
					t.Errorf("cold fallback object reads=%d want=1", coldObjects)
				}
				service.config.TenantRepository = original
				details.beforeRead = nil
			}
			previousHosted := service.config.Hosted
			service.config.Hosted = &HostedConfig{OrganizationID: string(organization)}
			scheduler, err := service.newReadModelMaintenance()
			service.config.Hosted = previousHosted
			if err != nil {
				t.Fatal(err)
			}
			for range 8 {
				if err := scheduler.Tick(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			objects := 0
			details.beforeRead = func(context.Context) error { objects++; return nil }
			service.config.TenantRepository, service.config.AccessRepository = measured, measured
			durations := make([]time.Duration, 0, samples)
			for range samples {
				started := time.Now()
				wire.reset()
				response := performHubAPIRequest(t, service, http.MethodGet, fixture.base+"/integration", fixture.token, nil)
				exchanges, statements := wire.stop()
				requireNativeStatus(t, response, http.StatusOK)
				var result ProjectIntegration
				decodeHubResponse(t, response, &result)
				if result.WorkflowMarkdown != markdown || len(result.States) != size {
					t.Errorf("tenant=%s size=%d wrong page: %+v", organization, size, result)
				}
				if len(result.States) == size && (!result.States[0].Dispatchable || !result.States[size-1].Terminal || !result.States[size-1].OperatorOnly || !slices.Equal(result.States[0].Transitions, []string{"State 00", "State 01"})) {
					t.Errorf("workflow state flags or ordered transitions changed: %+v", result.States)
				}
				t.Logf("tenant=%s size=%d statements=%d exchanges=%d objects=%d", organization, size, statements, exchanges, objects)
				if statements != 18 || exchanges != 15 {
					t.Errorf("wire budget: statements=%d exchanges=%d", statements, exchanges)
				}
				if objects != 0 {
					t.Errorf("integration page fetched %d objects", objects)
				}
				if expected != [2]int{} && expected != [2]int{statements, exchanges} {
					t.Errorf("cost changed with tenant or size: %v -> %v", expected, [2]int{statements, exchanges})
				}
				expected = [2]int{statements, exchanges}
				durations = append(durations, time.Since(started))
			}
			slices.Sort(durations)
			p95 := durations[(len(durations)*95+99)/100-1]
			t.Logf("tenant=%s size=%d samples=%d rtt=%s p95=%s", organization, size, samples, wire.delay, p95)
			if wire.delay > 0 && p95 > time.Second {
				t.Errorf("integration p95=%s exceeds one second", p95)
			}

		}
	}
}

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
			service.config.Hosted = &HostedConfig{OrganizationID: string(organization)}
			scheduler, err := service.newReadModelMaintenance()
			service.config.Hosted = previousHosted
			if err != nil {
				t.Fatal(err)
			}
			for range 16 {
				if err := scheduler.Tick(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
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
				if statements != 17 || exchanges != 15 || objects != 0 {
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
