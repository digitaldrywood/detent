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

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/testenv"
)

var migrationCount atomic.Int32

func TestNativeGuardDistinguishesTenantAndPlatformPolicies(t *testing.T) {
	db := processTemplate.Database(t)
	var database string
	if err := db.QueryRowContext(t.Context(), "SELECT current_database()").Scan(&database); err != nil {
		t.Fatal(err)
	}
	suffix := strings.TrimPrefix(database, "detent_test_")
	tenant, platform := "example_tenant_"+suffix, "example_platform_"+suffix
	if _, err := db.ExecContext(t.Context(), `CREATE ROLE `+tenant+` LOGIN PASSWORD 'fixture-only'; CREATE ROLE `+platform+` LOGIN PASSWORD 'fixture-only';
		ALTER TABLE records ENABLE ROW LEVEL SECURITY;
		ALTER TABLE records FORCE ROW LEVEL SECURITY;
		CREATE POLICY tenant_scope ON records TO `+tenant+` USING (organization_id=current_setting('detent.organization_id',true));
		CREATE POLICY platform_read ON records FOR SELECT TO `+platform+` USING (true);
		GRANT SELECT ON records TO `+tenant+`,`+platform+`;
		INSERT INTO records VALUES ('first','same','first'),('second','same','second')`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := db.ExecContext(context.WithoutCancel(t.Context()), `DROP OWNED BY `+tenant+`,`+platform+`; DROP ROLE `+tenant+`,`+platform); err != nil {
			t.Error(err)
		}
	})
	for _, test := range []struct {
		role             string
		rows, violations int
	}{{tenant, 0, 1}, {platform, 2, 0}} {
		config := processTemplate.NativeConfig(t, db, test.role, "fixture-only")
		violations := 0
		config.ConnConfig.Tracer.(*TenantGuard).report = func(string) { violations++ }
		conn, err := pgx.ConnectConfig(t.Context(), config.ConnConfig)
		if err != nil {
			t.Fatal(err)
		}
		var rows int
		err = conn.QueryRow(t.Context(), `SELECT count(*) FROM records`).Scan(&rows)
		closeErr := conn.Close(t.Context())
		if err != nil || closeErr != nil || rows != test.rows || violations != test.violations {
			t.Fatalf("role=%s rows=%d violations=%d query=%v close=%v", test.role, rows, violations, err, closeErr)
		}
	}
	if _, err := db.ExecContext(t.Context(), `CREATE POLICY tenant_global_read ON records FOR SELECT TO `+tenant+` USING (true); GRANT INSERT,UPDATE ON records TO `+tenant); err != nil {
		t.Fatal(err)
	}
	config := processTemplate.NativeConfig(t, db, tenant, "fixture-only")
	violations := 0
	config.ConnConfig.Tracer.(*TenantGuard).report = func(string) { violations++ }
	conn, err := pgx.ConnectConfig(t.Context(), config.ConnConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	var rows int
	if err := conn.QueryRow(t.Context(), `SELECT count(*) FROM records`).Scan(&rows); err != nil || rows != 2 || violations != 0 {
		t.Fatalf("global read rows=%d violations=%d err=%v", rows, violations, err)
	}
	if _, err := conn.Exec(t.Context(), `INSERT INTO records VALUES ('first','unguarded','value')`); err == nil || violations != 1 {
		t.Fatalf("tenant write violations=%d err=%v", violations, err)
	}
	locked, err := conn.Query(t.Context(), `SELECT id FROM records FOR UPDATE`)
	if err != nil {
		t.Fatal(err)
	}
	locked.Close()
	if violations != 2 {
		t.Fatalf("tenant lock violations=%d", violations)
	}
}

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
			if config.Host != tc.host || config.Database != "postgres" || config.SSLMode != "disable" || config.Password == "" {
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

func TestConnectionConfigDoesNotReadHostCredentials(t *testing.T) {
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
	cmd := exec.CommandContext(t.Context(), executable, "-test.run=^TestConnectionConfigDoesNotReadHostCredentials$", "-test.testlogfile="+logPath, "-test.timeout="+flag.Lookup("test.timeout").Value.String())
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
		if strings.HasPrefix(line, "open ") && strings.TrimPrefix(line, "open ") != os.DevNull {
			t.Errorf("explicit connection settings read a host credential file: %s", line)
		}
	}
}

func TestFixtureAdmissionWaitsBeforeCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := NewTemplate(nil)
		for i := range fixtureLimit {
			if _, err := p.acquire(t.Context(), fmt.Sprint(i)); err != nil {
				t.Fatal(err)
			}
		}
		nestedCtx, nestedCancel := context.WithCancel(t.Context())
		defer nestedCancel()
		nested := make(chan error, 1)
		go func() {
			release, err := p.acquire(nestedCtx, "0/child")
			if err == nil {
				release()
			}
			nested <- err
		}()
		synctest.Wait()
		select {
		case err := <-nested:
			if err != nil {
				t.Fatalf("nested fixture admission: %v", err)
			}
		default:
			nestedCancel()
			<-nested
			t.Fatal("nested fixture waited on its own top-level test slot")
		}
		ctx, cancel := context.WithCancel(t.Context())
		result := make(chan error, 1)
		go func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					result <- fmt.Errorf("fixture bypassed admission: %v", recovered)
				}
			}()
			_, _, err := p.clone(ctx, "waiting", nil)
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

func TestBinaryStderrDrainsOversizedRecords(t *testing.T) {
	const readiness = "database system is ready to accept connections\n"
	oversized := strings.Repeat("x", 2*bufio.MaxScanTokenSize) + "\n"
	for _, tc := range []struct {
		name, output, failure string
	}{
		{name: "before readiness", output: oversized + readiness},
		{name: "after readiness", output: readiness + strings.Repeat(oversized, 16) + readiness},
		{name: "fatal before readiness", output: "FATAL: could not create shared memory segment: No space left on device\n", failure: "could not create shared memory segment: No space left on device"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stderr := strings.NewReader(tc.output)
			ready := make(chan struct{})
			err := drainStderr(stderr, ready)
			if tc.failure != "" {
				if err == nil || !strings.Contains(err.Error(), tc.failure) {
					t.Fatalf("startup failure=%v, want %s", err, tc.failure)
				}
				select {
				case <-ready:
					t.Fatal("failed server reported readiness")
				default:
				}
				return
			}
			if err != nil {
				t.Fatalf("drain server stderr: %v", err)
			}
			if stderr.Len() != 0 {
				t.Fatalf("server stderr has %d unread bytes", stderr.Len())
			}
			select {
			case <-ready:
			default:
				t.Fatal("server readiness was not reported")
			}
		})
	}
}

