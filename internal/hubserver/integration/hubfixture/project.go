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

type Native struct {
	Hub     *Hub
	Project tracker.NativeProject
	Base    string
	Token   string
}

func NewDefaultNative(t *testing.T, cfg Config) Native {
	t.Helper()
	return NewNative(t, Open(t, cfg), "", "Example project")
}

func NewNative(t *testing.T, h *Hub, organization tracker.OrganizationID, name string) Native {
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
	return Native{Hub: h, Project: project, Base: ProjectBase(organization, project.ID), Token: credential.Token}
}

func ProjectBase(organization tracker.OrganizationID, project tracker.ProjectID) string {
	return OrganizationBase(organization) + "/projects/" + string(project)
}

func OrganizationBase(organization tracker.OrganizationID) string {
	return "/api/v2/organizations/" + string(organization)
}

func IssueBody() string { return strings.Repeat("Full issue content. ", 80) + IssueContractSections }

func (p Native) Organization() tracker.OrganizationID { return p.Project.OrganizationID }

func (p Native) Request(t *testing.T, method, path string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	return p.Hub.Request(t, method, path, p.Token, payload)
}

func (p Native) Create(t *testing.T, title string) tracker.NativeIssue {
	t.Helper()
	response := p.Hub.Request(t, http.MethodPost, p.Base+"/work-items", p.Token, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "create-" + title}, Title: title, Body: IssueBody(), State: "Todo"})
	RequireStatus(t, response, http.StatusOK)
	var issue tracker.NativeIssue
	Decode(t, response, &issue)
	return issue
}

func (p Native) Credential(t *testing.T, token string) (accessstore.Credential, error) {
	t.Helper()
	return p.Hub.Credential(t, p.Project.OrganizationID, token)
}

func (p Native) Worker(t *testing.T, name string) string {
	t.Helper()
	runner := hubtest.Runner(t, p.Hub.Repository, p.Project.OrganizationID, hubtest.RunnerOptions{Projects: []tracker.ProjectID{p.Project.ID}, DisplayName: name, Now: p.Hub.Now()})
	return runner.Credential.Token
}

func (p Native) ClearPolicy(t *testing.T) {
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

func States() []tracker.NativeState { return hubtest.States() }

func FormatTime(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }
