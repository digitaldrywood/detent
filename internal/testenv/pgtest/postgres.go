package pgtest

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/digitaldrywood/detent/internal/procgroup"
)

const Environment = "DETENT_TEST_POSTGRES"
const fixtureLimit = 4
const connectionLimit = 2

var errNotConfigured = errors.New("PostgreSQL 17 fixtures require DETENT_TEST_POSTGRES=binary or a disposable local postgres:// URL")

type Template struct {
	once    sync.Once
	migrate func(context.Context, *sql.DB) error
	server  *server
	name    string
	err     error
	slots   chan struct{}
}

type server struct {
	config *pgx.ConnConfig
	admin  *sql.DB
	stop   func() error
	mu     sync.Mutex
	owned  map[string]struct{}
}

func NewTemplate(migrate func(context.Context, *sql.DB) error) *Template {
	return &Template{migrate: migrate, slots: make(chan struct{}, fixtureLimit)}
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
	db, cleanup, err := p.clone(ctx, func(context.Context, *sql.DB) error { return nil })
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
	db := s.database(name)
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

func (p *Template) clone(ctx context.Context, setup func(context.Context, *sql.DB) error) (_ *sql.DB, _ func(context.Context) error, resultErr error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	select {
	case p.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	name, err := p.server.create(ctx, p.name)
	if err != nil {
		<-p.slots
		return nil, nil, err
	}
	db := p.server.database(name)
	cleanup := func(ctx context.Context) error {
		err := errors.Join(db.Close(), p.server.drop(ctx, name))
		<-p.slots
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
	err = errors.Join(err, p.server.admin.Close(), p.server.stop())
	p.server = nil
	return err
}

func (s *server) database(name string) *sql.DB {
	config := s.config.Copy()
	config.Database = name
	db := stdlib.OpenDB(*config)
	db.SetMaxOpenConns(connectionLimit)
	db.SetMaxIdleConns(connectionLimit)
	return db
}

func identifier(name string) string {
	return pgx.Identifier{name}.Sanitize()
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

func connectionConfig(selection string) (*pgx.ConnConfig, error) {
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
	u.Host = net.JoinHostPort(host, u.Port())
	u.Path = "/postgres"
	password, present := u.User.Password()
	if !present || password == "" {
		password = "detent-test-only"
	}
	u.User = url.UserPassword(u.User.Username(), password)
	u.RawQuery = url.Values{"sslmode": {"disable"}, "passfile": {""}, "application_name": {"detent_test"}}.Encode()
	config, err := pgx.ParseConfig(u.String())
	if err != nil {
		return nil, errors.New("invalid PostgreSQL fixture connection settings")
	}
	if socket != "" {
		config.Host = socket
		config.Fallbacks = nil
	}
	return config, nil
}

func openServer(ctx context.Context, selection string) (_ *server, resultErr error) {
	if selection == "" {
		return nil, errNotConfigured
	}
	stop := func() error { return nil }
	if selection == "binary" {
		var err error
		selection, stop, err = startBinary(ctx)
		if err != nil {
			return nil, err
		}
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, stop())
		}
	}()
	config, err := connectionConfig(selection)
	if err != nil {
		return nil, err
	}
	s := &server{config: config, stop: stop, owned: make(map[string]struct{})}
	s.admin = s.database("postgres")
	s.admin.SetMaxOpenConns(1)
	s.admin.SetMaxIdleConns(1)
	var version int
	if err := s.admin.QueryRowContext(ctx, "SHOW server_version_num").Scan(&version); err != nil {
		s.admin.Close()
		return nil, errors.New("connect to configured local PostgreSQL fixture server failed")
	}
	if version/10000 != 17 {
		s.admin.Close()
		return nil, fmt.Errorf("PostgreSQL fixture requires version 17, got %d", version/10000)
	}
	return s, nil
}

func startBinary(ctx context.Context) (_ string, _ func() error, resultErr error) {
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
	processCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	cmd := exec.CommandContext(processCtx, "postgres", "-D", "data", "-h", "", "-k", ".", "-c", "lc_messages=C", "-c", "max_connections=16", "-c", "logging_collector=off")
	cmd.Dir = dir
	cmd.Env = init.Env
	procgroup.Configure(processCtx, cmd)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return "", nil, err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return "", nil, err
	}
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(stderr)
		var once sync.Once
		for scanner.Scan() {
			if strings.Contains(scanner.Text(), "database system is ready to accept connections") {
				once.Do(func() { close(ready) })
			}
		}
		done <- errors.Join(scanner.Err(), cmd.Wait())
	}()
	stop := func() error {
		cancel()
		<-done
		return os.RemoveAll(dir)
	}
	select {
	case <-ready:
		return "postgres://detent_test@127.0.0.1:5432/postgres?host=" + url.QueryEscape(filepath.Join(dir, "data")), stop, nil
	case err := <-done:
		cancel()
		return "", nil, fmt.Errorf("disposable PostgreSQL exited before readiness: %w", err)
	case <-ctx.Done():
		return "", nil, errors.Join(ctx.Err(), stop())
	}
}
