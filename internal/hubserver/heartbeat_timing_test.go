package hubserver

import (
	"fmt"
	"net/http"
	"reflect"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/skills"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHeartbeatRefusalLeavesRunnerAndMachineUnchanged(t *testing.T) {
	t.Parallel()
	objects := &changeObjectFixture{}
	f := newDefaultNativeFixture(t, Config{Content: objects.call})
	r := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat)
	r.enroll(t)
	path := f.base + "/machines/" + string(r.binding.MachineID) + "/heartbeat"
	response := performHubAPIRequest(t, f.service, http.MethodPost, path, r.redemption.Credential, map[string]any{"display_name": "Example runner", "capacity": 2, "version": "test"})
	requireNativeStatus(t, response, http.StatusOK)
	ctx := t.Context()
	stored := func() (runnerstore.Identity, leasestore.Machine) {
		t.Helper()
		var identity runnerstore.Identity
		var machine leasestore.Machine
		if err := f.service.config.TenantRepository.ReadTenant(ctx, f.project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
			var err error
			identity, err = reader.RunnerIdentity(ctx, r.binding.RunnerID)
			if err != nil {
				return err
			}
			machine, err = reader.Machine(ctx, string(r.binding.MachineID))
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return identity, machine
	}
	priorIdentity, priorMachine := stored()
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
			response := performHubAPIRequest(t, f.service, http.MethodPost, path, r.redemption.Credential, map[string]any{"display_name": "Refused runner", "capacity": 1, "version": "must-not-persist", "project_configuration": valid, test.field: test.value})
			requireNativeStatus(t, response, http.StatusUnprocessableEntity)
			objects.mu.Lock()
			uploads := objects.writes - writesBefore
			objects.mu.Unlock()
			if uploads != 0 {
				t.Errorf("invalid heartbeat uploaded %d objects", uploads)
			}
			identity, machine := stored()
			if !reflect.DeepEqual(priorIdentity, identity) || !reflect.DeepEqual(priorMachine, machine) {
				t.Fatal("refused heartbeat changed the stored runner or machine")
			}
		})
	}
}
