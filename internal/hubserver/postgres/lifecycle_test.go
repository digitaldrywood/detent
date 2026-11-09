package postgres

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/digitaldrywood/detent/internal/pgtransport"

	"github.com/google/uuid"

	"github.com/digitaldrywood/detent/internal/testenv/pgtest"
)

func TestMigrateAppliesOlderPendingVersions(t *testing.T) {
	const first = "20261104090048_current_change_review_lookups.sql"
	const second = "20261104114549_current_machine_review_heads.sql"
	source := os.DirFS("migrations")
	files, err := fs.Glob(source, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	recovery := fstest.MapFS{}
	for _, name := range files {
		if name == first || name == second || name[:14] > "20261104214056" {
			continue
		}
		data, err := fs.ReadFile(source, name)
		if err != nil {
			t.Fatal(err)
		}
		recovery[name] = &fstest.MapFile{Data: data}
	}
	if len(recovery) != 112 {
		t.Fatalf("Recovery66 migrations = %d, want 112", len(recovery))
	}
	fixture := migratedTemplate(t, recovery, nil)
	for _, interrupted := range []bool{false, true} {
		name := "upgrade"
		if interrupted {
			name = "resume partial nontransactional migration"
		}
		t.Run(name, func(t *testing.T) {
			db := fixture.Database(t)
			config := migrationTestLogin(t, db, MigrationRoleName)
			var count int
			var version int64
			if err := db.QueryRowContext(t.Context(), "SELECT count(*), max(version_id) FROM goose_db_version WHERE version_id > 0 AND is_applied").Scan(&count, &version); err != nil {
				t.Fatal(err)
			}
			if count != 112 || version != 20261104214056 {
				t.Fatalf("Recovery66 ledger = %d/%d", count, version)
			}
			if err := validateSchema(t.Context(), db, source); !errors.Is(err, ErrSchema) {
				t.Fatalf("incomplete schema validation = %v", err)
			}
			if interrupted {
				partial := fstest.MapFS{}
				for _, file := range files {
					data, err := fs.ReadFile(source, file)
					if err != nil {
						t.Fatal(err)
					}
					if file == first {
						data = []byte(strings.Replace(string(data), "ALTER TABLE change_requests ADD COLUMN", "SELECT 1/0;\nALTER TABLE change_requests ADD COLUMN", 1))
					}
					partial[file] = &fstest.MapFile{Data: data}
				}
				err := Migrate(t.Context(), config, partial)
				var pgerr *pgconn.PgError
				if !errors.As(err, &pgerr) || pgerr.Code != "22012" {
					t.Fatalf("partial migration error = %v, want division by zero", err)
				}
				if err := validateSchema(t.Context(), db, source); !errors.Is(err, ErrSchema) {
					t.Fatalf("partial schema validation = %v", err)
				}
			}
			if err := Migrate(t.Context(), config, source); err != nil {
				t.Fatalf("upgrade Recovery66: %v", err)
			}
			if err := validateSchema(t.Context(), db, source); err != nil {
				t.Fatal(err)
			}
			rows, err := db.QueryContext(t.Context(), "SELECT version_id FROM goose_db_version WHERE version_id IN (20261104090048,20261104114549) AND is_applied ORDER BY id")
			if err != nil {
				t.Fatal(err)
			}
			var applied []int64
			for rows.Next() {
				var version int64
				if err := rows.Scan(&version); err != nil {
					t.Fatal(err)
				}
				applied = append(applied, version)
			}
			if err := errors.Join(rows.Err(), rows.Close()); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(applied, []int64{20261104090048, 20261104114549}) {
				t.Fatalf("older migration application order = %v", applied)
			}
			for _, check := range []struct{ name, query string }{
				{"rewritten owner and forced RLS", `SELECT pg_get_userbyid(relowner)='detent_migration_v2' AND relrowsecurity AND relforcerowsecurity FROM pg_class WHERE oid='current_change_reviews'::regclass`},
				{"rewritten grants", `SELECT bool_and(has_table_privilege('detent_runtime_v2','current_change_reviews',privilege)) AND NOT has_table_privilege('detent_platform_v2','current_change_reviews','SELECT') FROM unnest(ARRAY['SELECT','INSERT','UPDATE','DELETE']) AS privilege`},
				{"rewritten policy", `SELECT polroles=ARRAY['detent_runtime_v2'::regrole::oid,'detent_migration_v2'::regrole::oid] OR polroles=ARRAY['detent_migration_v2'::regrole::oid,'detent_runtime_v2'::regrole::oid] FROM pg_policy WHERE polrelid='current_change_reviews'::regclass AND polname='tenant_isolation'`},
				{"review constraints", `SELECT count(*)=5 AND bool_and(convalidated) FROM pg_constraint WHERE conrelid='current_change_reviews'::regclass AND contype IN ('p','f','c')`},
				{"replacement uniqueness", `SELECT NOT EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid='change_evidence'::regclass AND conname='change_evidence_organization_id_change_id_kind_source_key_key') AND EXISTS(SELECT 1 FROM pg_index WHERE indexrelid='change_evidence_source_identity_idx'::regclass AND indisunique AND indpred IS NOT NULL)`},
				{"concurrent indexes valid", `SELECT count(*)=5 AND bool_and(indisvalid AND indisready) FROM pg_index WHERE indexrelid IN ('change_evidence_source_identity_idx'::regclass,'current_change_human_decisions_idx'::regclass,'current_change_prior_approval_idx'::regclass,'current_change_review_pending_idx'::regclass,'change_current_machine_reviews_idx'::regclass)`},
			} {
				var valid bool
				if err := db.QueryRowContext(t.Context(), check.query).Scan(&valid); err != nil {
					t.Fatalf("%s: %v", check.name, err)
				}
				if !valid {
					t.Error(check.name)
				}
			}
			var before, after int
			if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM goose_db_version").Scan(&before); err != nil {
				t.Fatal(err)
			}
			if err := Migrate(t.Context(), config, source); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM goose_db_version").Scan(&after); err != nil {
				t.Fatal(err)
			}
			if after != before {
				t.Fatalf("idempotent migration changed ledger: %d -> %d", before, after)
			}
			runtime := migrationTestLogin(t, db, RuntimeRoleName)
			repository, err := Open(t.Context(), Config{Connection: runtime, Pool: PoolConfig{MaxConnections: 3, ServingConnections: 1, BackgroundConnections: 1}}, source)
			if err != nil {
				t.Fatalf("open upgraded schema: %v", err)
			}
			if err := repository.Close(); err != nil {
				t.Fatal(err)
			}
			missing := fstest.MapFS{}
			for _, file := range files {
				if file == first {
					continue
				}
				data, err := fs.ReadFile(source, file)
				if err != nil {
					t.Fatal(err)
				}
				missing[file] = &fstest.MapFile{Data: data}
			}
			if err := validateSchema(t.Context(), db, missing); !errors.Is(err, ErrSchema) {
				t.Fatalf("missing source validation = %v", err)
			}
			repository, err = Open(t.Context(), Config{Connection: runtime, Pool: PoolConfig{MaxConnections: 3, ServingConnections: 1, BackgroundConnections: 1}}, missing)
			if repository != nil {
				if err := repository.Close(); err != nil {
					t.Error(err)
				}
				t.Fatal("startup accepted applied older version absent from source")
			}
			if !errors.Is(err, ErrSchema) {
				t.Fatalf("missing source startup = %v", err)
			}
		})
	}
}

