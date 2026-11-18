package runnerfleet_test

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerHubProblems(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		code     string
		protocol int
		rejected bool
	}{
		{"version_unsupported", 3, false},
		{"settings_rejected", 2, true},
		{"update_applied_not_running", 2, false},
	} {
		t.Run(test.code, func(t *testing.T) {
			var clock atomic.Int64
			clock.Store(time.Now().UTC().Truncate(time.Microsecond).UnixNano())
			f := hubfixture.NewDefaultNative(t, hubfixture.Config{Clock: func() time.Time { return time.Unix(0, clock.Load()).UTC() }})
			r := hubfixture.PrepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim)
			r.Enroll(t)
			heartbeat := map[string]any{"display_name": "Runner", "capacity": 2, "version": "test", "protocol_major": test.protocol, "settings_rejected": test.rejected, "backend_isolation": isolation.Report{"codex": {isolation.NativeTrusted}}, "problems": []runnerauth.Problem{runnerauth.NewProblem("keep_awake_failed")}}
			if test.code == "update_applied_not_running" {
				now := f.Hub.Now()
				running := runnerauth.BuildEvidence{Version: "1.0.0", Commit: strings.Repeat("a", 40), Source: "release", SHA256: strings.Repeat("b", 64), OS: "linux", Architecture: "amd64", ObservedAt: now}
				applied := running
				applied.Version, applied.Commit, applied.SHA256, applied.VerifiedRelease = "1.0.1", strings.Repeat("c", 40), strings.Repeat("d", 64), true
				report := &runnerauth.UpdateObservation{Discovery: "unknown", Protocol: 1, Service: "detent", Supported: true, Running: running, ObservedAt: now,
					Receipt: &runnerauth.UpdateReceipt{Status: "applied", ExecutionArtifact: true, Applied: &applied, ObservedAt: now,
						Request: runnerauth.UpdateRequest{ID: "update-fixture", Service: "detent", Release: true, Version: "1.0.1", RequestedAt: now}}}
				report.Revision = report.BuildRevision()
				report.Receipt.Request.ExpectedBuildRevision = report.Revision
				if err := f.Hub.Repository.WriteTenant(t.Context(), f.Project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
					identity, err := w.RunnerIdentity(t.Context(), r.Binding.RunnerID)
					if err != nil {
						return err
					}
					if err := w.CreateRunnerUpdateRequest(t.Context(), runnerstore.UpdateRequestRecord{OrganizationID: f.Project.OrganizationID, RunnerID: r.Binding.RunnerID, Request: report.Receipt.Request}); err != nil {
						return err
					}
					return w.SetRunnerUpdateDesired(t.Context(), r.Binding.RunnerID, identity.Revision, report.Receipt.Request.ID)
				}); err != nil {
					t.Fatal(err)
				}
				heartbeat["update"] = report
				heartbeat["version"], heartbeat["os"], heartbeat["architecture"] = "1.0.0", "linux", "amd64"
			}
			post := func() {
				t.Helper()
				hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, f.Base+"/machines/"+string(r.Binding.MachineID)+"/heartbeat", r.Redemption.Credential, heartbeat), http.StatusOK)
			}
			post()
			read := func() runnerauth.Runner { t.Helper(); return readProblemFixtureRouting(t, f, r) }
			view := read()
			if view.Health != "needs_attention" || len(view.Problems) != 2 || view.Problems[0].FirstSeen.IsZero() {
				t.Fatalf("runner=%+v", view)
			}
			found := false
			for _, problem := range view.Problems {
				found = found || problem.Code == test.code
			}
			if !found {
				t.Fatalf("missing %s: %+v", test.code, view.Problems)
			}
			if test.code == "update_applied_not_running" {
				for _, problem := range view.Problems {
					if problem.Code == test.code && (!strings.Contains(problem.Message, "1.0.0") || !strings.Contains(problem.Message, "1.0.1")) {
						t.Fatalf("update skew lost build versions: %+v", problem)
					}
				}
			}
			if exclusions := view.Exclusions(f.Project.ID, policy.Requirements{}, false); len(exclusions) != 0 {
				t.Fatalf("diagnostics affected dispatch: %+v", exclusions)
			}
			current := clock.Load()
			clock.Store(current + int64(runnerauth.HeartbeatTimeout))
			view = read()
			clock.Store(current)
			if view.Health != "needs_attention" || view.ConnectionHealth != "offline" || len(view.Exclusions(f.Project.ID, policy.Requirements{}, false)) == 0 {
				t.Fatalf("offline diagnostic weakened dispatch: %+v", view)
			}
			heartbeat["protocol_major"] = 2
			heartbeat["settings_rejected"] = false
			heartbeat["backend_isolation"] = isolation.Report{"codex": {isolation.Sandbox, isolation.NativeTrusted}}
			delete(heartbeat, "problems")
			if report, ok := heartbeat["update"].(*runnerauth.UpdateObservation); ok {
				report.Running = *report.Receipt.Applied
				heartbeat["version"] = report.Running.Version
				report.Receipt.Status = "running"
				report.Receipt.Running = &report.Running
				report.Revision = report.BuildRevision()
			}
			post()
			view = read()
			if view.Health != "online" || len(view.Problems) != 0 {
				t.Fatalf("did not clear: %+v", view)
			}
		})
	}
}

