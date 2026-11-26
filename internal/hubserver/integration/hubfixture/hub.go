package hubfixture

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/internal/hubdeps"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const (
	AdminToken            = "detent_test_hub_admin_token_00000000000000000000000000000000"
	AdminCredentialID     = "test_hub_admin"
	Primary               = tracker.OrganizationID("org_first")
	Secondary             = tracker.OrganizationID("org_first_second")
	IssueContractSections = "\n## Acceptance criteria\nComplete the requested work.\n## Must not break\nExisting behavior.\n## How we know it worked\nRun the project checks."
)

type Config = hubserver.Config

type Hub struct {
	Service    *hubserver.Service
	Config     hubserver.Config
	Admin      *sql.DB
	Repository *postgres.Repository
	Primary    tracker.OrganizationID
	owner      *testing.T
}

func (h *Hub) Handler() http.Handler { return h.Service.Handler() }

func (h *Hub) Now() time.Time { return h.Config.Clock() }

func (h *Hub) AdminToken() string { return string(h.Config.InitialAdminToken) }

func (h *Hub) Request(t *testing.T, method, path, token string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	return Request(t, h.Handler(), method, path, token, payload)
}

func (h *Hub) WorkCall(t *testing.T, path, token, name string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return WorkCall(t, h.Handler(), path, token, name, body)
}

func SecretKeys(t *testing.T) *hubsecrets.Keyring {
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

func DiscardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func defaults(t *testing.T, cfg hubserver.Config) hubserver.Config {
	t.Helper()
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.SecretKeys == nil {
		cfg.SecretKeys = SecretKeys(t)
	}
	if cfg.RunDetails == nil {
		cfg.RunDetails = hubtest.NewDetails()
	}
	if cfg.Logger == nil {
		cfg.Logger = DiscardLogger()
	}
	if len(cfg.InitialAdminToken) == 0 {
		cfg.InitialAdminToken = []byte(AdminToken)
	}
	return cfg
}

func Open(t *testing.T, cfg hubserver.Config) *Hub {
	t.Helper()
	return OpenPool(t, cfg, nil)
}

func OpenPool(t *testing.T, cfg hubserver.Config, configure func(*pgxpool.Config)) *Hub {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping Postgres hubserver integration test in -short mode")
	}
	if cfg.Hosted != nil {
		t.Fatal("hubfixture.Open opens a local Hub; use OpenHosted")
	}
	cfg = defaults(t, cfg)
	admin := hubtest.Database(t)
	options := []hubtest.Option{hubtest.WithClock(cfg.Clock)}
	if configure != nil {
		options = append(options, hubtest.WithPoolConfig(configure))
	}
	repository := hubtest.RepositoryOn(t, admin, options...)
	for _, organization := range []tracker.OrganizationID{Secondary, Primary} {
		SeedOrganization(t, repository, organization, string(cfg.InitialAdminToken), cfg.Clock())
	}
	if _, err := admin.ExecContext(t.Context(), "UPDATE organizations SET local=true WHERE organization_id=$1", Primary); err != nil {
		t.Fatal(err)
	}
	return open(t, &Hub{Admin: admin, Primary: Primary, owner: t}, cfg, repository)
}

func OpenDeps(t *testing.T, cfg Config, deps hubdeps.Deps) *Hub {
	t.Helper()
	return Open(t, hubserver.WithDeps(cfg, deps))
}

func OpenWith(t *testing.T, configure func(*Config)) *Hub {
	t.Helper()
	var cfg Config
	configure(&cfg)
	return Open(t, cfg)
}

func SeedOrganization(t *testing.T, repository tenantstore.Repository, organization tracker.OrganizationID, adminToken string, now time.Time) {
	t.Helper()
	now = now.UTC().Truncate(time.Microsecond)
	hubtest.Organization(t, repository, hubtest.OrganizationOptions{ID: organization, Name: string(organization), Slug: strings.ReplaceAll(string(organization), "_", "-"), Now: now})
	SeedAdmin(t, repository, organization, adminToken, now)
}

func SeedAdmin(t *testing.T, repository tenantstore.Repository, organization tracker.OrganizationID, adminToken string, now time.Time) {
	t.Helper()
	now = now.UTC().Truncate(time.Microsecond)
	if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: AdminCredentialID, Hash: apikey.HashToken(adminToken), Name: "Hub test administrator", Scope: "admin", KeyScope: "admin", ProjectAccess: "all", Kind: "service", CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		return w.CreateHubIdentity(t.Context(), accessstore.HubIdentity{OrganizationID: organization, ID: hubapi.NewID("hub"), CursorKey: bytes.Repeat([]byte{0x5a}, 32)})
	}); err != nil {
		t.Fatal(err)
	}
}

func open(t *testing.T, h *Hub, cfg hubserver.Config, repository *postgres.Repository) *Hub {
	t.Helper()
	connection := hubtest.PostgresConfig(h.owner, h.Admin, hubtest.WithClock(cfg.Clock))
	cfg.Postgres = &connection
	cfg.DatabaseBackend = hubserver.DatabasePostgres
	cfg.TenantRepository = repository
	cfg.AccessRepository = repository
	cfg.BillingRepository = repository
	service, err := hubserver.Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	h.Service, h.Config, h.Repository = service, cfg, repository
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	return h
}

func (h *Hub) Reopen(t *testing.T) *Hub {
	t.Helper()
	return open(t, &Hub{Admin: h.Admin, Primary: h.Primary, owner: h.owner}, h.Config, hubtest.RepositoryOn(h.owner, h.Admin, hubtest.WithClock(h.Config.Clock)))
}

func (h *Hub) EnsureOrganization(t *testing.T, organization tracker.OrganizationID) {
	t.Helper()
	err := h.Repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
		_, err := r.AccessOrganization(t.Context())
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		SeedOrganization(t, h.Repository, organization, string(h.Config.InitialAdminToken), h.Now())
		return
	}
	if err != nil {
		t.Fatal(err)
	}
}

func (h *Hub) CreateOrganization(t *testing.T, name string) tracker.OrganizationID {
	t.Helper()
	response := h.Request(t, http.MethodPost, "/api/v2/organizations", string(h.Config.InitialAdminToken), map[string]string{"name": name})
	RequireStatus(t, response, http.StatusCreated)
	var created struct {
		ID tracker.OrganizationID `json:"organization_id"`
	}
	Decode(t, response, &created)
	if err := h.Repository.WriteTenant(t.Context(), created.ID, corestore.Serving, func(w tenantstore.Writer) error {
		now := h.Now().UTC().Truncate(time.Microsecond)
		return w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: created.ID, ID: hubapi.NewID("admin"), Name: "Example administrator", Hash: apikey.HashToken(string(h.Config.InitialAdminToken)), Scope: "admin", KeyScope: "admin", ProjectAccess: "all", Kind: "service", CreatedAt: now, UpdatedAt: now})
	}); err != nil {
		t.Fatal(err)
	}
	return created.ID
}

func (h *Hub) Credential(t *testing.T, organization tracker.OrganizationID, token string) (accessstore.Credential, error) {
	t.Helper()
	var credential accessstore.Credential
	err := h.Repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		credential, err = r.CredentialByHash(t.Context(), apikey.HashToken(token))
		return err
	})
	return credential, err
}
