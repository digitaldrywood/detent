package tenantlookup_test

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/pgtransport"
)

func TestTenantTimeoutPolicy(t *testing.T) {
	ctx := t.Context()
	admin := hubtest.Database(t)
	connection := hubtest.PostgresConfig(t, admin).Connection
	config, err := connection.PoolConfig()
	if err != nil {
		t.Fatal(err)
	}
	config.MaxConns = 1
	const query = `SELECT current_setting('statement_timeout'), current_setting('lock_timeout'),
		(SELECT source FROM pg_settings WHERE name='statement_timeout'),
		(SELECT source FROM pg_settings WHERE name='lock_timeout')`
	type settings struct{ statement, lock, statementSource, lockSource string }
	values := func(s *settings) []any { return []any{&s.statement, &s.lock, &s.statementSource, &s.lockSource} }
	base, err := pgx.ConnectConfig(ctx, config.ConnConfig.Copy())
	if err != nil {
		t.Fatal(err)
	}
	var initial settings
	err = base.QueryRow(ctx, query).Scan(values(&initial)...)
	closeErr := base.Close(ctx)
	if err := errors.Join(err, closeErr); err != nil {
		t.Fatal(err)
	}
	t.Logf("migrated runtime login defaults: statement_timeout=%s (%s), lock_timeout=%s (%s)", initial.statement, initial.statementSource, initial.lock, initial.lockSource)
	role := "ALTER ROLE " + pgx.Identifier{connection.User}.Sanitize() + " IN DATABASE " + pgx.Identifier{connection.Database}.Sanitize()
	for _, setting := range []string{"statement_timeout='5s'", "lock_timeout='100ms'"} {
		if _, err := admin.ExecContext(ctx, role+" SET "+setting); err != nil {
			t.Fatal(err)
		}
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	client, err := pgtransport.Adopt(pool, pool)
	if err != nil {
		t.Fatal(err)
	}
	scope := pgtransport.Scope{Organization: "org_example"}
	resolved := settings{"5s", "100ms", "database user", "database user"}
	bound := settings{"5s", "100ms", "session", "session"}
	verify := func(label string, got, expected settings) {
		t.Helper()
		if got != expected {
			t.Errorf("%s timeouts %+v, want %+v", label, got, expected)
		}
	}
	unbound := func(label string) {
		t.Helper()
		var s settings
		if err := pool.QueryRow(ctx, query).Scan(values(&s)...); err != nil {
			t.Fatal(err)
		}
		verify(label, s, resolved)
	}
	unbound("before binding")
	tx, err := client.Begin(ctx, scope, pgtransport.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var explicit settings
	if err := tx.QueryRow(ctx, query).Scan(values(&explicit)...); err != nil {
		t.Fatal(err)
	}
	verify("explicit", explicit, bound)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	unbound("after explicit commit")
	var implicit settings
	if err := client.ReadRow(ctx, scope, query, nil, values(&implicit)...); err != nil {
		t.Fatal(err)
	}
	verify("implicit", implicit, bound)
	unbound("after implicit commit")
	lock, err := admin.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback()
	if _, err := lock.ExecContext(ctx, `LOCK TABLE projects IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	var visible int
	err = client.ReadRow(ctx, scope, `SELECT count(*) FROM projects`, nil, &visible)
	var failure *pgconn.PgError
	if !errors.As(err, &failure) || failure.Code != "55P03" {
		t.Fatalf("protected read waiting on table lock: %v", err)
	}
	unbound("after lock timeout")
}