func TestRunnerConfigurationChangesDoNotRequireApproval(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		want int
	}{
		{"aligned", 0},
		{"local policies agree but Hub refuses", 0},
		{"pending selection still claims", 0},
		{"compatible approved revision", 0},
		{"compatible unapproved revision", 0},
		{"admission observation", 0},
		{"effective overrides admission", 0},
		{"policy observation", 0},
		{"approval revoked", 0},
		{"unauthorized project", 0},
		{"no policy evidence", 0},
		{"multiple projects", 0},
		{"clears after approval", 0},
		{"clears after alignment", 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := hubfixture.NewDefaultNative(t, hubfixture.Config{})
			r := hubfixture.PrepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim)
			r.Enroll(t)
			sources := workflowconfig.ProjectDefinitionSources{
				ConfigPath: "detent.yaml", HasConfig: true,
				Config:       []byte("schema: 1\ntracker:\n  kind: hub_native\n  repository: example-org/example-project\ngate:\n  run: make check-land\n  required_status_checks: []\n"),
				WorkflowPath: "WORKFLOW.md", Workflow: []byte("Implement the assigned issue.\n"),
			}
			workflow, err := workflowconfig.ParseProjectDefinition(sources)
			if err != nil {
				t.Fatal(err)
			}
			approved, err := workflowconfig.ResolvePolicy(workflow)
			if err != nil {
				t.Fatal(err)
			}
			workflow.Authored.Version = 1
			old, err := workflowconfig.ResolvePolicy(workflow)
			if err != nil {
				t.Fatal(err)
			}
			if old.ID == approved.ID {
				t.Fatal("historical canonical policy must have a distinct identity")
			}
			expectedID := hubfixture.Policy().ID
			if test.name == "compatible approved revision" {
				hubfixture.ApprovePolicy(t, f.Hub, f.Base+"/policy", old)
				expectedID = old.ID
			}
			hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPut, f.Base+"/policy", hubfixture.AdminToken, policy.Change{ExpectedID: expectedID, Policy: approved}), http.StatusOK)
			sources.Workflow = []byte("Implement the assigned issue and validate the policy.\n")
			changedWorkflow, err := workflowconfig.ParseProjectDefinition(sources)
			if err != nil {
				t.Fatal(err)
			}
			mismatch, err := workflowconfig.ResolvePolicy(changedWorkflow)
			if err != nil {
				t.Fatal(err)
			}
			effective, selected := mismatch, mismatch
			switch test.name {
			case "aligned", "effective overrides admission":
				effective, selected = approved, approved
			case "pending selection still claims":
				effective = approved
			case "compatible approved revision", "compatible unapproved revision":
				effective, selected = old, old
			}
			view := runnerauth.ProjectConfiguration{ProjectID: string(f.Project.ID), Authority: "local_global_configuration", Registered: true, RuntimeRegistered: true, Source: "configured_committed_workflow", SelectedPolicy: &selected, EffectivePolicy: &effective, ObservedAt: f.Hub.Now()}
			post := func(configuration *runnerauth.ProjectConfiguration) {
				t.Helper()
				hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, f.Base+"/machines/"+string(r.Binding.MachineID)+"/heartbeat", r.Redemption.Credential, map[string]any{"display_name": "runner", "capacity": 2, "version": "test", "protocol_major": 2, "backend_isolation": r.Redemption.BackendIsolation, "project_configuration": configuration}), http.StatusOK)
			}
			var configuration *runnerauth.ProjectConfiguration
			switch test.name {
			case "admission observation", "policy observation", "no policy evidence":
			default:
				configuration = &view
			}
			if test.name == "admission observation" || test.name == "effective overrides admission" || test.name == "compatible approved revision" || test.name == "compatible unapproved revision" {
				observation := tracker.NativeAdmissionObservation{Context: tracker.NativeAdmissionContext{PolicyID: mismatch.ID, ObservedAt: f.Hub.Now()}}
				if test.name == "compatible approved revision" || test.name == "compatible unapproved revision" {
					observation.Context.PolicyID = old.ID
				}
				if err := f.Hub.Repository.WriteTenant(t.Context(), f.Project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
					identity, err := w.RunnerIdentity(t.Context(), r.Binding.RunnerID)
					if err != nil {
						return err
					}
					observation.RunnerRevision, observation.ReceivedAt = identity.Revision, f.Hub.Now()
					return w.ReplaceRunnerAdmissionObservation(t.Context(), r.Binding.RunnerID, f.Project.ID, observation)
				}); err != nil {
					t.Fatal(err)
				}
			}
			if test.name == "policy observation" {
				if err := recordRunnerConfigurationPolicyObservation(t, f, r.Binding.RunnerID, mismatch); err != nil {
					t.Fatal(err)
				}
			}
			post(configuration)
			if test.name == "approval revoked" {
				f.ClearPolicy(t)
			}
			if test.name == "unauthorized project" {
				if err := f.Hub.Repository.WriteTenant(t.Context(), f.Project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
					identity, err := w.RunnerIdentity(t.Context(), r.Binding.RunnerID)
					if err != nil {
						return err
					}
					return w.DeleteAccessTokenGrant(t.Context(), identity.TokenID, f.Project.ID)
				}); err != nil {
					t.Fatal(err)
				}
			}
			if test.name == "multiple projects" {
				second := hubfixture.NewNative(t, f.Hub, f.Project.OrganizationID, "second-policy-project")
				hubfixture.ApprovePolicy(t, f.Hub, second.Base+"/policy", approved)
				if err := f.Hub.Repository.WriteTenant(t.Context(), f.Project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
					identity, err := w.RunnerIdentity(t.Context(), r.Binding.RunnerID)
					if err != nil {
						return err
					}
					return w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: f.Project.OrganizationID, TokenID: identity.TokenID, ProjectID: second.Project.ID})
				}); err != nil {
					t.Fatal(err)
				}
				if err := recordRunnerConfigurationPolicyObservation(t, second, r.Binding.RunnerID, mismatch); err != nil {
					t.Fatal(err)
				}
			}
			read := func(want int) runnerauth.Runner {
				t.Helper()
				response := f.Hub.Request(t, http.MethodGet, r.IdentityPath()+"/routing", hubfixture.AdminToken, nil)
				hubfixture.RequireStatus(t, response, http.StatusOK)
				var runner runnerauth.Runner
				hubfixture.Decode(t, response, &runner)
				if len(runner.Problems) != want || (runner.Health == "needs_attention") != (want > 0) {
					t.Fatalf("runner health=%s problems=%+v, want %d", runner.Health, runner.Problems, want)
				}
				return runner
			}
			read(test.want)
			if test.name == "compatible approved revision" || test.name == "compatible unapproved revision" {
				item := f.Create(t, "policy capacity candidate")
				configurePolicyCapacityRunner(t, f, r, f.Hub.Now(), f.Hub.Now())
				response := f.Hub.Request(t, http.MethodGet, f.Base+"/work-items/"+string(item.WorkItemID)+"/runtime", f.Token, nil)
				hubfixture.RequireStatus(t, response, http.StatusOK)
				var evidence tracker.NativeRuntimeEvidence
				hubfixture.Decode(t, response, &evidence)
				if len(evidence.Capacity) != 1 || evidence.Capacity[0].RunnerID != r.Binding.RunnerID || (evidence.Capacity[0].Available > 0) != (test.want == 0) {
					t.Fatalf("policy compatibility capacity=%+v", evidence.Capacity)
				}
			}
			if test.name == "clears after approval" {
				hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPut, f.Base+"/policy", hubfixture.AdminToken, policy.Change{ExpectedID: approved.ID, Policy: mismatch}), http.StatusOK)
				read(0)
			}
			if test.name == "clears after alignment" {
				view.SelectedPolicy, view.EffectivePolicy = &approved, &approved
				post(&view)
				read(0)
			}
		})
	}
}

