package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"reflect"
	"slices"
	"testing"
	"testing/fstest"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/testenv/pgtest"
)

func TestHubMigrationVerificationScope(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name                                           string
		valid, missingHistory, outOfOrder, newerSchema bool
	}{
		{"valid migration", true, false, false, false},
		{"foreign key failure rolls back the version", false, false, false, false},
		{"missing earlier migration is refused", true, true, false, false},
		{"out-of-order foreign key failure rolls back the version", false, false, true, false},
		{"newer timestamp schema is refused", true, false, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			admin := postgresStartupTemplate.Database(t)
			migrationConnection := startupConnection(t, admin, "detent_migration_v2", "")
			runtimeConnection := startupConnection(t, admin, "detent_runtime_v2", "")
			const seedVersion int64 = 20261005115959
			const firstVersion int64 = 20261005120000
			const secondVersion int64 = 20261005120001
			files := hubMigrationFiles(t)
			files["20261005115959_seed.sql"] = &fstest.MapFile{Data: []byte("-- +goose Up\nCREATE TABLE migration_seed(id integer PRIMARY KEY);\nINSERT INTO migration_seed VALUES(1),(2);\n")}
			if err := postgres.Migrate(t.Context(), migrationConnection, files); err != nil {
				t.Fatal(err)
			}
			migration := &fstest.MapFile{Data: []byte("-- +goose Up\nCREATE TABLE migration_parent(id integer PRIMARY KEY);\nCREATE TABLE migration_child(parent_id integer REFERENCES migration_parent(id) DEFERRABLE INITIALLY DEFERRED);\nINSERT INTO migration_child VALUES(1),(2);\n")}
			if test.valid {
				migration.Data = append(migration.Data, []byte("INSERT INTO migration_parent VALUES(1),(2);\n")...)
			}
			previousVersion, wantVersion := seedVersion, firstVersion
			if test.outOfOrder {
				files["20261005120001_later.sql"] = &fstest.MapFile{Data: []byte("-- +goose Up\nSELECT 1;\n")}
				if err := postgres.Migrate(t.Context(), migrationConnection, files); err != nil {
					t.Fatal(err)
				}
				previousVersion, wantVersion = secondVersion, secondVersion
			}
			files["20261005120000_foreign_keys.sql"] = migration
			err := postgres.Migrate(t.Context(), migrationConnection, files)
			if !test.valid {
				var state interface{ SQLState() string }
				if !errors.As(err, &state) || state.SQLState() != "23503" {
					t.Fatalf("invalid migration error=%v", err)
				}
				var absent bool
				if err := admin.QueryRowContext(t.Context(), "SELECT to_regclass('public.migration_child') IS NULL").Scan(&absent); err != nil || !absent {
					t.Fatalf("failed migration retained its table: absent=%t error=%v", absent, err)
				}
				if version := hubSyntheticMigrationVersion(t, admin); version != previousVersion {
					t.Fatalf("failed migration advanced schema: version=%d", version)
				}
				migration.Data = append(migration.Data, []byte("INSERT INTO migration_parent VALUES(1),(2);\n")...)
				err = postgres.Migrate(t.Context(), migrationConnection, files)
			}
			if err != nil {
				t.Fatal(err)
			}
			if current := hubSyntheticMigrationVersion(t, admin); current != wantVersion {
				t.Fatalf("migration version=%d", current)
			}
			if _, err := admin.ExecContext(t.Context(), "INSERT INTO migration_child VALUES(999)"); err == nil {
				t.Fatal("migration disabled foreign key enforcement")
			} else {
				var state interface{ SQLState() string }
				if !errors.As(err, &state) || state.SQLState() != "23503" {
					t.Fatalf("foreign key refusal=%v", err)
				}
			}
			if test.missingHistory {
				if _, err := admin.ExecContext(t.Context(), "DELETE FROM goose_db_version WHERE version_id=$1", seedVersion); err != nil {
					t.Fatal(err)
				}
			}
			if test.newerSchema {
				if _, err := admin.ExecContext(t.Context(), "INSERT INTO goose_db_version(version_id,is_applied) VALUES($1,true)", secondVersion); err != nil {
					t.Fatal(err)
				}
			}
			config := postgres.Config{Connection: runtimeConnection, Pool: postgres.PoolConfig{MaxConnections: 3, ServingConnections: 1, BackgroundConnections: 1}}
			before := hubPostgresMigrationLedger(t, admin)
			repository, err := postgres.Open(t.Context(), config, files)
			if test.missingHistory || test.newerSchema {
				if repository != nil {
					if closeErr := repository.Close(); closeErr != nil {
						t.Error(closeErr)
					}
					t.Error("invalid history returned an open repository")
				}
				if !errors.Is(err, postgres.ErrSchema) {
					t.Fatalf("invalid migration history error=%v", err)
				}
				if !reflect.DeepEqual(hubPostgresMigrationLedger(t, admin), before) {
					t.Fatal("schema refusal rewrote history")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := repository.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), `ALTER TABLE migration_seed OWNER TO CURRENT_USER;
ALTER TABLE migration_parent OWNER TO CURRENT_USER;
ALTER TABLE migration_child OWNER TO CURRENT_USER;
REVOKE ALL ON migration_seed,migration_parent,migration_child FROM detent_migration_v2,detent_runtime_v2`); err != nil {
				t.Fatal(err)
			}
			if err := postgres.Migrate(t.Context(), migrationConnection, files); err != nil {
				t.Fatal(err)
			}
			repository, err = postgres.Open(t.Context(), config, files)
			if err != nil {
				t.Fatalf("unchanged schema tried to read application data: %v", err)
			}
			if err := repository.Close(); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(hubPostgresMigrationLedger(t, admin), before) {
				t.Fatal("restart repeated or rewrote migration history")
			}
		})
	}
}

