package runnerfleet_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"reflect"
	"sync/atomic"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/skills"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type heartbeatUploadProbe struct {
	*hubtest.Details
	uploads atomic.Int64
}

func (p *heartbeatUploadProbe) StoreDetail(ctx context.Context, organization string, data []byte) (attachment.ArchiveReference, error) {
	p.uploads.Add(1)
	return p.Details.StoreDetail(ctx, organization, data)
}

func TestRejectedHeartbeatLeavesNoReports(t *testing.T) {
	t.Parallel()
	objects := &heartbeatUploadProbe{Details: hubtest.NewDetails()}
	f := hubfixture.NewDefaultNative(t, hubfixture.Config{RunDetails: objects})
	r := hubfixture.PrepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat)
	r.Enroll(t)
	r.ReportCapacity(t)
	path := f.Base + "/machines/" + string(r.Binding.MachineID) + "/heartbeat"
	now := f.Hub.Now()
	stored := func() (runnerstore.Identity, leasestore.Machine) {
		t.Helper()
		var identity runnerstore.Identity
		var machine leasestore.Machine
		if err := f.Hub.Repository.ReadTenant(t.Context(), f.Organization(), corestore.Serving, func(reader tenantstore.Reader) error {
			var err error
			identity, err = reader.RunnerIdentity(t.Context(), r.Binding.RunnerID)
			if err != nil {
				return err
			}
			machine, err = reader.Machine(t.Context(), string(r.Binding.MachineID))
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return identity, machine
	}
	priorIdentity, priorMachine := stored()
	valid := runnerauth.ProjectConfiguration{ProjectID: string(f.Project.ID), Authority: "local_global_configuration", Source: "cloud_workflow", ObservedAt: now}
	valid.Diagnostics = &runnerauth.ProjectDiagnostics{ObservedAt: now, Source: "runner_runtime_and_durable_attempt_owners"}
	for i := range 20 {
		item := f.Create(t, fmt.Sprintf("Admission %d", i))
		valid.Diagnostics.Admissions = append(valid.Diagnostics.Admissions, runnerauth.DiagnosticAdmission{IssueID: string(item.WorkItemID), WorkItemRevision: int64(item.Revision), ObservedAt: now, Result: "skipped", Predicate: fmt.Sprintf("project_capacity_full_%d", i)})
	}
	badPolicy := hubfixture.Policy()
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
			uploads := objects.uploads.Load()
			payload := map[string]any{"display_name": "Example runner", "capacity": 1, "version": "must-not-persist", "project_configuration": valid}
			payload[test.field] = test.value
			response := f.Hub.Request(t, http.MethodPost, path, r.Redemption.Credential, payload)
			hubfixture.RequireStatus(t, response, http.StatusUnprocessableEntity)
			if objects.uploads.Load() != uploads {
				t.Fatal("refused heartbeat uploaded an object")
			}
			identity, machine := stored()
			if !reflect.DeepEqual(priorIdentity, identity) || !reflect.DeepEqual(priorMachine, machine) {
				t.Fatal("refused heartbeat changed the stored runner or machine")
			}
		})
	}
}
