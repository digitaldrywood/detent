package pgtest

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/stdlib"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/digitaldrywood/detent/internal/pgtransport"

	"github.com/google/uuid"

	"github.com/digitaldrywood/detent/internal/procgroup"
)

const Environment = "DETENT_TEST_POSTGRES"
const fixtureLimit = 4
const connectionLimit = 2

var errNotConfigured = errors.New("PostgreSQL 17 or 18 fixtures require DETENT_TEST_POSTGRES=binary or a disposable local postgres:// URL")

type Template struct {
	once    sync.Once
	migrate func(context.Context, *sql.DB) error
	server  *server
	name    string
	err     error
	mu      sync.Mutex
	owners  map[string]int
	changed chan struct{}
}

type server struct {
	config *pgtransport.Connection
	admin  *sql.DB
	stop   func(context.Context) error
	mu     sync.Mutex
	owned  map[string]struct{}
}

func NewTemplate(migrate func(context.Context, *sql.DB) error) *Template {
	return &Template{migrate: migrate, owners: make(map[string]int), changed: make(chan struct{})}
}

func (p *Template) Database(t *testing.T) *sql.DB {
	t.Helper()
	ctx, cancel := testContext(t)
	defer cancel()
	p.once.Do(func() { p.err = p.initialize(ctx, os.Getenv(Environment)) })
	if errors.Is(p.err, errNotConfigured) {
		t.Skip(p.err)
	}
	if p.err != nil {
		t.Fatalf("initialize PostgreSQL fixture: %v", p.err)
	}
	db, cleanup, err := p.clone(ctx, t.Name(), func(context.Context, *sql.DB) error { return nil })
	if err != nil {
		t.Fatalf("clone PostgreSQL fixture: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := testContext(t)
		defer cancel()
		if err := cleanup(ctx); err != nil {
			t.Errorf("clean PostgreSQL fixture: %v", err)
		}
	})
	return db
}

