package runnerfleet_test

import (
	"net/http"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerProjectRouteAccessByScope(t *testing.T) {
	t.Parallel()
	type decision struct {
		project         string
		register, claim int
	}
	allowed := decision{register: http.StatusOK, claim: http.StatusOK}
	registered := decision{register: http.StatusOK}
	forbidden := decision{register: http.StatusForbidden, claim: http.StatusForbidden}
	unauthorized := decision{register: http.StatusUnauthorized, claim: http.StatusUnauthorized}
	on := func(project string, want decision) decision {
		want.project = project
		return want
	}
	for _, test := range []struct {
		name   string
		scope  string
		before []decision
		change func(*testing.T, nativeFixture, runnerFixture)
		after  []decision
	}{
		{name: "organization runner", scope: "organization", before: []decision{on("first", allowed), on("second", allowed), on("foreign", unauthorized)}},
		{name: "selected runner", scope: "projects", before: []decision{on("first", allowed), on("second", forbidden), on("foreign", unauthorized)}},
		{name: "removed organization runner", scope: "organization", before: []decision{on("second", registered)}, change: func(t *testing.T, f nativeFixture, r runnerFixture) {
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodDelete, r.identityPath(), testHubAdminToken, nil), http.StatusNoContent)
		}, after: []decision{on("first", unauthorized), on("second", unauthorized)}},
		{name: "scope reduced organization runner", scope: "organization", before: []decision{on("second", registered)}, change: func(t *testing.T, f nativeFixture, r runnerFixture) {
			change := runnerauth.RoutingChange{ExpectedRevision: 1, Routing: runnerauth.Routing{Scope: "projects", DisplayName: "Runner", State: "active", CapacityLimit: 2, ProjectIDs: []tracker.ProjectID{f.project.ID}}}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, change), http.StatusOK)
		}, after: []decision{on("first", allowed), on("second", forbidden)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			first := newNativeFixture(t, nil, "", "first")
			fixtures := map[string]nativeFixture{"first": first, "second": newNativeFixture(t, first.service, first.project.OrganizationID, "second"), "foreign": newNativeFixture(t, first.service, "org_first_second", "foreign")}
			for _, f := range fixtures {
				approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			}
			r := prepareRunnerForScope(t, first, test.scope, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
			r.enroll(t)
			reportRunnerCapacity(t, first.service, first.base, r.redemption.Credential, r.binding.MachineID, r.redemption.DisplayName, r.redemption.BackendIsolation, r.redemption.Capacity)
			check := func(decisions []decision) {
				t.Helper()
				for _, want := range decisions {
					f := fixtures[want.project]
					registration := map[string]any{"id": r.binding.MachineID, "hostname": "customer-host", "display_name": "Runner", "capacity": 2, "version": "test", "backend_isolation": r.redemption.BackendIsolation}
					if got := performHubAPIRequest(t, first.service, http.MethodPost, f.base+"/machines/register", r.redemption.Credential, registration).Code; got != want.register {
						t.Errorf("register on %s project status=%d, want %d", want.project, got, want.register)
					}
					if want.claim == 0 {
						continue
					}
					claim := tracker.NativeClaim{PolicyID: hubTestPolicy().ID, WorkItemID: f.create(t, "work").WorkItemID, MachineID: r.binding.MachineID, SessionID: hubapi.NewID("session"), TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
					if got := performHubAPIRequest(t, first.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim).Code; got != want.claim {
						t.Errorf("claim on %s project status=%d, want %d", want.project, got, want.claim)
					}
				}
			}
			check(test.before)
			if test.change != nil {
				test.change(t, first, r)
				check(test.after)
			}
		})
	}
}
