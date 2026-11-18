package tenantlookup_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/testenv/pgtest"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type poolProbe struct {
	name      string
	onAcquire func()
	pid       uint32
}

type poolProbeKey struct{}
type poolTrace struct{ t *testing.T }

func (poolTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}
func (poolTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
func (poolTrace) TraceAcquireStart(ctx context.Context, _ *pgxpool.Pool, _ pgxpool.TraceAcquireStartData) context.Context {
	if probe, ok := ctx.Value(poolProbeKey{}).(*poolProbe); ok && probe.onAcquire != nil {
		probe.onAcquire()
	}
	return ctx
}

const unboundProjects = `SELECT pg_backend_pid(), count(*), coalesce(current_setting('detent.organization_id',true),'') FROM projects WHERE id=$1`

func checkUnbound(t *testing.T, row pgx.Row, wantPID uint32) {
	t.Helper()
	var pid uint32
	var visible int
	var scope string
	if err := row.Scan(&pid, &visible, &scope); err != nil || pid == 0 || pid != wantPID || visible != 0 || scope != "" {
		t.Errorf("unbound PID %d (want %d) exposed %d rows, scope %q: %v", pid, wantPID, visible, scope, err)
	}
}

func (tr poolTrace) TraceAcquireEnd(ctx context.Context, _ *pgxpool.Pool, data pgxpool.TraceAcquireEndData) {
	if probe, ok := ctx.Value(poolProbeKey{}).(*poolProbe); ok && data.Conn != nil {
		probe.pid = data.Conn.PgConn().PID()
		if probe.onAcquire != nil {
			tr.t.Logf("unbound after cancellation backend PID %d", probe.pid)
			checkUnbound(tr.t, data.Conn.QueryRow(pgtest.UnboundProbe(ctx), unboundProjects, "shared"), probe.pid)
		}
		tr.t.Logf("%s backend PID %d", probe.name, probe.pid)
	}
}

func TestReadRowPoolIsolation(t *testing.T) {
	ctx := testenv.WaitContext(t, t.Context())
	admin := hubtest.Database(t)
	repo := hubtest.RepositoryOn(t, admin)
	config, err := hubtest.PostgresConfig(t, admin).Connection.PoolConfig()
	if err != nil {
		t.Fatal(err)
	}
	config.MaxConns = 1
	config.ConnConfig.Tracer = poolTrace{t}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	clients := map[tracker.OrganizationID]*pgtransport.Pools{}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		hubtest.Organization(t, repo, hubtest.OrganizationOptions{ID: org})
		hubtest.Project(t, repo, org, hubtest.ProjectOptions{ID: "shared", Name: "payload-" + string(org)})
		client, err := pgtransport.Adopt(pool, pool)
		if err != nil {
			t.Fatal(err)
		}
		clients[org] = client
	}
	var restricted bool
	if err := pool.QueryRow(ctx, `SELECT row_security_active('public.projects') AND pg_has_role(current_user,'detent_runtime_v2','member') AND NOT rolsuper AND NOT rolbypassrls FROM pg_roles WHERE rolname=current_user`).Scan(&restricted); err != nil || !restricted {
		t.Fatalf("runtime login with active project RLS = %t, error %v", restricted, err)
	}
	const read = `SELECT pg_backend_pid(), string_agg(name, ',' ORDER BY name) FROM projects WHERE id=$1`
	bound := func(org tracker.OrganizationID, onAcquire func()) uint32 {
		t.Helper()
		probe := poolProbe{name: "bound " + string(org), onAcquire: onAcquire}
		var payload string
		var pid uint32
		err := clients[org].ReadRow(context.WithValue(ctx, poolProbeKey{}, &probe), pgtransport.Scope{Organization: org}, read, []any{"shared"}, &pid, &payload)
		if err != nil || pid == 0 || pid != probe.pid || payload != "payload-"+string(org) {
			t.Fatalf("%s PID %d payload %q: %v", org, pid, payload, err)
		}
		return pid
	}
	unbound := func(after string) uint32 {
		t.Helper()
		probe := poolProbe{name: "unbound after " + after}
		row := pool.QueryRow(pgtest.UnboundProbe(context.WithValue(ctx, poolProbeKey{}, &probe)), unboundProjects, "shared")
		checkUnbound(t, row, probe.pid)
		return probe.pid
	}
	reused := func(got, want uint32) {
		t.Helper()
		if got != want {
			t.Fatalf("backend PID %d, want reuse of %d", got, want)
		}
	}
	first := pgtransport.Scope{Organization: "org_first"}
	pid := bound(first.Organization, nil)
	reused(unbound("success"), pid)
	reused(bound("org_second", nil), pid)
	reused(unbound("second success"), pid)
	sqlError := poolProbe{name: "SQL error"}
	var number int
	err = clients[first.Organization].ReadRow(context.WithValue(ctx, poolProbeKey{}, &sqlError), first, `SELECT 1/(length(name)-length(name)) FROM projects WHERE id=$1`, []any{"shared"}, &number)
	var failure *pgconn.PgError
	if !errors.As(err, &failure) || failure.Code != "22012" {
		t.Fatalf("division by zero: %v", err)
	}
	reused(sqlError.pid, pid)
	reused(unbound("SQL error"), pid)
	reused(bound("org_second", nil), pid)
	reused(unbound("success before early close"), pid)
	early := poolProbe{name: "early close"}
	var payload string
	err = clients[first.Organization].ReadRow(context.WithValue(ctx, poolProbeKey{}, &early), first, `SELECT name,n FROM projects CROSS JOIN generate_series(1,3) AS n WHERE id=$1 ORDER BY n`, []any{"shared"}, &payload, &number)
	if err != nil || number != 1 || payload != "payload-org_first" {
		t.Fatalf("first of multiple rows: %q, %d, %v", payload, number, err)
	}
	reused(early.pid, pid)
	reused(unbound("early close"), pid)
	reused(bound("org_second", nil), pid)
	reused(unbound("success before cancellation"), pid)

	lock, err := admin.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback()
	if _, err := lock.ExecContext(ctx, `SELECT pg_advisory_xact_lock(728)`); err != nil {
		t.Fatal(err)
	}
	blocked := poolProbe{name: "blocked read"}
	cancelCtx, cancel := context.WithCancel(context.WithValue(ctx, poolProbeKey{}, &blocked))
	var workers sync.WaitGroup
	var canceledErr error
	workers.Go(func() {
		var name string
		canceledErr = clients[first.Organization].ReadRow(cancelCtx, first, `SELECT name FROM projects CROSS JOIN pg_advisory_xact_lock_shared(728) WHERE id=$1`, []any{"shared"}, &name)
	})
	defer func() {
		cancel()
		lock.Rollback()
		workers.Wait()
	}()
	testenv.WaitUntil(t, ctx, func() error {
		var waiting bool
		if err := lock.QueryRowContext(ctx, `SELECT cardinality(pg_blocking_pids($1))>0`, pid).Scan(&waiting); err != nil {
			return err
		}
		if !waiting {
			return errors.New("read has not reached lock wait")
		}
		return nil
	})
	nextPID := bound("org_second", cancel)
	workers.Wait()
	if !errors.Is(canceledErr, context.Canceled) {
		t.Fatalf("blocked read returned %v", canceledErr)
	}
	reused(blocked.pid, pid)
	reused(unbound("success after cancellation"), nextPID)
	reused(bound("org_first", nil), nextPID)
	reused(unbound("final success"), nextPID)
}
