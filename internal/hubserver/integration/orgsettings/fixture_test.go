package orgsettings_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestMain(m *testing.M) { hubtest.Main(m) }

const testHubAdminToken = "example-organization-settings-administrator"
const fleetProtocolMeta = `{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/clientInfo":{"name":"fleet-test","version":"1"}}`
const browserHostedOrganizationBase = "/api/v2/organizations/org_browser_preview"

type memoryProjectIcons struct {
	objects  map[string][]byte
	afterPut func(context.Context) error
}

func (m *memoryProjectIcons) PutIcon(ctx context.Context, organization, id string, content []byte) error {
	m.objects[organization+"/"+id] = append([]byte{}, content...)
	if m.afterPut != nil {
		return m.afterPut(ctx)
	}
	return nil
}

func (m *memoryProjectIcons) DeleteIcon(ctx context.Context, organization, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	delete(m.objects, organization+"/"+id)
	return nil
}

type settingsFixture struct {
	service    *hubserver.Service
	admin      *sql.DB
	repository *postgres.Repository
	config     hubserver.Config
	project    tracker.NativeProject
	base       string
	token      string
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

func openHub(t *testing.T, cfg hubserver.Config) (*hubserver.Service, *sql.DB, *postgres.Repository, hubserver.Config) {
	t.Helper()
	admin := hubtest.Database(t)
	connection := hubtest.PostgresConfig(t, admin, hubtest.WithMaxConnections(4))
	cfg.DatabaseBackend, cfg.Postgres = hubserver.DatabasePostgres, &connection
	cfg.InitialAdminToken = []byte(testHubAdminToken)
	cfg.SecretKeys = secretKeys(t)
	cfg.RunDetails = hubtest.NewDetails()
	cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	repository := hubtest.RepositoryOn(t, admin, hubtest.WithMaxConnections(4))
	if cfg.Hosted != nil {
		seedHostedOrganization(t, repository, cfg)
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
	return service, admin, repository, cfg
}

func seedHostedOrganization(t *testing.T, repository *postgres.Repository, cfg hubserver.Config) {
	t.Helper()
	organization := tracker.OrganizationID(cfg.Hosted.OrganizationID)
	now := cfg.Clock().UTC().Truncate(time.Microsecond)
	cursor := bytes.Repeat([]byte{0x6b}, 32)
	envelope, err := cfg.SecretKeys.Seal(cursor, accessstore.SecretAAD(organization, "", accessstore.NativeCursorKeyKind))
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

func newSettingsFixture(t *testing.T, cfg hubserver.Config) settingsFixture {
	t.Helper()
	service, admin, repository, cfg := openHub(t, cfg)
	var organization tracker.OrganizationID
	if cfg.Hosted != nil {
		organization = tracker.OrganizationID(cfg.Hosted.OrganizationID)
	} else {
		var err error
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
	project := hubtest.Project(t, repository, organization, hubtest.ProjectOptions{Name: "Example project", RequireDependencies: true})
	hubtest.Policy(t, repository, cfg.RunDetails, project, hubtest.PolicyOptions{})
	operator := hubtest.Credential(t, repository, organization, hubtest.CredentialOptions{Projects: []tracker.ProjectID{project.ID}})
	return settingsFixture{service: service, admin: admin, repository: repository, config: cfg, project: project, base: "/api/v2/organizations/" + string(organization) + "/projects/" + string(project.ID), token: operator.Token}
}

func (f settingsFixture) otherOrganization(t *testing.T, name string) tracker.OrganizationID {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	organization := hubtest.Organization(t, f.repository, hubtest.OrganizationOptions{Name: name, Now: now}).OrganizationID
	if err := f.repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		return w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: "test_hub_admin", Name: "Example administrator", Hash: apikey.HashToken(testHubAdminToken), Scope: "admin", KeyScope: "admin", ProjectAccess: "all", Kind: "service", CreatedAt: now, UpdatedAt: now})
	}); err != nil {
		t.Fatal(err)
	}
	return organization
}

func newHubAPIRequest(t *testing.T, method, path, token string, payload any) *http.Request {
	t.Helper()
	var body []byte
	if payload != nil {
		var err error
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
	return request
}

func performHubAPIRequest(t *testing.T, service *hubserver.Service, method, path, token string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, newHubAPIRequest(t, method, path, token, payload))
	return response
}

func performHubWorkCall(t *testing.T, service *hubserver.Service, path, token, name string, input any) *httptest.ResponseRecorder {
	t.Helper()
	request := newHubAPIRequest(t, http.MethodPost, path, token, input)
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
		t.Fatalf("status=%d body=%s want=%d", response.Code, response.Body.String(), want)
	}
}

func decodeHubResponse(t *testing.T, response *httptest.ResponseRecorder, output any) {
	t.Helper()
	if err := json.Unmarshal(response.Body.Bytes(), output); err != nil {
		t.Fatal(err)
	}
}

