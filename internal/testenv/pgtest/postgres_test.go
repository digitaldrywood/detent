package pgtest

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/testenv"
)

var migrationCount atomic.Int32

var processTemplate = NewTemplate(func(ctx context.Context, db *sql.DB) error {
	migrationCount.Add(1)
	return migrateFixture(ctx, db)
})

func migrateFixture(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE organizations (id text PRIMARY KEY, name text NOT NULL);
		CREATE TABLE records (
			organization_id text NOT NULL REFERENCES organizations(id),
			id text NOT NULL,
			value text NOT NULL,
			PRIMARY KEY (organization_id, id)
		);
		INSERT INTO organizations VALUES ('first', 'First'), ('second', 'Second');
	`)
	return err
}

func TestMain(m *testing.M) {
	flag.Parse()
	timeout, err := time.ParseDuration(flag.Lookup("test.timeout").Value.String())
	if err != nil || timeout <= 0 {
		fmt.Fprintln(os.Stderr, "PostgreSQL fixtures require a nonzero go test -timeout")
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := testenv.ClearGitEnvironment(); err != nil {
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
	if err := processTemplate.Close(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}

func TestConnectionConfig(t *testing.T) {
	for _, tc := range []struct {
		name      string
		selection string
		host      string
		wantError bool
	}{
		{name: "IPv4", selection: "postgres://example:fake@127.0.0.1:5432/postgres", host: "127.0.0.1"},
		{name: "IPv6", selection: "postgresql://example:fake@[::1]:5432/postgres", host: "::1"},
		{name: "localhost avoids DNS", selection: "postgres://example@localhost:5432/postgres", host: "127.0.0.1"},
		{name: "Unix socket", selection: "postgres://example@localhost:5432/postgres?host=/example", host: "/example"},
		{name: "remote", selection: "postgres://example:secret@example.invalid:5432/postgres", wantError: true},
		{name: "non-loopback IP", selection: "postgres://example@192.0.2.1:5432/postgres", wantError: true},
		{name: "missing host", selection: "postgres://example@:5432/postgres", wantError: true},
		{name: "missing user", selection: "postgres://127.0.0.1:5432/postgres", wantError: true},
		{name: "missing port", selection: "postgres://example@localhost/postgres", wantError: true},
		{name: "invalid port", selection: "postgres://example@localhost:70000/postgres", wantError: true},
		{name: "service discovery", selection: "postgres://example@localhost:5432/postgres?service=production", wantError: true},
		{name: "host override", selection: "postgres://example@localhost:5432/postgres?host=example.invalid", wantError: true},
		{name: "multiple hosts", selection: "postgres://example@localhost:5432/postgres?host=/example&host=/other", wantError: true},
		{name: "keyword DSN", selection: "host=localhost user=example", wantError: true},
		{name: "malformed", selection: "%secret", wantError: true},
		{name: "malformed query", selection: "postgres://example@localhost:5432/postgres?host=/example&bad=%zz", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config, err := connectionConfig(tc.selection)
			if (err != nil) != tc.wantError {
				t.Fatalf("connectionConfig error = %v, want error %v", err, tc.wantError)
			}
			if err != nil {
				if strings.Contains(err.Error(), "secret") {
					t.Fatal("connection error exposed a password")
				}
				return
			}
			if config.Host != tc.host || config.Database != "postgres" || config.TLSConfig != nil || len(config.Fallbacks) != 0 {
				t.Fatal("connection did not preserve isolated local settings")
			}
		})
	}
	for _, key := range []string{"PGHOST", "PGSERVICE", "PGPASSWORD", "PGOPTIONS", "PGCONNECT_TIMEOUT"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, "ambient-secret")
			_, err := connectionConfig("postgres://example@localhost:5432/postgres")
			if err == nil || !strings.Contains(err.Error(), key) || strings.Contains(err.Error(), "ambient-secret") {
				t.Fatalf("ambient setting error = %v", err)
			}
		})
	}
}

func TestUnconfiguredPostgresSkips(t *testing.T) {
	t.Setenv(Environment, "")
	t.Setenv("PATH", t.TempDir())
	t.Setenv("PGHOST", "example.invalid")
	var skipped bool
	t.Run("fixture", func(t *testing.T) {
		defer func() { skipped = t.Skipped() }()
		NewTemplate(func(context.Context, *sql.DB) error {
			t.Error("unconfigured fixture ran migrations")
			return nil
		}).Database(t)
		t.Error("unconfigured fixture returned a database")
	})
	if !skipped {
		t.Fatal("unconfigured PostgreSQL fixture did not skip")
	}
}

func TestBinarySetupFailureCleanup(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	t.Setenv("TMP", dir)
	t.Setenv("TEMP", dir)
	t.Setenv("PATH", dir)
	_, err := openServer(t.Context(), "binary")
	if err == nil {
		t.Fatal("missing initdb did not fail setup")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatal("failed binary setup left cluster files")
	}
}

func TestPostgresFailedSetupCleanup(t *testing.T) {
	processTemplate.Database(t)
	selection := serverSelection(processTemplate.server)
	sentinel := errors.New("fixture setup failure")
	for _, tc := range []struct {
		name   string
		cancel bool
		panics bool
	}{
		{name: "migration failure"},
		{name: "canceled migration", cancel: true},
		{name: "migration panic", panics: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bounded, finish := testContext(t)
			defer finish()
			ctx, cancel := context.WithCancel(bounded)
			defer cancel()
			var databaseName string
			p := NewTemplate(func(ctx context.Context, db *sql.DB) error {
				if err := db.QueryRowContext(ctx, "SELECT current_database()").Scan(&databaseName); err != nil {
					return err
				}
				if tc.cancel {
					cancel()
				}
				if tc.panics {
					panic(sentinel)
				}
				return sentinel
			})
			requireSetupFailure(t, func() error { return p.initialize(ctx, selection) }, sentinel, tc.panics)
			requireDatabaseAbsent(t, databaseName)
		})
	}
	for _, tc := range []struct {
		name   string
		cancel bool
		panics bool
	}{
		{name: "fixture setup failure"},
		{name: "canceled fixture setup", cancel: true},
		{name: "fixture setup panic", panics: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bounded, finish := testContext(t)
			defer finish()
			ctx, cancel := context.WithCancel(bounded)
			defer cancel()
			var databaseName string
			requireSetupFailure(t, func() error {
				_, _, err := processTemplate.clone(ctx, func(ctx context.Context, db *sql.DB) error {
					if err := db.QueryRowContext(ctx, "SELECT current_database()").Scan(&databaseName); err != nil {
						return err
					}
					if tc.cancel {
						cancel()
					}
					if tc.panics {
						panic(sentinel)
					}
					return sentinel
				})
				return err
			}, sentinel, tc.panics)
			requireDatabaseAbsent(t, databaseName)
		})
	}
}

func requireSetupFailure(t *testing.T, setup func() error, want error, panics bool) {
	t.Helper()
	if !panics {
		if err := setup(); !errors.Is(err, want) {
			t.Fatalf("setup error = %v, want %v", err, want)
		}
		return
	}
	defer func() {
		if got := recover(); got != want {
			t.Fatalf("setup panic = %v, want %v", got, want)
		}
	}()
	if err := setup(); err != nil {
		t.Fatalf("setup returned instead of panicking: %v", err)
	}
}

func requireDatabaseAbsent(t *testing.T, name string) {
	t.Helper()
	if name == "" {
		t.Fatal("setup did not reach the allocated database")
	}
	var exists bool
	if err := processTemplate.server.admin.QueryRowContext(t.Context(), "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", name).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatalf("failed setup leaked database %s", name)
	}
}

func serverSelection(s *server) string {
	u := &url.URL{Scheme: "postgres", User: url.UserPassword(s.config.User, s.config.Password), Host: fmt.Sprintf("127.0.0.1:%d", s.config.Port), Path: "/postgres"}
	if strings.HasPrefix(s.config.Host, "/") {
		u.RawQuery = url.Values{"host": {s.config.Host}}.Encode()
	} else {
		u.Host = net.JoinHostPort(s.config.Host, fmt.Sprint(s.config.Port))
	}
	return u.String()
}

func TestPostgresFixtureBounds(t *testing.T) {
	processTemplate.Database(t)
	p := NewTemplate(migrateFixture)
	bounded, finish := testContext(t)
	defer finish()
	if err := p.initialize(bounded, serverSelection(processTemplate.server)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := testContext(t)
		defer cancel()
		if err := p.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	var releases []func(context.Context) error
	t.Cleanup(func() {
		ctx, cancel := testContext(t)
		defer cancel()
		for _, release := range releases {
			if err := release(ctx); err != nil {
				t.Error(err)
			}
		}
	})
	for range fixtureLimit {
		db, release, err := p.clone(t.Context(), func(context.Context, *sql.DB) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
		if db.Stats().MaxOpenConnections != connectionLimit {
			t.Fatal("fixture connection pool is unbounded")
		}
		first, err := db.Conn(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { first.Close() })
		second, err := db.Conn(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { second.Close() })
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := db.Conn(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("exhausted connection pool error = %v", err)
		}
		first.Close()
		second.Close()
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := p.clone(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("exhausted fixture limit error = %v", err)
	}
	if err := releases[0](t.Context()); err != nil {
		t.Fatal(err)
	}
	releases = releases[1:]
	_, release, err := p.clone(t.Context(), func(context.Context, *sql.DB) error { return nil })
	if err != nil {
		t.Fatalf("released fixture capacity was not reusable: %v", err)
	}
	releases = append(releases, release)
}

func TestPostgresParallelIsolation(t *testing.T) {
	var names sync.Map
	var opened atomic.Int32
	var release sync.Once
	ready := make(chan struct{})
	t.Run("parallel fixtures", func(t *testing.T) {
		for _, name := range []string{"one", "two", "three", "four", "five", "six", "seven", "eight"} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				t.Cleanup(func() {
					if t.Failed() {
						release.Do(func() { close(ready) })
					}
				})
				db := processTemplate.Database(t)
				if opened.Add(1) == fixtureLimit {
					release.Do(func() { close(ready) })
				}
				select {
				case <-ready:
				case <-t.Context().Done():
					t.Fatal(t.Context().Err())
				}
				var databaseName string
				if err := db.QueryRowContext(t.Context(), "SELECT current_database()").Scan(&databaseName); err != nil {
					t.Fatal(err)
				}
				if _, loaded := names.LoadOrStore(databaseName, true); loaded {
					t.Fatal("parallel fixtures reused a database")
				}
				for _, organization := range []string{"first", "second"} {
					if _, err := db.ExecContext(t.Context(), "INSERT INTO records VALUES ($1, 'same-key', $2)", organization, name); err != nil {
						t.Fatal(err)
					}
					var value string
					if err := db.QueryRowContext(t.Context(), "SELECT value FROM records WHERE organization_id=$1 AND id='same-key'", organization).Scan(&value); err != nil {
						t.Fatal(err)
					}
					if value != name {
						t.Fatalf("fixture value = %q, want %q", value, name)
					}
				}
				if _, err := db.ExecContext(t.Context(), "ALTER TABLE records ADD COLUMN fixture_only text"); err != nil {
					t.Fatal(err)
				}
			})
		}
	})
	if errors.Is(processTemplate.err, errNotConfigured) {
		t.Skip(errNotConfigured)
	}
	if processTemplate.err != nil {
		t.Fatal(processTemplate.err)
	}
	if got := migrationCount.Load(); got != 1 {
		t.Fatalf("process template migrations = %d, want 1", got)
	}
	var allowed bool
	if err := processTemplate.server.admin.QueryRowContext(t.Context(), "SELECT datallowconn FROM pg_database WHERE datname=$1", processTemplate.name).Scan(&allowed); err != nil {
		t.Fatal(err)
	}
	if allowed {
		t.Fatal("template still permits connections during cloning")
	}
	db := processTemplate.server.database(processTemplate.name)
	defer db.Close()
	if err := db.PingContext(t.Context()); err == nil {
		t.Fatal("template accepted a connection")
	}
	names.Range(func(key, _ any) bool {
		requireDatabaseAbsent(t, key.(string))
		return true
	})
}

func TestPostgresOwnership(t *testing.T) {
	db := processTemplate.Database(t)
	for _, name := range []string{"postgres", "template0", "template1", "detent_test_not_owned"} {
		t.Run(name, func(t *testing.T) {
			if err := processTemplate.server.drop(t.Context(), name); err == nil {
				t.Fatal("drop accepted a database not owned by this process")
			}
		})
	}
	p := NewTemplate(migrateFixture)
	ctx, cancel := testContext(t)
	defer cancel()
	if err := p.initialize(ctx, serverSelection(processTemplate.server)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := testContext(t)
		defer cancel()
		if err := p.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	if err := processTemplate.server.drop(t.Context(), p.name); err == nil {
		t.Fatal("fixture accepted another owner's generated database")
	}
	if err := p.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.PingContext(t.Context()); err != nil {
		t.Fatalf("closing attached template damaged another owner's fixture: %v", err)
	}
}
