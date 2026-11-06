package pgtest

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
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
		pattern = `(?i)\b(` + strings.Join(names, "|") + `)\b`
	}
	return &TenantGuard{tenants: make(map[*pgx.Conn]string), failed: make(map[*pgx.Conn]bool), protected: regexp.MustCompile(pattern), report: report}
}
func (g *TenantGuard) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, tracedQueryKey{}, tracedQuery{data.SQL, data.Args})
}
func (g *TenantGuard) TraceQueryEnd(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryEndData) {
	query, _ := ctx.Value(tracedQueryKey{}).(tracedQuery)
	g.record(ctx, conn, query.sql, query.args, data.Err)
}
func (g *TenantGuard) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	return ctx
}
func (g *TenantGuard) TraceBatchQuery(ctx context.Context, conn *pgx.Conn, data pgx.TraceBatchQueryData) {
	g.record(ctx, conn, data.SQL, data.Args, data.Err)
}
func (g *TenantGuard) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}
func (g *TenantGuard) record(ctx context.Context, conn *pgx.Conn, query string, args []any, err error) {
	if err != nil {
		var state interface{ SQLState() string }
		if errors.As(err, &state) && state.SQLState() == "25P02" {
			return
		}
	}
	normalized := strings.ToUpper(strings.TrimSpace(query))
	g.mu.Lock()
	defer g.mu.Unlock()
	if err == nil && (strings.HasPrefix(normalized, "BEGIN") || normalized == "COMMIT" || normalized == "ROLLBACK") {
		delete(g.tenants, conn)
		delete(g.failed, conn)
		return
	}
	if strings.Contains(query, "set_config('detent.organization_id', $1, true)") && len(args) > 0 {
		if err != nil {
			g.failed[conn] = true
			return
		}
		tenant, _ := args[0].(string)
		g.tenants[conn] = tenant
		return
	}
	probe, _ := ctx.Value(unboundProbeKey{}).(bool)
	if !probe && !(err != nil && g.failed[conn]) && g.protected.MatchString(query) && g.tenants[conn] == "" {
		g.report("protected PostgreSQL query executed without a bound tenant")
	}
}
