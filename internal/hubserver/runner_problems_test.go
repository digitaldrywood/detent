package hubserver

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerProblemsHeartbeat(t *testing.T) {
	for _, code := range []string{"tier_unavailable", "backend_missing", "host_service_unreachable", "settings_invalid", "keep_awake_failed"} {
		t.Run(code, func(t *testing.T) {
			f := newDefaultNativeFixture(t, Config{})
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat)
			r.enroll(t)
			heartbeat := map[string]any{"display_name": "Runner", "capacity": 2, "version": "test", "backend_isolation": isolation.Report{"test": {isolation.Sandbox, isolation.NativeTrusted}}}
			heartbeat["problems"] = []map[string]any{{"project_id": "site", "code": code, "message": "Tooling needs repair", "fix_hint": "Repair the runner tooling"}}
			post := func() {
				t.Helper()
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, heartbeat), http.StatusOK)
			}
			read := func() (string, []runnerauth.Problem) {
				t.Helper()
				response := performHubAPIRequest(t, f.service, http.MethodGet, r.identityPath()+"/routing", testHubAdminToken, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var view struct {
					Health   string               `json:"health"`
					Problems []runnerauth.Problem `json:"problems"`
				}
				decodeHubResponse(t, response, &view)
				return view.Health, view.Problems
			}
			post()
			health, problems := read()
			if health != "needs_attention" || len(problems) != 1 || problems[0].Code != code || problems[0].ProjectID != "site" || problems[0].FixHint == "" || problems[0].FirstSeen.IsZero() {
				t.Fatalf("health=%s problems=%+v", health, problems)
			}
			first := problems[0].FirstSeen
			post()
			_, problems = read()
			if !problems[0].FirstSeen.Equal(first) {
				t.Fatal("first-seen time changed for a continuing problem")
			}
			delete(heartbeat, "problems")
			post()
			health, problems = read()
			if health != "online" || len(problems) != 0 {
				t.Fatalf("problem did not clear: health=%s problems=%+v", health, problems)
			}
		})
	}
}

func TestRunnerHubProblems(t *testing.T) {
	for _, test := range []struct {
		code     string
		protocol int
		rejected bool
	}{
		{"version_unsupported", 3, false},
		{"settings_rejected", 2, true},
	} {
		t.Run(test.code, func(t *testing.T) {
			f := newDefaultNativeFixture(t, Config{})
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim)
			r.enroll(t)
			heartbeat := map[string]any{"display_name": "Runner", "capacity": 2, "version": "test", "protocol_major": test.protocol, "settings_rejected": test.rejected, "backend_isolation": isolation.Report{"codex": {isolation.NativeTrusted}}, "problems": []runnerauth.Problem{runnerauth.NewProblem("keep_awake_failed")}}
			post := func() {
				t.Helper()
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, heartbeat), http.StatusOK)
			}
			post()
			read := func() runnerauth.Runner {
				t.Helper()
				view, err := readRunner(t.Context(), f.service.database.db, f.project.OrganizationID, r.binding.RunnerID, f.service.config.now())
				if err != nil {
					t.Fatal(err)
				}
				return view
			}
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
			if exclusions := view.Exclusions(f.project.ID, policy.Requirements{}, false); len(exclusions) != 0 {
				t.Fatalf("diagnostics affected dispatch: %+v", exclusions)
			}
			view, err := readRunner(t.Context(), f.service.database.db, f.project.OrganizationID, r.binding.RunnerID, f.service.config.now().Add(runnerauth.HeartbeatTimeout))
			if err != nil {
				t.Fatal(err)
			}
			if view.Health != "needs_attention" || view.ConnectionHealth != "offline" || len(view.Exclusions(f.project.ID, policy.Requirements{}, false)) == 0 {
				t.Fatalf("offline diagnostic weakened dispatch: %+v", view)
			}
			heartbeat["protocol_major"] = 2
			heartbeat["settings_rejected"] = false
			heartbeat["backend_isolation"] = isolation.Report{"codex": {isolation.Sandbox, isolation.NativeTrusted}}
			delete(heartbeat, "problems")
			post()
			view = read()
			if view.Health != "online" || len(view.Problems) != 0 {
				t.Fatalf("did not clear: %+v", view)
			}
		})
	}
}

