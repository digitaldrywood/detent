package schemaupgrade_test

import (
	"errors"
	"reflect"
	"slices"
	"testing"
	"testing/fstest"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"
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
			admin := pgstoretest.Template.Database(t)
			migrationConnection := roleConnection(t, pgstoretest.Template, admin, "detent_migration_v2")
			runtimeConnection := roleConnection(t, pgstoretest.Template, admin, "detent_runtime_v2")
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
			connection := roleConnection(t, fixture, admin, "detent_migration_v2")
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
