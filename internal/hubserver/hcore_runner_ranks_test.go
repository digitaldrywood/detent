package hubserver

import (
	"fmt"
	"maps"
	"net/http"
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
			runners = append(runners, runner)
			expected = append(expected, map[tracker.ProjectID]int{first.project.ID: 7, second.project.ID: 3})
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
