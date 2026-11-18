package runnerfleet_test

import (
	"maps"
	"net/http"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerScopeConversion(t *testing.T) {
	for _, convert := range []bool{false, true} {
		name := "project scope unchanged"
		if convert {
			name = "convert while running"
		}
		t.Run(name, func(t *testing.T) {
			f := hubfixture.NewDefaultNative(t, hubfixture.Config{})
			second := hubfixture.NewNative(t, f.Hub, f.Project.OrganizationID, "second")
			r := hubfixture.PrepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
			r.Enroll(t)
			r.ReportCapacity(t)
			ranks := map[tracker.ProjectID]int{f.Project.ID: 2, second.Project.ID: 1}
			response := f.Hub.Request(t, http.MethodPut, r.IdentityPath()+"/routing", hubfixture.AdminToken, runnerauth.RoutingChange{ExpectedRevision: 1, Routing: runnerauth.Routing{Scope: "projects", DisplayName: "Runner", State: "active", CapacityLimit: 2, ProjectIDs: []tracker.ProjectID{f.Project.ID}, ProjectRanks: ranks}})
			hubfixture.RequireStatus(t, response, http.StatusOK)
			var before runnerauth.Runner
			hubfixture.Decode(t, response, &before)
			hubfixture.ApprovePolicy(t, f.Hub, f.Base+"/policy", hubfixture.Policy())
			item := f.Create(t, "Active conversion work")
			claim := tracker.NativeClaim{PolicyID: hubfixture.Policy().ID, WorkItemID: item.WorkItemID, MachineID: r.Binding.MachineID, SessionID: "scope-conversion", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}}
			response = f.Hub.Request(t, http.MethodPost, f.Base+"/claims", r.Redemption.Credential, claim)
			hubfixture.RequireStatus(t, response, http.StatusOK)
			var lease tracker.NativeLease
			hubfixture.Decode(t, response, &lease)
			hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, f.Base+"/work-items/"+string(item.WorkItemID)+"/events", r.Redemption.Credential, hubfixture.StartedEvent(lease)), http.StatusOK)
			if convert {
				change := runnerauth.RoutingChange{ExpectedRevision: before.Revision, Routing: before.Routing}
				change.Scope, change.ProjectIDs, change.ProjectRanks = "organization", nil, nil
				hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPut, r.IdentityPath()+"/routing", hubfixture.AdminToken, change), http.StatusOK)
			}
			response = f.Hub.Request(t, http.MethodGet, r.IdentityPath()+"/routing", r.Redemption.Credential, nil)
			hubfixture.RequireStatus(t, response, http.StatusOK)
			var after runnerauth.Runner
			hubfixture.Decode(t, response, &after)
			stored, err := readRunnerRanks(t, f.Hub, f.Project.OrganizationID, r.Binding.RunnerID)
			if err != nil {
				t.Fatal(err)
			}
			after.ProjectRankOverrides = stored
			if after.Binding != before.Binding || !maps.Equal(after.ProjectRankOverrides, ranks) || after.Used != 1 || len(after.Leases) != 1 || after.Leases[0].ID != lease.ID {
				t.Fatalf("conversion changed identity, rank overrides or active lease: %+v", after)
			}
			queued := second.Create(t, "New project work")
			second.ClearPolicy(t)
			claim.WorkItemID, claim.SessionID = queued.WorkItemID, "scope-second-project"
			response = f.Hub.Request(t, http.MethodPost, second.Base+"/claims", r.Redemption.Credential, claim)
			if response.Code == http.StatusOK {
				t.Fatal("new project claimed before policy approval")
			}
			hubfixture.ApprovePolicy(t, f.Hub, second.Base+"/policy", hubfixture.Policy())
			response = f.Hub.Request(t, http.MethodPost, second.Base+"/claims", r.Redemption.Credential, claim)
			if convert {
				hubfixture.RequireStatus(t, response, http.StatusOK)
			} else if response.Code == http.StatusOK {
				t.Fatal("unconverted runner claimed an ungranted project")
			}
			response = f.Hub.Request(t, http.MethodPost, f.Base+"/machines/"+string(r.Binding.MachineID)+"/heartbeat", r.Redemption.Credential, map[string]any{"display_name": "Runner", "capacity": 2, "version": "test", "backend_isolation": r.Redemption.BackendIsolation})
			hubfixture.RequireStatus(t, response, http.StatusOK)
			var snapshot runnerauth.RoutingSnapshot
			hubfixture.Decode(t, response, &snapshot)
			if snapshot.RunnerID != before.RunnerID || snapshot.Routing.Scope != after.Scope {
				t.Fatalf("refresh did not retain identity and pick up scope: %+v", snapshot)
			}
		})
	}
}
