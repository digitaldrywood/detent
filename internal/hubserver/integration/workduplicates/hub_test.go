package workduplicates_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/workduplicates"
	"github.com/digitaldrywood/detent/internal/hubserver/workitemread"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestMain(m *testing.M) { hubtest.Main(m) }

const issueContractTestSections = "\n## Acceptance criteria\nComplete the requested work.\n## Must not break\nExisting behavior.\n## How we know it worked\nRun the project checks."

type organization struct {
	ID tracker.OrganizationID `json:"organization_id"`
}

type archiveIssueRequest struct {
	tracker.Mutation
	ExpectedRevision tracker.Revision `json:"expected_revision,string"`
}

type duplicateChallenge struct {
	PossibleDuplicates []hubscope.PossibleDuplicate `json:"possible_duplicates"`
	ChallengeToken     string                       `json:"challenge_token"`
}

type hub struct {
	service    *hubserver.Service
	admin      *sql.DB
	repository tenantstore.Repository
	details    *hubtest.Details
	token      string
	first      tracker.OrganizationID
	second     tracker.OrganizationID
}

type nativeFixture struct {
	hub
	project tracker.NativeProject
	base    string
	token   string
}

func openHub(t *testing.T) hub {
	t.Helper()
	admin := hubtest.Database(t)
	h := hub{admin: admin, repository: hubtest.RepositoryOn(t, admin), details: hubtest.NewDetails(), token: hubapi.NewID("token")}
	connection := hubtest.PostgresConfig(t, admin)
	keys, err := hubsecrets.FromEnvironment(func(name string) string {
		switch name {
		case "DETENT_HUB_SECRET_KEYS":
			return `{"1":"` + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x6b}, 32)) + `"}`
		case "DETENT_HUB_SECRET_KEY_VERSION":
			return "1"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	h.service, err = hubserver.Open(t.Context(), hubserver.Config{DatabaseBackend: hubserver.DatabasePostgres, Postgres: &connection, SecretKeys: keys, RunDetails: h.details, InitialAdminToken: []byte(h.token), WebhookMaintenanceInterval: 24 * time.Hour, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.service.Close(); err != nil {
			t.Error(err)
		}
	})
	h.first = h.organization(t, "Example organization")
	h.second = h.organization(t, "Second example organization")
	return h
}

func (h hub) organization(t *testing.T, name string) tracker.OrganizationID {
	t.Helper()
	response := performHubAPIRequest(t, h.service, http.MethodPost, "/api/v2/organizations", h.token, map[string]string{"name": name})
	requireNativeStatus(t, response, http.StatusCreated)
	var org organization
	decodeHubResponse(t, response, &org)
	if err := h.repository.WriteTenant(t.Context(), org.ID, corestore.Serving, func(w tenantstore.Writer) error {
		return w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org.ID, ID: hubapi.NewID("admin"), Name: "Example administrator", Hash: apikey.HashToken(h.token), Scope: "admin", KeyScope: "admin", ProjectAccess: "all", Kind: "service", CreatedAt: time.Now(), UpdatedAt: time.Now()})
	}); err != nil {
		t.Fatal(err)
	}
	return org.ID
}

func newDefaultNativeFixture(t *testing.T) nativeFixture {
	t.Helper()
	h := openHub(t)
	return h.fixture(t, h.first, "Example project")
}

func (h hub) fixture(t *testing.T, org tracker.OrganizationID, name string) nativeFixture {
	t.Helper()
	project := hubtest.Project(t, h.repository, org, hubtest.ProjectOptions{Name: name + " " + hubapi.NewID("fixture"), RequireDependencies: true})
	credential := hubtest.Credential(t, h.repository, org, hubtest.CredentialOptions{Projects: []tracker.ProjectID{project.ID}})
	hubtest.Policy(t, h.repository, h.details, project, hubtest.PolicyOptions{})
	return nativeFixture{hub: h, project: project, base: "/api/v2/organizations/" + string(org) + "/projects/" + string(project.ID), token: credential.Token}
}

func (f nativeFixture) create(t *testing.T, name string) tracker.NativeIssue {
	t.Helper()
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "create-" + name}, Title: name, Body: strings.Repeat("Full issue content. ", 80) + issueContractTestSections, State: "Todo"})
	requireNativeStatus(t, response, http.StatusOK)
	var issue tracker.NativeIssue
	decodeHubResponse(t, response, &issue)
	return issue
}

