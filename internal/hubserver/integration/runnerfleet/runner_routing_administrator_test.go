package runnerfleet_test

import (
	"net/http"
	"testing"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerRoutingAdministratorControls(t *testing.T) {
	t.Parallel()
	h := hubfixture.Open(t, hubfixture.Config{})
	for _, test := range []struct {
		name   string
		change func(*runnerauth.RoutingChange)
		worker bool
		want   int
	}{
		{"normalized tags", func(c *runnerauth.RoutingChange) { c.Tags = []string{" LINUX ", "Build", "linux"} }, false, http.StatusOK},
		{"worker self assignment", func(*runnerauth.RoutingChange) {}, true, http.StatusForbidden},
		{"stale revision", func(c *runnerauth.RoutingChange) { c.ExpectedRevision = 5 }, false, http.StatusConflict},
		{"invalid tag", func(c *runnerauth.RoutingChange) { c.Tags = []string{"not a tag"} }, false, http.StatusUnprocessableEntity},
		{"invalid state", func(c *runnerauth.RoutingChange) { c.State = "unknown" }, false, http.StatusUnprocessableEntity},
		{"negative capacity", func(c *runnerauth.RoutingChange) { c.CapacityLimit = -1 }, false, http.StatusUnprocessableEntity},
		{"nonowner allowlist edit", func(c *runnerauth.RoutingChange) { c.ProjectIDs = []tracker.ProjectID{} }, false, http.StatusForbidden},
		{"unknown project", func(c *runnerauth.RoutingChange) { c.ProjectIDs = []tracker.ProjectID{"prj_unknown"} }, false, http.StatusUnprocessableEntity},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := hubfixture.NewNative(t, h, h.CreateOrganization(t, test.name), "Administration project")
			r := hubfixture.PrepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
			r.Enroll(t)
			if test.name == "nonowner allowlist edit" {
				if _, err := f.Hub.Admin.ExecContext(t.Context(), "UPDATE runner_enrollments SET created_by = (SELECT id FROM api_tokens WHERE token_hash = $1) WHERE runner_id = $2", apikey.HashToken(f.Token), r.Binding.RunnerID); err != nil {
					t.Fatal(err)
				}
			}
			change := runnerauth.RoutingChange{ExpectedRevision: 1, Routing: runnerauth.Routing{DisplayName: "Trusted", State: "active", Tags: []string{"production"}, CapacityLimit: 1, ProjectIDs: []tracker.ProjectID{f.Project.ID}}}
			test.change(&change)
			token := hubfixture.AdminToken
			if test.worker {
				token = r.Redemption.Credential
			}
			hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPut, r.IdentityPath()+"/routing", token, change), test.want)
			hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodGet, r.OrgBase+"/runners", r.Redemption.Credential, nil), http.StatusForbidden)
			response := f.Hub.Request(t, http.MethodGet, r.IdentityPath()+"/routing", r.Redemption.Credential, nil)
			hubfixture.RequireStatus(t, response, http.StatusOK)
			var before runnerauth.Runner
			hubfixture.Decode(t, response, &before)
			if test.want == http.StatusOK && (len(before.Tags) != 2 || before.Tags[0] != "build" || before.Tags[1] != "linux") {
				t.Fatalf("tags = %#v", before.Tags)
			}
			hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, f.Base+"/machines/register", r.Redemption.Credential, map[string]any{"id": r.Binding.MachineID, "hostname": "forged", "display_name": "forged", "capacity": 999, "version": "test", "os": "linux", "architecture": "arm64"}), http.StatusOK)
			response = f.Hub.Request(t, http.MethodGet, r.IdentityPath()+"/routing", r.Redemption.Credential, nil)
			var after runnerauth.Runner
			hubfixture.Decode(t, response, &after)
			if after.HostCapacity != 2 || after.DisplayName != before.DisplayName || after.CapacityLimit != before.CapacityLimit || after.Hostname != before.Hostname || after.OS != "linux" {
				t.Fatalf("worker changed administrator controls: %#v", after)
			}
			if test.want != http.StatusOK && len(after.Tags) != 0 {
				t.Fatalf("rejected edit assigned tags: %#v", after.Tags)
			}
		})
	}
}
