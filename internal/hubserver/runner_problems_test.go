package hubserver

import (
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerProblemsHeartbeat(t *testing.T) {
	for _, test := range []struct {
		code   string
		sprite bool
		tier   string
	}{
		{"tier_unavailable", false, "sandbox"},
		{"backend_missing", false, "sandbox"},
		{"backend_probe_failed", false, "sandbox"},
		{"host_service_unreachable", false, "sandbox"},
		{"settings_invalid", false, "sandbox"},
		{"keep_awake_failed", false, "sandbox"},
		{"tier_unavailable", true, "sandbox"},
		{"tier_unavailable", true, "native-trusted"},
	} {
		t.Run(test.code+"/"+test.tier+"/sprite="+strconv.FormatBool(test.sprite), func(t *testing.T) {
			code := test.code
			f := newDefaultNativeFixture(t, Config{})
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat)
			r.redemption.IsolationTier = test.tier
			if test.sprite {
				r.redemption.SpriteName = r.redemption.Hostname
			}
			r.enroll(t)
			heartbeat := map[string]any{"display_name": "Runner", "capacity": 2, "version": "test", "backend_isolation": isolation.Report{"test": {isolation.Sandbox, isolation.NativeTrusted}}}
			observed := time.Now().UTC().Truncate(time.Microsecond).Add(-time.Minute)
			heartbeat["problems"] = []map[string]any{{"reported_at": observed, "project_id": "site", "code": code, "message": "Tooling needs repair", "fix_hint": "Repair the runner tooling", "subject": "codex", "check": "codex login status", "error_output": "Not logged in; OPENAI_API_KEY=private-key", "fix_command": "codex login --device-auth"}}
			post := func() {
				t.Helper()
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, heartbeat), http.StatusOK)
			}
			read := func() (string, []runnerauth.Problem) {
				t.Helper()
				response := performHubAPIRequest(t, f.service, http.MethodGet, r.identityPath()+"/routing", testHubAdminToken, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var view runnerauth.Runner
				decodeHubResponse(t, response, &view)
				return view.Health, view.Problems
			}
			post()
			health, problems := read()
			if health != "needs_attention" || len(problems) != 1 || problems[0].Code != code || problems[0].ProjectID != "site" || problems[0].FixHint == "" || problems[0].FirstSeen.IsZero() {
				t.Fatalf("health=%s problems=%+v", health, problems)
			}
			wantMessage := "Tooling needs repair"
			if test.sprite && test.tier == "sandbox" {
				wantMessage = "Sandbox on a Fly Sprite needs a passing isolation probe on that runner: update the runner, then switch it to Sandbox once it reports sandbox support."
			}
			if problems[0].Message != wantMessage {
				t.Fatalf("problem message = %q, want %q", problems[0].Message, wantMessage)
			}
			if problems[0].Subject != "codex" || problems[0].Check != "codex login status" || problems[0].FixCommand != "codex login --device-auth" || problems[0].ReportedAt.IsZero() || strings.Contains(problems[0].ErrorOutput, "private-key") {
				t.Fatalf("unsafe or missing details: %+v", problems)
			}
			runner := readProblemFixtureRouting(t, f, r)
			for _, connection := range []string{"online", "offline"} {
				runner.ConnectionHealth = connection
				fleet := hostedFleetRunnerView(runner, "test", map[tracker.ProjectID]bool{f.project.ID: true}, f.service.config.now())
				if fleet.Health != "needs_attention" || fleet.CapacityHealth != connection {
					t.Fatalf("project warning changed host availability: health=%s capacity_health=%s connection=%s", fleet.Health, fleet.CapacityHealth, connection)
				}
			}
			runner.Problems[0].ProjectID = ""
			if fleet := hostedFleetRunnerView(runner, "test", nil, f.service.config.now()); fleet.CapacityHealth != "" || fleet.Health != "needs_attention" {
				t.Fatalf("runner-wide failure lost its availability warning: %+v", fleet)
			}
			if !problems[0].ReportedAt.Equal(observed) {
				t.Fatalf("heartbeat replaced probe observation time: %+v", problems[0])
			}
			reported := problems[0].ReportedAt
			_, again := read()
			if !again[0].ReportedAt.Equal(reported) {
				t.Fatal("read updated report timestamp")
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
		{"update_applied_not_running", 2, false},
	} {
		t.Run(test.code, func(t *testing.T) {
			var clock atomic.Int64
			clock.Store(time.Now().UTC().Truncate(time.Microsecond).UnixNano())
			f := newDefaultNativeFixture(t, Config{Clock: func() time.Time { return time.Unix(0, clock.Load()).UTC() }})
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim)
			r.enroll(t)
			heartbeat := map[string]any{"display_name": "Runner", "capacity": 2, "version": "test", "protocol_major": test.protocol, "settings_rejected": test.rejected, "backend_isolation": isolation.Report{"codex": {isolation.NativeTrusted}}, "problems": []runnerauth.Problem{runnerauth.NewProblem("keep_awake_failed")}}
			if test.code == "update_applied_not_running" {
				now := f.service.config.now()
				running := runnerauth.BuildEvidence{Version: "1.0.0", Commit: strings.Repeat("a", 40), Source: "release", SHA256: strings.Repeat("b", 64), OS: "linux", Architecture: "amd64", ObservedAt: now}
				applied := running
				applied.Version, applied.Commit, applied.SHA256, applied.VerifiedRelease = "1.0.1", strings.Repeat("c", 40), strings.Repeat("d", 64), true
				report := &runnerauth.UpdateObservation{Discovery: "unknown", Protocol: 1, Service: "detent", Supported: true, Running: running, ObservedAt: now,
					Receipt: &runnerauth.UpdateReceipt{Status: "applied", ExecutionArtifact: true, Applied: &applied, ObservedAt: now,
						Request: runnerauth.UpdateRequest{ID: "update-fixture", Service: "detent", Release: true, Version: "1.0.1", RequestedAt: now}}}
				report.Revision = report.BuildRevision()
				report.Receipt.Request.ExpectedBuildRevision = report.Revision
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
					identity, err := w.RunnerIdentity(t.Context(), r.binding.RunnerID)
					if err != nil {
						return err
					}
					if err := w.CreateRunnerUpdateRequest(t.Context(), runnerstore.UpdateRequestRecord{OrganizationID: f.project.OrganizationID, RunnerID: r.binding.RunnerID, Request: report.Receipt.Request}); err != nil {
						return err
					}
					return w.SetRunnerUpdateDesired(t.Context(), r.binding.RunnerID, identity.Revision, report.Receipt.Request.ID)
				}); err != nil {
					t.Fatal(err)
				}
				heartbeat["update"] = report
				heartbeat["version"], heartbeat["os"], heartbeat["architecture"] = "1.0.0", "linux", "amd64"
			}
			post := func() {
				t.Helper()
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, heartbeat), http.StatusOK)
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
			if exclusions := view.Exclusions(f.project.ID, policy.Requirements{}, false); len(exclusions) != 0 {
				t.Fatalf("diagnostics affected dispatch: %+v", exclusions)
			}
			current := clock.Load()
			clock.Store(current + int64(runnerauth.HeartbeatTimeout))
			view = read()
			clock.Store(current)
			if view.Health != "needs_attention" || view.ConnectionHealth != "offline" || len(view.Exclusions(f.project.ID, policy.Requirements{}, false)) == 0 {
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
			f := newDefaultNativeFixture(t, Config{})
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim)
			r.enroll(t)
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
			expectedID := hubTestPolicy().ID
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
			if test.name == "admission observation" || test.name == "effective overrides admission" || test.name == "compatible approved revision" || test.name == "compatible unapproved revision" {
				observation := tracker.NativeAdmissionObservation{Context: tracker.NativeAdmissionContext{PolicyID: mismatch.ID, ObservedAt: f.service.config.now()}}
				if test.name == "compatible approved revision" || test.name == "compatible unapproved revision" {
					observation.Context.PolicyID = old.ID
				}
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
					identity, err := w.RunnerIdentity(t.Context(), r.binding.RunnerID)
					if err != nil {
						return err
					}
					observation.RunnerRevision, observation.ReceivedAt = identity.Revision, f.service.config.now()
					return w.ReplaceRunnerAdmissionObservation(t.Context(), r.binding.RunnerID, f.project.ID, observation)
				}); err != nil {
					t.Fatal(err)
				}
			}
			if test.name == "policy observation" {
				if err := recordRunnerConfigurationPolicyObservation(t, f, r.binding.RunnerID, mismatch); err != nil {
					t.Fatal(err)
				}
			}
			post(configuration)
			if test.name == "approval revoked" {
				clearRunnerConfigurationPolicy(t, f)
			}
			if test.name == "unauthorized project" {
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
					identity, err := w.RunnerIdentity(t.Context(), r.binding.RunnerID)
					if err != nil {
						return err
					}
					return w.DeleteAccessTokenGrant(t.Context(), identity.TokenID, f.project.ID)
				}); err != nil {
					t.Fatal(err)
				}
			}
			if test.name == "multiple projects" {
				second := newNativeFixture(t, f.service, f.project.OrganizationID, "second-policy-project")
				approveHubTestPolicy(t, f.service, second.base+"/policy", approved)
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
					identity, err := w.RunnerIdentity(t.Context(), r.binding.RunnerID)
					if err != nil {
						return err
					}
					return w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: f.project.OrganizationID, TokenID: identity.TokenID, ProjectID: second.project.ID})
				}); err != nil {
					t.Fatal(err)
				}
				if err := recordRunnerConfigurationPolicyObservation(t, second, r.binding.RunnerID, mismatch); err != nil {
					t.Fatal(err)
				}
			}
			read := func(want int) runnerauth.Runner {
				t.Helper()
				response := performHubAPIRequest(t, f.service, http.MethodGet, r.identityPath()+"/routing", testHubAdminToken, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var runner runnerauth.Runner
				decodeHubResponse(t, response, &runner)
				if len(runner.Problems) != want || (runner.Health == "needs_attention") != (want > 0) {
					t.Fatalf("runner health=%s problems=%+v, want %d", runner.Health, runner.Problems, want)
				}
				return runner
			}
			read(test.want)
			if test.name == "compatible approved revision" || test.name == "compatible unapproved revision" {
				item := f.create(t, "policy capacity candidate")
				configurePolicyCapacityRunner(t, f, r, f.service.config.now(), f.service.config.now())
				response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(item.WorkItemID)+"/runtime", f.token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var evidence tracker.NativeRuntimeEvidence
				decodeHubResponse(t, response, &evidence)
				if len(evidence.Capacity) != 1 || evidence.Capacity[0].RunnerID != r.binding.RunnerID || (evidence.Capacity[0].Available > 0) != (test.want == 0) {
					t.Fatalf("policy compatibility capacity=%+v", evidence.Capacity)
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

func TestRunnerCachedProbeProblemsHeartbeat(t *testing.T) {
	for _, test := range []struct {
		name, code             string
		cached, backend, mixed bool
		visible                bool
	}{
		{name: "cached probe", code: "backend_probe_failed", cached: true},
		{name: "uncached probe", code: "backend_probe_failed", visible: true},
		{name: "backend failure", code: "backend_probe_failed", cached: true, backend: true, visible: true},
		{name: "settings failure", code: "settings_invalid", cached: true, visible: true},
		{name: "cached probe with settings failure", code: "backend_probe_failed", cached: true, mixed: true, visible: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Now().UTC().Truncate(time.Microsecond)
			var clock atomic.Int64
			clock.Store(now.UnixNano())
			f := newDefaultNativeFixture(t, Config{Clock: func() time.Time { return time.Unix(0, clock.Load()).UTC() }})
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat)
			r.enroll(t)
			problem := runnerauth.NewProblem(test.code)
			problem.Subject = "backend_isolation"
			problem.Check = "runner isolation probe"
			problem.CachedCapabilities = test.cached
			if test.backend {
				problem.Subject = "example-project/codex"
				problem.Check = "codex login status"
			}
			problems := []runnerauth.Problem{problem}
			if test.mixed {
				problems = append(problems, runnerauth.NewProblem("settings_invalid"))
			}
			heartbeat := map[string]any{"display_name": "Runner", "capacity": 2, "version": "test", "backend_isolation": isolation.Report{"example-project/codex": {isolation.NativeTrusted}}, "problems": problems}
			post := func() {
				t.Helper()
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, heartbeat), http.StatusOK)
			}
			read := func() runnerauth.Runner {
				t.Helper()
				response := performHubAPIRequest(t, f.service, http.MethodGet, r.identityPath()+"/routing", testHubAdminToken, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var view runnerauth.Runner
				decodeHubResponse(t, response, &view)
				return view
			}
			first := now
			for _, age := range []time.Duration{0, time.Minute, 2*time.Minute - time.Nanosecond, 2 * time.Minute, 3 * time.Minute} {
				now = first.Add(age)
				clock.Store(now.UnixNano())
				if age == 2*time.Minute && !test.visible {
					if view := read(); view.Health != "needs_attention" || len(view.Problems) != 1 {
						t.Fatalf("persistent failure did not age into visibility: %+v", view)
					}
				}
				post()
				if age == time.Minute {
					post()
				}
				view := read()
				visible := test.visible || age >= 2*time.Minute
				if (view.Health == "needs_attention") != visible || (len(view.Problems) > 0) != visible {
					t.Fatalf("age=%v health=%s problems=%+v", age, view.Health, view.Problems)
				}
				fleet := hostedFleetRunnerView(view, "test", nil, now)
				if (len(fleet.Problems) > 0) != visible || (fleet.Health == "needs_attention") != visible {
					t.Fatalf("diagnostics and health differ at %v: %+v", age, fleet)
				}
				if test.mixed && age < 2*time.Minute && (len(fleet.Problems) != 1 || fleet.Problems[0].Code != "settings_invalid") {
					t.Fatalf("genuine workflow error hidden: %+v", fleet.Problems)
				}
				var stored []runnerauth.Problem
				if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
					var err error
					stored, err = reader.RunnerProblems(t.Context(), r.binding.RunnerID)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if len(stored) != len(problems) || !stored[0].FirstSeen.Equal(first) || stored[0].CachedCapabilities != test.cached {
					t.Fatalf("raw problem continuity lost at %v: %+v", age, stored)
				}
			}
			delete(heartbeat, "problems")
			post()
			if view := read(); view.Health != "online" || len(view.Problems) != 0 {
				t.Fatalf("completed probe did not clear failure: %+v", view)
			}
			now = now.Add(time.Second)
			clock.Store(now.UnixNano())
			heartbeat["problems"] = problems
			post()
			view := read()
			if (view.Health == "needs_attention") != test.visible {
				t.Fatalf("new occurrence inherited prior persistence: %+v", view)
			}
		})
	}
}

