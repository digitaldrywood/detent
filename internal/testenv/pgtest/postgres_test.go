package pgtest

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/procgroup"
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
			if config.Host != tc.host || config.Database != "postgres" || config.SSLMode != "disable" || config.Passfile != "" || config.Password == "" || len(config.Multi) != 0 {
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

func TestConnectionConfigDoesNotScanHost(t *testing.T) {
	if os.Getenv("DETENT_TEST_CONFIG_CHILD") == "1" {
		if _, err := fmt.Fprintln(os.Stdout, "ready"); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(os.Stdin, make([]byte, 1)); err != nil {
			t.Fatal(err)
		}
		for _, selection := range []string{
			"postgres://example:fake@127.0.0.1:5432/postgres",
			"postgresql://example:fake@[::1]:5432/postgres",
			"postgres://example@localhost:5432/postgres",
			"postgres://example@localhost:5432/postgres?host=/example",
		} {
			config, err := connectionConfig(selection)
			if err != nil {
				t.Fatal(err)
			}
			s := &server{config: config}
			db, err := s.database("postgres")
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "actions.log")
	t.Setenv("DETENT_TEST_CONFIG_CHILD", "1")
	t.Setenv("GOCOVERDIR", t.TempDir())
	cmd := exec.CommandContext(t.Context(), executable, "-test.run=^TestConnectionConfigDoesNotScanHost$", "-test.testlogfile="+logPath, "-test.timeout="+flag.Lookup("test.timeout").Value.String())
	procgroup.Configure(t.Context(), cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	t.Cleanup(func() {
		if !waited {
			stdin.Close()
			if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				t.Error(err)
			}
			if err := cmd.Wait(); err != nil {
				t.Logf("stop configuration helper: %v", err)
			}
		}
	})
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() || scanner.Text() != "ready" {
		t.Fatalf("configuration helper did not become ready: %v", scanner.Err())
	}
	if _, err := stdin.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	for scanner.Scan() {
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	waited = true
	if err != nil {
		t.Fatalf("configuration helper: %v; %s", err, stderr.String())
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "getenv DETENT_TEST_CONFIG_CHILD\n") {
		t.Fatal("configuration helper did not record filesystem actions")
	}
	for line := range strings.SplitSeq(string(log), "\n") {
		if strings.HasPrefix(line, "stat ") || strings.HasPrefix(line, "open ") {
			t.Errorf("explicit connection settings scanned the filesystem: %s", line)
		}
	}
}

func TestFixtureAdmissionWaitsBeforeCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := NewTemplate(nil)
		for range fixtureLimit {
			p.slots <- struct{}{}
		}
		ctx, cancel := context.WithCancel(t.Context())
		result := make(chan error, 1)
		go func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					result <- fmt.Errorf("fixture bypassed admission: %v", recovered)
				}
			}()
			_, _, err := p.clone(ctx, nil)
			result <- err
		}()
		t.Cleanup(func() {
			cancel()
			synctest.Wait()
		})
		synctest.Wait()
		select {
		case err := <-result:
			t.Fatalf("active fifth fixture did not wait for capacity: %v", err)
		default:
		}
		cancel()
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatalf("waiting fixture cancellation = %v", err)
		}
	})
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

type admissionContext struct {
	context.Context
	reached chan struct{}
	once    sync.Once
}

func (c *admissionContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.reached) })
	return c.Context.Done()
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
	attempt := &admissionContext{Context: ctx, reached: make(chan struct{})}
	type cloneResult struct {
		release func(context.Context) error
		err     error
	}
	result := make(chan cloneResult, 1)
	go func() {
		_, release, err := p.clone(attempt, func(context.Context, *sql.DB) error { return nil })
		result <- cloneResult{release: release, err: err}
	}()
	joined := false
	t.Cleanup(func() {
		cancel()
		if !joined {
			pending := <-result
			if pending.release != nil {
				ctx, cancel := testContext(t)
				defer cancel()
				if err := pending.release(ctx); err != nil {
					t.Error(err)
				}
			}
		}
	})
	<-attempt.reached
	if err := releases[0](t.Context()); err != nil {
		t.Fatal(err)
	}
	releases = releases[1:]
	admitted := <-result
	joined = true
	if admitted.err != nil {
		t.Fatalf("releasing fixture capacity did not admit the active fifth request: %v", admitted.err)
	}
	releases = append(releases, admitted.release)
}

func TestPostgresRoleWaitsForOwnerBackendExit(t *testing.T) {
	fixture := processTemplate.Database(t)
	ctx, cancel := testContext(t)
	defer cancel()
	var name string
	if err := fixture.QueryRowContext(ctx, "SELECT current_database()").Scan(&name); err != nil {
		t.Fatal(err)
	}
	role := name + "_role"
	if _, err := processTemplate.server.admin.ExecContext(ctx, "CREATE ROLE "+identifier(role)+" LOGIN"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := testContext(t)
		defer cancel()
		if _, err := processTemplate.server.admin.ExecContext(ctx, "DROP ROLE "+identifier(role)); err != nil {
			t.Error(err)
		}
	})
	first := processTemplate.OpenRole(t, fixture, role, "detent-test-only")
	owner, err := fixture.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { owner.Close() })
	fixture.SetMaxOpenConns(2)
	observer, err := processTemplate.server.database("postgres")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { observer.Close() })
	var adminPID int
	if err := processTemplate.server.admin.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&adminPID); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var second *sql.DB
	go func() {
		defer close(done)
		second = processTemplate.OpenRole(t, fixture, role, "detent-test-only")
	}()
	t.Cleanup(func() {
		owner.Close()
		<-done
	})
	for {
		select {
		case <-done:
			t.Fatal("second role connection completed before the owner backend exited")
		default:
		}
		var waiting bool
		if err := observer.QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity WHERE pid = $1 AND query LIKE '%pg_stat_activity%'
		)`, adminPID).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	<-done
	for _, db := range []*sql.DB{first, second} {
		if db == nil {
			t.Fatal("role connection was not opened after the owner backend exited")
		}
		if err := db.PingContext(ctx); err != nil {
			t.Fatalf("role connection is unusable: %v", err)
		}
	}
	var limit int
	if err := observer.QueryRowContext(ctx, "SELECT datconnlimit FROM pg_database WHERE datname = $1", name).Scan(&limit); err != nil {
		t.Fatal(err)
	}
	if limit != 2 {
		t.Fatalf("database connection limit = %d, want 2", limit)
	}
}

func TestPostgresParallelIsolation(t *testing.T) {
	var names sync.Map
	t.Run("parallel fixtures", func(t *testing.T) {
		for _, name := range []string{"one", "two", "three", "four", "five", "six", "seven", "eight"} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				db := processTemplate.Database(t)
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
	db, err := processTemplate.server.database(processTemplate.name)
	if err != nil {
		t.Fatal(err)
	}
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