func TestBinaryStderrFloodAndCleanup(t *testing.T) {
	for _, tc := range []struct {
		name    string
		expired bool
	}{
		{name: "canceled caller"},
		{name: "expired deadline", expired: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, finish := testContext(t)
			defer finish()
			p := NewTemplate(migrateFixture)
			p.cluster = &binaryCluster{owner: p}
			if err := p.initialize(ctx, "binary"); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Dir(p.server.config.Host)
			t.Cleanup(func() {
				if p.server != nil {
					if err := p.Close(ctx); err != nil {
						t.Error(err)
					}
				}
			})
			if _, err := p.server.admin.ExecContext(ctx, `DO $$ BEGIN
				FOR i IN 1..16 LOOP RAISE LOG '%', repeat('x', 131072); END LOOP;
			END $$; CHECKPOINT`); err != nil {
				t.Fatalf("server stalled while writing stderr: %v", err)
			}
			name, err := p.server.create(ctx, p.name)
			if err != nil {
				t.Fatal(err)
			}
			if err := p.server.drop(ctx, name); err != nil {
				t.Fatalf("drop after stderr flood: %v", err)
			}
			closeCtx, cancel := context.WithCancel(ctx)
			if tc.expired {
				cancel()
				closeCtx, cancel = context.WithDeadline(ctx, time.Time{})
			}
			cancel()
			err = p.Close(closeCtx)
			if tc.expired {
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("expired cleanup error = %v", err)
				}
			} else if err != nil {
				t.Fatalf("canceled caller prevented cleanup: %v", err)
			}
			if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("server cleanup left cluster files: %v", err)
			}
		})
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
				_, _, err := processTemplate.clone(ctx, t.Name(), func(ctx context.Context, db *sql.DB) error {
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
	for i := range fixtureLimit {
		db, release, err := p.clone(t.Context(), fmt.Sprint(i), func(context.Context, *sql.DB) error { return nil })
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
		_, release, err := p.clone(attempt, "waiting", func(context.Context, *sql.DB) error { return nil })
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
	if _, err := processTemplate.server.admin.ExecContext(ctx, "CREATE ROLE "+identifier(role)+" LOGIN PASSWORD 'detent-test-only'"); err != nil {
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
				for fixture := range fixtureLimit + 1 {
					db := processTemplate.Database(t)
					var databaseName string
					if err := db.QueryRowContext(t.Context(), "SELECT current_database()").Scan(&databaseName); err != nil {
						t.Fatal(err)
					}
					if _, loaded := names.LoadOrStore(databaseName, true); loaded {
						t.Fatal("parallel fixtures reused a database")
					}
					for _, organization := range []string{"first", "second"} {
						if _, err := db.ExecContext(t.Context(), "INSERT INTO records VALUES ($1, 'same-key', $2)", organization, fmt.Sprintf("%s-%d", name, fixture)); err != nil {
							t.Fatal(err)
						}
						var value string
						if err := db.QueryRowContext(t.Context(), "SELECT value FROM records WHERE organization_id=$1 AND id='same-key'", organization).Scan(&value); err != nil {
							t.Fatal(err)
						}
						if want := fmt.Sprintf("%s-%d", name, fixture); value != want {
							t.Fatalf("fixture value = %q, want %q", value, want)
						}
					}
					if _, err := db.ExecContext(t.Context(), "ALTER TABLE records ADD COLUMN fixture_only text"); err != nil {
						t.Fatal(err)
					}
					if err := db.Close(); err != nil {
						t.Fatal(err)
					}
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

func TestBinaryDurabilitySettingsAndURLPreservation(t *testing.T) {
	postgres, err := exec.LookPath("postgres")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		attached bool
		want     string
	}{
		{name: "disposable binary", want: "off"},
		{name: "caller durable URL", attached: true, want: "on"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := testContext(t)
			defer cancel()
			selection := "binary"
			if tc.attached {
				bin := t.TempDir()
				wrapper := "#!/bin/sh\nexec '" + strings.ReplaceAll(postgres, "'", "'\\''") + "' \"$@\" -c fsync=on -c synchronous_commit=on -c full_page_writes=on\n"
				if err := os.WriteFile(filepath.Join(bin, "postgres"), []byte(wrapper), 0700); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
				var stop func(context.Context) error
				selection, stop, err = startBinary(ctx)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := stop(ctx); err != nil {
						t.Error(err)
					}
				})
			}
			s, err := openServer(ctx, selection)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := errors.Join(s.admin.Close(), s.stop(ctx)); err != nil {
					t.Error(err)
				}
			})
			for _, setting := range []string{"fsync", "synchronous_commit", "full_page_writes"} {
				var got string
				if err := s.admin.QueryRowContext(ctx, "SHOW "+setting).Scan(&got); err != nil {
					t.Fatal(err)
				}
				if got != tc.want {
					t.Errorf("%s = %s, want %s", setting, got, tc.want)
				}
			}
		})
	}
}

