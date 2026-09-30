package hubserver

import (
	"net/http"
	"path/filepath"
	"testing"
	"time"

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
			heartbeat["problems"] = []map[string]any{{"code": code, "message": "Tooling needs repair", "fix_hint": "Repair the runner tooling"}}
			post := func() {
				t.Helper()
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, heartbeat), http.StatusOK)
			}
			read := func() (string, []struct {
				Code      string    `json:"code"`
				Message   string    `json:"message"`
				FixHint   string    `json:"fix_hint"`
				FirstSeen time.Time `json:"first_seen"`
			}) {
				t.Helper()
				response := performHubAPIRequest(t, f.service, http.MethodGet, r.identityPath()+"/routing", testHubAdminToken, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var view struct {
					Health   string `json:"health"`
					Problems []struct {
						Code      string    `json:"code"`
						Message   string    `json:"message"`
						FixHint   string    `json:"fix_hint"`
						FirstSeen time.Time `json:"first_seen"`
					} `json:"problems"`
				}
				decodeHubResponse(t, response, &view)
				return view.Health, view.Problems
			}
			post()
			health, problems := read()
			if health != "needs_attention" || len(problems) != 1 || problems[0].Code != code || problems[0].FixHint == "" || problems[0].FirstSeen.IsZero() {
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
		code           string
		protocol       int
		rejected, home bool
	}{
		{"version_unsupported", 3, false, false},
		{"settings_rejected", 2, true, false},
		{"home_project_unservable", 2, false, true},
	} {
		t.Run(test.code, func(t *testing.T) {
			f := newDefaultNativeFixture(t, Config{})
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim)
			r.enroll(t)
			if test.home {
				change := runnerauth.RoutingChange{ExpectedRevision: 1, Routing: runnerauth.Routing{DisplayName: "Runner", State: "active", CapacityLimit: 2, ProjectIDs: []tracker.ProjectID{f.project.ID}, HomeProjectIDs: []tracker.ProjectID{f.project.ID}}}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, change), http.StatusOK)
			}
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

func TestHostedRunnerProblemsVisibility(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	r := runnerauth.Runner{Health: "needs_attention", ConnectionHealth: "online", Problems: []runnerauth.Problem{runnerauth.NewProblem("home_project_unservable")}, Routing: runnerauth.Routing{HomeProjectIDs: []tracker.ProjectID{"prj_visible", "prj_hidden"}}}
	view := hostedFleetRunnerView(r, "test", map[tracker.ProjectID]bool{"prj_visible": true}, now)
	if view.Health != "online" || len(view.Problems) != 0 {
		t.Fatalf("hidden home diagnostics leaked: %+v", view)
	}
	view = hostedFleetRunnerView(r, "test", map[tracker.ProjectID]bool{"prj_visible": true, "prj_hidden": true}, now)
	if view.Health != "needs_attention" || len(view.Problems) != 1 {
		t.Fatalf("visible diagnostic missing: %+v", view)
	}
	if len(r.Problems) != 1 {
		t.Fatal("visibility filtering mutated the original runner")
	}
}

func TestRunnerHomeProblemSettingsClear(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), now: func() time.Time { return now }})
	f := newNativeFixture(t, service, "", "home-problem-settings")
	r := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat)
	r.redemption.BackendIsolation = isolation.Report{"codex": {isolation.NativeTrusted}}
	r.enroll(t)
	change := runnerauth.RoutingChange{ExpectedRevision: 1, Routing: runnerauth.Routing{DisplayName: "Runner", State: "active", CapacityLimit: 2, ProjectIDs: []tracker.ProjectID{f.project.ID}}}
	for _, home := range []bool{true, false, true} {
		now = now.Add(time.Minute)
		change.HomeProjectIDs = []tracker.ProjectID{}
		if home {
			change.HomeProjectIDs = []tracker.ProjectID{f.project.ID}
		}
		response := performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, change)
		requireNativeStatus(t, response, http.StatusOK)
		var view runnerauth.Runner
		decodeHubResponse(t, response, &view)
		if home {
			if view.Health != "needs_attention" || len(view.Problems) != 1 || !view.Problems[0].FirstSeen.Equal(now) {
				t.Fatalf("new home problem=%+v, now=%s", view.Problems, now)
			}
		} else if len(view.Problems) != 0 {
			t.Fatalf("resolved home problem remained: %+v", view.Problems)
		}
		change.ExpectedRevision++
	}
}
