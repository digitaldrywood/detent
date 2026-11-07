package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestIntegrationPageDoesNotReadObjects(t *testing.T) {
	details := &memoryRunDetails{}
	service := newPostgresNativeService(t, Config{RunDetails: hubmaintenance.LimitDetails(details)})
	service.echo.GET(nativeBase+"/integration", service.getProjectIntegrationPage, service.requireAPIScope(apiScopeWorker, apiScopeOperator))
	original := service.database.postgres
	value, _ := projectRecordDatabases.Load(original)
	wire := &boardWire{}
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
					record.States = append(record.States, tracker.NativeState{Name: fmt.Sprintf("State %02d", index), Transitions: []string{"State 00"}})
				}
				_, err = w.UpdateProjectRecord(t.Context(), record)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			if size == 2 {
				requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, fixture.base+"/integration", fixture.token, nil), http.StatusServiceUnavailable)
			}
			scheduler, err := hubmaintenance.New(maintenanceTenantRegistry{{Organization: string(organization), Generation: 1}}, []hubmaintenance.Feature{service.IntegrationPageMaintenanceFeature()})
			if err != nil {
				t.Fatal(err)
			}
			if err := scheduler.Tick(t.Context()); err != nil {
				t.Fatal(err)
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
				t.Logf("tenant=%s size=%d statements=%d exchanges=%d objects=%d", organization, size, statements, exchanges, objects)
				if statements != 16 || exchanges != 13 {
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
