package hubfixture

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Project struct {
	Hub     *Hub
	Project tracker.NativeProject
	Base    string
	Token   string
}

func NewDefaultProject(t *testing.T, cfg Config) Project {
	t.Helper()
	return NewProject(t, Open(t, cfg), "", "Example project")
}

func NewProject(t *testing.T, h *Hub, organization tracker.OrganizationID, name string) Project {
	t.Helper()
	if h == nil {
		h = Open(t, Config{})
	}
	if organization == "" {
		organization = h.Primary
	}
	h.EnsureOrganization(t, organization)
	now := h.Now().UTC().Truncate(time.Microsecond)
	project := hubtest.Project(t, h.Repository, organization, hubtest.ProjectOptions{Name: name, RequireDependencies: true})
	credential := hubtest.Credential(t, h.Repository, organization, hubtest.CredentialOptions{Projects: []tracker.ProjectID{project.ID}, Now: now})
	hubtest.Policy(t, h.Repository, h.Config.RunDetails, project, hubtest.PolicyOptions{Now: now})
	return Project{Hub: h, Project: project, Base: ProjectBase(organization, project.ID), Token: credential.Token}
}

func ProjectBase(organization tracker.OrganizationID, project tracker.ProjectID) string {
	return OrganizationBase(organization) + "/projects/" + string(project)
}

func OrganizationBase(organization tracker.OrganizationID) string {
	return "/api/v2/organizations/" + string(organization)
}

func IssueBody() string { return strings.Repeat("Full issue content. ", 80) + IssueContractSections }

func (p Project) Organization() tracker.OrganizationID { return p.Project.OrganizationID }

func (p Project) Request(t *testing.T, method, path string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	return p.Hub.Request(t, method, path, p.Token, payload)
}

func (p Project) Create(t *testing.T, title string) tracker.NativeIssue {
	t.Helper()
	response := p.Hub.Request(t, http.MethodPost, p.Base+"/work-items", p.Token, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "create-" + title}, Title: title, Body: IssueBody(), State: "Todo"})
	RequireStatus(t, response, http.StatusOK)
	var issue tracker.NativeIssue
	Decode(t, response, &issue)
	return issue
}

func (p Project) Credential(t *testing.T, token string) (accessstore.Credential, error) {
	t.Helper()
	return p.Hub.Credential(t, p.Project.OrganizationID, token)
}

func (p Project) Worker(t *testing.T, name string) string {
	t.Helper()
	runner := hubtest.Runner(t, p.Hub.Repository, p.Project.OrganizationID, hubtest.RunnerOptions{Projects: []tracker.ProjectID{p.Project.ID}, DisplayName: name, Now: p.Hub.Now()})
	return runner.Credential.Token
}

func (p Project) ClearPolicy(t *testing.T) {
	t.Helper()
	if _, err := p.Hub.Admin.ExecContext(t.Context(), "DELETE FROM project_policies WHERE organization_id=$1 AND project_id=$2", p.Project.OrganizationID, p.Project.ID); err != nil {
		t.Fatal(err)
	}
}

func Policy() policy.Descriptor { return hubtest.PolicyDescriptor() }

func ApprovePolicy(t *testing.T, h *Hub, path string, descriptor policy.Descriptor) {
	t.Helper()
	current := h.Request(t, http.MethodGet, path, string(h.Config.InitialAdminToken), nil)
	var approval policy.Approval
	if current.Code == http.StatusOK {
		Decode(t, current, &approval)
	}
	RequireStatus(t, h.Request(t, http.MethodPut, path, string(h.Config.InitialAdminToken), policy.Change{ExpectedID: approval.Policy.ID, Policy: descriptor}), http.StatusOK)
}
