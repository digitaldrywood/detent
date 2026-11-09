package hubserver

import (
	"bytes"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"testing"

	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerProjectSchedulingRanksUsesPostgres(t *testing.T) {
	service := newClockedPostgresNativeService(t, Config{})
	var runners []runnerFixture
	var expected []map[tracker.ProjectID]int
	for _, organization := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		first := newNativeFixture(t, service, organization, "First ranked project")
		second := newNativeFixture(t, service, organization, "Second ranked project")
		if _, err := usagePostgresAdmin(t, service).ExecContext(t.Context(), "UPDATE projects SET scheduling_rank=CASE WHEN id=$1 THEN 7 ELSE 11 END WHERE organization_id=$2", first.project.ID, organization); err != nil {
			t.Fatal(err)
		}
		for index := range 2 {
			runner := prepareRunner(t, first, runnerauth.Read)
			runner.enroll(t)
			change := map[string]any{"expected_revision": 1, "display_name": "Ranked runner", "state": "active", "capacity_limit": 2,
				"project_ids": []tracker.ProjectID{first.project.ID, second.project.ID}, "project_ranks": map[tracker.ProjectID]int{second.project.ID: 3}, "idempotency_key": fmt.Sprintf("rank-%d", index)}
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPut, runner.identityPath()+"/routing", testHubAdminToken, change), http.StatusOK)
			response := performHubAPIRequest(t, service, http.MethodGet, runner.identityPath()+"/routing", testHubAdminToken, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var current runnerauth.Runner
			decodeHubResponse(t, response, &current)
			rename := runnerRoutingRequest{RoutingChange: runnerauth.RoutingChange{ExpectedRevision: current.Revision, Routing: current.Routing}, IdempotencyKey: fmt.Sprintf("rename-%d", index)}
			rename.DisplayName = "Renamed runner"
			renamed := performHubAPIRequest(t, service, http.MethodPut, runner.identityPath()+"/routing", testHubAdminToken, rename)
			requireNativeStatus(t, renamed, http.StatusOK)
			var view runnerauth.Runner
			decodeHubResponse(t, renamed, &view)
			if !maps.Equal(view.ProjectRanks, current.ProjectRanks) {
				t.Fatalf("rename changed effective rank response: %v want %v", view.ProjectRanks, current.ProjectRanks)
			}

			for _, access := range []struct {
				name, scope string
				projects    []tracker.ProjectID
				want        map[tracker.ProjectID]int
			}{
				{"remove second", "projects", []tracker.ProjectID{first.project.ID}, map[tracker.ProjectID]int{first.project.ID: 7}},
				{"organization", "organization", nil, map[tracker.ProjectID]int{first.project.ID: 7, second.project.ID: 3}},
			} {
				existing := performHubAPIRequest(t, service, http.MethodGet, runner.identityPath()+"/routing", testHubAdminToken, nil)
				requireNativeStatus(t, existing, http.StatusOK)
				var current runnerauth.Runner
				decodeHubResponse(t, existing, &current)
				input := runnerRoutingRequest{RoutingChange: runnerauth.RoutingChange{ExpectedRevision: current.Revision, Routing: current.Routing}, IdempotencyKey: fmt.Sprintf("%s-%d", access.name, index)}
				input.Scope, input.ProjectIDs, input.ProjectRanks = access.scope, access.projects, nil
				applied := performHubAPIRequest(t, service, http.MethodPut, runner.identityPath()+"/routing", testHubAdminToken, input)
				requireNativeStatus(t, applied, http.StatusOK)
				replay := performHubAPIRequest(t, service, http.MethodPut, runner.identityPath()+"/routing", testHubAdminToken, input)
				requireNativeStatus(t, replay, http.StatusOK)
				if !bytes.Equal(applied.Body.Bytes(), replay.Body.Bytes()) {
					t.Fatalf("%s replay changed routing receipt", access.name)
				}
				var written runnerauth.Runner
				decodeHubResponse(t, applied, &written)
				readback := performHubAPIRequest(t, service, http.MethodGet, runner.identityPath()+"/routing", testHubAdminToken, nil)
				requireNativeStatus(t, readback, http.StatusOK)
				var stored runnerauth.Runner
				decodeHubResponse(t, readback, &stored)
				if !maps.Equal(written.ProjectRanks, access.want) || !reflect.DeepEqual(written.Routing, stored.Routing) || written.Revision != stored.Revision {
					t.Fatalf("%s PUT/replay/GET disagree: PUT=%+v GET=%+v want ranks=%v", access.name, written.Routing, stored.Routing, access.want)
				}
			}
			runners = append(runners, runner)
			expected = append(expected, map[tracker.ProjectID]int{first.project.ID: 13, second.project.ID: 3})
		}
		if _, err := usagePostgresAdmin(t, service).ExecContext(t.Context(), "UPDATE projects SET scheduling_rank=13 WHERE organization_id=$1 AND id=$2", organization, first.project.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	service = reopenPostgresNativeService(t, service)
	for index, runner := range runners {
		path := runner.identityPath() + "/routing"
		requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, path, "", nil), http.StatusUnauthorized)
		foreign := tracker.OrganizationID("org_first_second")
		if index >= 2 {
			foreign = "org_first"
		}
		crossPath := "/api/v2/organizations/" + string(foreign) + "/runners/" + runner.binding.RunnerID + "/routing"
		requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, crossPath, testHubAdminToken, nil), http.StatusNotFound)
		response := performHubAPIRequest(t, service, http.MethodGet, path, testHubAdminToken, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var stored runnerauth.Runner
		decodeHubResponse(t, response, &stored)
		if !maps.Equal(stored.ProjectRanks, expected[index]) {
			t.Fatalf("runner %d persisted ranks=%v want=%v", index, stored.ProjectRanks, expected[index])
		}
	}
}
