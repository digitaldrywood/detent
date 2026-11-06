package pgtest

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type TenantGuard struct {
	mu        sync.Mutex
	tenants   map[*pgx.Conn]string
	failed    map[*pgx.Conn]bool
	protected *regexp.Regexp
	report    func(string)
}

type unboundProbeKey struct{}

func UnboundProbe(ctx context.Context) context.Context {
	return context.WithValue(ctx, unboundProbeKey{}, true)
}

type tracedQuery struct {
	sql  string
	args []any
}
type tracedQueryKey struct{}

func NewTenantGuard(relations []string, report func(string)) *TenantGuard {
	names := make([]string, 0, len(relations))
	for _, relation := range relations {
		_, name, found := strings.Cut(relation, ".")
		if !found {
			name = relation
		}
		names = append(names, regexp.QuoteMeta(name))
	}
	pattern := "a^"
	if len(names) > 0 {
		pattern = `(?i)\b(?:FROM|JOIN|UPDATE|INTO|TABLE)\s+(?:public\.)?"?(` + strings.Join(names, "|") + `)\b`
	}
	return &TenantGuard{tenants: make(map[*pgx.Conn]string), failed: make(map[*pgx.Conn]bool), protected: regexp.MustCompile(pattern), report: report}
}
func (g *TenantGuard) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, tracedQueryKey{}, tracedQuery{data.SQL, data.Args})
}
func (g *TenantGuard) TraceQueryEnd(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryEndData) {
	query, ok := ctx.Value(tracedQueryKey{}).(tracedQuery)
	if !ok {
		g.report("PostgreSQL query trace context is missing")
		return
	}
	g.record(ctx, conn, query.sql, query.args, data.CommandTag, data.Err)
	g.expire(conn)
}
func (g *TenantGuard) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	return ctx
}
func (g *TenantGuard) TraceBatchQuery(ctx context.Context, conn *pgx.Conn, data pgx.TraceBatchQueryData) {
	g.record(ctx, conn, data.SQL, data.Args, data.CommandTag, data.Err)
}
func (g *TenantGuard) TraceBatchEnd(_ context.Context, conn *pgx.Conn, _ pgx.TraceBatchEndData) {
	g.expire(conn)
}
func (g *TenantGuard) record(ctx context.Context, conn *pgx.Conn, query string, args []any, tag pgconn.CommandTag, err error) {
	if err != nil {
		var state interface{ SQLState() string }
		if errors.As(err, &state) && state.SQLState() == "25P02" {
			return
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if err == nil && (tag.String() == "BEGIN" || tag.String() == "COMMIT" || tag.String() == "ROLLBACK") {
		delete(g.tenants, conn)
		delete(g.failed, conn)
		return
	}
	if strings.Contains(query, "set_config('detent.organization_id', $1, true)") && len(args) > 0 {
		if err != nil {
			g.failed[conn] = true
			return
		}
		tenant, ok := args[0].(string)
		if !ok {
			delete(g.tenants, conn)
			return
		}
		g.tenants[conn] = tenant
		return
	}
	probe, hasProbe := ctx.Value(unboundProbeKey{}).(bool)
	if (!hasProbe || !probe) && (err == nil || !g.failed[conn]) && g.protected.MatchString(query) && g.tenants[conn] == "" {
		g.report("protected PostgreSQL query executed without a bound tenant")
	}
}

func (g *TenantGuard) expire(conn *pgx.Conn) {
	if conn.PgConn().TxStatus() != 'I' && !conn.IsClosed() {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.tenants, conn)
	delete(g.failed, conn)
}
