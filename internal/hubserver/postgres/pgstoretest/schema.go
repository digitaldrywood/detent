package pgstoretest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pressly/goose/v3"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/testenv/pgtest"
)

var Template = pgtest.NewTemplate(func(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, postgres.RolesSQL()); err != nil {
		return fmt.Errorf("provision PostgreSQL test roles: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, `SET ROLE detent_migration_v2`); err != nil {
		return err
	}
	provider, err := MigrationProvider(db)
	if err != nil {
		return err
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("apply PostgreSQL core migrations: %w", err)
	}
	return nil
})

func MigrationProvider(db *sql.DB) (*goose.Provider, error) {
	migrations, err := postgres.MigrationFiles()
	if err != nil {
		return nil, err
	}
	return goose.NewProvider(goose.DialectPostgres, db, postgres.RoleMigrations(migrations),
		goose.WithAllowOutofOrder(true), goose.WithGoMigrations(postgres.ConversationMigrations(migrations)...), goose.WithDisableGlobalRegistry(true), goose.WithSlog(slog.New(slog.DiscardHandler)))
}

func MigrationDatabaseAtVersion(t *testing.T, version int64) (*sql.DB, *pgtest.Template) {
	t.Helper()
	return migrationDatabase(t, version, false)
}

func RuntimeMigrationDatabaseAtVersion(t *testing.T, version int64) (*sql.DB, *pgtest.Template) {
	t.Helper()
	return migrationDatabase(t, version, true)
}

func migrationDatabase(t *testing.T, version int64, runtime bool) (*sql.DB, *pgtest.Template) {
	t.Helper()
	fixture := pgtest.NewTemplate(func(ctx context.Context, db *sql.DB) error {
		if _, err := db.ExecContext(ctx, postgres.RolesSQL()); err != nil {
			return err
		}
		db.SetMaxOpenConns(1)
		if _, err := db.ExecContext(ctx, "SET ROLE detent_migration_v2"); err != nil {
			return err
		}
		if version == 0 {
			return nil
		}
		provider, err := MigrationProvider(db)
		if err != nil {
			return err
		}
		if _, err := provider.UpTo(ctx, version); err != nil {
			return err
		}
		if runtime {
			for _, required := range []int64{20261117223946, 20261117224903, 20261117235300, 20261118005350} {
				if required > version {
					if _, err := provider.ApplyVersion(ctx, required, true); err != nil {
						return err
					}
				}
			}
		}
		return nil
	})
	t.Cleanup(func() {
		deadline, _ := t.Deadline()
		ctx, cancel := context.WithDeadline(context.WithoutCancel(t.Context()), deadline)
		defer cancel()
		if err := fixture.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return fixture.Database(t), fixture
}

func SeedCore(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, org := range []string{"org_first", "org_second"} {
		if _, err := db.ExecContext(t.Context(), `INSERT INTO organizations(organization_id,name,local,created_at) VALUES($1,$1,false,$2) ON CONFLICT(organization_id) DO NOTHING`, org, HistoryAt()); err != nil {
			t.Fatal(err)
		}
		for _, suffix := range []string{"shared", "other"} {
			for _, statement := range []string{
				`INSERT INTO projects (organization_id,id,name,slug,profile) VALUES ($1,'prj_' || $2,$2,$2,'native')`,
				`INSERT INTO issues (organization_id,id,native_id,project_id,number,title,native_source_key) VALUES ($1,CASE WHEN $2='shared' THEN 41 ELSE 42 END,'wi_' || $2,'prj_' || $2,1,$2,'source_' || $2)`,
				`INSERT INTO native_comments (organization_id,id,project_id,work_item_id,sequence,body_object_key,actor_kind,actor_principal_id,source_key) VALUES ($1,'comment_' || $2,'prj_' || $2,'wi_' || $2,1,$1 || '/body/' || $2,'human','example','source_' || $2)`,
			} {
				if _, err := db.ExecContext(t.Context(), statement, org, suffix); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}

func RequireSQLState(t *testing.T, err error, state string) {
	t.Helper()
	var pgerr *pgconn.PgError
	if !errors.As(err, &pgerr) || pgerr.Code != state {
		t.Fatalf("expected PostgreSQL SQLSTATE %s, got %v", state, err)
	}
}
