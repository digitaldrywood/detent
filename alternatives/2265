package hubfixture

import (
	"database/sql"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
)

type databaseTemplate struct{}

var Template databaseTemplate

func (databaseTemplate) Database(t *testing.T) *sql.DB {
	t.Helper()
	return hubtest.Database(t)
}

func (databaseTemplate) NativeConfig(t *testing.T, admin *sql.DB, role, password string) *pgxpool.Config {
	t.Helper()
	return hubtest.NativeConfig(t, admin, role, password)
}

func RepositoryWithAdmin(t *testing.T, maxConnections int, configure ...func(*pgxpool.Config)) (*postgres.Repository, *sql.DB) {
	t.Helper()
	admin := hubtest.Database(t)
	options := []hubtest.Option{hubtest.WithMaxConnections(max(maxConnections, hubtest.DefaultMaxConnections))}
	for _, apply := range configure {
		options = append(options, hubtest.WithPoolConfig(apply))
	}
	return hubtest.RepositoryOn(t, admin, options...), admin
}
