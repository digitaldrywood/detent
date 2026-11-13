package hubfixture_test

import (
	"net/http"
	"sync"
	"testing"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/internal/hubdeps"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestMain(m *testing.M) { hubtest.Main(m) }

func TestProjectsInTwoOrganizationsAreUsableAndIsolated(t *testing.T) {
	t.Parallel()
	h := hubfixture.Open(t, hubfixture.Config{})
	first := hubfixture.NewNative(t, h, hubfixture.Primary, "first")
	second := hubfixture.NewNative(t, h, hubfixture.Secondary, "second")
	created := hubfixture.NewNative(t, h, h.CreateOrganization(t, "Example organization"), "created")
	for _, p := range []hubfixture.Native{first, second, created} {
		if issue := p.Create(t, "isolated"); issue.Number != 1 || issue.ProjectID != p.Project.ID {
			t.Fatalf("issue = %d in %s, want number 1 in %s", issue.Number, issue.ProjectID, p.Project.ID)
		}
		hubfixture.RequireStatus(t, h.Request(t, http.MethodGet, p.Base, hubfixture.AdminToken, nil), http.StatusOK)
	}
	for _, test := range []struct{ token, base string }{{first.Token, second.Base}, {second.Token, first.Base}, {first.Token, created.Base}} {
		if response := h.Request(t, http.MethodGet, test.base+"/work-items", test.token, nil); response.Code == http.StatusOK {
			t.Fatalf("foreign project read status = %d: %s", response.Code, response.Body.String())
		}
	}
	reopened := h.Reopen(t)
	hubfixture.RequireStatus(t, reopened.Request(t, http.MethodGet, second.Base, second.Token, nil), http.StatusOK)
}

func TestRunnersInTwoOrganizationsAreUsableAndIsolated(t *testing.T) {
	t.Parallel()
	h := hubfixture.Open(t, hubfixture.Config{})
	var runners []hubfixture.Runner
	for _, organization := range []tracker.OrganizationID{hubfixture.Primary, hubfixture.Secondary} {
		r := hubfixture.PrepareRunner(t, hubfixture.NewNative(t, h, organization, "runner"), runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
		r.Enroll(t)
		r.ReportCapacity(t)
		hubfixture.RequireStatus(t, h.Request(t, http.MethodGet, r.IdentityPath()+"/routing", hubfixture.AdminToken, nil), http.StatusOK)
		runners = append(runners, r)
	}
	foreign := h.Request(t, http.MethodPost, runners[1].Native.Base+"/machines/"+string(runners[0].Binding.MachineID)+"/heartbeat", runners[0].Redemption.Credential, map[string]any{"display_name": "foreign", "capacity": 1, "version": "test"})
	if foreign.Code == http.StatusOK {
		t.Fatalf("foreign heartbeat status = %d: %s", foreign.Code, foreign.Body.String())
	}
}

func TestChangesInTwoOrganizationsAreUsableAndIsolated(t *testing.T) {
	t.Parallel()
	h := hubfixture.Open(t, hubfixture.Config{})
	first := hubfixture.NewChange(t, h, hubfixture.Primary)
	second := hubfixture.NewChange(t, h, hubfixture.Secondary)
	for _, c := range []hubfixture.Change{first, second} {
		version := c.Publish(t, "publish", "")
		if detail := c.Detail(t); detail.Change.ID != c.Change.ID || len(detail.Versions) != 1 || detail.Versions[0].ID != version.ID {
			t.Fatalf("detail = %#v, want one version %s", detail.Change, version.ID)
		}
	}
	if response := h.Request(t, http.MethodGet, second.Path, first.Token, nil); response.Code == http.StatusOK {
		t.Fatalf("foreign change read status = %d: %s", response.Code, response.Body.String())
	}
}

func TestHostedOrganizationsAreUsableAndIsolated(t *testing.T) {
	t.Parallel()
	h, project, base := hubfixture.OpenHostedSecurity(t)
	owner := h.User(t, "owner", "owner", "owner@example.test", "write", "")
	viewer := h.User(t, "viewer", "viewer", "viewer@example.test", "read", "")
	response := h.Request(t, owner, http.MethodPost, base+"/work-items", tracker.CreateIssue{Body: hubfixture.IssueBody(), Mutation: tracker.Mutation{IdempotencyKey: "isolation"}, Title: "isolated", State: "Todo"})
	hubfixture.RequireStatus(t, response, http.StatusOK)
	hubfixture.RequireStatus(t, h.Request(t, viewer, http.MethodGet, base+"/work-items", nil), http.StatusOK)
	h.Revoke(t, viewer, project)
	if response := h.Request(t, viewer, http.MethodGet, base+"/work-items", nil); response.Code == http.StatusOK {
		t.Fatalf("revoked viewer status = %d", response.Code)
	}
	h.EnsureOrganization(t, hubfixture.Secondary)
	other := hubfixture.NewNative(t, h.Hub, hubfixture.Secondary, "other")
	if response := h.Request(t, owner, http.MethodGet, other.Base+"/work-items", nil); response.Code == http.StatusOK {
		t.Fatalf("hosted session read another organization: %s", response.Body.String())
	}
	second, _, _ := hubfixture.OpenHostedSecurity(t)
	if response := second.Request(t, owner, http.MethodGet, base+"/work-items", nil); response.Code == http.StatusOK {
		t.Fatalf("session crossed hosted databases: %s", response.Body.String())
	}
}

func TestMCPInTwoOrganizationsIsUsableAndIsolated(t *testing.T) {
	t.Parallel()
	h := hubfixture.Open(t, hubfixture.Config{})
	first := hubfixture.NewNative(t, h, hubfixture.Primary, "first")
	second := hubfixture.NewNative(t, h, hubfixture.Secondary, "second")
	for _, p := range []hubfixture.Native{first, second} {
		call := hubfixture.MCP(t, h.Handler(), hubfixture.MCPPath(p.Organization()), p.Token)
		if reply := call("tools/list", "", nil); len(reply.Result) == 0 {
			t.Fatalf("tools/list = %s", reply.Error)
		}
	}
	foreign := hubfixture.MCP(t, h.Handler(), hubfixture.MCPPath(second.Organization()), first.Token)("tools/list", "", nil)
	if len(foreign.Result) != 0 && len(foreign.Error) == 0 {
		t.Fatalf("foreign MCP tools/list = %s", foreign.Result)
	}
}

func TestInjectedDependenciesServeTwoOrganizations(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	generated := map[string]bool{}
	h := hubfixture.OpenDeps(t, hubfixture.Config{}, hubdeps.Deps{GenerateToken: func() (string, error) {
		token, err := apikey.GenerateToken()
		mu.Lock()
		defer mu.Unlock()
		generated[token] = true
		return token, err
	}})
	var runners []hubfixture.Runner
	for _, organization := range []tracker.OrganizationID{hubfixture.Primary, hubfixture.Secondary} {
		r := hubfixture.PrepareRunner(t, hubfixture.NewNative(t, h, organization, "injected"), runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
		mu.Lock()
		injected := generated[r.Enrollment.Token]
		mu.Unlock()
		if !injected {
			t.Fatalf("%s enrollment token was not issued by the injected generator", organization)
		}
		runners = append(runners, r)
	}
	foreign := h.Request(t, http.MethodPost, runners[1].OrgBase+"/runner-enrollments/redeem", runners[0].Enrollment.Token, runners[0].Redemption)
	if foreign.Code == http.StatusCreated {
		t.Fatalf("foreign enrollment redeem status = %d: %s", foreign.Code, foreign.Body.String())
	}
	for _, r := range runners {
		r.Enroll(t)
		r.ReportCapacity(t)
	}
	reopened := h.Reopen(t)
	second := hubfixture.PrepareRunner(t, hubfixture.NewNative(t, reopened, hubfixture.Secondary, "reopened"), runnerauth.Read)
	mu.Lock()
	defer mu.Unlock()
	if !generated[second.Enrollment.Token] {
		t.Fatal("reopened Hub dropped the injected generator")
	}
}
