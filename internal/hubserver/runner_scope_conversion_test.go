package hubserver

import (
	"maps"
	"net/http"
	"testing"

	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerScopeConversion(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}
	for _, convert := range []bool{false, true} {
		name := "project scope unchanged"
		if convert {
			name = "convert while running"
		}
		t.Run(name, func(t *testing.T) {
			f := newDefaultNativeFixture(t, Config{})
			second := newNativeFixture(t, f.service, f.project.OrganizationID, "second")
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
			r.enroll(t)
			ranks := map[tracker.ProjectID]int{f.project.ID: 2, second.project.ID: 1}
			response := performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, runnerauth.RoutingChange{ExpectedRevision: 1, Routing: runnerauth.Routing{Scope: "projects", DisplayName: "Runner", State: "active", CapacityLimit: 2, ProjectIDs: []tracker.ProjectID{f.project.ID}, ProjectRanks: ranks}})
			requireNativeStatus(t, response, http.StatusOK)
			var before runnerauth.Runner
			decodeHubResponse(t, response, &before)
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			item := f.create(t, "Active conversion work")
			claim := tracker.NativeClaim{PolicyID: hubTestPolicy().ID, WorkItemID: item.WorkItemID, MachineID: r.binding.MachineID, SessionID: "scope-conversion", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}}
			response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim)
			requireNativeStatus(t, response, http.StatusOK)
			var lease tracker.NativeLease
			decodeHubResponse(t, response, &lease)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(item.WorkItemID)+"/events", r.redemption.Credential, nativeStartedEvent(lease)), http.StatusOK)
			if convert {
				change := runnerauth.RoutingChange{ExpectedRevision: before.Revision, Routing: before.Routing}
				change.Scope, change.ProjectIDs, change.ProjectRanks = "organization", nil, nil
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, change), http.StatusOK)
			}
			response = performHubAPIRequest(t, f.service, http.MethodGet, r.identityPath()+"/routing", r.redemption.Credential, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var after runnerauth.Runner
			decodeHubResponse(t, response, &after)
			stored, err := readRunnerWithClock(t.Context(), f.service.database.db, f.project.OrganizationID, r.binding.RunnerID, f.service.config.now)
			if err != nil {
				t.Fatal(err)
			}
			after.ProjectRankOverrides = stored.ProjectRankOverrides
			if after.Binding != before.Binding || !maps.Equal(after.ProjectRankOverrides, ranks) || after.Used != 1 || len(after.Leases) != 1 || after.Leases[0].ID != lease.ID {
				t.Fatalf("conversion changed identity, rank overrides or active lease: %+v", after)
			}
			queued := second.create(t, "New project work")
			claim.WorkItemID, claim.SessionID = queued.WorkItemID, "scope-second-project"
			response = performHubAPIRequest(t, f.service, http.MethodPost, second.base+"/claims", r.redemption.Credential, claim)
			if response.Code == http.StatusOK {
				t.Fatal("new project claimed before policy approval")
			}
			approveHubTestPolicy(t, f.service, second.base+"/policy", hubTestPolicy())
			response = performHubAPIRequest(t, f.service, http.MethodPost, second.base+"/claims", r.redemption.Credential, claim)
			if convert {
				requireNativeStatus(t, response, http.StatusOK)
			} else if response.Code == http.StatusOK {
				t.Fatal("unconverted runner claimed an ungranted project")
			}
			response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, map[string]any{"capacity": 2, "version": "test", "backend_isolation": r.redemption.BackendIsolation})
			requireNativeStatus(t, response, http.StatusOK)
			var snapshot runnerauth.RoutingSnapshot
			decodeHubResponse(t, response, &snapshot)
			if snapshot.RunnerID != before.RunnerID || snapshot.Routing.Scope != after.Scope {
				t.Fatalf("refresh did not retain identity and pick up scope: %+v", snapshot)
			}
		})
	}
}
