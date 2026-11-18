package pgstoretest

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/multitracer"
	"github.com/jackc/pgx/v5/pgxpool"
)

type ReportCountingTracer struct {
	Count atomic.Int64
}

func (c *ReportCountingTracer) record(query string) {
	command := strings.ToUpper(strings.Fields(query)[0])
	if command != "BEGIN" && command != "COMMIT" && command != "ROLLBACK" {
		c.Count.Add(1)
	}
}

func (c *ReportCountingTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	c.record(data.SQL)
	return ctx
}

func (c *ReportCountingTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (c *ReportCountingTracer) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	return ctx
}

func (c *ReportCountingTracer) TraceBatchQuery(_ context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	c.record(data.SQL)
}

func (c *ReportCountingTracer) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

func TracedFeaturePool(t *testing.T, source *pgxpool.Pool, tracer pgx.QueryTracer) *pgxpool.Pool {
	t.Helper()
	config := source.Config()
	config.MaxConns = int32(SmallPool().MaxConnections)
	if config.ConnConfig.Tracer == nil {
		config.ConnConfig.Tracer = tracer
	} else {
		config.ConnConfig.Tracer = multitracer.New(config.ConnConfig.Tracer, tracer)
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}