func TestCurrentSchemaFingerprintUsesLatestMigrationState(t *testing.T) {
	t.Parallel()
	admin := postgresStartupTemplate.Database(t)
	config := startupConfig(startupConnection(t, admin, "detent_runtime_v2", ""))
	before, err := openPostgresDatabase(t.Context(), *config.Postgres)
	if err != nil {
		t.Fatal(err)
	}
	version, fingerprint := before.schemaVersion, before.schemaFingerprint
	if err := before.Close(); err != nil {
		t.Fatal(err)
	}
	if version == 0 || fingerprint == "" {
		t.Fatal("validated schema metadata is empty")
	}
	var earlierVersion int64
	if err := admin.QueryRowContext(t.Context(), "SELECT min(version_id) FROM public.goose_db_version WHERE version_id > 0 AND is_applied").Scan(&earlierVersion); err != nil {
		t.Fatal(err)
	}
	if earlierVersion <= 0 || earlierVersion >= version {
		t.Fatalf("earlier migration = %d, want a positive version before %d", earlierVersion, version)
	}
	for _, changedVersion := range []int64{earlierVersion, version} {
		for _, applied := range []bool{false, true} {
			if _, err := admin.ExecContext(t.Context(), "INSERT INTO public.goose_db_version(version_id,is_applied) VALUES($1,$2)", changedVersion, applied); err != nil {
				t.Fatal(err)
			}
			reopened, err := openPostgresDatabase(t.Context(), *config.Postgres)
			if !applied {
				if reopened != nil {
					if closeErr := reopened.Close(); closeErr != nil {
						t.Error(closeErr)
					}
					t.Error("latest Down state returned an open database")
				}
				if !errors.Is(err, postgres.ErrSchema) {
					t.Fatalf("migration %d latest Down state: %v, want schema refusal", changedVersion, err)
				}
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			if reopened.schemaVersion != version || reopened.schemaFingerprint != fingerprint {
				t.Errorf("restored schema metadata=(%d,%q), want (%d,%q)", reopened.schemaVersion, reopened.schemaFingerprint, version, fingerprint)
			}
			if err := reopened.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestHubTimestampMigrations(t *testing.T) {
	t.Parallel()
	fixture := hubPostgresMigrationFixture(t)
	for _, test := range []struct {
		name                string
		upgrade, outOfOrder bool
	}{
		{"fresh database", false, false},
		{"upgrade from earlier PostgreSQL timestamp", true, false},
		{"older timestamp lands after newer timestamp", true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			admin := fixture.Database(t)
			connection := startupConnection(t, admin, "detent_migration_v2", "")
			files := fstest.MapFS{"20261005110000_seed.sql": {Data: []byte("-- +goose Up\nCREATE TABLE timestamp_marker(id integer PRIMARY KEY,value text NOT NULL);\nINSERT INTO timestamp_marker VALUES(1,'preserved'),(2,'second preserved');\n")}}
			if test.upgrade {
				if err := postgres.Migrate(t.Context(), connection, files); err != nil {
					t.Fatal(err)
				}
			}
			files["20261005120001_second.sql"] = &fstest.MapFile{Data: []byte("-- +goose Up\nCREATE TABLE timestamp_second AS SELECT * FROM timestamp_marker;\n")}
			if test.outOfOrder {
				if err := postgres.Migrate(t.Context(), connection, files); err != nil {
					t.Fatal(err)
				}
			}
			files["20261005120000_first.sql"] = &fstest.MapFile{Data: []byte("-- +goose Up\nCREATE TABLE timestamp_first AS SELECT * FROM timestamp_marker;\n")}
			if err := postgres.Migrate(t.Context(), connection, files); err != nil {
				t.Fatal(err)
			}
			for _, table := range []string{"timestamp_marker", "timestamp_first", "timestamp_second"} {
				rows, err := admin.QueryContext(t.Context(), "SELECT id,value FROM "+table+" ORDER BY id")
				if err != nil {
					t.Fatal(err)
				}
				var ids []int
				var values []string
				for rows.Next() {
					var id int
					var value string
					if err := rows.Scan(&id, &value); err != nil {
						t.Fatal(err)
					}
					ids = append(ids, id)
					values = append(values, value)
				}
				if err := errors.Join(rows.Err(), rows.Close()); err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(ids, []int{1, 2}) || !slices.Equal(values, []string{"preserved", "second preserved"}) {
					t.Fatalf("%s changed preserved data: ids=%v values=%v", table, ids, values)
				}
			}
			history := hubPostgresMigrationLedger(t, admin)
			var versions []int64
			for _, row := range history {
				versions = append(versions, row.version)
			}
			want := []int64{20261005110000, 20261005120000, 20261005120001}
			if test.outOfOrder {
				want = []int64{20261005110000, 20261005120001, 20261005120000}
			}
			if len(versions) != len(files) || !slices.Equal(versions, want) {
				t.Fatalf("applied versions=%v want=%v", versions, want)
			}
			if err := postgres.Migrate(t.Context(), connection, files); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(hubPostgresMigrationLedger(t, admin), history) {
				t.Fatal("timestamp restart rewrote migration history")
			}
		})
	}
}

func hubMigrationFiles(t *testing.T) fstest.MapFS {
	t.Helper()
	migrations, err := fs.Sub(migrationFiles, "postgres/migrations")
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

func hubPostgresMigrationFixture(t *testing.T) *pgtest.Template {
	t.Helper()
	fixture := pgtest.NewTemplate(func(ctx context.Context, db *sql.DB) error {
		roles, err := os.ReadFile("postgres/roles.sql")
		if err != nil {
			return err
		}
		_, err = db.ExecContext(ctx, string(roles))
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
