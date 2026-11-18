package labels_test

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const testHubAdminToken = "example-label-admin"
const workItemRecordBody = "## Acceptance criteria\nThe issue is served.\n## Must not break\nOther tenants.\n## How we know it worked\nRead the issue."

type nativeOrganization struct {
	ID         tracker.OrganizationID
	Name, Slug string
}

func TestMain(m *testing.M) { hubtest.Main(m) }

func openHub(t *testing.T, admin *sql.DB, repository *postgres.Repository) *hubserver.Service {
	t.Helper()
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
	service, err := hubserver.Open(t.Context(), hubserver.Config{
		DatabaseBackend: hubserver.DatabasePostgres, Postgres: &connection,
		TenantRepository: repository, AccessRepository: repository, BillingRepository: repository,
		InitialAdminToken: []byte(testHubAdminToken), SecretKeys: keys,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), RunDetails: hubtest.NewDetails(),
	})
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

func performHubAPIRequest(t *testing.T, service *hubserver.Service, method, path, token string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	var body []byte
	var err error
	if payload != nil {
		body, err = json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	return response
}

func requireNativeStatus(t *testing.T, response *httptest.ResponseRecorder, expected int) {
	t.Helper()
	if response.Code != expected {
		t.Fatalf("status=%d body=%s want=%d", response.Code, response.Body.String(), expected)
	}
}

func decodeHubResponse(t *testing.T, response *httptest.ResponseRecorder, value any) {
	t.Helper()
	if err := json.Unmarshal(response.Body.Bytes(), value); err != nil {
		t.Fatal(err)
	}
}

func itemPathFor(organization tracker.OrganizationID, project tracker.ProjectID, item tracker.NativeWorkItemID) string {
	return "/api/v2/organizations/" + string(organization) + "/projects/" + string(project) + "/work-items/" + string(item)
}

func validateQuery(params url.Values, fields ...string) error {
	fields = append(fields, "archive_url")
	if value, present := params["archive_url"]; present && (len(value) != 1 || value[0] != "true" && value[0] != "false") {
		return hubapi.PublicInvalid("archive_url supports true or false")
	}
	allowed := map[string]bool{"limit": true, "cursor": true}
	for _, field := range fields {
		allowed[field] = true
	}
	for key, values := range params {
		if !allowed[key] || len(values) != 1 || len(values[0]) > 4096 {
			return hubapi.PublicInvalid("Query contains an unsupported field or value")
		}
	}
	return nil
}
