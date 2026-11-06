package pgtest

import (
	"database/sql"
	"net"
	"net/url"
	"strconv"
	"testing"

	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/jackc/pgx/v5/pgxpool"
)

func (p *Template) OpenNativeRole(t *testing.T, fixture *sql.DB, role, password string) *pgxpool.Pool {
	t.Helper()
	var name string
	if err := fixture.QueryRowContext(t.Context(), `SELECT current_database()`).Scan(&name); err != nil {
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
	source := p.server.config
	u := &url.URL{Scheme: "postgres", User: url.UserPassword(role, password), Host: net.JoinHostPort("127.0.0.1", strconv.Itoa(int(source.Port))), Path: "/" + name}
	q := url.Values{"sslmode": {"disable"}}
	if source.Host[0] == '/' {
		q.Set("host", source.Host)
	}
	u.RawQuery = q.Encode()
	config, err := pgxpool.ParseConfig(u.String())
	if err != nil {
		t.Fatal("configure native PostgreSQL fixture")
	}
	pgtransport.Configure(config)
	config.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal("open native PostgreSQL fixture")
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(t.Context()); err != nil {
		t.Fatal("connect native PostgreSQL fixture")
	}
	return pool
}
