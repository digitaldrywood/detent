package coreimport_test

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pressly/goose/v3"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/testenv/pgtest"
)

func hubMigrationProvider(db *sql.DB) (*goose.Provider, error) {
	migrations, err := postgres.MigrationFiles()
	if err != nil {
		return nil, err
	}
	return goose.NewProvider(goose.DialectPostgres, db, postgres.RoleMigrations(migrations),
		goose.WithGoMigrations(postgres.ConversationMigrations(migrations)...), goose.WithDisableGlobalRegistry(true), goose.WithSlog(slog.New(slog.DiscardHandler)))
}

func openHubRepository(ctx context.Context, config postgres.Config) (*postgres.Repository, error) {
	migrations, err := postgres.MigrationFiles()
	if err != nil {
		return nil, err
	}
	return postgres.Open(ctx, config, migrations)
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

type keyedDetails struct {
	mu   sync.Mutex
	data map[string][]byte
}

func newKeyedDetails() *keyedDetails {
	return &keyedDetails{data: map[string][]byte{}}
}

func (d *keyedDetails) StoreDetail(_ context.Context, organization string, data []byte) (attachment.ArchiveReference, error) {
	ref, _, err := attachment.PrepareArchive(organization, 1, bytes.NewReader(data))
	if err != nil {
		return ref, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.data[ref.Key] = bytes.Clone(data)
	return ref, nil
}

func (d *keyedDetails) ReadDetail(_ context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	if ref.Validate(organization) != nil {
		return nil, attachment.ErrInvalid
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	data, ok := d.data[ref.Key]
	if !ok {
		return nil, artifact.ErrMissing
	}
	return bytes.Clone(data), nil
}