func TestRunnerApprovedPolicyProblems(t *testing.T) {
	for _, test := range []struct {
		name string
		want int
	}{
		{"aligned", 0},
		{"local policies agree but Hub refuses", 1},
		{"pending selection still claims", 0},
		{"compatible approved revision", 0},
		{"compatible unapproved revision", 1},
		{"admission observation", 1},
		{"effective overrides admission", 0},
		{"policy observation", 1},
		{"approval revoked", 1},
		{"unauthorized project", 0},
		{"no policy evidence", 0},
		{"multiple projects", 2},
		{"clears after approval", 1},
		{"clears after alignment", 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newDefaultNativeFixture(t, Config{})
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim)
			r.enroll(t)
			sources := workflowconfig.ProjectDefinitionSources{
				ConfigPath: "detent.yaml", HasConfig: true,
				Config:       []byte("schema: 1\ntracker:\n  kind: hub_native\n  repository: digitaldrywood/detent\ngate:\n  run: make check-land\n  required_status_checks: []\n"),
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
			expectedID := ""
			if test.name == "compatible approved revision" {
				approveHubTestPolicy(t, f.service, f.base+"/policy", old)
				expectedID = old.ID
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/policy", testHubAdminToken, policy.Change{ExpectedID: expectedID, Policy: approved}), http.StatusOK)
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
			view := runnerauth.ProjectConfiguration{ProjectID: string(f.project.ID), Authority: "local_global_configuration", Registered: true, RuntimeRegistered: true, Source: "configured_committed_workflow", SelectedPolicy: &selected, EffectivePolicy: &effective, ObservedAt: f.service.config.now()}
			post := func(configuration *runnerauth.ProjectConfiguration) {
				t.Helper()
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, map[string]any{"display_name": "runner", "capacity": 2, "version": "test", "protocol_major": 2, "backend_isolation": r.redemption.BackendIsolation, "project_configuration": configuration}), http.StatusOK)
			}
			var configuration *runnerauth.ProjectConfiguration
			switch test.name {
			case "admission observation", "policy observation", "no policy evidence":
			default:
				configuration = &view
			}
			scope := string(f.project.OrganizationID) + "/" + string(f.project.ID)
			if test.name == "admission observation" || test.name == "effective overrides admission" || test.name == "compatible approved revision" || test.name == "compatible unapproved revision" {
				observation := tracker.NativeAdmissionObservation{Context: tracker.NativeAdmissionContext{PolicyID: mismatch.ID, ObservedAt: f.service.config.now()}}
				if test.name == "compatible approved revision" || test.name == "compatible unapproved revision" {
					observation.Context.PolicyID = old.ID
				}
				raw, err := json.Marshal(observation)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE machines SET capabilities_json = json_set(capabilities_json, ?, json(?)) WHERE id = ?", "$.native_admission."+r.binding.RunnerID+"."+view.ProjectID, string(raw), r.binding.MachineID); err != nil {
					t.Fatal(err)
				}
			}
			if test.name == "policy observation" {
				if err := storeObservedPolicy(t.Context(), f.service.database.db, scope, r.binding.RunnerID, policy.Observation{Descriptor: mismatch}, f.service.config.now()); err != nil {
					t.Fatal(err)
				}
			}
			post(configuration)
			if test.name == "approval revoked" {
				if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM project_policies WHERE scope = ?", scope); err != nil {
					t.Fatal(err)
				}
			}
			if test.name == "unauthorized project" {
				if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM token_grants WHERE token_id = (SELECT token_id FROM runner_identities WHERE id = ?)", r.binding.RunnerID); err != nil {
					t.Fatal(err)
				}
			}
			if test.name == "multiple projects" {
				second := newNativeFixture(t, f.service, f.project.OrganizationID, "second-policy-project")
				approveHubTestPolicy(t, f.service, second.base+"/policy", approved)
				if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO token_grants (token_id, organization_id, project_id) SELECT token_id, organization_id, ? FROM runner_identities WHERE id = ?", second.project.ID, r.binding.RunnerID); err != nil {
					t.Fatal(err)
				}
				if err := storeObservedPolicy(t.Context(), f.service.database.db, string(second.project.OrganizationID)+"/"+string(second.project.ID), r.binding.RunnerID, policy.Observation{Descriptor: mismatch}, f.service.config.now()); err != nil {
					t.Fatal(err)
				}
			}
			read := func(want int) runnerauth.Runner {
				t.Helper()
				runner, err := readRunner(t.Context(), f.service.database.db, f.project.OrganizationID, r.binding.RunnerID, f.service.config.now())
				if err != nil {
					t.Fatal(err)
				}
				if len(runner.Problems) != want || (runner.Health == "needs_attention") != (want > 0) {
					t.Fatalf("runner health=%s problems=%+v, want %d", runner.Health, runner.Problems, want)
				}
				for _, problem := range runner.Problems {
					approvedID := approved.ID
					if test.name == "approval revoked" {
						approvedID = "none"
					}
					if problem.Code != "policy_mismatch" || problem.ProjectID == "" || !strings.Contains(problem.Message, effective.ID) || !strings.Contains(problem.Message, approvedID) || !strings.Contains(problem.FixHint, "Approve the pending policy") || !strings.Contains(problem.FixHint, "update the runner's project files") || !problem.FirstSeen.Equal(runner.LastHeartbeatAt) {
						t.Fatalf("policy problem=%+v", problem)
					}
				}
				return runner
			}
			read(test.want)
			if test.name == "local policies agree but Hub refuses" {
				var problems []runnerauth.Problem
				var raw string
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT problems_json FROM runner_identities WHERE id = ?", r.binding.RunnerID).Scan(&raw); err != nil || json.Unmarshal([]byte(raw), &problems) != nil || len(problems) != 1 {
					t.Fatalf("first heartbeat problems=%s err=%v", raw, err)
				}
			}
			if test.name == "compatible approved revision" || test.name == "compatible unapproved revision" {
				matches, err := approvedPolicyRevisionMatches(t.Context(), f.service.database.db, scope, old.ID, approved)
				if err != nil || matches != (test.want == 0) {
					t.Fatalf("claim policy compatibility=%v err=%v", matches, err)
				}
				f.create(t, "policy capacity candidate")
				routing, err := json.Marshal(map[string]any{"project_ranks": map[string]int{view.ProjectID: 1}})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE runner_identities SET routing_settings_json = ? WHERE id = ?", string(routing), r.binding.RunnerID); err != nil {
					t.Fatal(err)
				}
				snapshot, err := readHealthSnapshot(t.Context(), f.service.database.db, f.project.OrganizationID, f.service.config.now())
				if err != nil {
					t.Fatal(err)
				}
				if (snapshot.Projects[0].FreeSlots > 0) != (test.want == 0) {
					t.Fatalf("policy compatibility capacity=%+v", snapshot.Projects[0])
				}
			}
			if test.name == "clears after approval" {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/policy", testHubAdminToken, policy.Change{ExpectedID: approved.ID, Policy: mismatch}), http.StatusOK)
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
