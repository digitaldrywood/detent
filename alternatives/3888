package hubserver

import (
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerProblemsHeartbeat(t *testing.T) {
	t.Parallel()
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
				fleet := hostedFleetRunnerView(runner, "test", map[tracker.ProjectID]bool{f.project.ID: true}, f.service.config.Clock())
				if fleet.Health != "needs_attention" || fleet.CapacityHealth != connection {
					t.Fatalf("project warning changed host availability: health=%s capacity_health=%s connection=%s", fleet.Health, fleet.CapacityHealth, connection)
				}
			}
			runner.Problems[0].ProjectID = ""
			if fleet := hostedFleetRunnerView(runner, "test", nil, f.service.config.Clock()); fleet.CapacityHealth != "" || fleet.Health != "needs_attention" {
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

func TestRunnerCachedProbeProblemsHeartbeat(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