type hostedProvider struct {
	auth.HostedProvider
	mu      sync.Mutex
	members map[string]auth.Membership
}

func (p *hostedProvider) Memberships(_ context.Context, user, organization string) ([]auth.Membership, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	member, ok := p.members[user]
	if !ok || member.OrganizationID != organization {
		return nil, nil
	}
	return []auth.Membership{member}, nil
}

func (p *hostedProvider) CurrentSession(_ context.Context, identity auth.HostedIdentity) (auth.HostedIdentity, error) {
	return identity, nil
}

func (p *hostedProvider) SetMembershipRole(_ context.Context, user, role string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	member := p.members[user]
	member.Role.Slug = role
	p.members[user] = member
	return nil
}

func hostedPlans(collaborationBytes int64) hubserver.HostedPlansConfig {
	return hubserver.HostedPlansConfig{Base: hubserver.PlanReference{ID: "pilot_free", Version: 1}, WindowSeconds: 3600, RetentionWindows: 24, ConnectedSeconds: 90, InvitationSeconds: 86400, Plans: []hubserver.HostedPlan{{PlanReference: hubserver.PlanReference{ID: "pilot_free", Version: 1}, Features: []string{"collaboration", "native_execution", "github_integration"}, Allowances: map[string]int64{"members": 10, "projects": 10, "repositories": 10, "registered_runners": 10, "connected_runners": 10, "concurrent_work": 5, "api_mutations": 1000000, "ingested_events": 1000000, "collaboration_bytes": collaborationBytes, "history_records": 10000}}}}
}

type hostedFixture struct {
	settingsFixture
	provider *hostedProvider
	members  map[string]hubtest.SeededMember
	cookies  map[string]*http.Cookie
}

func newHostedFixture(t *testing.T, cfg hubserver.Config, collaborationBytes int64) *hostedFixture {
	t.Helper()
	provider := &hostedProvider{members: map[string]auth.Membership{}}
	plans := hostedPlans(collaborationBytes)
	cfg.Hosted = &hubserver.HostedConfig{OrganizationID: "org_browser_preview", WorkOSOrganizationID: "provider_org_browser_preview", PublicURL: "https://browser.example.test", Provider: provider, Plans: &plans}
	base := newSettingsFixture(t, cfg)
	f := &hostedFixture{settingsFixture: base, provider: provider, members: map[string]hubtest.SeededMember{}, cookies: map[string]*http.Cookie{}}
	organization := tracker.OrganizationID(base.config.Hosted.OrganizationID)
	for _, account := range []struct {
		name, role string
		write      bool
	}{{"owner", "owner", true}, {"viewer", "viewer", false}} {
		member := hubtest.Member(t, f.repository, organization, hubtest.MemberOptions{Role: account.role, Projects: []tracker.ProjectID{f.project.ID}, ReadOnly: !account.write, ManageRunners: account.write})
		f.members[account.name] = member
		membership := auth.Membership{ID: member.Member.MembershipID, UserID: member.Member.UserID, OrganizationID: base.config.Hosted.WorkOSOrganizationID, Status: "active"}
		membership.Role.Slug = account.role
		provider.members[member.Member.UserID] = membership
		now := time.Now().UTC().Truncate(time.Microsecond)
		token := hubapi.NewID("session")
		if err := f.repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			return w.CreateAccessSession(t.Context(), accessstore.Session{OrganizationID: organization, Hash: apikey.HashToken(token), Email: member.Member.Email, Subject: member.Member.UserID, ProviderOrganizationID: base.config.Hosted.WorkOSOrganizationID, SessionID: hubapi.NewID("provider_session"), IdentityCreatedAt: now, IdentityExpiresAt: now.Add(time.Hour), CreatedAt: now, ExpiresAt: now.Add(time.Hour)})
		}); err != nil {
			t.Fatal(err)
		}
		f.cookies[account.name] = &http.Cookie{Name: "detent_hosted_session", Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode}
	}
	return f
}

func (f *hostedFixture) api(t *testing.T, account, method, path string, value any, want int) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	f.service.Handler().ServeHTTP(response, f.request(t, account, method, path, value))
	requireNativeStatus(t, response, want)
	return response
}

func (f *hostedFixture) request(t *testing.T, account, method, path string, value any) *http.Request {
	t.Helper()
	var body string
	if value != nil {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		body = string(raw)
	}
	request := httptest.NewRequest(method, f.config.Hosted.PublicURL+path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", f.config.Hosted.PublicURL)
	if cookie := f.cookies[account]; cookie != nil {
		request.AddCookie(cookie)
		digest := sha256.Sum256([]byte("detent-hosted-csrf:" + cookie.Value))
		request.Header.Set("X-CSRF-Token", hex.EncodeToString(digest[:]))
	}
	return request
}
