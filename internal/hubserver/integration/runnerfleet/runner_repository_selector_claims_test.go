package runnerfleet_test

import (
	"net/http"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRepositoryRunnerSelectorsCannotStealWork(t *testing.T) {
	t.Parallel()
	a := hubfixture.NewDefaultNative(t, hubfixture.Config{})
	b := hubfixture.NewNative(t, a.Hub, a.Project.OrganizationID, "linux-project")
	mac := hubfixture.PrepareRunner(t, a, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
	mac.Enroll(t)
	linux := hubfixture.PrepareRunner(t, b, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
	linux.Enroll(t)
	for _, r := range []hubfixture.Runner{mac, linux} {
		tags := []string{"macos"}
		if r.Binding.RunnerID == linux.Binding.RunnerID {
			tags = []string{"linux", "build"}
		}
		change := runnerauth.RoutingChange{ExpectedRevision: 1, Routing: runnerauth.Routing{DisplayName: "Builder", State: "active", Tags: tags, CapacityLimit: 2, ProjectIDs: []tracker.ProjectID{a.Project.ID, b.Project.ID}}}
		hubfixture.RequireStatus(t, a.Hub.Request(t, http.MethodPut, r.IdentityPath()+"/routing", hubfixture.AdminToken, change), http.StatusOK)
		r.ReportCapacity(t)
	}
	macPolicy := hubfixture.Policy()
	macPolicy.Requirements.MachineID = string(mac.Binding.MachineID)
	macPolicy = macPolicy.WithID()
	linuxPolicy := hubfixture.Policy()
	linuxPolicy.Requirements.RequiredTags = []string{"build", "linux"}
	linuxPolicy = linuxPolicy.WithID()
	hubfixture.ApprovePolicy(t, a.Hub, a.Base+"/policy", macPolicy)
	hubfixture.ApprovePolicy(t, a.Hub, b.Base+"/policy", linuxPolicy)
	aIssue := a.Create(t, "exact Mac work")
	bIssue := b.Create(t, "Linux pool work")
	for _, test := range []struct {
		name       string
		fixture    hubfixture.Native
		runner     hubfixture.Runner
		descriptor policy.Descriptor
		issue      tracker.NativeWorkItemID
		want       int
	}{
		{"Linux cannot take Mac work", a, linux, macPolicy, aIssue.WorkItemID, http.StatusConflict},
		{"Mac cannot take Linux work", b, mac, linuxPolicy, bIssue.WorkItemID, http.StatusConflict},
		{"Mac accepts exact work", a, mac, macPolicy, aIssue.WorkItemID, http.StatusOK},
		{"Linux accepts matching work", b, linux, linuxPolicy, bIssue.WorkItemID, http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			claim := tracker.NativeClaim{PolicyID: test.descriptor.ID, WorkItemID: test.issue, MachineID: test.runner.Binding.MachineID, SessionID: test.name, TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
			hubfixture.RequireStatus(t, a.Hub.Request(t, http.MethodPost, test.fixture.Base+"/claims", test.runner.Redemption.Credential, claim), test.want)
		})
	}
}
