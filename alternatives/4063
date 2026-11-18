package threads_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/threads"
	"github.com/digitaldrywood/detent/internal/hubserver/threadstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type teamThreadInput = operatortool.ThreadArguments
type teamThread = threadstore.Thread
type teamPost = threadstore.Post
type teamThreadRead = threadstore.Read
type teamThreadPage = threadstore.Page
type teamUnreadSummary = threads.UnreadSummary

func TestMain(m *testing.M) { hubtest.Main(m) }

func threadHTTP(t *testing.T, repository tenantstore.Repository, organization tracker.OrganizationID, clock *hubtest.Clock, hosted bool) (*hubtest.HTTPHarness, *threads.Service, *hubtest.Details) {
	return threadHTTPWithDetails(t, repository, organization, clock, hosted, nil)
}
func threadHTTPWithDetails(t *testing.T, repository tenantstore.Repository, organization tracker.OrganizationID, clock *hubtest.Clock, hosted bool, details *hubtest.Details) (*hubtest.HTTPHarness, *threads.Service, *hubtest.Details) {
	t.Helper()
	if details == nil {
		details = hubtest.NewDetails()
	}
	var service *threads.Service
	var hostedFacts *hubauth.Hosted
	if hosted {
		hostedFacts = &hubauth.Hosted{}
	}
	h := hubtest.NewHTTP(t, hubtest.HTTPOptions{Repository: repository, Organization: organization, Now: clock.Now, Hosted: hostedFacts}, func(group *echo.Group, deps hubtest.HTTPDeps) {
		service = threads.New(threads.Deps{Repository: repository, Details: func() attachment.DetailStore { return details }, Now: clock.Now, Organization: func() tracker.OrganizationID { return organization }, RecordError: hubauth.RecordError})
		threads.RegisterRoutes(group, threads.HTTPDeps{Service: func() *threads.Service { return service }, Auth: func() *hubauth.Authenticator { return deps.Auth }, Operator: deps.Operator, Error: deps.Error})
	})
	return h, service, details
}

type nativeFixture struct {
	service *hubtest.HTTPHarness
	project tracker.NativeProject
	token   string
	clock   *hubtest.Clock
	details *hubtest.Details
}

func newTeamThreadNativeFixture(t *testing.T, service *hubtest.HTTPHarness, organization tracker.OrganizationID, name string) nativeFixture {
	t.Helper()
	clock := hubtest.NewClock(time.Now().UTC())
	repository := tenantstore.Repository(nil)
	if service != nil {
		repository = service.Repository
		clock = hubtest.NewClock(service.Now())
	} else {
		repository = hubtest.Repository(t)
	}
	if organization == "" {
		organization = hubtest.Organization(t, repository, hubtest.OrganizationOptions{}).OrganizationID
	} else {
		var exists bool
		err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
			_, err := r.AccessTenant(t.Context())
			exists = err == nil
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if !exists {
			hubtest.Organization(t, repository, hubtest.OrganizationOptions{ID: organization})
		}
	}
	project := hubtest.Project(t, repository, organization, hubtest.ProjectOptions{Name: name})
	h, _, details := threadHTTP(t, repository, organization, clock, false)
	actor := h.OperatorKey(t, project.ID)
	return nativeFixture{service: h, project: project, token: actor.Token, clock: clock, details: details}
}
func (f nativeFixture) create(t *testing.T, title string) tracker.NativeIssue {
	t.Helper()
	credential, status, err := f.service.Deps.Auth.AuthenticateRepositoryAPIHash(t.Context(), f.service.Repository, f.project.OrganizationID, apikey.HashToken(f.token), "", "")
	if err != nil || status != http.StatusOK {
		t.Fatalf("authenticate issue actor: %d %v", status, err)
	}
	return hubtest.Issue(t, f.service.Repository, f.details, f.project, hubtest.IssueOptions{Title: title, Body: "Example issue body", Actor: hubauth.Actor(credential, f.project.OrganizationID), Now: f.clock.Now()}).Issue
}
func threadBase(f nativeFixture) string {
	return "/api/v2/organizations/" + string(f.project.OrganizationID) + "/threads"
}
func performHubAPIRequest(t *testing.T, h *hubtest.HTTPHarness, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return h.Request(t, hubtest.HTTPActor{Token: token}, method, path, body).Recorder
}
func requireNativeStatus(t *testing.T, r *httptest.ResponseRecorder, want int) {
	t.Helper()
	if r.Code != want {
		t.Fatalf("status=%d body=%s want=%d", r.Code, r.Body.String(), want)
	}
}
func decodeHubResponse(t *testing.T, r *httptest.ResponseRecorder, value any) {
	t.Helper()
	if err := json.Unmarshal(r.Body.Bytes(), value); err != nil {
		t.Fatal(err)
	}
}
func createTestThread(t *testing.T, f nativeFixture, scope string, issue tracker.NativeWorkItemID) teamThread {
	t.Helper()
	in := teamThreadInput{Scope: scope, Title: "Shared research", Key: hubapi.NewID("intent")}
	if scope == "project" {
		in.ProjectID = string(f.project.ID)
	}
	if scope == "issue" {
		in.IssueID = string(issue)
		in.ProjectID = "prj_ignored"
	}
	r := performHubAPIRequest(t, f.service, http.MethodPost, threadBase(f), f.token, in)
	requireNativeStatus(t, r, http.StatusCreated)
	var out teamThread
	decodeHubResponse(t, r, &out)
	return out
}