func migratedTemplate(t *testing.T, migrations fs.FS, seed func(context.Context, *sql.DB) error) *pgtest.Template {
	t.Helper()
	fixture := pgtest.NewTemplate(func(ctx context.Context, db *sql.DB) error {
		roles, err := os.ReadFile("roles.sql")
		if err != nil {
			return err
		}
		if _, err := db.ExecContext(ctx, string(roles)); err != nil {
			return err
		}
		db.SetMaxOpenConns(1)
		if _, err := db.ExecContext(ctx, "SET ROLE detent_migration_v2"); err != nil {
			return err
		}
		provider, err := migrationProvider(db, migrations)
		if err != nil {
			return err
		}
		if _, err := provider.Up(ctx); err != nil {
			return err
		}
		if seed == nil {
			return nil
		}
		if _, err := db.ExecContext(ctx, "RESET ROLE"); err != nil {
			return err
		}
		return seed(ctx, db)
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

func migrationTestLogin(t *testing.T, db *sql.DB, group string) pgtransport.Connection {
	t.Helper()
	var config pgtransport.Connection
	if os.Getenv(pgtest.Environment) == "binary" {
		var directory string
		if err := db.QueryRowContext(t.Context(), "SELECT current_setting('data_directory'), current_setting('unix_socket_directories'), current_setting('port')::integer").Scan(&directory, &config.Host, &config.Port); err != nil {
			t.Fatal(err)
		}
		if !filepath.IsAbs(config.Host) {
			config.Host = filepath.Join(directory, config.Host)
		}
	} else {
		var err error
		config, err = pgtransport.ParseConnection(os.Getenv(pgtest.Environment))
		if err != nil {
			t.Fatal("parse isolated PostgreSQL connection")
		}
	}
	config.SSLMode = "disable"
	config.User = "example_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	config.Password = uuid.NewString()
	if err := db.QueryRowContext(t.Context(), "SELECT current_database()").Scan(&config.Database); err != nil {
		t.Fatal(err)
	}
	quoted := pgx.Identifier{config.User}.Sanitize()
	if _, err := db.ExecContext(t.Context(), "ALTER DATABASE "+pgx.Identifier{config.Database}.Sanitize()+" CONNECTION LIMIT 8"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), "CREATE ROLE "+quoted+" LOGIN PASSWORD "+pgtransport.QuoteLiteral(config.Password)+" IN ROLE "+pgx.Identifier{group}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		deadline, _ := t.Deadline()
		ctx, cancel := context.WithDeadline(context.WithoutCancel(t.Context()), deadline)
		defer cancel()
		if _, err := db.ExecContext(ctx, "DROP OWNED BY "+quoted); err != nil {
			t.Error(err)
		}
		if _, err := db.ExecContext(ctx, "DROP ROLE "+quoted); err != nil {
			t.Error(err)
		}
	})
	return config
}

func TestSchemaRequiresMigrationSourcesBeforeDatabase(t *testing.T) {
	for _, source := range []fs.FS{nil, fstest.MapFS{}} {
		if err := validateNativeSchema(t.Context(), nil, source); !errors.Is(err, ErrSchema) {
			t.Fatalf("missing migration sources=%v", err)
		}
	}
}
