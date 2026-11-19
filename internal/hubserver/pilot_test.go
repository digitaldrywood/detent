package hubserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPilotReconciliationHonorsRepositoryBinding(t *testing.T) {
	t.Parallel()
	clock := &leaseTestClock{value: time.Now().UTC().Truncate(time.Microsecond)}
	f := newDefaultNativeFixture(t, Config{Clock: clock.Now, ReconcileInterval: time.Millisecond})
	backend := &scriptedReconcileBackend{}
	f.service.config.ReconcileBackend = backend
	for _, enabled := range []bool{true, false} {
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
			project, err := w.ProjectRecord(t.Context(), f.project.ID)
			if err != nil {
				return err
			}
			project.Settings.Repository, project.Settings.RepositoryEnabled = "example-org/example-project", enabled
			_, err = w.UpdateProjectRecord(t.Context(), project)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if enabled {
			backend.steps = append(backend.steps, reconcileStep{snapshot: ReconcileSnapshot{Repository: RepositorySource{NodeID: "R_repo", Owner: "example-org", Name: "example-project", UpdatedAt: clock.Now()}}})
		}
		clock.Advance(time.Second)
		if err := f.service.maintainGitHubReconciliationRecords(t.Context()); err != nil {
			t.Fatal(err)
		}
		requests := backend.Requests()
		if len(requests) != 1 || requests[0].Profile != "native" || !requests[0].SkipIssues || requests[0].SkipRepository {
			t.Fatalf("repository reconciliation requests=%+v (enabled=%t)", requests, enabled)
		}
	}
}

func pilotHostedRunner(t *testing.T, f *browserHostedFixture, index int) runnerauth.Redemption {
	t.Helper()
	base := "/api/v2/organizations/" + f.service.config.Hosted.OrganizationID
	binding := runnerauth.NewBinding()
	response := f.setupRequest(t, "owner", http.MethodPost, base+"/runner-enrollments", runnerauth.EnrollmentRequest{Binding: binding, ProjectIDs: []tracker.ProjectID{tracker.ProjectID(f.project), tracker.ProjectID(f.privateProject)}, Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events, runnerauth.Collaborate}, TTLSeconds: 900})
	requireNativeStatus(t, response, http.StatusCreated)
	var enrollment runnerauth.Enrollment
	decodeHubResponse(t, response, &enrollment)
	credential, err := apikey.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	redemption := runnerauth.Redemption{BackendIsolation: isolation.Report{"test": {isolation.Sandbox, isolation.NativeTrusted}}, Binding: binding, Credential: credential, Hostname: fmt.Sprintf("pilot-host-%d", index), DisplayName: fmt.Sprintf("Pilot runner %d", index), Capacity: 2, Version: "test", OS: "linux", Architecture: "amd64"}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, base+"/runner-enrollments/redeem", enrollment.Token, redemption), http.StatusCreated)
	requireNativeStatus(t, f.setupRequest(t, "owner", http.MethodPut, base+"/runners/"+binding.RunnerID+"/routing", map[string]any{"expected_revision": 1, "display_name": redemption.DisplayName, "tags": []string{"linux", "gpu"}, "state": "active", "capacity_limit": 2, "project_ids": []string{f.project, f.privateProject}}), http.StatusOK)
	reportRunnerCapacity(t, f.service, base+"/projects/"+f.project, credential, binding.MachineID, redemption.DisplayName, redemption.BackendIsolation, redemption.Capacity)
	return redemption
}