func recordRunnerConfigurationPolicyObservation(t *testing.T, f hubfixture.Native, runner string, descriptor policy.Descriptor) error {
	t.Helper()
	revision, err := eventstore.NewContent(f.Hub.Config.RunDetails).PrepareProjectPolicy(t.Context(), f.Project.OrganizationID, descriptor)
	if err != nil {
		return err
	}
	revision.ProjectID = f.Project.ID
	return f.Hub.Repository.WriteTenant(t.Context(), f.Project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.RecordProjectPolicyObservation(t.Context(), policystore.Observation{OrganizationID: f.Project.OrganizationID, ProjectID: f.Project.ID, RunnerID: runner, PolicyID: descriptor.ID, ObservedAt: f.Hub.Now(), Revision: revision})
		return err
	})
}

func configurePolicyCapacityRunner(t *testing.T, f hubfixture.Native, runner hubfixture.Runner, heartbeat, created time.Time) {
	t.Helper()
	if err := f.Hub.Repository.WriteTenant(t.Context(), f.Project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		identity, err := w.RunnerIdentity(t.Context(), runner.Binding.RunnerID)
		if err != nil {
			return err
		}
		identity.State = "active"
		identity.ProjectIDs = []tracker.ProjectID{f.Project.ID}
		identity.ProjectRanks = map[tracker.ProjectID]int{f.Project.ID: 1}
		if err := w.UpdateRunnerRouting(t.Context(), identity.Revision, identity); err != nil {
			return err
		}
		identity.LastHeartbeatAt = heartbeat
		identity.ExpiresAt = f.Hub.Now().Add(time.Hour)
		return w.UpdateRunnerIdentity(t.Context(), identity)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Hub.Admin.ExecContext(t.Context(), "UPDATE runner_identities SET created_at=$1 WHERE organization_id=$2 AND runner_id=$3", created, f.Project.OrganizationID, runner.Binding.RunnerID); err != nil {
		t.Fatal(err)
	}
}

func readProblemFixtureRouting(t *testing.T, f hubfixture.Native, runner hubfixture.Runner) runnerauth.Runner {
	t.Helper()
	response := f.Hub.Request(t, http.MethodGet, runner.IdentityPath()+"/routing", hubfixture.AdminToken, nil)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var view runnerauth.Runner
	hubfixture.Decode(t, response, &view)
	if view.RunnerID != runner.Binding.RunnerID || view.MachineID != runner.Binding.MachineID || view.OrganizationID != f.Project.OrganizationID {
		t.Fatalf("routing returned wrong runner: %+v", view)
	}
	return view
}
