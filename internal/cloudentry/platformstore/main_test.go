package platformstore

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"database/sql/driver"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/pressly/goose/v3"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/testenv/pgtest"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var template = pgtest.NewTemplate(func(ctx context.Context, db *sql.DB) error {
	roles, err := os.ReadFile("../../hubserver/postgres/roles.sql")
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, string(roles)); err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, `SET ROLE `+postgres.MigrationRoleName); err != nil {
		return err
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, postgres.RoleMigrations(os.DirFS("../../hubserver/postgres/migrations")), goose.WithDisableGlobalRegistry(true), goose.WithSlog(slog.New(slog.DiscardHandler)))
	if err != nil {
		return err
	}
	_, err = provider.Up(ctx)
	return err
})

func TestMain(m *testing.M) {
	flag.Parse()
	timeout, err := time.ParseDuration(flag.Lookup("test.timeout").Value.String())
	if err != nil || timeout <= 0 {
		fmt.Fprintln(os.Stderr, "platform tests require a process deadline")
		os.Exit(1)
	}
	if err := testenv.ClearGitEnvironment(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, key := range []string{"DETENT_HUB_SECRET_KEYS", "DETENT_HUB_SECRET_KEY_VERSION"} {
		os.Unsetenv(key)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	code := m.Run()
	if err := template.Close(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	cancel()
	os.Exit(code)
}

func rolePool(t *testing.T, admin *sql.DB, group, attributes string, wrappers ...func(driver.Connector) driver.Connector) *sql.DB {
	t.Helper()
	name := "example_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	password := uuid.NewString()
	quoted := pq.QuoteIdentifier(name)
	if _, err := admin.ExecContext(t.Context(), "CREATE ROLE "+quoted+" LOGIN PASSWORD "+pq.QuoteLiteral(password)+" "+attributes); err != nil {
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
	if group != "" {
		if _, err := admin.ExecContext(t.Context(), "GRANT "+group+" TO "+quoted); err != nil {
			t.Fatal(err)
		}
	}
	pool := template.OpenRole(t, admin, name, password)
	if len(wrappers) == 0 {
		return pool
	}
	var database string
	if err := pool.QueryRowContext(t.Context(), "SELECT current_database()").Scan(&database); err != nil {
		t.Fatal(err)
	}
	if err := pool.Close(); err != nil {
		t.Fatal(err)
	}
	config, err := pq.NewConfig(os.Getenv(pgtest.Environment))
	if err != nil {
		t.Fatal("configure test connection")
	}
	config.Database, config.User, config.Password = database, name, password
	config.SSLMode = pq.SSLModeDisable
	connector, err := pq.NewConnectorConfig(config)
	if err != nil {
		t.Fatal("configure test connector")
	}
	pool = sql.OpenDB(wrappers[0](connector))
	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Error(err)
		}
	})
	return pool
}

func testConfig(t *testing.T) Config {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	return Config{Pool: PoolConfig{MaxConnections: 3, ServingConnections: 1, BackgroundConnections: 1}, SigningKey: key, Now: func() time.Time { return now }}
}

func repository(t *testing.T, db *sql.DB, config Config) *Repository {
	t.Helper()
	r, err := New(t.Context(), rolePool(t, db, postgres.PlatformRoleName, ""), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	})
	return r
}

func seedOrganizations(t *testing.T, r *Repository) {
	t.Helper()
	for _, id := range []string{"first", "second"} {
		changed, err := r.Register(t.Context(), Organization{ID: tracker.OrganizationID("org_" + id), ProviderID: "provider_" + id, Name: "Example " + id, State: "ready", Endpoint: "unix:/example/" + id + ".sock", Generation: 1})
		if err != nil || !changed {
			t.Fatalf("register %s: changed=%v err=%v", id, changed, err)
		}
	}
}

func createSession(t *testing.T, r *Repository, id string) Session {
	t.Helper()
	identity := auth.HostedIdentity{Subject: "subject_" + id, SessionID: "session_" + id, CreatedAt: r.now(), ExpiresAt: r.now().Add(time.Hour)}
	s := Session{Hash: "hash_" + id, Subject: identity.Subject, Email: id + "@example.com", CSRFSecret: "csrf_" + id, Identity: identity}
	if err := r.CreateSession(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	return s
}

func authorize(t *testing.T, r *Repository, s Session, organization tracker.OrganizationID, id string) Authorization {
	t.Helper()
	identity := s.Identity
	identity.OrganizationID = "provider_" + strings.TrimPrefix(string(organization), "org_")
	identity.SessionID = id
	result, _, err := r.Authorize(t.Context(), s.Hash, organization, identity, auth.HostedTokens{AccessToken: "access_" + id, RefreshToken: "refresh_" + id}, s.Email)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func replicaRepository(t *testing.T, original *Repository, config Config) *Repository {
	t.Helper()
	r, err := New(t.Context(), original.pool, config)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
