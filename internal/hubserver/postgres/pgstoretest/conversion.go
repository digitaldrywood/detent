package pgstoretest

import (
	"bytes"
	"context"
	"database/sql"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/storageconversion"
	"github.com/digitaldrywood/detent/internal/testenv/pgtest"
	"github.com/jackc/pgx/v5/pgxpool"
)

func CoreWorkEventRepository(t *testing.T, db *sql.DB, fixture *pgtest.Template) (*postgres.Repository, *pgxpool.Pool) {
	t.Helper()
	pool := RolePoolFromTemplate(t, fixture, db, "detent_runtime_v2", "")
	repository, err := postgres.NewRepository(t.Context(), pool, SmallPool())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
	return repository, pool
}

func ConvertPayload(_ context.Context, _ storageconversion.Candidate, raw []byte) ([]byte, error) {
	return append(bytes.Clone(raw), '\n'), nil
}
