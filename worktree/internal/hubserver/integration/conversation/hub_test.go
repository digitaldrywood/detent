package conversation_test

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

	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestMain(m *testing.M) { hubtest.Main(m) }

type hub struct {
	service      *hubserver.Service
	admin        *sql.DB
	repository   *postgres.Repository
	organization tracker.OrganizationID
	token        string
}

func openHub(t *testing.T, cfg hubserver.Config) hub {
	t.Helper()
	admin := hubtest.Database(t)
	h := hub{admin: admin, repository: hubtest.RepositoryOn(t, admin), token: hubapi.NewID("token")}
	return h.reopen(t, cfg)
}

func (h hub) reopen(t *testing.T, cfg hubserver.Config) hub {
	t.Helper()
	connection := hubtest.PostgresConfig(t, h.admin)
	cfg.DatabaseBackend = hubserver.DatabasePostgres
	cfg.Postgres = &connection
	cfg.InitialAdminToken = []byte(h.token)
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.SecretKeys == nil {
		cfg.SecretKeys = secretKeys(t)
	}
	service, err := hubserver.Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	organization, err := h.repository.DesignatedLocalOrganization(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	h.service, h.organization = service, organization
	return h
}

func secretKeys(t *testing.T) *hubsecrets.Keyring {
	t.Helper()
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
	return keys
}

func (h hub) mcpCall(t *testing.T, path, token, name string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(raw)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Mcp-Protocol-Version", "2026-07-28")
	request.Header.Set("Mcp-Method", "tools/call")
	request.Header.Set("Mcp-Name", name)
	response := httptest.NewRecorder()
	h.service.Handler().ServeHTTP(response, request)
	return response
}

func requireStatus(t *testing.T, r *httptest.ResponseRecorder, want int) {
	t.Helper()
	if r.Code != want {
		t.Fatalf("status=%d body=%s want=%d", r.Code, r.Body.String(), want)
	}
}

func decodeResponse(t *testing.T, r *httptest.ResponseRecorder, value any) {
	t.Helper()
	if err := json.Unmarshal(r.Body.Bytes(), value); err != nil {
		t.Fatal(err)
	}
}
