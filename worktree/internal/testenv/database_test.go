package testenv

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/testenv/pgtest"
)

var testPostgresTemplate = pgtest.NewTemplate(func(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
CREATE TABLE organizations (id text PRIMARY KEY);
CREATE TABLE records (
    organization_id text NOT NULL REFERENCES organizations(id),
    id text NOT NULL,
    value text NOT NULL,
    PRIMARY KEY (organization_id, id)
);
CREATE TABLE principals (id text PRIMARY KEY, name text NOT NULL UNIQUE, note text);
CREATE TABLE grants (org text NOT NULL, label text NOT NULL, UNIQUE(org, label));
CREATE TABLE covering (id text NOT NULL, note text NOT NULL);
CREATE UNIQUE INDEX covering_note ON covering(note) INCLUDE (id);
CREATE TABLE conditional (id text NOT NULL, note text NOT NULL);
CREATE UNIQUE INDEX conditional_partial ON conditional(note) WHERE id <> '';
CREATE UNIQUE INDEX conditional_expression ON conditional(lower(note));
CREATE SCHEMA other;
CREATE TABLE other.grants (org text NOT NULL, label text NOT NULL UNIQUE);
`)
	return err
})

func TestMain(m *testing.M) {
	flag.Parse()
	timeout, err := time.ParseDuration(flag.Lookup("test.timeout").Value.String())
	if err != nil || timeout <= 0 {
		fmt.Fprintln(os.Stderr, "PostgreSQL fixtures require a nonzero go test -timeout")
		os.Exit(1)
	}
	if err := ClearGitEnvironment(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "PG") {
			if err := os.Unsetenv(key); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
		}
	}
	code := m.Run()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := testPostgresTemplate.Close(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}

func TestPostgresDatabaseConnectionReopensIsolatedService(t *testing.T) {
	t.Parallel()
	connection := testPostgresTemplate.DatabaseConnection(t)
	open := func(connection pgtransport.Connection) *pgxpool.Pool {
		t.Helper()
		config, err := connection.PoolConfig()
		if err != nil {
			t.Fatal("configure PostgreSQL service fixture")
		}
		config.MaxConns = 2
		pool, err := pgxpool.NewWithConfig(t.Context(), config)
		if err != nil {
			t.Fatal("open PostgreSQL service fixture")
		}
		t.Cleanup(pool.Close)
		return pool
	}
	pool := open(connection)
	for _, organization := range []string{"first", "second"} {
		if _, err := pool.Exec(t.Context(), `INSERT INTO organizations VALUES ($1)`, organization); err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"one", "two"} {
			if _, err := pool.Exec(t.Context(), `INSERT INTO records VALUES ($1, $2, $3)`, organization, id, organization+"-"+id); err != nil {
				t.Fatal(err)
			}
		}
	}
	pool.Close()
	pool = open(connection)
	for _, organization := range []string{"first", "second"} {
		for _, id := range []string{"one", "two"} {
			var value string
			if err := pool.QueryRow(t.Context(), `SELECT value FROM records WHERE organization_id = $1 AND id = $2`, organization, id).Scan(&value); err != nil {
				t.Fatal(err)
			}
			if value != organization+"-"+id {
				t.Fatalf("reopened record = %q, want %q", value, organization+"-"+id)
			}
		}
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO records VALUES ('first', 'one', 'duplicate')`); err == nil {
		t.Fatal("reopened fixture accepted a duplicate tenant key")
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO records VALUES ('missing', 'one', 'foreign')`); err == nil {
		t.Fatal("reopened fixture accepted a missing organization")
	}
	second := testPostgresTemplate.DatabaseConnection(t)
	if second.Database == connection.Database {
		t.Fatal("service fixtures share a database")
	}
	isolated := open(second)
	var records int
	if err := isolated.QueryRow(t.Context(), `SELECT count(*) FROM records`).Scan(&records); err != nil {
		t.Fatal(err)
	}
	if records != 0 {
		t.Fatalf("new service fixture contains %d records", records)
	}
}

type databaseFixtureCloser func() error

func (f databaseFixtureCloser) Close() error { return f() }

func TestDatabaseTemplateKeepsClosedImageAndPrivateCopies(t *testing.T) {
	t.Parallel()
	var opens atomic.Int64
	pathForTest := DatabaseTemplate(func(_ context.Context, path string) (io.Closer, error) {
		opens.Add(1)
		return databaseFixtureCloser(func() error {
			// The committed image becomes readable only when the opener closes.
			return os.WriteFile(path, []byte("closed image"), 0o600)
		}), nil
	})
	first, second := pathForTest(t), pathForTest(t)
	if first == second || opens.Load() != 1 {
		t.Fatalf("copies=%q,%q opens=%d", first, second, opens.Load())
	}
	if err := os.WriteFile(first, []byte("first test writes"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(second)
	if err != nil || string(data) != "closed image" {
		t.Fatalf("second copy=%q error=%v", data, err)
	}
	third := pathForTest(t)
	data, err = os.ReadFile(third)
	if err != nil || string(data) != "closed image" || opens.Load() != 1 {
		t.Fatalf("template mutated: third=%q opens=%d error=%v", data, opens.Load(), err)
	}
	info, err := os.Stat(second)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("copy permissions=%v", info.Mode().Perm())
	}
}
