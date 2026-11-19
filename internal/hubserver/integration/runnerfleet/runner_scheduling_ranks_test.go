package runnerfleet_test

import (
	"bytes"
	"maps"
	"net/http"
	"reflect"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerProjectSchedulingRanksUsesPostgres(t *testing.T) {
	t.Parallel()
	service := hubfixture.Open(t, hubfixture.Config{})
	var runners []hubfixture.Runner
	var expected []map[tracker.ProjectID]int
	for _, organization := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		first := hubfixture.NewNative(t, service, organization, "First ranked project")
		second := hubfixture.NewNative(t, service, organization, "Second ranked project")
		if _, err := service.Admin.ExecContext(t.Context(), "UPDATE projects SET scheduling_rank=CASE WHEN id=$1 THEN 7 ELSE 11 END WHERE organization_id=$2", first.Project.ID, organization); err != nil {
			t.Fatal(err)
		}
		{
			runner := hubfixture.PrepareRunner(t, first, runnerauth.Read)
			runner.Enroll(t)
			change := map[string]any{"expected_revision": 1, "display_name": "Ranked runner", "state": "active", "capacity_limit": 2,
				"project_ids": []tracker.ProjectID{first.Project.ID, second.Project.ID}, "project_ranks": map[tracker.ProjectID]int{second.Project.ID: 3}, "idempotency_key": "rank"}
			hubfixture.RequireStatus(t, service.Request(t, http.MethodPut, runner.IdentityPath()+"/routing", hubfixture.AdminToken, change), http.StatusOK)
			response := service.Request(t, http.MethodGet, runner.IdentityPath()+"/routing", hubfixture.AdminToken, nil)
			hubfixture.RequireStatus(t, response, http.StatusOK)
			var current runnerauth.Runner
			hubfixture.Decode(t, response, &current)
			rename := runnerRoutingRequest{RoutingChange: runnerauth.RoutingChange{ExpectedRevision: current.Revision, Routing: current.Routing}, IdempotencyKey: "rename"}
			rename.DisplayName = "Renamed runner"
			renamed := service.Request(t, http.MethodPut, runner.IdentityPath()+"/routing", hubfixture.AdminToken, rename)
			hubfixture.RequireStatus(t, renamed, http.StatusOK)
			var view runnerauth.Runner
			hubfixture.Decode(t, renamed, &view)
			if !maps.Equal(view.ProjectRanks, current.ProjectRanks) {
				t.Fatalf("rename changed effective rank response: %v want %v", view.ProjectRanks, current.ProjectRanks)
			}

			for _, access := range []struct {
				name, scope string
				projects    []tracker.ProjectID
				want        map[tracker.ProjectID]int
			}{
				{"remove second", "projects", []tracker.ProjectID{first.Project.ID}, map[tracker.ProjectID]int{first.Project.ID: 7}},
				{"organization", "organization", nil, map[tracker.ProjectID]int{first.Project.ID: 7, second.Project.ID: 3}},
			} {
				existing := service.Request(t, http.MethodGet, runner.IdentityPath()+"/routing", hubfixture.AdminToken, nil)
				hubfixture.RequireStatus(t, existing, http.StatusOK)
				var current runnerauth.Runner
				hubfixture.Decode(t, existing, &current)
				input := runnerRoutingRequest{RoutingChange: runnerauth.RoutingChange{ExpectedRevision: current.Revision, Routing: current.Routing}, IdempotencyKey: access.name}
				input.Scope, input.ProjectIDs, input.ProjectRanks = access.scope, access.projects, nil
				applied := service.Request(t, http.MethodPut, runner.IdentityPath()+"/routing", hubfixture.AdminToken, input)
				hubfixture.RequireStatus(t, applied, http.StatusOK)
				replay := service.Request(t, http.MethodPut, runner.IdentityPath()+"/routing", hubfixture.AdminToken, input)
				hubfixture.RequireStatus(t, replay, http.StatusOK)
				if !bytes.Equal(applied.Body.Bytes(), replay.Body.Bytes()) {
					t.Fatalf("%s replay changed routing receipt", access.name)
				}
				var written runnerauth.Runner
				hubfixture.Decode(t, applied, &written)
				readback := service.Request(t, http.MethodGet, runner.IdentityPath()+"/routing", hubfixture.AdminToken, nil)
				hubfixture.RequireStatus(t, readback, http.StatusOK)
				var stored runnerauth.Runner
				hubfixture.Decode(t, readback, &stored)
				if !maps.Equal(written.ProjectRanks, access.want) || !reflect.DeepEqual(written.Routing, stored.Routing) || written.Revision != stored.Revision {
					t.Fatalf("%s PUT/replay/GET disagree: PUT=%+v GET=%+v want ranks=%v", access.name, written.Routing, stored.Routing, access.want)
				}
			}
			runners = append(runners, runner)
			expected = append(expected, map[tracker.ProjectID]int{first.Project.ID: 13, second.Project.ID: 3})
		}
		if _, err := service.Admin.ExecContext(t.Context(), "UPDATE projects SET scheduling_rank=13 WHERE organization_id=$1 AND id=$2", organization, first.Project.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := service.Service.Close(); err != nil {
		t.Fatal(err)
	}
	service = service.Reopen(t)
	for index, runner := range runners {
		path := runner.IdentityPath() + "/routing"
		hubfixture.RequireStatus(t, service.Request(t, http.MethodGet, path, "", nil), http.StatusUnauthorized)
		foreign := tracker.OrganizationID("org_first_second")
		if index >= 1 {
			foreign = "org_first"
		}
		crossPath := "/api/v2/organizations/" + string(foreign) + "/runners/" + runner.Binding.RunnerID + "/routing"
		hubfixture.RequireStatus(t, service.Request(t, http.MethodGet, crossPath, hubfixture.AdminToken, nil), http.StatusNotFound)
		response := service.Request(t, http.MethodGet, path, hubfixture.AdminToken, nil)
		hubfixture.RequireStatus(t, response, http.StatusOK)
		var stored runnerauth.Runner
		hubfixture.Decode(t, response, &stored)
		if !maps.Equal(stored.ProjectRanks, expected[index]) {
			t.Fatalf("runner %d persisted ranks=%v want=%v", index, stored.ProjectRanks, expected[index])
		}
	}
}
