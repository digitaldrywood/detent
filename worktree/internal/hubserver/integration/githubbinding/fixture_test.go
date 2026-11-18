package githubbinding_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/githubbinding"
	"github.com/digitaldrywood/detent/internal/hubserver/githubcontract"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestMain(m *testing.M) { hubtest.Main(m) }

const testHubAdminToken = "example-repository-binding-administrator"
const fleetProtocolMeta = `{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/clientInfo":{"name":"fleet-test","version":"1"}}`

type ProjectIntegration = hubserver.ProjectIntegration
type bindNativeRepositoryRequest = githubbinding.Request
type ReconcileRequest = githubcontract.ReconcileRequest
type ReconcileSnapshot = githubcontract.ReconcileSnapshot
type RepositorySource = githubcontract.RepositorySource

type bindingFixture struct {
	service      *hubserver.Service
	repository   *postgres.Repository
	organization tracker.OrganizationID
	config       hubserver.Config
}

func newBindingFixture(t *testing.T, cfg hubserver.Config) bindingFixture {
	t.Helper()
	admin := hubtest.Database(t)
	config := hubtest.PostgresConfig(t, admin, hubtest.WithMaxConnections(4))
	cfg.DatabaseBackend, cfg.Postgres = hubserver.DatabasePostgres, &config
	cfg.InitialAdminToken = []byte(testHubAdminToken)
	cfg.RunDetails = hubtest.NewDetails()
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
	cfg.SecretKeys = keys
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.Hosted != nil {
		repository := hubtest.RepositoryOn(t, admin, hubtest.WithMaxConnections(4))
		organization := tracker.OrganizationID(cfg.Hosted.OrganizationID)
		now := cfg.Clock().UTC().Truncate(time.Microsecond)
		cursor := bytes.Repeat([]byte{0x6b}, 32)
		envelope, err := keys.Seal(cursor, accessstore.SecretAAD(organization, "", accessstore.NativeCursorKeyKind))
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: "Example organization", Slug: "local-organization", CreatedAt: now}); err != nil {
				return err
			}
			if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: organization, ProviderID: cfg.Hosted.WorkOSOrganizationID, PublicURL: cfg.Hosted.PublicURL, Deployment: "origin"}); err != nil {
				return err
			}
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: "test_hub_admin", Name: "Example administrator", Hash: apikey.HashToken(testHubAdminToken), Scope: "admin", KeyScope: "admin", ProjectAccess: "all", Kind: "service", CreatedAt: now, UpdatedAt: now}); err != nil {
				return err
			}
			if err := w.CreateHubIdentity(t.Context(), accessstore.HubIdentity{OrganizationID: organization, ID: hubapi.NewID("hub"), CursorKey: cursor}); err != nil {
				return err
			}
			return w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: organization, Kind: accessstore.NativeCursorKeyKind, Envelope: envelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: organization, Kind: accessstore.NativeCursorKeyKind, Actor: "fixture", Event: "create", KeyVersion: envelope.Version, RecordedAt: now})
		}); err != nil {
			t.Fatal(err)
		}
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
	repository := hubtest.RepositoryOn(t, admin, hubtest.WithMaxConnections(4))
	organization := tracker.OrganizationID("")
	if cfg.Hosted != nil {
		organization = tracker.OrganizationID(cfg.Hosted.OrganizationID)
	} else {
		organization, err = repository.DesignatedLocalOrganization(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			return w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: organization, ProviderID: "provider_" + string(organization)})
		}); err != nil {
			t.Fatal(err)
		}
	}
	return bindingFixture{service: service, repository: repository, organization: organization, config: cfg}
}

type nativeFixture struct {
	service *hubserver.Service
	project tracker.NativeProject
	base    string
	token   string
}

func (f bindingFixture) project(t *testing.T, name string) nativeFixture {
	t.Helper()
	project := hubtest.Project(t, f.repository, f.organization, hubtest.ProjectOptions{Name: name, RequireDependencies: true})
	hubtest.Policy(t, f.repository, f.config.RunDetails, project, hubtest.PolicyOptions{})
	return nativeFixture{service: f.service, project: project, base: "/api/v2/organizations/" + string(f.organization) + "/projects/" + string(project.ID), token: testHubAdminToken}
}

func performHubAPIRequest(t *testing.T, service *hubserver.Service, method, path, token string, input any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(raw))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	return response
}

func performHubWorkCall(t *testing.T, service *hubserver.Service, path, token, name string, input any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Mcp-Protocol-Version", "2026-07-28")
	request.Header.Set("Mcp-Method", "tools/call")
	request.Header.Set("Mcp-Name", name)
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
func decodeHubResponse(t *testing.T, response *httptest.ResponseRecorder, output any) {
	t.Helper()
	if err := json.Unmarshal(response.Body.Bytes(), output); err != nil {
		t.Fatal(err)
	}
}
func projectCall[T any](t *testing.T, name, project, key string, input T) operatortool.Call {
	t.Helper()
	raw, err := json.Marshal(operatortool.ProjectRequest[T]{ProjectID: project, RequestID: key, Input: input})
	if err != nil {
		t.Fatal(err)
	}
	return operatortool.Call{Name: name, Arguments: raw}
}

type reconcileStep struct {
	snapshot ReconcileSnapshot
	err      error
}
type scriptedReconcileBackend struct {
	mu       sync.Mutex
	steps    []reconcileStep
	requests []ReconcileRequest
}

func (b *scriptedReconcileBackend) Reconcile(_ context.Context, request ReconcileRequest) (ReconcileSnapshot, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.requests = append(b.requests, request)
	if len(b.steps) == 0 {
		return ReconcileSnapshot{}, nil
	}
	step := b.steps[0]
	b.steps = b.steps[1:]
	return step.snapshot, step.err
}
func (b *scriptedReconcileBackend) Requests() []ReconcileRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]ReconcileRequest(nil), b.requests...)
}
