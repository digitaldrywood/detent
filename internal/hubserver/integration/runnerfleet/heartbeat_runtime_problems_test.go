package runnerfleet_test

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerProblemsHeartbeat(t *testing.T) {
	t.Parallel()
	var clock atomic.Int64
	now := func() time.Time { return time.UnixMicro(clock.Load()).UTC() }
	clock.Store(time.Now().UnixMicro())
	h := hubfixture.Open(t, hubfixture.Config{Clock: now})
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
			f := hubfixture.NewNative(t, h, h.Primary, t.Name())
			r := hubfixture.PrepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat)
			r.Redemption.IsolationTier = test.tier
			if test.sprite {
				r.Redemption.SpriteName = r.Redemption.Hostname
			}
			r.Enroll(t)
			heartbeat := map[string]any{"display_name": "Runner", "capacity": 2, "version": "test", "backend_isolation": isolation.Report{"test": {isolation.Sandbox, isolation.NativeTrusted}}}
			observed := now().Add(-time.Minute)
			heartbeat["problems"] = []map[string]any{{"reported_at": observed, "project_id": string(f.Project.ID), "code": code, "message": "Tooling needs repair", "fix_hint": "Repair the runner tooling", "subject": "codex", "check": "codex login status", "error_output": "Not logged in; OPENAI_API_KEY=private-key", "fix_command": "codex login --device-auth"}}
			post := func() {
				t.Helper()
				hubfixture.RequireStatus(t, h.Request(t, http.MethodPost, f.Base+"/machines/"+string(r.Binding.MachineID)+"/heartbeat", r.Redemption.Credential, heartbeat), http.StatusOK)
			}
			read := func() (string, []runnerauth.Problem) {
				t.Helper()
				response := h.Request(t, http.MethodGet, r.IdentityPath()+"/routing", h.AdminToken(), nil)
				hubfixture.RequireStatus(t, response, http.StatusOK)
				var view runnerauth.Runner
				hubfixture.Decode(t, response, &view)
				return view.Health, view.Problems
			}
			post()
			health, problems := read()
			if health != "needs_attention" || len(problems) != 1 || problems[0].Code != code || problems[0].ProjectID != string(f.Project.ID) || problems[0].FixHint == "" || problems[0].FirstSeen.IsZero() {
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
			for _, connection := range []string{"online", "offline"} {
				if connection == "offline" {
					clock.Add(runnerauth.HeartbeatTimeout.Microseconds())
				}
				fleet := readHeartbeatProblemFleet(t, f, r)
				if fleet.Health != "needs_attention" || fleet.CapacityHealth != connection {
					t.Fatalf("project warning changed host availability: health=%s capacity_health=%s connection=%s", fleet.Health, fleet.CapacityHealth, connection)
				}
			}
			heartbeat["problems"].([]map[string]any)[0]["project_id"] = ""
			post()
			if fleet := readHeartbeatProblemFleet(t, f, r); fleet.CapacityHealth != "" || fleet.Health != "needs_attention" {
				t.Fatalf("runner-wide failure lost its availability warning: %+v", fleet)
			}
			heartbeat["problems"].([]map[string]any)[0]["project_id"] = string(f.Project.ID)
			post()
			_, problems = read()
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

func TestRunnerCachedProbeProblemsHeartbeat(t *testing.T) {
	t.Parallel()
	var clock atomic.Int64
	clock.Store(time.Now().Truncate(time.Microsecond).UnixNano())
	h := hubfixture.Open(t, hubfixture.Config{Clock: func() time.Time { return time.Unix(0, clock.Load()).UTC() }})
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
			now := h.Now()
			f := hubfixture.NewNative(t, h, h.Primary, test.name)
			r := hubfixture.PrepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat)
			r.Enroll(t)
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
				hubfixture.RequireStatus(t, h.Request(t, http.MethodPost, f.Base+"/machines/"+string(r.Binding.MachineID)+"/heartbeat", r.Redemption.Credential, heartbeat), http.StatusOK)
			}
			read := func() runnerauth.Runner {
				t.Helper()
				response := h.Request(t, http.MethodGet, r.IdentityPath()+"/routing", h.AdminToken(), nil)
				hubfixture.RequireStatus(t, response, http.StatusOK)
				var view runnerauth.Runner
				hubfixture.Decode(t, response, &view)
				return view
			}
			first := now
			for _, age := range []time.Duration{0, 2*time.Minute - time.Nanosecond, 2 * time.Minute} {
				now = first.Add(age)
				clock.Store(now.UnixNano())
				if age == 2*time.Minute && !test.visible {
					if view := read(); view.Health != "needs_attention" || len(view.Problems) != 1 {
						t.Fatalf("persistent failure did not age into visibility: %+v", view)
					}
				}
				post()
				view := read()
				visible := test.visible || age >= 2*time.Minute
				if (view.Health == "needs_attention") != visible || (len(view.Problems) > 0) != visible {
					t.Fatalf("age=%v health=%s problems=%+v", age, view.Health, view.Problems)
				}
				fleet := readHeartbeatProblemFleet(t, f, r)
				if (len(fleet.Problems) > 0) != visible || (fleet.Health == "needs_attention") != visible {
					t.Fatalf("diagnostics and health differ at %v: %+v", age, fleet)
				}
				if test.mixed && age < 2*time.Minute && (len(fleet.Problems) != 1 || fleet.Problems[0].Code != "settings_invalid") {
					t.Fatalf("genuine workflow error hidden: %+v", fleet.Problems)
				}
				var stored []runnerauth.Problem
				if err := h.Repository.ReadTenant(t.Context(), f.Project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
					var err error
					stored, err = reader.RunnerProblems(t.Context(), r.Binding.RunnerID)
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
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Microsecond)
	h := hubfixture.Open(t, hubfixture.Config{Clock: func() time.Time { return now }})
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
		{name: "expired implementation", phase: "implementation", age: 90 * time.Second, want: true},
		{name: "released attempt", phase: "completed", age: 3 * time.Hour, released: true},
		{name: "expired lease", phase: "completed", age: 3 * time.Hour, expired: true},
		{name: "terminal attempt", phase: "completed", age: 3 * time.Hour, finished: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := hubfixture.NewNative(t, h, h.CreateOrganization(t, test.name), "Runtime problem test")
			r := hubfixture.PrepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim, runnerauth.Events)
			r.Enroll(t)
			other := hubfixture.PrepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim, runnerauth.Events)
			other.Enroll(t)
			r.ReportCapacity(t)
			hubfixture.PublishCapacity(t, f, r, hubfixture.CapacityReport(now))
			issue := f.Create(t, "Example work")
			response := h.Request(t, http.MethodPost, f.Base+"/claims", r.Redemption.Credential, hubfixture.ProviderClaim(r, issue, "example-session"))
			hubfixture.RequireStatus(t, response, http.StatusOK)
			var lease tracker.NativeLease
			hubfixture.Decode(t, response, &lease)
			event := hubfixture.StartedEvent(lease)
			event.Data.Runtime = &tracker.NativeRuntimeObservation{LocalAttemptID: 42, Generation: 1, Phase: test.phase, HeartbeatAt: now.Add(-test.age)}
			hubfixture.RequireStatus(t, h.Request(t, http.MethodPost, f.Base+"/work-items/"+string(issue.WorkItemID)+"/events", r.Redemption.Credential, event), http.StatusOK)
			renewed, expires := now, now.Add(90*time.Second)
			if test.expired {
				renewed, expires = now.Add(-90*time.Second), now
			}
			if _, err := h.Admin.ExecContext(t.Context(), "UPDATE leases SET acquired_at=LEAST(acquired_at,$1), renewed_at=$1, expires_at=$2 WHERE organization_id=$3 AND lease_id=$4", renewed, expires, f.Project.OrganizationID, lease.ID); err != nil {
				t.Fatal(err)
			}
			if test.released {
				if _, err := h.Admin.ExecContext(t.Context(), "UPDATE leases SET released_at=$1 WHERE organization_id=$2 AND lease_id=$3", now, f.Project.OrganizationID, lease.ID); err != nil {
					t.Fatal(err)
				}
			}
			if test.finished {
				if _, err := h.Admin.ExecContext(t.Context(), "UPDATE native_attempts SET status='failed' WHERE organization_id=$1 AND project_id=$2 AND id=$3", f.Project.OrganizationID, f.Project.ID, event.Data.AttemptID); err != nil {
					t.Fatal(err)
				}
			}
			read := func(runner hubfixture.Runner) runnerauth.Runner {
				t.Helper()
				response := h.Request(t, http.MethodGet, runner.IdentityPath()+"/routing", h.AdminToken(), nil)
				hubfixture.RequireStatus(t, response, http.StatusOK)
				var view runnerauth.Runner
				hubfixture.Decode(t, response, &view)
				return view
			}
			view := read(r)
			if (len(view.Problems) == 1) != test.want || (view.Health == "needs_attention") != test.want {
				t.Fatalf("health=%s problems=%+v, want expired runtime=%v", view.Health, view.Problems, test.want)
			}
			if test.want {
				admission := func() tracker.NativeRuntimeAdmission {
					t.Helper()
					response := f.Request(t, http.MethodGet, f.Base+"/work-items/"+string(issue.WorkItemID)+"/runtime", nil)
					hubfixture.RequireStatus(t, response, http.StatusOK)
					var evidence tracker.NativeRuntimeEvidence
					hubfixture.Decode(t, response, &evidence)
					index := slices.IndexFunc(evidence.Admission, func(entry tracker.NativeRuntimeAdmission) bool { return entry.RunnerID == r.Binding.RunnerID })
					if index < 0 {
						t.Fatalf("runtime omitted admission for runner %s: %+v", r.Binding.RunnerID, evidence.Unavailable)
					}
					return evidence.Admission[index]
				}
				before := admission()
				problem := view.Problems[0]
				if problem.Code != "runtime_expired" || problem.ProjectID != string(f.Project.ID) || problem.Subject != event.Data.AttemptID || problem.FirstSeen.IsZero() || !problem.ReportedAt.Equal(now) {
					t.Fatalf("missing attempt attribution: %+v", problem)
				}
				event.Type, event.IdempotencyKey, event.Data.Sequence = "run.observed", "fresh-runtime", 2
				event.Data.Runtime.HeartbeatAt = now
				hubfixture.RequireStatus(t, h.Request(t, http.MethodPost, f.Base+"/work-items/"+string(issue.WorkItemID)+"/events", r.Redemption.Credential, event), http.StatusOK)
				if fresh := read(r); len(fresh.Problems) != 0 || fresh.Health != "online" {
					t.Fatalf("runtime warning did not clear: %+v", fresh)
				}
				after := admission()
				if before.Outcome != after.Outcome || before.ReasonCode != after.ReasonCode {
					t.Fatalf("runtime problem changed runner admission: before=%+v after=%+v", before, after)
				}
			}
			if foreign := read(other); len(foreign.Problems) != 0 {
				t.Fatalf("runtime warning leaked to another runner: %+v", foreign.Problems)
			}
		})
	}
}

type heartbeatProblemFleetRunner struct {
	ID             string               `json:"id"`
	Health         string               `json:"health"`
	CapacityHealth string               `json:"capacity_health"`
	Problems       []runnerauth.Problem `json:"problems"`
}

func readHeartbeatProblemFleet(t *testing.T, f hubfixture.Native, r hubfixture.Runner) heartbeatProblemFleetRunner {
	t.Helper()
	response := f.Hub.Request(t, http.MethodGet, r.OrgBase+"/fleet", f.Hub.AdminToken(), nil)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var fleet struct {
		Runners []heartbeatProblemFleetRunner `json:"runners"`
	}
	hubfixture.Decode(t, response, &fleet)
	for _, entry := range fleet.Runners {
		if entry.ID == r.Binding.RunnerID {
			return entry
		}
	}
	t.Fatalf("fleet omitted runner %s", r.Binding.RunnerID)
	return heartbeatProblemFleetRunner{}
}
