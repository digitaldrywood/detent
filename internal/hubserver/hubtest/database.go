package hubtest

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/testenv/pgtest"
)

const DefaultMaxConnections = 3

const reservedConnections = 2

var template = pgtest.NewTemplate(migrate)

func migrate(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, postgres.RolesSQL()); err != nil {
		return fmt.Errorf("create PostgreSQL roles: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, "SET ROLE "+postgres.MigrationRoleName); err != nil {
		return fmt.Errorf("select migration role: %w", err)
	}
	migrations, err := postgres.MigrationFiles()
	if err != nil {
		return fmt.Errorf("open migrations: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, postgres.RoleMigrations(migrations),
		goose.WithGoMigrations(postgres.ConversationMigrations(migrations)...), goose.WithDisableGlobalRegistry(true),
		goose.WithSlog(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		return fmt.Errorf("create migration provider: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

func Main(m *testing.M) {
	flag.Parse()
	timeout, err := time.ParseDuration(flag.Lookup("test.timeout").Value.String())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ctx := context.Background()
	cancel := func() {}
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
	}
	if err := testenv.ClearGitEnvironment(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	if err := template.Close(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	cancel()
	os.Exit(code)
}

func Database(t *testing.T) *sql.DB {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping Postgres hubtest fixture in -short mode")
	}
	return template.Database(t)
}

type settings struct {
	maxConnections int
	clock          func() time.Time
	configure      []func(*pgxpool.Config)
}

type Option func(*settings)

func WithMaxConnections(count int) Option {
	return func(s *settings) { s.maxConnections = count }
}

func WithClock(clock func() time.Time) Option {
	return func(s *settings) { s.clock = clock }
}

func WithPoolConfig(configure func(*pgxpool.Config)) Option {
	return func(s *settings) { s.configure = append(s.configure, configure) }
}

func Repository(t *testing.T, options ...Option) *postgres.Repository {
	t.Helper()
	return RepositoryOn(t, Database(t), options...)
}

func RepositoryOn(t *testing.T, admin *sql.DB, options ...Option) *postgres.Repository {
	t.Helper()
	selected := settings{maxConnections: DefaultMaxConnections}
	for _, option := range options {
		option(&selected)
	}
	if selected.maxConnections < DefaultMaxConnections {
		t.Fatalf("PostgreSQL repository pool needs at least %d connections", DefaultMaxConnections)
	}
	reserveConnections(t, admin, selected.maxConnections)
	name, password := loginRole(t, admin)
	native := template.NativeConfig(t, admin, name, password)
	native.MaxConns = int32(selected.maxConnections)
	for _, configure := range selected.configure {
		configure(native)
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), native)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	budget := postgres.PoolConfig{MaxConnections: selected.maxConnections, ServingConnections: selected.maxConnections - 1, BackgroundConnections: 1}
	var repository *postgres.Repository
	if selected.clock == nil {
		repository, err = postgres.NewRepository(t.Context(), pool, budget)
	} else {
		repository, err = postgres.NewRepository(t.Context(), pool, budget, selected.clock)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
	return repository
}

func PostgresConfig(t *testing.T, admin *sql.DB, options ...Option) postgres.Config {
	t.Helper()
	selected := settings{maxConnections: DefaultMaxConnections}
	for _, option := range options {
		option(&selected)
	}
	if selected.maxConnections < DefaultMaxConnections {
		t.Fatalf("PostgreSQL repository pool needs at least %d connections", DefaultMaxConnections)
	}
	reserveConnections(t, admin, selected.maxConnections)
	name, password := loginRole(t, admin)
	return postgres.Config{Connection: template.Connection(t, admin, name, password), Pool: postgres.PoolConfig{MaxConnections: selected.maxConnections, ServingConnections: selected.maxConnections - 1, BackgroundConnections: 1}, LeaseClock: selected.clock}
}

func loginRole(t *testing.T, admin *sql.DB) (string, string) {
	t.Helper()
	name, password := hubapi.NewID("example"), hubapi.NewID("password")
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err := admin.ExecContext(t.Context(), "CREATE ROLE "+quoted+" LOGIN PASSWORD "+pgtransport.QuoteLiteral(password)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		if deadline, ok := t.Deadline(); ok {
			var cancel context.CancelFunc
			ctx, cancel = context.WithDeadline(ctx, deadline)
			defer cancel()
		}
		if _, err := admin.ExecContext(ctx, "DROP OWNED BY "+quoted); err != nil {
			t.Error(err)
		}
		if _, err := admin.ExecContext(ctx, "DROP ROLE "+quoted); err != nil {
			t.Error(err)
		}
	})
	if _, err := admin.ExecContext(t.Context(), "GRANT "+postgres.RuntimeRoleName+" TO "+quoted); err != nil {
		t.Fatal(err)
	}
	return name, password
}

var connectionBudgets sync.Mutex

func reserveConnections(t *testing.T, admin *sql.DB, count int) {
	t.Helper()
	connectionBudgets.Lock()
	defer connectionBudgets.Unlock()
	var name string
	var limit int
	if err := admin.QueryRowContext(t.Context(), "SELECT datname, datconnlimit FROM pg_database WHERE datname = current_database()").Scan(&name, &limit); err != nil {
		t.Fatal(err)
	}
	if limit < 0 {
		return
	}
	limit = max(limit, reservedConnections) + count
	if _, err := admin.ExecContext(t.Context(), "ALTER DATABASE "+pgx.Identifier{name}.Sanitize()+" CONNECTION LIMIT "+strconv.Itoa(limit)); err != nil {
		t.Fatal(err)
	}
}

func ConnectionLimit(t *testing.T, admin *sql.DB) int {
	t.Helper()
	var limit int
	if err := admin.QueryRowContext(t.Context(), "SELECT datconnlimit FROM pg_database WHERE datname = current_database()").Scan(&limit); err != nil {
		t.Fatal(err)
	}
	return limit
}

func NativeConfig(t *testing.T, admin *sql.DB, role, password string) *pgxpool.Config {
	t.Helper()
	return template.NativeConfig(t, admin, role, password)
}
