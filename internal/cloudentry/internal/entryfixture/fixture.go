package entryfixture

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/cloudentry"
	"github.com/digitaldrywood/detent/internal/cloudentry/internal/entrytest"
	"github.com/digitaldrywood/detent/internal/cloudentry/internal/harness"
	"github.com/digitaldrywood/detent/internal/cloudentry/platformstore"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/testenv"
)

var admins sync.Map

type Fixture struct {
	Service  *cloudentry.Service
	Provider *harness.FakeProvider
	Tenants  map[string]http.Handler
	Fixtures map[string]harness.Tenant
	Seams    *Seams
	DB       *sql.DB
}

type Seams struct {
	mu            sync.Mutex
	now           func() time.Time
	transport     func(cloudentry.Organization) (http.RoundTripper, error)
	generateToken func() (string, error)
}

func (s *Seams) Now() time.Time {
	s.mu.Lock()
	now := s.now
	s.mu.Unlock()
	return now()
}

func (s *Seams) NowFunc() func() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.now
}

func (s *Seams) SetNow(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = now
}

func (s *Seams) Transport(organization cloudentry.Organization) (http.RoundTripper, error) {
	s.mu.Lock()
	transport := s.transport
	s.mu.Unlock()
	return transport(organization)
}

func (s *Seams) TransportFunc() func(cloudentry.Organization) (http.RoundTripper, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.transport
}

func (s *Seams) SetTransport(transport func(cloudentry.Organization) (http.RoundTripper, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.transport = transport
}

func (s *Seams) GenerateToken() (string, error) {
	s.mu.Lock()
	generate := s.generateToken
	s.mu.Unlock()
	return generate()
}

func (s *Seams) GenerateTokenFunc() func() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.generateToken
}

func (s *Seams) SetGenerateToken(generate func() (string, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.generateToken = generate
}

func New(t *testing.T, configure ...func(*cloudentry.Config)) Fixture {
	t.Helper()
	return NewWithConfig(t, slog.New(slog.DiscardHandler), configure)
}

func NewWithLogger(t *testing.T, logger *slog.Logger, configure ...func(*hubserver.Config)) Fixture {
	t.Helper()
	return NewWithConfig(t, logger, nil, configure...)
}

func NewWithConfig(t *testing.T, logger *slog.Logger, configureEntry []func(*cloudentry.Config), configure ...func(*hubserver.Config)) Fixture {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping Postgres cloudentry integration test in -short mode")
	}
	seed := make([]byte, ed25519.SeedSize)
	seed[0] = 42
	key := ed25519.NewKeyFromSeed(seed)
	provider := harness.NewFakeProvider()
	provider.Users["user_alice"] = "alice@example.test"
	provider.Users["user_bob"] = "bob@example.test"
	provider.Users["user_carol"] = "carol@example.test"
	provider.Users["user_support"] = "support@example.test"
	provider.Member("user_alice", "porg_alpha", "owner")
	provider.Member("user_alice", "porg_beta", "owner")
	provider.Member("user_bob", "porg_beta", "member")
	database := harness.SharedPostgresDatabase(t)
	alpha, alphaHandler := harness.NewTenantIn(t, database, provider, key, "org_alpha", "porg_alpha", "user_alice", "Alpha secret project", configure...)
	beta, betaHandler := harness.NewTenantIn(t, database, provider, key, "org_beta", "porg_beta", "user_alice", "Beta secret project", configure...)
	tenants := map[string]http.Handler{harness.SocketEndpoint("alpha.sock"): alphaHandler, harness.SocketEndpoint("beta.sock"): betaHandler}
	cfg := cloudentry.Config{
		Platform:  cloudentry.PlatformConfig{BootstrapAdminEmail: "bootstrap@example.test"},
		PublicURL: harness.PublicURL, ListenAddress: "127.0.0.1:0", Issuer: "entry", SigningKey: key, Provider: provider, StaffEmails: []string{"staff@example.test", "support@example.test"}, SupportActors: []string{"support@example.test"}, StateDir: t.TempDir(),
		LookupEnv: func(string) string { return "" }, Logger: logger, ClientFS: fstest.MapFS{},
		Transport: func(organization cloudentry.Organization) (http.RoundTripper, error) {
			return entrytest.HandlerTransport{Handler: tenants[organization.Endpoint]}, nil
		},
	}
	for _, configure := range configureEntry {
		configure(&cfg)
	}
	var admin *sql.DB
	if cfg.Storage.Backend == "" {
		admin = database
		PostgresConfig(t, admin)(&cfg)
	} else if stored, ok := admins.Load(cfg.Storage.PostgresPool); ok && cfg.Storage.PostgresPool != nil {
		if db, ok := stored.(*sql.DB); ok {
			admin = db
		}
	} else if stored, ok := admins.Load(cfg.Storage.PostgresDSN); ok {
		if db, ok := stored.(*sql.DB); ok {
			admin = db
		}
	}
	seams := &Seams{now: cfg.Now, transport: cfg.Transport, generateToken: cfg.GenerateToken}
	if seams.now == nil {
		seams.now = time.Now
	}
	if seams.generateToken == nil {
		seams.generateToken = apikey.GenerateToken
	}
	cfg.Now, cfg.Transport, cfg.GenerateToken = seams.Now, seams.Transport, seams.GenerateToken
	service, err := cloudentry.Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Log(err)
		}
	})
	for _, tenant := range []struct {
		fixture  harness.Tenant
		endpoint string
		name     string
	}{{alpha, harness.SocketEndpoint("alpha.sock"), "Alpha"}, {beta, harness.SocketEndpoint("beta.sock"), "Beta"}} {
		if _, err := service.Registry().Register(t.Context(), cloudentry.Organization{ID: tenant.fixture.ID, ProviderID: tenant.fixture.Provider, Name: tenant.name, Endpoint: tenant.endpoint, Generation: 1}); err != nil {
			t.Fatal(err)
		}
	}
	return Fixture{Service: service, Provider: provider, Tenants: tenants, Fixtures: map[string]harness.Tenant{"org_alpha": alpha, "org_beta": beta}, Seams: seams, DB: admin}
}

