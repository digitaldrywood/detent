package runnerfleet_test

import (
	"net/http"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerRoutingRevocationAndDrain(t *testing.T) {
	t.Parallel()
	h := hubfixture.Open(t, hubfixture.Config{})
	for _, scope := range []string{"projects", "organization"} {
		for _, test := range []struct {
			name, state string
			tags        []string
			access      bool
			want        int
		}{
			{"rename", "active", []string{"build"}, true, http.StatusOK},
			{"drain active lease", "draining", []string{"build"}, true, http.StatusOK},
			{"disable", "disabled", []string{"build"}, true, http.StatusConflict},
			{"remove required tag", "active", nil, true, http.StatusConflict},
			{"remove access", "active", []string{"build"}, false, http.StatusNotFound},
		} {
			t.Run(scope+"/"+test.name, func(t *testing.T) {
				f := hubfixture.NewNative(t, h, h.CreateOrganization(t, scope+" "+test.name), "Revocation project")
				r := hubfixture.PrepareRunnerForScope(t, f, scope, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
				r.Enroll(t)
				change := runnerauth.RoutingChange{ExpectedRevision: 1, Routing: runnerauth.Routing{Scope: scope, DisplayName: "First", State: "active", Tags: []string{"build"}, CapacityLimit: 2, ProjectIDs: []tracker.ProjectID{f.Project.ID}}}
				if scope == "organization" {
					change.ProjectIDs = nil
				}
				hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPut, r.IdentityPath()+"/routing", hubfixture.AdminToken, change), http.StatusOK)
				r.ReportCapacity(t)
				descriptor := hubfixture.Policy()
				descriptor.Requirements.RequiredTags = []string{"build"}
				descriptor = descriptor.WithID()
				hubfixture.ApprovePolicy(t, f.Hub, f.Base+"/policy", descriptor)
				issue := f.Create(t, "work")
				claim := tracker.NativeClaim{PolicyID: descriptor.ID, WorkItemID: issue.WorkItemID, MachineID: r.Binding.MachineID, SessionID: "work", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
				response := f.Hub.Request(t, http.MethodPost, f.Base+"/claims", r.Redemption.Credential, claim)
				hubfixture.RequireStatus(t, response, http.StatusOK)
				var lease tracker.NativeLease
				hubfixture.Decode(t, response, &lease)
				change.ExpectedRevision, change.DisplayName, change.State, change.Tags = 2, "Renamed", test.state, test.tags
				if !test.access {
					change.Scope = "projects"
					change.ProjectIDs = nil
				}
				hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPut, r.IdentityPath()+"/routing", hubfixture.AdminToken, change), http.StatusOK)
				for _, action := range []string{"renew", "validate"} {
					hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, f.Base+"/leases/"+string(lease.ID)+"/"+action, r.Redemption.Credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 90}), test.want)
				}
				event := hubfixture.StartedEvent(lease)
				event.IdempotencyKey, event.Data.PolicyID = "event", descriptor.ID
				hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, f.Base+"/work-items/"+string(issue.WorkItemID)+"/events", r.Redemption.Credential, event), test.want)
			})
		}
	}
}
