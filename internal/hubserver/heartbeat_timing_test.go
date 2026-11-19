package hubserver

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/multitracer"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/skills"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHeartbeatPreparationBeforeWriter(t *testing.T) {
	t.Parallel()
	objects := &changeObjectFixture{}
	repository, _ := projectRecordPostgresWithAdmin(t, 4, func(config *pgxpool.Config) {
		config.ConnConfig.Tracer = multitracer.New(config.ConnConfig.Tracer, claimPreflightTrace{})
	})
	f := newDefaultNativeFixture(t, Config{Content: objects.call, TenantRepository: repository})
	if err := f.service.database.postgres.Close(); err != nil {
		t.Fatal(err)
	}
	f.service.database.postgres = repository
	f.service.config.TenantRepository = repository
	f.service.requestMetrics.Repository = repository
	f.service.config.AccessRepository = repository
	f.service.config.BillingRepository = repository
	f.service.database.billingRepository = repository
	r := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat)
	r.enroll(t)
	path := f.base + "/machines/" + string(r.binding.MachineID) + "/heartbeat"
	positive := &claimPreflightProbe{attempted: make(chan struct{})}
	request := newHubAPIRequest(t, http.MethodPost, path, r.redemption.Credential, map[string]any{"display_name": "Example runner", "capacity": 2, "version": "test"})
	response := httptest.NewRecorder()
	f.service.Handler().ServeHTTP(response, request.WithContext(context.WithValue(request.Context(), claimPreflightProbeKey{}, positive)))
	requireNativeStatus(t, response, http.StatusOK)
	if positive.writers.Load() == 0 || positive.mutations.Load() == 0 {
		t.Fatal("heartbeat probe did not observe PostgreSQL writes")
	}
	f.service.tokenUseWork.Wait()
	now := f.service.config.Clock()
	valid := runnerauth.ProjectConfiguration{ProjectID: string(f.project.ID), Authority: "local_global_configuration", Source: "cloud_workflow", ObservedAt: now}
	valid.Diagnostics = &runnerauth.ProjectDiagnostics{ObservedAt: now, Source: "runner_runtime_and_durable_attempt_owners"}
	for i := range 20 {
		item := f.create(t, fmt.Sprintf("Admission %d", i))
		valid.Diagnostics.Admissions = append(valid.Diagnostics.Admissions, runnerauth.DiagnosticAdmission{IssueID: string(item.WorkItemID), WorkItemRevision: int64(item.Revision), ObservedAt: now, Result: "skipped", Predicate: fmt.Sprintf("project_capacity_full_%d", i)})
	}
	badPolicy := hubTestPolicy()
	badPolicy.ID = "invalid"
	for _, test := range []struct {
		name, field string
		value       any
	}{
		{"invalid policy digest", "project_configuration", func() runnerauth.ProjectConfiguration { view := valid; view.SelectedPolicy = &badPolicy; return view }()},
		{"foreign configuration", "project_configuration", func() runnerauth.ProjectConfiguration { view := valid; view.ProjectID = "prj_foreign"; return view }()},
		{"invalid diagnostics", "project_configuration", func() runnerauth.ProjectConfiguration {
			view := valid
			view.Diagnostics = &runnerauth.ProjectDiagnostics{}
			return view
		}()},
		{"invalid update", "update", runnerauth.UpdateObservation{}},
		{"invalid provider reports", "provider_reports", []providercapacity.Report{{Backend: "invalid"}}},
		{"invalid checkout", "checkout_repository", "invalid"},
		{"invalid workspace isolation", "workspace_isolation", "invalid"},
		{"invalid admission", "admission", tracker.NativeAdmissionObservation{}},
		{"invalid capacity", "capacity_configuration", runnerauth.CapacityConfig{}},
		{"invalid local checks", "local_checks", runnerauth.LocalChecks{}},
		{"invalid skills", "skills", []skills.ProviderSkill{{Name: ""}}},
		{"invalid sprite", "sprite_name", "invalid sprite"},
		{"foreign sprite", "sprite_name", "other-runner"},
		{"invalid problem", "problems", []runnerauth.Problem{{Code: "invalid"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			objects.mu.Lock()
			writesBefore := objects.writes
			objects.mu.Unlock()
			request := newHubAPIRequest(t, http.MethodPost, path, r.redemption.Credential, map[string]any{"display_name": "Example runner", "capacity": 2, "version": "test", "project_configuration": valid, test.field: test.value})
			response := httptest.NewRecorder()
			probe := &claimPreflightProbe{attempted: make(chan struct{})}
			f.service.Handler().ServeHTTP(response, request.WithContext(context.WithValue(request.Context(), claimPreflightProbeKey{}, probe)))
			requireNativeStatus(t, response, http.StatusUnprocessableEntity)
			objects.mu.Lock()
			uploads := objects.writes - writesBefore
			objects.mu.Unlock()
			if uploads != 0 {
				t.Errorf("invalid heartbeat uploaded %d objects", uploads)
			}
			if probe.writers.Load() != 0 || probe.mutations.Load() != 0 {
				t.Fatalf("invalid preparation opened %d PostgreSQL writers and issued %d mutations", probe.writers.Load(), probe.mutations.Load())
			}
		})
	}
}
