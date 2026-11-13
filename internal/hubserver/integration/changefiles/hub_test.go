package changefiles_test

import (
	"bytes"
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
	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
	"reflect"
)

func TestMain(m *testing.M) { hubtest.Main(m) }

type organization struct {
	ID tracker.OrganizationID `json:"organization_id"`
}

type hub struct {
	service    *hubserver.Service
	admin      *sql.DB
	repository tenantstore.Repository
	details    *hubtest.Details
	token      string
	config     hubserver.Config
}

func openHub(t *testing.T) hub {
	t.Helper()
	return openHubWith(t, func(*hubserver.Config) {})
}

func openHubWith(t *testing.T, configure func(*hubserver.Config)) hub {
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
	h.config = hubserver.Config{DatabaseBackend: hubserver.DatabasePostgres, Postgres: &connection, SecretKeys: keys, RunDetails: h.details, InitialAdminToken: []byte(h.token), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	configure(&h.config)
	h.service, err = hubserver.Open(t.Context(), h.config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.service.Close(); err != nil {
			t.Error(err)
		}
	})
	return h
}

type fixture struct {
	hub
	project tracker.NativeProject
	base    string
	token   string
	runners map[string]hubtest.SeededRunner
}

func (h hub) fixture(t *testing.T, org tracker.OrganizationID) fixture {
	t.Helper()
	project := hubtest.Project(t, h.repository, org, hubtest.ProjectOptions{Name: "Example project " + hubapi.NewID("fixture")})
	credential := hubtest.Credential(t, h.repository, org, hubtest.CredentialOptions{Projects: []tracker.ProjectID{project.ID}})
	hubtest.Policy(t, h.repository, h.details, project, hubtest.PolicyOptions{})
	return fixture{hub: h, project: project, base: "/api/v2/organizations/" + string(org) + "/projects/" + string(project.ID), token: credential.Token, runners: map[string]hubtest.SeededRunner{}}
}

func (f fixture) create(t *testing.T, title string) tracker.NativeIssue {
	t.Helper()
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "create-" + title}, Title: title, Body: strings.Repeat("Full issue content. ", 80) + "\n## Acceptance criteria\nComplete the requested work.\n## Must not break\nExisting behavior.\n## How we know it worked\nRun the project checks."})
	requireNativeStatus(t, response, http.StatusOK)
	var issue tracker.NativeIssue
	decodeHubResponse(t, response, &issue)
	return issue
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

func (h hub) reopen(t *testing.T) *hubserver.Service {
	t.Helper()
	service, err := hubserver.Open(t.Context(), h.config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	return service
}

type changeFixture struct {
	fixture
	issue  tracker.NativeIssue
	change tracker.ChangeRequest
	path   string
	rules  tracker.ChangeReviewPolicy
}

func newChangeFixture(t *testing.T) changeFixture {
	t.Helper()
	return newChangeFixtureOn(t, openHub(t))
}

func (h hub) organization(t *testing.T) tracker.OrganizationID {
	t.Helper()
	response := performHubAPIRequest(t, h.service, http.MethodPost, "/api/v2/organizations", h.token, map[string]string{"name": "Example organization"})
	requireNativeStatus(t, response, http.StatusCreated)
	var org organization
	decodeHubResponse(t, response, &org)
	return org.ID
}

func newChangeFixtureOn(t *testing.T, h hub) changeFixture {
	t.Helper()
	response := performHubAPIRequest(t, h.service, http.MethodPost, "/api/v2/organizations", h.token, map[string]string{"name": "Example organization"})
	requireNativeStatus(t, response, http.StatusCreated)
	var org organization
	decodeHubResponse(t, response, &org)
	if err := h.repository.WriteTenant(t.Context(), org.ID, corestore.Serving, func(w tenantstore.Writer) error {
		return w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org.ID, ID: hubapi.NewID("admin"), Name: "Example administrator", Hash: apikey.HashToken(h.token), Scope: "admin", KeyScope: "admin", ProjectAccess: "all", Kind: "service", CreatedAt: time.Now(), UpdatedAt: time.Now()})
	}); err != nil {
		t.Fatal(err)
	}
	f := h.fixture(t, org.ID)
	credential, err := f.credential(t, f.token)
	if err != nil {
		t.Fatal(err)
	}
	rules := tracker.ChangeReviewPolicy{PolicyID: hubtest.PolicyDescriptor().ID, RequireReview: true, RequiredChecks: []tracker.ChangeCheckSpec{{Name: "test", PrincipalID: credential.ID, WorkflowID: "ci.yml", WorkflowSHA256: policy.Digest([]byte("trusted CI")), Source: "independent", MaxAgeSeconds: 3600}}}
	response = performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/change-review-policy", h.token, tracker.ApproveChangeReviewPolicy{Mutation: tracker.Mutation{IdempotencyKey: "rules"}, Policy: rules})
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &rules)
	issue := f.create(t, "work")
	path := f.base + "/work-items/" + string(issue.WorkItemID) + "/changes"
	response = performHubAPIRequest(t, f.service, http.MethodPost, path, f.token, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "change"}, Title: "Native change", Body: "Proposed work"})
	requireNativeStatus(t, response, http.StatusOK)
	var change tracker.ChangeRequest
	decodeHubResponse(t, response, &change)
	return changeFixture{fixture: f, issue: issue, change: change, path: path + "/" + change.ID, rules: rules}
}

func (f fixture) credential(t *testing.T, token string) (accessstore.Credential, error) {
	t.Helper()
	var credential accessstore.Credential
	err := f.repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		credential, err = r.CredentialByHash(t.Context(), apikey.HashToken(token))
		return err
	})
	return credential, err
}

func (f fixture) worker(t *testing.T, name string) string {
	t.Helper()
	runner := hubtest.Runner(t, f.repository, f.project.OrganizationID, hubtest.RunnerOptions{Projects: []tracker.ProjectID{f.project.ID}, DisplayName: name})
	f.runners[runner.Credential.Token] = runner
	return runner.Credential.Token
}

func (f changeFixture) detail(t *testing.T) tracker.ChangeDetail {
	t.Helper()
	response := performHubAPIRequest(t, f.service, http.MethodGet, f.path, f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var detail tracker.ChangeDetail
	decodeHubResponse(t, response, &detail)
	response = performHubAPIRequest(t, f.service, http.MethodGet, f.path+"?section=versions", f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var current tracker.ChangeDetail
	decodeHubResponse(t, response, &current)
	if !reflect.DeepEqual(current.Summary, detail.Summary) {
		t.Fatalf("current summary = %#v, full summary = %#v, error = %v", current.Summary, detail.Summary, nil)
	}
	return detail
}

func testJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