func (f nativeFixture) createIndependent(t *testing.T, name string) tracker.NativeIssue {
	t.Helper()
	request := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "create-" + name}, Title: name, Body: strings.Repeat("Full issue content. ", 80) + issueContractTestSections, State: "Todo"}
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, request)
	if response.Code == http.StatusConflict {
		var failure hubapi.APIErrorResponse
		decodeHubResponse(t, response, &failure)
		var challenge duplicateChallenge
		decodeHubResponse(t, response, &challenge)
		if failure.Code != "possible_duplicates" || challenge.ChallengeToken == "" || len(challenge.PossibleDuplicates) == 0 {
			t.Fatalf("independent fixture challenge: %s", response.Body.String())
		}
		request.ChallengeToken = challenge.ChallengeToken
		for _, match := range challenge.PossibleDuplicates {
			request.NotDuplicateOf = append(request.NotDuplicateOf, match.Number)
		}
		response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, request)
	}
	requireNativeStatus(t, response, http.StatusOK)
	var issue tracker.NativeIssue
	decodeHubResponse(t, response, &issue)
	return issue
}

func (h hub) authenticator() *hubauth.Authenticator {
	return hubauth.New(hubauth.Deps{Repository: h.repository, AccessRepository: h.repository, Organization: h.first, Now: time.Now, Classify: hubscope.OperatorAuthorizationError})
}

func (h hub) authenticateTenantAPIHash(t *testing.T, org tracker.OrganizationID, token string) (hubapi.APICredential, int, error) {
	t.Helper()
	return hubscope.NativeCredentialResult(h.authenticator().AuthenticateRepositoryAPIHash(t.Context(), h.repository, org, apikey.HashToken(token), "", ""))
}

func authInput(scope hubscope.NativeScope) hubauth.Input {
	return hubauth.Input{Organization: scope.Organization, Project: scope.Project, Credential: hubscope.AuthCredential(scope.Credential), Admin: scope.RequireHostedAdmin, Action: scope.Action, IntegrationRepository: scope.IntegrationRepository, IntegrationOwned: scope.IntegrationOwned}
}

func (h hub) duplicateDeps() workduplicates.Deps {
	authenticator := h.authenticator()
	authorize := func(ctx context.Context, r accessstore.Reader, scope hubscope.NativeScope, write bool) error {
		return hubapi.FromFeatureError(authenticator.Authorize(ctx, r, authInput(scope), write))
	}
	reader := workitemread.New(workitemread.Deps{Tenants: h.repository, Access: h.repository, Details: h.details, Now: time.Now, Organization: h.first, Authority: func(scope hubscope.NativeScope, write bool) accessstore.Authority {
		return hubauth.Authority(authInput(scope), write)
	}, Authorize: authorize})
	return workduplicates.Deps{Tenants: h.repository, Details: h.details, Now: time.Now, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Read: reader.Read, AuthorizeRecord: func(ctx context.Context, r accessstore.Reader, scope hubscope.NativeScope) error {
		if err := authorize(ctx, r, scope, true); err != nil {
			return err
		}
		if scope.Credential.Hosted == nil {
			return nil
		}
		member, err := r.AuthorizeAccess(ctx, hubauth.Authority(authInput(scope), true), time.Now())
		if err != nil {
			return err
		}
		if member.UserID != "" && hubapi.LesserHostedRole(member.Role, scope.Credential.HostedRole) != scope.Credential.HostedRole {
			return auth.ErrHostedIdentity
		}
		return nil
	}}
}

func refreshNativeDuplicateSearchFixture(t *testing.T, h hub, organization tracker.OrganizationID) {
	t.Helper()
	for range 20000 {
		more, err := searchstore.NewContent(h.details).RefreshOne(t.Context(), tenantstore.SearchRepository{Repository: h.repository}, organization)
		if err != nil {
			t.Fatal(err)
		}
		if !more {
			return
		}
	}
	t.Fatal("search fixture did not finish its bounded source population")
}

func performHubAPIRequest(t *testing.T, service *hubserver.Service, method, target, token string, value any) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	var err error
	if value != nil {
		raw, err = json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequest(method, target, bytes.NewReader(raw)).WithContext(t.Context())
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	return response
}

func requireNativeStatus(t *testing.T, response *httptest.ResponseRecorder, want int) {
	t.Helper()
	if response.Code != want {
		t.Fatalf("status = %d, want %d: %s", response.Code, want, response.Body.String())
	}
}

func decodeHubResponse(t *testing.T, response *httptest.ResponseRecorder, value any) {
	t.Helper()
	if err := json.Unmarshal(response.Body.Bytes(), value); err != nil {
		t.Fatal(err)
	}
}
