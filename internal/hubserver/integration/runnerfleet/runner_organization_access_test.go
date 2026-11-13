package runnerfleet_test

import (
	"net/http"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
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
		change func(*testing.T, hubfixture.Native, hubfixture.Runner)
		after  []decision
	}{
		{name: "organization runner", scope: "organization", before: []decision{on("first", allowed), on("second", allowed), on("foreign", unauthorized)}},
		{name: "selected runner", scope: "projects", before: []decision{on("first", allowed), on("second", forbidden), on("foreign", unauthorized)}},
		{name: "removed organization runner", scope: "organization", before: []decision{on("second", registered)}, change: func(t *testing.T, f hubfixture.Native, r hubfixture.Runner) {
			hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodDelete, r.IdentityPath(), hubfixture.AdminToken, nil), http.StatusNoContent)
		}, after: []decision{on("first", unauthorized), on("second", unauthorized)}},
		{name: "scope reduced organization runner", scope: "organization", before: []decision{on("second", registered)}, change: func(t *testing.T, f hubfixture.Native, r hubfixture.Runner) {
			change := runnerauth.RoutingChange{ExpectedRevision: 1, Routing: runnerauth.Routing{Scope: "projects", DisplayName: "Runner", State: "active", CapacityLimit: 2, ProjectIDs: []tracker.ProjectID{f.Project.ID}}}
			hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPut, r.IdentityPath()+"/routing", hubfixture.AdminToken, change), http.StatusOK)
		}, after: []decision{on("first", allowed), on("second", forbidden)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			first := hubfixture.NewNative(t, nil, "", "first")
			fixtures := map[string]hubfixture.Native{"first": first, "second": hubfixture.NewNative(t, first.Hub, first.Project.OrganizationID, "second"), "foreign": hubfixture.NewNative(t, first.Hub, "org_first_second", "foreign")}
			for _, f := range fixtures {
				hubfixture.ApprovePolicy(t, f.Hub, f.Base+"/policy", hubfixture.Policy())
			}
			r := hubfixture.PrepareRunnerForScope(t, first, test.scope, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
			r.Enroll(t)
			hubfixture.ReportRunnerCapacity(t, first.Hub, first.Base, r.Redemption.Credential, r.Binding.MachineID, r.Redemption.DisplayName, r.Redemption.BackendIsolation, r.Redemption.Capacity)
			check := func(decisions []decision) {
				t.Helper()
				for _, want := range decisions {
					f := fixtures[want.project]
					registration := map[string]any{"id": r.Binding.MachineID, "hostname": "customer-host", "display_name": "Runner", "capacity": 2, "version": "test", "backend_isolation": r.Redemption.BackendIsolation}
					if got := first.Hub.Request(t, http.MethodPost, f.Base+"/machines/register", r.Redemption.Credential, registration).Code; got != want.register {
						t.Errorf("register on %s project status=%d, want %d", want.project, got, want.register)
					}
					if want.claim == 0 {
						continue
					}
					claim := tracker.NativeClaim{PolicyID: hubfixture.Policy().ID, WorkItemID: f.Create(t, "work").WorkItemID, MachineID: r.Binding.MachineID, SessionID: hubapi.NewID("session"), TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
					if got := first.Hub.Request(t, http.MethodPost, f.Base+"/claims", r.Redemption.Credential, claim).Code; got != want.claim {
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
