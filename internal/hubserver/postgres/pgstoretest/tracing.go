package pgstoretest

import (
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/multitracer"
	"github.com/jackc/pgx/v5/pgxpool"
)

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