func TestRunnerExpiredRuntimeProblems(t *testing.T) {
	for _, test := range []struct {
		name     string
		phase    string
		age      time.Duration
		released bool
		expired  bool
		finished bool
		want     bool
	}{
		{name: "fresh completion", phase: "completed", age: 89 * time.Second},
		{name: "expired completion", phase: "completed", age: 90 * time.Second, want: true},
		{name: "hours of renewal", phase: "completed", age: 3 * time.Hour, want: true},
		{name: "expired implementation", phase: "implementation", age: 90 * time.Second, want: true},
		{name: "released attempt", phase: "completed", age: 3 * time.Hour, released: true},
		{name: "expired lease", phase: "completed", age: 3 * time.Hour, expired: true},
		{name: "terminal attempt", phase: "completed", age: 3 * time.Hour, finished: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Now().UTC().Truncate(time.Microsecond)
			f := newDefaultNativeFixture(t, Config{Clock: func() time.Time { return now }})
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim, runnerauth.Events)
			r.enroll(t)
			other := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim, runnerauth.Events)
			other.enroll(t)
			r.reportCapacity(t)
			publishCapacity(t, f, r, capacityReport(now))
			issue := f.create(t, "Example work")
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, providerClaim(r, issue, "example-session"))
			requireNativeStatus(t, response, http.StatusOK)
			var lease tracker.NativeLease
			decodeHubResponse(t, response, &lease)
			event := nativeStartedEvent(lease)
			event.Data.Runtime = &tracker.NativeRuntimeObservation{LocalAttemptID: 42, Generation: 1, Phase: test.phase, HeartbeatAt: now.Add(-test.age)}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/events", r.redemption.Credential, event), http.StatusOK)
			renewed, expires := now, now.Add(90*time.Second)
			if test.expired {
				renewed, expires = now.Add(-90*time.Second), now
			}
			if _, err := usagePostgresAdmin(t, f.service).ExecContext(t.Context(), "UPDATE leases SET acquired_at=LEAST(acquired_at,$1), renewed_at=$1, expires_at=$2 WHERE organization_id=$3 AND lease_id=$4", renewed, expires, f.project.OrganizationID, lease.ID); err != nil {
				t.Fatal(err)
			}
			if test.released {
				if _, err := usagePostgresAdmin(t, f.service).ExecContext(t.Context(), "UPDATE leases SET released_at=$1 WHERE organization_id=$2 AND lease_id=$3", now, f.project.OrganizationID, lease.ID); err != nil {
					t.Fatal(err)
				}
			}
			if test.finished {
				if _, err := usagePostgresAdmin(t, f.service).ExecContext(t.Context(), "UPDATE native_attempts SET status='failed' WHERE organization_id=$1 AND project_id=$2 AND id=$3", f.project.OrganizationID, f.project.ID, event.Data.AttemptID); err != nil {
					t.Fatal(err)
				}
			}
			read := func(runner runnerFixture) runnerauth.Runner {
				t.Helper()
				response := performHubAPIRequest(t, f.service, http.MethodGet, runner.identityPath()+"/routing", testHubAdminToken, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var view runnerauth.Runner
				decodeHubResponse(t, response, &view)
				return view
			}
			view := read(r)
			if (len(view.Problems) == 1) != test.want || (view.Health == "needs_attention") != test.want {
				t.Fatalf("health=%s problems=%+v, want expired runtime=%v", view.Health, view.Problems, test.want)
			}
			var snapshot runnerstore.RuntimeCapacitySnapshot
			if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
				var err error
				snapshot, err = reader.(runnerstore.RuntimeCapacityReader).RuntimeCapacitySnapshot(t.Context(), runnerstore.RuntimeCapacityQuery{ProjectID: f.project.ID, RunnerID: r.binding.RunnerID, At: now})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if snapshot.Truncated {
				t.Fatal("runtime capacity snapshot truncated fixture facts")
			}
			admissions := runtimeRunnersFromSnapshot(snapshot, hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID}, now)
			if len(admissions) != 2 || admissions[0].RunnerID != r.binding.RunnerID || admissions[1].RunnerID != other.binding.RunnerID {
				t.Fatalf("dispatch runner target = %+v", admissions)
			}
			admission := admissions[0]
			if len(admission.Leases) != 0 || len(admission.Problems) != 0 || admission.Health != "online" || !reflect.DeepEqual(admission.Exclusions(f.project.ID, policy.Requirements{}, false), view.Exclusions(f.project.ID, policy.Requirements{}, false)) {
				t.Fatalf("runtime display enrichment changed admission: %+v", admission)
			}
			if test.want {
				problem := view.Problems[0]
				if problem.Code != "runtime_expired" || problem.ProjectID != string(f.project.ID) || problem.Subject != event.Data.AttemptID || problem.FirstSeen.IsZero() || !problem.ReportedAt.Equal(now) {
					t.Fatalf("missing attempt attribution: %+v", problem)
				}
				event.Type, event.IdempotencyKey, event.Data.Sequence = "run.observed", "fresh-runtime", 2
				event.Data.Runtime.HeartbeatAt = now
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/events", r.redemption.Credential, event), http.StatusOK)
				if fresh := read(r); len(fresh.Problems) != 0 || fresh.Health != "online" {
					t.Fatalf("runtime warning did not clear: %+v", fresh)
				}
			}
			if foreign := read(other); len(foreign.Problems) != 0 {
				t.Fatalf("runtime warning leaked to another runner: %+v", foreign.Problems)
			}
		})
	}
}