func TestTemplatesShareBinaryClusterAndKeepOwnership(t *testing.T) {
	root := processTemplate.Database(t)
	ctx, cancel := testContext(t)
	defer cancel()
	for range 2 {
		p := NewTemplate(migrateFixture)
		if err := p.initialize(ctx, "binary"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := p.Close(ctx); err != nil {
				t.Error(err)
			}
		})
		if p.server.config.Host != processTemplate.server.config.Host {
			t.Fatal("Templates started separate disposable clusters")
		}
		if p.server.admin != processTemplate.server.admin {
			t.Fatal("Templates opened separate administrator pools")
		}
		if p.admission != processTemplate.admission {
			t.Fatal("Templates do not share server admission")
		}
		if err := p.server.drop(ctx, processTemplate.name); err == nil {
			t.Fatal("shared server accepted another Template's database")
		}
		db, cleanup, err := p.clone(ctx, t.Name(), func(context.Context, *sql.DB) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		if err := db.PingContext(ctx); err != nil {
			t.Fatal(err)
		}
		if err := cleanup(ctx); err != nil {
			t.Fatal(err)
		}
		if err := p.Close(ctx); err != nil {
			t.Fatal(err)
		}
		if err := root.PingContext(ctx); err != nil {
			t.Fatalf("borrower close stopped shared cluster: %v", err)
		}
	}
}

func TestOwnerMigrationFailureKeepsBinaryLifetime(t *testing.T) {
	ctx, cancel := testContext(t)
	defer cancel()
	sentinel := errors.New("initial migration failed")
	owner := NewTemplate(func(context.Context, *sql.DB) error { return sentinel })
	owner.cluster = &binaryCluster{owner: owner}
	t.Cleanup(func() {
		if err := owner.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	if err := owner.initialize(ctx, "binary"); !errors.Is(err, sentinel) {
		t.Fatalf("initial migration: %v", err)
	}
	if err := owner.cluster.server.admin.PingContext(ctx); err != nil {
		t.Fatalf("failed migration ended the binary lifetime: %v", err)
	}
	retry := NewTemplate(migrateFixture)
	retry.cluster = owner.cluster
	t.Cleanup(func() {
		if err := retry.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	if err := retry.initialize(ctx, "binary"); err != nil {
		t.Fatalf("retry on the original server: %v", err)
	}
}

func TestSharedBinaryClusterSupportsFullTemplatePools(t *testing.T) {
	ctx, cancel := testContext(t)
	defer cancel()
	const templateCount = 16
	templates := make([]*Template, templateCount)
	for i := range templates {
		p := NewTemplate(migrateFixture)
		templates[i] = p
		t.Cleanup(func() {
			if err := p.Close(ctx); err != nil {
				t.Error(err)
			}
		})
		if err := p.initialize(ctx, "binary"); err != nil {
			t.Fatal(err)
		}
	}
	hold := make(chan struct{})
	var release sync.Once
	var workers sync.WaitGroup
	var connected atomic.Int32
	results := make(chan error, templateCount)
	t.Cleanup(func() { release.Do(func() { close(hold) }); workers.Wait() })
	for i, p := range templates {
		workers.Go(func() {
			var connections []*sql.Conn
			var cleanups []func(context.Context) error
			defer func() {
				for _, connection := range connections {
					if err := connection.Close(); err != nil {
						t.Error(err)
					}
				}
				for _, cleanup := range cleanups {
					if err := cleanup(ctx); err != nil {
						t.Error(err)
					}
				}
			}()
			err := func() error {
				for owner := range fixtureLimit {
					db, cleanup, err := p.clone(ctx, fmt.Sprintf("owner-%d", owner), func(context.Context, *sql.DB) error { return nil })
					if err != nil {
						return fmt.Errorf("template %d owner %d: %w", i, owner, err)
					}
					cleanups = append(cleanups, cleanup)
					for range connectionLimit {
						connection, err := db.Conn(ctx)
						if err != nil {
							return fmt.Errorf("template %d owner %d pool: %w", i, owner, err)
						}
						connections = append(connections, connection)
						connected.Add(1)
					}
				}
				return nil
			}()
			results <- err
			<-hold
		})
	}
	for range templateCount {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
	if got, want := connected.Load(), int32(templateCount*fixtureLimit*connectionLimit); got != want {
		t.Errorf("held connections=%d, want %d", got, want)
	}
	release.Do(func() { close(hold) })
	workers.Wait()
}
