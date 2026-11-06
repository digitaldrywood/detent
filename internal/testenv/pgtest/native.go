package pgtest

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/pgtransport"
)

func (p *Template) NativeConfig(t *testing.T, fixture *sql.DB, role, password string) *pgxpool.Config {
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
	source := p.server.config.Clone()
	if role == "" {
		role = source.User
	}
	if password == "" {
		password = source.Password
	}
	source.Database, source.User, source.Password = name, role, password
	config, err := source.PoolConfig()
	if err != nil {
		t.Fatal("configure native PostgreSQL fixture")
	}
	pgtransport.Configure(config)
	config.MaxConns = 1
	var names []string
	readable, lockable := map[string]bool{}, map[string]bool{}
	rows, err := fixture.QueryContext(t.Context(), `WITH applicable AS (
        SELECT p.* FROM pg_policy p
        WHERE 0=ANY(p.polroles) OR EXISTS (
            SELECT 1 FROM pg_roles r WHERE r.oid=ANY(p.polroles) AND pg_has_role($1::name,r.oid,'USAGE')
        )
    ), unrestricted AS (
        SELECT p.polrelid,p.polcmd,pg_get_expr(p.polqual,p.polrelid)='true' AS visible,
            COALESCE(pg_get_expr(p.polwithcheck,p.polrelid),pg_get_expr(p.polqual,p.polrelid))='true' AS writable
        FROM applicable p WHERE p.polpermissive
    )
    SELECT n.nspname||'.'||c.relname,
        EXISTS (SELECT 1 FROM unrestricted p WHERE p.polrelid=c.oid AND p.polcmd='*' AND p.visible AND p.writable)
            AND NOT EXISTS (SELECT 1 FROM applicable p WHERE p.polrelid=c.oid AND NOT p.polpermissive),
        EXISTS (SELECT 1 FROM unrestricted p WHERE p.polrelid=c.oid AND p.polcmd IN ('*','r') AND p.visible)
            AND NOT EXISTS (SELECT 1 FROM applicable p WHERE p.polrelid=c.oid AND p.polcmd IN ('*','r') AND NOT p.polpermissive),
        EXISTS (SELECT 1 FROM unrestricted p WHERE p.polrelid=c.oid AND p.polcmd IN ('*','w') AND p.visible)
            AND NOT EXISTS (SELECT 1 FROM applicable p WHERE p.polrelid=c.oid AND p.polcmd IN ('*','w') AND NOT p.polpermissive)
    FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE c.relrowsecurity`, role)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var all, read, lock bool
		if err := rows.Scan(&name, &all, &read, &lock); err != nil {
			t.Fatal(err)
		}
		if all {
			continue
		}
		names = append(names, name)
		_, relation, _ := strings.Cut(name, ".")
		readable[relation], lockable[relation] = read, read && lock
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	guard := NewTenantGuard(names, func(message string) { t.Error(message) })
	guard.readable, guard.lockable = readable, lockable
	config.ConnConfig.Tracer = guard
	fixture.SetMaxIdleConns(0)
	fixture.SetMaxOpenConns(1)
	return config
}

func (p *Template) OpenNativeRole(t *testing.T, fixture *sql.DB, role, password string) *pgxpool.Pool {
	t.Helper()
	config := p.NativeConfig(t, fixture, role, password)
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

func (p *Template) Connection(t *testing.T, fixture *sql.DB, role, password string) pgtransport.Connection {
	t.Helper()
	config := p.NativeConfig(t, fixture, role, password).ConnConfig
	return pgtransport.Connection{Host: config.Host, Port: config.Port, Database: config.Database, User: config.User, Password: config.Password, SSLMode: "disable"}
}