type hostedThreadFixture struct {
	service       *hubtest.HTTPHarness
	project       tracker.ProjectID
	projectRecord tracker.NativeProject
	base          string
	users         map[string]hubtest.HTTPActor
	threads       *threads.Service
	details       *hubtest.Details
}

func newHostedSecurityFixture(t *testing.T) hostedThreadFixture {
	t.Helper()
	repository := hubtest.Repository(t)
	organization := hubtest.Organization(t, repository, hubtest.OrganizationOptions{ID: "org_security"}).OrganizationID
	project := hubtest.Project(t, repository, organization, hubtest.ProjectOptions{})
	h, service, details := threadHTTP(t, repository, organization, hubtest.NewClock(time.Now().UTC()), true)
	return hostedThreadFixture{service: h, project: project.ID, projectRecord: project, base: "/api/v2/organizations/" + string(organization) + "/projects/" + string(project.ID), users: map[string]hubtest.HTTPActor{}, threads: service, details: details}
}
func newVerificationFixture(t *testing.T) hostedThreadFixture {
	t.Helper()
	v := newHostedSecurityFixture(t)
	for _, name := range []string{"reporter", "tester", "member"} {
		role := "reporter"
		if name == "member" {
			role = "member"
		}
		v.users[name] = v.user(t, name, role, name+"@example.test", "write", "")
	}
	return v
}
func (v hostedThreadFixture) user(t *testing.T, name, role, email, grant, unused string) hubtest.HTTPActor {
	t.Helper()
	options := hubtest.MemberOptions{Role: role, Email: email}
	if grant != "" {
		options.Projects = []tracker.ProjectID{v.project}
		options.ReadOnly = grant != "write"
	}
	return v.service.MemberSession(t, options)
}
func (v hostedThreadFixture) request(t *testing.T, user hubtest.HTTPActor, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return v.service.Request(t, user, method, path, body).Recorder
}
func (v hostedThreadFixture) grant(t *testing.T, user hubtest.HTTPActor, write, manage bool) {
	t.Helper()
	if err := v.service.Repository.WriteTenant(t.Context(), v.service.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: v.service.Organization, ProjectID: v.project, UserID: user.Member.Member.UserID, CanWrite: write, ManageRunner: manage}); err != nil {
			return err
		}
		return w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: v.service.Organization, ProjectID: v.project, TokenID: user.Credential.ID})
	}); err != nil {
		t.Fatal(err)
	}
}
func (v hostedThreadFixture) issue(t *testing.T, filer, title string) tracker.NativeIssue {
	t.Helper()
	return hubtest.Issue(t, v.service.Repository, v.details, v.projectRecord, hubtest.IssueOptions{Title: title, Body: "Example private report", Actor: tracker.Actor{Kind: "human", PrincipalID: v.users[filer].Credential.ID}, Now: v.service.Now()}).Issue
}

type teamThreadReadGuardKey struct{}
type teamThreadReadGuard struct{ tenantstore.Repository }

func (r teamThreadReadGuard) ReadTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	depth, ok := ctx.Value(teamThreadReadGuardKey{}).(*atomic.Int32)
	if !ok {
		return r.Repository.ReadTenant(ctx, organization, workload, read)
	}
	if depth.Add(1) != 1 {
		depth.Add(-1)
		return errors.New("thread read requested another reader while holding a reader")
	}
	defer depth.Add(-1)
	return r.Repository.ReadTenant(ctx, organization, workload, read)
}

type countingReader struct {
	reader io.Reader
	read   int
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.read += n
	return n, err
}