func recordRunnerConfigurationPolicyObservation(t *testing.T, f nativeFixture, runner string, descriptor policy.Descriptor) error {
	t.Helper()
	revision, err := eventstore.NewContent(f.service.runDetailStore()).PrepareProjectPolicy(t.Context(), f.project.OrganizationID, descriptor)
	if err != nil {
		return err
	}
	revision.ProjectID = f.project.ID
	return f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.RecordProjectPolicyObservation(t.Context(), policystore.Observation{OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, RunnerID: runner, PolicyID: descriptor.ID, ObservedAt: f.service.config.now(), Revision: revision})
		return err
	})
}

func configurePolicyCapacityRunner(t *testing.T, f nativeFixture, runner runnerFixture, heartbeat, created time.Time) {
	t.Helper()
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		identity, err := w.RunnerIdentity(t.Context(), runner.binding.RunnerID)
		if err != nil {
			return err
		}
		identity.State = "active"
		identity.ProjectIDs = []tracker.ProjectID{f.project.ID}
		identity.ProjectRanks = map[tracker.ProjectID]int{f.project.ID: 1}
		if err := w.UpdateRunnerRouting(t.Context(), identity.Revision, identity); err != nil {
			return err
		}
		identity.LastHeartbeatAt = heartbeat
		identity.ExpiresAt = f.service.config.now().Add(time.Hour)
		return w.UpdateRunnerIdentity(t.Context(), identity)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := nativeAttemptTestDatabase(t, f).ExecContext(t.Context(), "UPDATE runner_identities SET created_at=$1 WHERE organization_id=$2 AND runner_id=$3", created, f.project.OrganizationID, runner.binding.RunnerID); err != nil {
		t.Fatal(err)
	}
}

func clearRunnerConfigurationPolicy(t *testing.T, f nativeFixture) {
	t.Helper()
	if _, err := nativeAttemptTestDatabase(t, f).ExecContext(t.Context(), "DELETE FROM project_policies WHERE organization_id=$1 AND project_id=$2", f.project.OrganizationID, f.project.ID); err != nil {
		t.Fatal(err)
	}
}

func readProblemFixtureRouting(t *testing.T, f nativeFixture, runner runnerFixture) runnerauth.Runner {
	t.Helper()
	response := performHubAPIRequest(t, f.service, http.MethodGet, runner.identityPath()+"/routing", testHubAdminToken, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var view runnerauth.Runner
	decodeHubResponse(t, response, &view)
	if view.RunnerID != runner.binding.RunnerID || view.MachineID != runner.binding.MachineID || view.OrganizationID != f.project.OrganizationID {
		t.Fatalf("routing returned wrong runner: %+v", view)
	}
	return view
}
