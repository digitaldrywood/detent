package searchrecords_test

import (
	"database/sql"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"
)

type searchFixture struct {
	repository *postgres.Repository
	objects    *pgstoretest.HistoryObjects
	admin      *sql.DB
	pool       *pgxpool.Pool
}
