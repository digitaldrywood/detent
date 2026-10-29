package hubserver

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/onboarding"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestProjectOnboardingBatch(t *testing.T) {
	for _, test := range []struct {
		name                           string
		state                          string
		stale, failedChecks, exhausted bool
		ready                          bool
	}{
		{name: "ready projects", ready: true},
		{name: "disabled runner", state: "disabled"},
		{name: "draining runner", state: "draining"},
		{name: "offline runner", stale: true},
		{name: "failed checks", failedChecks: true},
		{name: "exhausted subscription remains ready", exhausted: true, ready: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newNativeFixture(t, nil, "", "first")
			if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
				t.Fatal(err)
			}
			other := newNativeFixture(t, f.service, f.project.OrganizationID, "second")
			if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO organizations(id,name,local,created_at) VALUES ('org_foreign','Foreign',0,?)", formatHubTime(f.service.config.now())); err != nil {
				t.Fatal(err)
			}
			foreign := newNativeFixture(t, f.service, "org_foreign", "foreign")
			for index, fixture := range []nativeFixture{f, other} {
				approveHubTestPolicy(t, f.service, fixture.base+"/policy", hubTestPolicy())
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET checkout_repository=? WHERE id=?", "example-org/example-project", fixture.project.ID); err != nil {
					t.Fatal(err)
				}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, fixture.base+"/onboarding", fixture.token, map[string]any{"idempotency_key": "onboarding-batch", "progress": onboarding.Progress{Artifacts: "local"}}), http.StatusOK)
				r := prepareRunner(t, fixture, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
				r.enroll(t)
				checks := runnerauth.LocalChecks{Checkout: "passed", Doctor: "passed", Provider: "passed"}
				if index == 0 && test.failedChecks {
					checks.Checkout = "failed"
				}
				raw, err := json.Marshal(map[tracker.ProjectID]runnerauth.LocalChecks{fixture.project.ID: checks})
				if err != nil {
					t.Fatal(err)
				}
				state := "active"
				heartbeat := f.service.config.now()
				if index == 0 {
					if test.state != "" {
						state = test.state
					}
					if test.stale {
						heartbeat = heartbeat.Add(-time.Hour)
					}
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE runner_identities SET local_checks_json=?,state=?,last_heartbeat_at=? WHERE id=?", string(raw), state, formatHubTime(heartbeat), r.binding.RunnerID); err != nil {
					t.Fatal(err)
				}
				if index == 0 && test.exhausted {
					report := capacityReport(heartbeat)
					report.Availability = "exhausted"
					raw, err := json.Marshal([]any{report})
					if err != nil {
						t.Fatal(err)
					}
					if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE runner_identities SET provider_reports_json=? WHERE id=?", string(raw), r.binding.RunnerID); err != nil {
						t.Fatal(err)
					}
				}
			}
			projects := []appBootstrapProject{{ID: string(f.project.ID)}, {ID: string(other.project.ID)}, {ID: string(foreign.project.ID)}}
			counter := installReadQueryCounter(t, f.service.database)
			batch, err := f.service.projectOnboardingBatch(t.Context(), f.project.OrganizationID, projects)
			if err != nil {
				t.Fatal(err)
			}
			if queries := counter.queries.Load(); queries > 7 {
				t.Fatalf("onboarding batch used %d queries, want at most 7 independent of project and runner counts", queries)
			}
			if len(batch) != 2 {
				t.Fatalf("batch crossed organization boundary: %v", batch)
			}
			for _, fixture := range []nativeFixture{f, other} {
				got := batch[fixture.project.ID]
				want := true
				if fixture.project.ID == f.project.ID {
					want = test.ready
				}
				if got.Ready != want || len(got.Runners) != 1 || got.Policy == nil || len(got.Steps) != 4 {
					t.Fatalf("project %s readiness=%t runners=%d policy=%v steps=%v, want ready=%t", fixture.project.Name, got.Ready, len(got.Runners), got.Policy, got.Steps, want)
				}
				detailed, err := f.service.projectOnboarding(t.Context(), nativeScope{organization: fixture.project.OrganizationID, project: fixture.project.ID, credential: apiCredential{Scope: apiScopeAdmin}})
				if err != nil {
					t.Fatal(err)
				}
				if got.Ready != detailed.Ready {
					t.Fatalf("batch and detail disagree: batch=%t detail=%t", got.Ready, detailed.Ready)
				}
			}
		})
	}
}
