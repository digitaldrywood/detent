package schemaupgrade_test

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/testenv/pgtest"
)

func hubPostgresMigrationFixture(t *testing.T) *pgtest.Template {
	t.Helper()
	fixture := pgtest.NewTemplate(func(ctx context.Context, db *sql.DB) error {
		_, err := db.ExecContext(ctx, postgres.RolesSQL())
		return err
	})
	t.Cleanup(func() {
		deadline, _ := t.Deadline()
		ctx, cancel := context.WithDeadline(context.WithoutCancel(t.Context()), deadline)
		defer cancel()
		if err := fixture.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return fixture
}

func roleConnection(t *testing.T, template *pgtest.Template, admin *sql.DB, group string) pgtransport.Connection {
	t.Helper()
	var database string
	if err := admin.QueryRowContext(t.Context(), "SELECT current_database()").Scan(&database); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), "ALTER DATABASE "+pgx.Identifier{database}.Sanitize()+" CONNECTION LIMIT 6"); err != nil {
		t.Fatal(err)
	}
	user, password := "example_"+strings.ReplaceAll(uuid.NewString(), "-", ""), uuid.NewString()
	name := pgx.Identifier{user}.Sanitize()
	if _, err := admin.ExecContext(t.Context(), "CREATE ROLE "+name+" LOGIN PASSWORD "+pgtransport.QuoteLiteral(password)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		deadline, ok := t.Deadline()
		if !ok {
			t.Error("PostgreSQL tests require a process deadline")
			return
		}
		ctx, cancel := context.WithDeadline(context.WithoutCancel(t.Context()), deadline)
		defer cancel()
		if _, err := admin.ExecContext(ctx, "DROP OWNED BY "+name); err != nil {
			t.Error(err)
		}
		if _, err := admin.ExecContext(ctx, "DROP ROLE "+name); err != nil {
			t.Error(err)
		}
	})
	if _, err := admin.ExecContext(t.Context(), "GRANT "+pgx.Identifier{group}.Sanitize()+" TO "+name); err != nil {
		t.Fatal(err)
	}
	return template.Connection(t, admin, user, password)
}

func hubMigrationFiles(t *testing.T) fstest.MapFS {
	t.Helper()
	migrations, err := postgres.MigrationFiles()
	if err != nil {
		t.Fatal(err)
	}
	files := fstest.MapFS{}
	entries, err := fs.ReadDir(migrations, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		data, err := fs.ReadFile(migrations, entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		files[entry.Name()] = &fstest.MapFile{Data: data}
	}
	return files
}

func hubSyntheticMigrationVersion(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	var version int64
	if err := db.QueryRowContext(t.Context(), "SELECT coalesce(max(version_id),0) FROM goose_db_version WHERE is_applied AND version_id BETWEEN 20261005000000 AND 20261005235959").Scan(&version); err != nil {
		t.Fatal(err)
	}
	return version
}

type hubPostgresMigrationRow struct {
	id, version int64
	applied     bool
	at          time.Time
}

func hubPostgresMigrationLedger(t *testing.T, db *sql.DB) []hubPostgresMigrationRow {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), "SELECT id,version_id,is_applied,tstamp FROM goose_db_version WHERE version_id>0 ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	var history []hubPostgresMigrationRow
	for rows.Next() {
		var row hubPostgresMigrationRow
		if err := rows.Scan(&row.id, &row.version, &row.applied, &row.at); err != nil {
			t.Fatal(err)
		}
		history = append(history, row)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	return history
}
