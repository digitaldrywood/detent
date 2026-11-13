package integrationpage_test

import (
	"context"
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
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest/pgwire"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestIntegrationPageDoesNotReadObjects(t *testing.T) {
	f := newReadFixture(t)
	details, original, admin := f.details, f.original, f.admin
	wire := &pgwire.Wire{PingEveryAcquire: true}
	samples := 1
	if os.Getenv("SHELLPERF_RTT_MS") == "12" {
		wire.Delay = 12 * time.Millisecond
		samples = 20
	}
	measured := pgwire.Repository(t, admin, hubtest.PostgresConfig(t, admin).Connection, wire)
	t.Cleanup(func() { f.use(original) })
	feature := f.pageService().MaintenanceFeature()
	tenant := hubmaintenance.Tenant{Organization: "org_first", Generation: 1}
	budget := (hubmaintenance.Budget{Items: 1, Bytes: hubmaintenance.ByteLimit}).Context(t.Context(), tenant)
	tenant.Generation++
	if err := feature.Step(budget, tenant, hubmaintenance.Budget{}); err == nil {
		t.Fatal("accepted mismatched tenant generation")
	}
	var expected [2]int
	for _, organization := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		f.use(original)
		details.beforeRead = nil
		fixture := f.project(t, organization)
		for _, size := range []int{2, 50} {
			f.use(original)
			details.beforeRead = nil
			markdown := fmt.Sprintf("Workflow for %s with %d states", organization, size)
			raw, err := json.Marshal(markdown)
			if err != nil {
				t.Fatal(err)
			}
			payload, err := eventstore.NewContent(hubmaintenance.LimitDetails(details)).PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: organization}, raw)
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
				probe := &snapshotRepository{Repository: original, RequestReader: original}
				f.use(probe)
				coldObjects := 0
				details.beforeRead = func(context.Context) error {
					coldObjects++
					if probe.active.Load() != 0 {
						t.Error("cold integration holds a request snapshot during object I/O")
					}
					return nil
				}
				requireStatus(t, f.request(t, fixture), http.StatusOK)
				if coldObjects != 1 {
					t.Errorf("cold fallback object reads=%d want=1", coldObjects)
				}
				f.use(original)
				details.beforeRead = nil
			}
			scheduler, err := hubmaintenance.New(selectedTenant{hubmaintenance.Tenant{Organization: string(organization), Generation: 1}}, []hubmaintenance.Feature{f.pageService().MaintenanceFeature()})
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
			f.use(measured)
			durations := make([]time.Duration, 0, samples)
			for range samples {
				started := time.Now()
				wire.Reset()
				response := f.request(t, fixture)
				exchanges, statements := wire.Stop()
				requireStatus(t, response, http.StatusOK)
				var result projectstore.Integration
				decodeResponse(t, response, &result)
				if result.WorkflowMarkdown != markdown || len(result.States) != size {
					t.Errorf("tenant=%s size=%d wrong page: %+v", organization, size, result)
				}
				if len(result.States) == size && (!result.States[0].Dispatchable || !result.States[size-1].Terminal || !result.States[size-1].OperatorOnly || !slices.Equal(result.States[0].Transitions, []string{"State 00", "State 01"})) {
					t.Errorf("workflow state flags or ordered transitions changed: %+v", result.States)
				}
				t.Logf("tenant=%s size=%d statements=%d exchanges=%d objects=%d", organization, size, statements, exchanges, objects)
				if statements != 21 || exchanges != 15 {
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
			t.Logf("tenant=%s size=%d samples=%d rtt=%s p95=%s", organization, size, samples, wire.Delay, p95)
			if wire.Delay > 0 && p95 > time.Second {
				t.Errorf("integration p95=%s exceeds one second", p95)
			}

		}
	}
}

func TestIntegrationPageRechecksAuthorityBeforeLiveRead(t *testing.T) {
	for _, revoked := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		t.Run(string(revoked), func(t *testing.T) {
			f := newReadFixture(t)
			projects := map[tracker.OrganizationID]readProject{}
			for _, organization := range []tracker.OrganizationID{"org_first", "org_first_second"} {
				projects[organization] = f.project(t, organization)
			}
			target := projects[revoked]
			probe := &revokingRepository{Repository: f.original, RequestReader: f.original}
			probe.afterSnapshot = func(ctx context.Context) error {
				return f.original.WriteTenant(context.WithoutCancel(ctx), revoked, corestore.Serving, func(w tenantstore.Writer) error {
					return w.DeleteAccessTokenGrant(ctx, target.actor.Credential.ID, target.project.ID)
				})
			}
			f.use(probe)
			t.Cleanup(func() { f.use(f.original) })
			response := f.request(t, target)
			requireStatus(t, response, http.StatusNotFound)
			var refusal struct {
				Code string `json:"code"`
			}
			decodeResponse(t, response, &refusal)
			if refusal.Code != "not_found" {
				t.Fatalf("revoked live read code = %q, want not_found: %s", refusal.Code, response.Body.String())
			}
			if probe.afterSnapshot != nil {
				t.Fatal("authority was not revoked between the page snapshot and the live read")
			}
			if calls := f.live.Load(); calls != 0 {
				t.Fatalf("live callback ran %d times for a revoked caller", calls)
			}
			fresh := f.request(t, target)
			if fresh.Code != response.Code || fresh.Body.String() != response.Body.String() {
				t.Fatalf("revoked live read answer %d %s differs from a fresh hidden-resource answer %d %s", response.Code, response.Body.String(), fresh.Code, fresh.Body.String())
			}
			if calls := f.live.Load(); calls != 0 {
				t.Fatalf("live callback ran %d times for a hidden project", calls)
			}
			for organization, project := range projects {
				if organization == revoked {
					continue
				}
				requireStatus(t, f.request(t, project), http.StatusOK)
				if calls := f.live.Load(); calls != 1 {
					t.Fatalf("authorized tenant %s live callback calls = %d, want 1", organization, calls)
				}
			}
		})
	}
}
