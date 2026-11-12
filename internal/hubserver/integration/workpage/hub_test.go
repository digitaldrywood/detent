package workpage_test

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

	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
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
	h.service, err = hubserver.Open(t.Context(), hubserver.Config{DatabaseBackend: hubserver.DatabasePostgres, Postgres: &connection, SecretKeys: keys, RunDetails: h.details, InitialAdminToken: []byte(h.token), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
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
}

func (h hub) fixture(t *testing.T, org tracker.OrganizationID) fixture {
	t.Helper()
	project := hubtest.Project(t, h.repository, org, hubtest.ProjectOptions{Name: "Example project"})
	credential := hubtest.Credential(t, h.repository, org, hubtest.CredentialOptions{Projects: []tracker.ProjectID{project.ID}})
	hubtest.Policy(t, h.repository, h.details, project, hubtest.PolicyOptions{})
	return fixture{hub: h, project: project, base: "/api/v2/organizations/" + string(org) + "/projects/" + string(project.ID), token: credential.Token}
}

func (f fixture) create(t *testing.T, title string) tracker.NativeIssue {
	t.Helper()
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "create-" + title}, Title: title, Body: strings.Repeat("Full issue content. ", 80) + "\n## Acceptance criteria\nComplete the requested work.\n## Must not break\nExisting behavior.\n## How we know it worked\nRun the project checks."})
	requireNativeStatus(t, response, http.StatusOK)
	var issue tracker.NativeIssue
	decodeHubResponse(t, response, &issue)
	return issue
}

func (h hub) refreshBoard(t *testing.T, org tracker.OrganizationID) {
	t.Helper()
	var after tracker.NativeWorkItemID
	var attempted []tracker.NativeWorkItemID
	for {
		ctx := (hubmaintenance.Budget{Items: 1, Bytes: hubserver.BoardMaintenanceBytes}).Context(t.Context(), hubmaintenance.Tenant{Organization: string(org), Generation: 1})
		next, processed, err := h.service.MaintainBoardRecord(ctx, org, after, attempted...)
		if err != nil {
			t.Fatal(err)
		}
		if !processed {
			return
		}
		after = next
		attempted = append(attempted, next)
	}
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
