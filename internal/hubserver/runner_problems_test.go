package hubserver

import (
	"net/http"
	"testing"

	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
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