func (p *Template) OpenRole(t *testing.T, fixture *sql.DB, role, password string) *sql.DB {
	t.Helper()
	ctx, cancel := testContext(t)
	defer cancel()
	var name string
	if err := fixture.QueryRowContext(ctx, `SELECT current_database()`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if p.server == nil {
		t.Fatal("PostgreSQL template is not initialized")
	}
	p.server.mu.Lock()
	_, owned := p.server.owned[name]
	p.server.mu.Unlock()
	if !owned || name == p.name {
		t.Fatal("PostgreSQL role connection requires an owned fixture")
	}
	fixture.SetMaxIdleConns(0)
	fixture.SetMaxOpenConns(1)
	for {
		var connected bool
		if err := p.server.admin.QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity WHERE datname = $1 AND usename = $2
		)`, name, p.server.config.User).Scan(&connected); err != nil {
			t.Fatalf("wait for PostgreSQL fixture owner backends: %v", err)
		}
		if !connected {
			break
		}
	}
	config := p.server.config.Clone()
	config.Database, config.User, config.Password = name, role, password
	connector, err := fixtureConnector(config)
	if err != nil {
		t.Fatal("configure PostgreSQL fixture role connector")
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close PostgreSQL fixture role connection: %v", err)
		}
	})
	if err := db.PingContext(ctx); err != nil {
		var pgerr *pgconn.PgError
		if errors.As(err, &pgerr) {
			t.Fatalf("connect PostgreSQL fixture role: SQLSTATE %s", pgerr.Code)
		}
		t.Fatal("connect PostgreSQL fixture role")
	}
	return db
}

func testContext(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	deadline, ok := t.Deadline()
	if !ok {
		t.Fatal("PostgreSQL fixtures require a nonzero go test -timeout")
	}
	return context.WithDeadline(context.WithoutCancel(t.Context()), deadline)
}

func cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if deadline, ok := ctx.Deadline(); ok {
		return context.WithDeadline(context.WithoutCancel(ctx), deadline)
	}
	return context.WithCancel(context.WithoutCancel(ctx))
}

func (p *Template) initialize(ctx context.Context, selection string) (resultErr error) {
	s, err := openServer(ctx, selection)
	if err != nil {
		return err
	}
	p.server = s
	complete := false
	defer func() {
		if !complete {
			cleanupCtx, cancel := cleanupContext(ctx)
			defer cancel()
			resultErr = errors.Join(resultErr, p.Close(cleanupCtx))
		}
	}()
	name, err := s.create(ctx, "template0")
	if err != nil {
		return err
	}
	p.name = name
	db, err := s.database(name)
	if err != nil {
		return err
	}
	defer db.Close()
	err = db.PingContext(ctx)
	if err == nil {
		err = p.migrate(ctx, db)
	}
	if err = errors.Join(err, db.Close()); err != nil {
		return fmt.Errorf("migrate PostgreSQL template: %w", err)
	}
	_, err = s.admin.ExecContext(ctx, "ALTER DATABASE "+identifier(name)+" ALLOW_CONNECTIONS false")
	complete = err == nil
	return err
}

func (p *Template) acquire(ctx context.Context, name string) (func(), error) {
	owner, _, _ := strings.Cut(name, "/")
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p.mu.Lock()
		if p.owners[owner] > 0 || len(p.owners) < fixtureLimit {
			p.owners[owner]++
			p.mu.Unlock()
			return func() {
				p.mu.Lock()
				defer p.mu.Unlock()
				p.owners[owner]--
				if p.owners[owner] == 0 {
					delete(p.owners, owner)
					close(p.changed)
					p.changed = make(chan struct{})
				}
			}, nil
		}
		changed := p.changed
		p.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (p *Template) clone(ctx context.Context, owner string, setup func(context.Context, *sql.DB) error) (_ *sql.DB, _ func(context.Context) error, resultErr error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	release, err := p.acquire(ctx, owner)
	if err != nil {
		return nil, nil, err
	}
	name, err := p.server.create(ctx, p.name)
	if err != nil {
		release()
		return nil, nil, err
	}
	db, err := p.server.database(name)
	if err != nil {
		cleanupCtx, cancel := cleanupContext(ctx)
		defer cancel()
		err = errors.Join(err, p.server.drop(cleanupCtx, name))
		release()
		return nil, nil, err
	}
	cleanup := func(ctx context.Context) error {
		err := errors.Join(db.Close(), p.server.drop(ctx, name))
		release()
		return err
	}
	complete := false
	defer func() {
		if !complete {
			cleanupCtx, cancel := cleanupContext(ctx)
			defer cancel()
			resultErr = errors.Join(resultErr, cleanup(cleanupCtx))
		}
	}()
	err = db.PingContext(ctx)
	if err == nil {
		err = setup(ctx, db)
	}
	if err != nil {
		return nil, nil, err
	}
	complete = true
	return db, cleanup, nil
}

func (p *Template) Close(ctx context.Context) error {
	if p.server == nil {
		return nil
	}
	ctx, cancel := cleanupContext(ctx)
	defer cancel()
	var err error
	p.server.mu.Lock()
	names := make([]string, 0, len(p.server.owned))
	for name := range p.server.owned {
		if name != p.name {
			names = append(names, name)
		}
	}
	p.server.mu.Unlock()
	for _, name := range names {
		err = errors.Join(err, p.server.drop(ctx, name))
	}
	if p.name != "" {
		err = errors.Join(err, p.server.drop(ctx, p.name))
		p.name = ""
	}
	err = errors.Join(err, p.server.admin.Close(), p.server.stop(ctx))
	p.server = nil
	return err
}

func (s *server) database(name string) (*sql.DB, error) {
	config := s.config.Clone()
	config.Database = name
	connector, err := fixtureConnector(config)
	if err != nil {
		return nil, fmt.Errorf("configure PostgreSQL fixture connector: %w", err)
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(connectionLimit)
	db.SetMaxIdleConns(connectionLimit)
	return db, nil
}

func identifier(name string) string {
	var quoted bytes.Buffer
	quoted.WriteString(pgx.Identifier{name}.Sanitize())
	return quoted.String()
}

func (s *server) create(ctx context.Context, template string) (string, error) {
	name := "detent_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	s.mu.Lock()
	s.owned[name] = struct{}{}
	s.mu.Unlock()
	_, err := s.admin.ExecContext(ctx, "CREATE DATABASE "+identifier(name)+" TEMPLATE "+identifier(template)+fmt.Sprintf(" CONNECTION LIMIT %d", connectionLimit))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			s.mu.Lock()
			delete(s.owned, name)
			s.mu.Unlock()
			return "", fmt.Errorf("create PostgreSQL fixture: %w", err)
		}
		cleanupCtx, cancel := cleanupContext(ctx)
		defer cancel()
		return "", errors.Join(err, s.drop(cleanupCtx, name))
	}
	return name, nil
}

func (s *server) drop(ctx context.Context, name string) error {
	s.mu.Lock()
	_, owned := s.owned[name]
	s.mu.Unlock()
	if !owned {
		return errors.New("refuse to drop a PostgreSQL database not owned by this fixture")
	}
	_, err := s.admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+identifier(name)+" WITH (FORCE)")
	if err == nil {
		s.mu.Lock()
		delete(s.owned, name)
		s.mu.Unlock()
	}
	return err
}

func connectionConfig(selection string) (*pgtransport.Connection, error) {
	u, err := url.Parse(selection)
	if err != nil || u == nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.User == nil || u.User.Username() == "" || u.Fragment != "" {
		return nil, errors.New("PostgreSQL fixture requires a local postgres://user:password@127.0.0.1:port/postgres URL")
	}
	host := u.Hostname()
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return nil, errors.New("invalid PostgreSQL fixture URL parameters")
	}
	socket := query.Get("host")
	if u.RawQuery != "" && (len(query) != 1 || len(query["host"]) != 1 || !filepath.IsAbs(socket)) {
		return nil, errors.New("PostgreSQL fixture URL permits only an absolute Unix socket host parameter")
	}
	if host == "localhost" {
		host = "127.0.0.1"
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return nil, errors.New("PostgreSQL fixtures refuse non-loopback hosts")
	}
	if u.Port() == "" {
		return nil, errors.New("PostgreSQL fixture URL requires an explicit port")
	}
	for _, entry := range os.Environ() {
		key, value, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "PG") && value != "" {
			return nil, fmt.Errorf("unset ambient PostgreSQL setting %s for isolated fixtures", key)
		}
	}
	port, err := strconv.ParseUint(u.Port(), 10, 16)
	if err != nil {
		return nil, errors.New("invalid PostgreSQL fixture port")
	}
	password, present := u.User.Password()
	if !present || password == "" {
		password = "detent-test-only"
	}
	if socket != "" {
		host = socket
	}
	return &pgtransport.Connection{
		Host:     host,
		Port:     uint16(port),
		Database: "postgres",
		User:     u.User.Username(),
		Password: password,
		SSLMode:  "disable",
	}, nil
}

func openServer(ctx context.Context, selection string) (_ *server, resultErr error) {
	if selection == "" {
		return nil, errNotConfigured
	}
	stop := func(context.Context) error { return nil }
	if selection == "binary" {
		var err error
		selection, stop, err = startBinary(ctx)
		if err != nil {
			return nil, err
		}
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, stop(ctx))
		}
	}()
	config, err := connectionConfig(selection)
	if err != nil {
		return nil, err
	}
	s := &server{config: config, stop: stop, owned: make(map[string]struct{})}
	s.admin, err = s.database("postgres")
	if err != nil {
		return nil, err
	}
	s.admin.SetMaxOpenConns(1)
	s.admin.SetMaxIdleConns(1)
	var version int
	if err := s.admin.QueryRowContext(ctx, "SHOW server_version_num").Scan(&version); err != nil {
		s.admin.Close()
		return nil, errors.New("connect to configured local PostgreSQL fixture server failed")
	}
	major := version / 10000
	if major != 17 && major != 18 {
		s.admin.Close()
		return nil, fmt.Errorf("PostgreSQL fixture requires version 17 or 18, got %d", major)
	}
	return s, nil
}

func startBinary(ctx context.Context) (_ string, _ func(context.Context) error, resultErr error) {
	dir, err := os.MkdirTemp("", "pg-")
	if err != nil {
		return "", nil, err
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, os.RemoveAll(dir))
		}
	}()
	init := exec.CommandContext(ctx, "initdb", "-D", "data", "-U", "detent_test", "--auth=trust", "--no-locale", "--encoding=UTF8")
	init.Dir = dir
	init.Env = []string{"PATH=" + os.Getenv("PATH"), "LC_ALL=C", "TMPDIR=" + os.TempDir()}
	procgroup.Configure(ctx, init)
	if output, err := init.CombinedOutput(); err != nil {
		return "", nil, fmt.Errorf("initialize disposable PostgreSQL: %w: %s", err, output)
	}
	lifetime, owner, err := os.Pipe()
	if err != nil {
		return "", nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, lifetime.Close()) }()
	processCtx, cancel := cleanupContext(ctx)
	const supervise = `
exec 3<&0
"$@" < /dev/null 3<&- &
postmaster=$!
(
	while IFS= read -r mode; do
		if [ "$mode" = fast ]; then
			kill -INT "$postmaster" 2>/dev/null
		fi
	done
	kill -TERM "$postmaster" 2>/dev/null
	kill -QUIT "$postmaster" 2>/dev/null
) <&3 3<&- &
watchdog=$!
exec 3<&-
wait "$postmaster"
status=$?
kill -TERM "$watchdog" 2>/dev/null
wait "$watchdog" 2>/dev/null
exit "$status"
`
	cmd := exec.CommandContext(processCtx, "/bin/sh", "-c", supervise, "pgtest", "postgres", "-D", "data", "-h", "", "-k", ".", "-c", "lc_messages=C", "-c", "max_connections=16", "-c", "logging_collector=off", "-c", "fsync=off", "-c", "synchronous_commit=off", "-c", "full_page_writes=off")
	cmd.Dir = dir
	cmd.Env = init.Env
	cmd.Stdin = lifetime
	procgroup.Configure(processCtx, cmd)
	cmd.Cancel = func() error {
		if err := owner.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			return err
		}
		return nil
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return "", nil, errors.Join(err, owner.Close())
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return "", nil, errors.Join(err, owner.Close())
	}
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- errors.Join(drainStderr(stderr, ready), cmd.Wait())
	}()
	stop := func(ctx context.Context) error {
		ctx, finish := cleanupContext(ctx)
		defer finish()
		defer cancel()
		_, writeErr := io.WriteString(owner, "fast\n")
		select {
		case err := <-done:
			return errors.Join(writeErr, owner.Close(), err, os.RemoveAll(dir))
		case <-ctx.Done():
			cancel()
			<-done
			return errors.Join(ctx.Err(), os.RemoveAll(dir))
		}
	}
	select {
	case <-ready:
		return "postgres://detent_test@127.0.0.1:5432/postgres?host=" + url.QueryEscape(filepath.Join(dir, "data")), stop, nil
	case err := <-done:
		cancel()
		return "", nil, errors.Join(fmt.Errorf("disposable PostgreSQL exited before readiness: %w", err), owner.Close())
	case <-ctx.Done():
		return "", nil, errors.Join(ctx.Err(), stop(ctx))
	}
}

func drainStderr(stderr io.Reader, ready chan struct{}) error {
	reader := bufio.NewReader(stderr)
	for {
		line, err := reader.ReadSlice('\n')
		if bytes.Contains(line, []byte("database system is ready to accept connections")) {
			close(ready)
			_, err := io.Copy(io.Discard, reader)
			return err
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func fixtureConnector(connection pgtransport.Connection) (driver.Connector, error) {
	config, err := connection.PoolConfig()
	if err != nil {
		return nil, err
	}
	return stdlib.GetConnector(*config.ConnConfig), nil
}
