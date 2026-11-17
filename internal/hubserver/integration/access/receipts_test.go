package access_test

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
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/accessreceipts"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const testHubAdminToken = "example-access-admin"

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

func TestPostgresHostedOperationReceiptsExpiredHTTPResponse(t *testing.T) {
	admin := hubtest.Database(t)
	repository := hubtest.RepositoryOn(t, admin)
	service := openHub(t, admin, repository)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		hubtest.Organization(t, repository, hubtest.OrganizationOptions{ID: org})
		credential := hubtest.Credential(t, repository, org, hubtest.CredentialOptions{Scope: "admin"})
		project := hubtest.Project(t, repository, org, hubtest.ProjectOptions{})
		details := hubtest.NewDetails()
		hubtest.Policy(t, repository, details, project, hubtest.PolicyOptions{})
		path := "/api/v2/organizations/" + string(org) + "/projects/" + string(project.ID) + "/work-items"
		input := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "expired-command"}, Title: "Example issue", State: "Todo", Body: "## Acceptance criteria\nServe the issue.\n## Must not break\nTenant isolation.\n## How we know it worked\nRead the issue."}
		receipts := accessreceipts.New(accessreceipts.Deps{Repository: repository, TenantRepository: repository, Organization: org, Now: time.Now, Details: details, ResolveJSON: func(_ context.Context, raw []byte) ([]byte, error) { return raw, nil }})
		command := accessreceipts.Command{Organization: org, Actor: credential.ID, Operation: "POST " + path, Key: input.IdempotencyKey, Input: input}
		if claimed, _, err := receipts.Claim(t.Context(), command); err != nil || !claimed {
			t.Fatalf("claim: %t %v", claimed, err)
		}
		if _, err := receipts.Complete(t.Context(), command, map[string]bool{"ok": true}); err != nil {
			t.Fatal(err)
		}
		if err := receipts.Retain(t.Context(), time.Now().Add(48*time.Hour)); err != nil {
			t.Fatal(err)
		}
		response := performHubAPIRequest(t, service, http.MethodPost, path, credential.Token, input)
		if response.Code != http.StatusGone {
			t.Fatalf("expired replay HTTP status=%d error=%v: %s", response.Code, nil, response.Body)
		}
	}
}