func PostgresConfig(t *testing.T, databases ...*sql.DB) func(*cloudentry.Config) {
	t.Helper()
	return PostgresStorageConfig(t, false, databases...)
}

func PostgresRestartConfig(t *testing.T, databases ...*sql.DB) func(*cloudentry.Config) {
	t.Helper()
	return PostgresStorageConfig(t, true, databases...)
}

func PostgresStorageConfig(t *testing.T, restart bool, databases ...*sql.DB) func(*cloudentry.Config) {
	t.Helper()
	var admin *sql.DB
	if len(databases) == 0 {
		admin = harness.PostgresDatabase(t)
	} else {
		admin = databases[0]
	}
	name := "example_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	password := uuid.NewString()
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err := admin.ExecContext(t.Context(), "CREATE ROLE "+quoted+" LOGIN PASSWORD "+pgtransport.QuoteLiteral(password)+" IN ROLE "+postgres.PlatformRoleName); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		deadline, ok := t.Deadline()
		if !ok {
			t.Error("missing process deadline")
			return
		}
		ctx, cancel := context.WithDeadline(context.WithoutCancel(t.Context()), deadline)
		defer cancel()
		if _, err := admin.ExecContext(ctx, "DROP OWNED BY "+quoted); err != nil {
			t.Error(err)
		}
		if _, err := admin.ExecContext(ctx, "DROP ROLE "+quoted); err != nil {
			t.Error(err)
		}
	})
	objects := harness.NewSpaces(t, false)
	return func(cfg *cloudentry.Config) {
		defer func() {
			if cfg.Storage.PostgresPool != nil {
				admins.Store(cfg.Storage.PostgresPool, admin)
			}
			if cfg.Storage.PostgresDSN != "" {
				admins.Store(cfg.Storage.PostgresDSN, admin)
			}
		}()
		cfg.Storage = cloudentry.StorageConfig{Backend: "postgres", Pool: platformstore.PoolConfig{MaxConnections: 5, ServingConnections: 3, BackgroundConnections: 1}}
		if restart {
			native := harness.Postgres.NativeConfig(t, admin, name, password)
			cfg.Storage.PostgresDSN = native.ConnString()
		} else {
			native := harness.Postgres.NativeConfig(t, admin, name, password)
			native.MaxConns = int32(cfg.Storage.Pool.MaxConnections)
			var err error
			cfg.Storage.PostgresPool, err = pgxpool.NewWithConfig(t.Context(), native)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(cfg.Storage.PostgresPool.Close)
		}
		if cfg.Attachments == nil {
			config := objects.Config()
			config.Endpoint = "https://storage.example.test"
			cfg.Attachments, cfg.AttachmentTransport = &config, objects.Transport
		}
	}
}

func Main(m *testing.M) {
	for _, key := range []string{"RESEND_API_KEY", "DETENT_HUB_SECRET_KEYS", "DETENT_HUB_SECRET_KEY_VERSION"} {
		if err := os.Unsetenv(key); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	if err := testenv.ClearGitEnvironment(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	flag.Parse()
	deadline, err := time.ParseDuration(flag.Lookup("test.timeout").Value.String())
	if err != nil || deadline <= 0 {
		fmt.Fprintln(os.Stderr, "entry tests require a process deadline")
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	code := m.Run()
	if err := harness.Postgres.Close(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	cancel()
	os.Exit(code)
}