func TestPilotHostedWorkloads(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		runners int
		jobs    int
	}{
		{"idle_baseline", 0, 0},
		{"idle_one_runner", 1, 0},
		{"idle_sixteen_runners", 16, 0},
		{"active_one_runner", 1, 100},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newBrowserHostedFixture(t, true)
			hostedTestPlans(t, f.service, map[string]int64{"registered_runners": 20, "connected_runners": 20})
			base := "/api/v2/organizations/org_browser_preview/projects/" + f.project
			descriptor := hubTestPolicy()
			requireNativeStatus(t, f.setupRequest(t, "owner", http.MethodPut, base+"/onboarding/policy", policy.Change{Policy: descriptor}), http.StatusOK)
			var runners []runnerauth.Redemption
			for index := range test.runners {
				runners = append(runners, pilotHostedRunner(t, f, index))
			}
			before := pilotUsage(t, f.service)
			started := time.Now()
			for range 10 {
				for _, runner := range runners {
					path := base + "/machines/" + string(runner.MachineID) + "/heartbeat"
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, runner.Credential, map[string]any{"backend_isolation": isolation.Report{"test": {isolation.Sandbox, isolation.NativeTrusted}}, "display_name": runner.DisplayName, "capacity": 2, "version": "test", "os": "linux", "architecture": "amd64"}), http.StatusOK)
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, base+"/work-items?state=Todo", runner.Credential, nil), http.StatusOK)
				}
				requireNativeStatus(t, f.page(t, "owner", "/organization/plan"), http.StatusOK)
			}
			for index := range test.jobs {
				var context strings.Builder
				for segment := range 32 {
					fmt.Fprintf(&context, "workload%03dsegment%02dx ", index, segment)
				}
				response := f.setupRequest(t, "owner", http.MethodPost, base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("pilot-job-%d", index)}, Title: fmt.Sprintf("Pilot workload%03dx", index), Body: strings.Repeat(context.String(), 2) + issueContractTestSections, State: "Todo"})
				requireNativeStatus(t, response, http.StatusOK)
				var issue tracker.NativeIssue
				decodeHubResponse(t, response, &issue)
				path := base + "/work-items/" + string(issue.WorkItemID)
				requireNativeStatus(t, f.setupRequest(t, "owner", http.MethodPost, path+"/comments", tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "discussion"}, Body: "Synthetic pilot discussion"}), http.StatusOK)
				runner := runners[0]
				response = performHubAPIRequest(t, f.service, http.MethodPost, base+"/claims", runner.Credential, tracker.NativeClaim{PolicyID: descriptor.ID, WorkItemID: issue.WorkItemID, MachineID: runner.MachineID, SessionID: fmt.Sprintf("session-%d", index), TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}})
				requireNativeStatus(t, response, http.StatusOK)
				var lease tracker.NativeLease
				decodeHubResponse(t, response, &lease)
				start := nativeStartedEvent(lease)
				finish := start
				finish.Type, finish.IdempotencyKey, finish.Data.Sequence, finish.Data.Outcome = "run.finished", "finish", 2, "succeeded"
				for _, event := range []tracker.NativeRunEvent{start, finish} {
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", runner.Credential, event), http.StatusOK)
				}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, base+"/leases/"+string(lease.ID)+"/release", runner.Credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
			}
			elapsed := time.Since(started)
			after := pilotUsage(t, f.service)
			for metric := range after {
				after[metric] -= before[metric]
			}
			if after["heartbeats"] != int64(test.runners*10) || after["ingested_events"] != int64(test.jobs*4) {
				t.Fatalf("usage does not match admitted workload: %v", after)
			}
			usage, err := hubapi.FeatureResult(f.service.database.plansService().PlanUsage(t.Context(), time.Now()))
			if err != nil || usage.Usage["concurrent_work"] != 0 {
				t.Fatalf("completed workload retained reservations: %v, %v", usage.Usage, err)
			}
			payload, err := json.Marshal(map[string]any{"workload": test.name, "runners": test.runners, "jobs": test.jobs, "rounds": 10, "elapsed_microseconds": elapsed.Microseconds(), "usage_delta": after})
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("PILOT workload %s", payload)
		})
	}
}

func pilotUsage(t *testing.T, service *Service) map[string]int64 {
	t.Helper()
	rows, err := hostedPostgresAdmin(t, service).QueryContext(t.Context(), "SELECT metric, SUM(amount)::bigint FROM hosted_usage_windows GROUP BY metric")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	values := make(map[string]int64)
	for rows.Next() {
		var metric string
		var amount int64
		if err := rows.Scan(&metric, &amount); err != nil {
			t.Fatal(err)
		}
		values[metric] = amount
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return values
}
