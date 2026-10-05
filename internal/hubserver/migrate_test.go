package hubserver

import (
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/pressly/goose/v3"
	goosedb "github.com/pressly/goose/v3/database"
)

func TestHubMigrationVerificationScope(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name           string
		valid          bool
		missingHistory bool
		outOfOrder     bool
		newerSchema    bool
	}{
		{"valid migration", true, false, false, false},
		{"foreign key failure rolls back the version", false, false, false, false},
		{"missing earlier migration is refused", true, true, false, false},
		{"out-of-order foreign key failure rolls back the version", false, false, true, false},
		{"newer timestamp schema is refused", true, false, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			db, err := sql.Open("sqlite", sqliteDSN(filepath.Join(t.TempDir(), "hub.db"), defaultBusyTimeout))
			if err != nil {
				t.Fatal(err)
			}
			db.SetMaxOpenConns(1)
			t.Cleanup(func() { _ = db.Close() })
			store, err := goosedb.NewStore(goosedb.DialectSQLite3, hubSchemaTable)
			if err != nil {
				t.Fatal(err)
			}
			migration := &fstest.MapFile{Data: []byte("-- +goose Up\nCREATE TABLE migration_parent (id INTEGER PRIMARY KEY);\nCREATE TABLE migration_child (parent_id INTEGER REFERENCES migration_parent(id));\nINSERT INTO migration_child VALUES (1);\n")}
			if test.valid {
				migration.Data = append(migration.Data, []byte("INSERT INTO migration_parent VALUES (1);\n")...)
			}
			files := fstest.MapFS{
				"00001_fixture.sql":          {Data: []byte("-- +goose Up\nCREATE TABLE migration_seed (id INTEGER);\n")},
				"20261005120000_fixture.sql": migration,
			}
			migrationStore := &hubMigrationStore{Store: store, verifyVersion: -1}
			newProvider := func() *goose.Provider {
				t.Helper()
				provider, err := goose.NewProvider(goose.DialectCustom, db, files,
					goose.WithStore(migrationStore),
					goose.WithAllowOutofOrder(true),
					goose.WithDisableGlobalRegistry(true), goose.WithSlog(discardLogger()))
				if err != nil {
					t.Fatal(err)
				}
				return provider
			}
			provider := newProvider()
			if _, err := db.ExecContext(t.Context(), "CREATE TEMP VIEW pragma_foreign_key_check AS SELECT * FROM missing_scan_probe"); err != nil {
				t.Fatal(err)
			}
			if _, _, err := provider.GetVersions(t.Context()); err != nil {
				t.Fatalf("version-table creation performed a data scan: %v", err)
			}
			if _, err := db.ExecContext(t.Context(), "DROP VIEW pragma_foreign_key_check"); err != nil {
				t.Fatal(err)
			}
			wantVersion := int64(20261005120000)
			previousVersion := int64(1)
			if test.outOfOrder {
				delete(files, "20261005120000_fixture.sql")
				files["20261005120001_fixture.sql"] = &fstest.MapFile{Data: []byte("-- +goose Up\nSELECT 1;\n")}
				if _, err := applyHubMigrations(t.Context(), db, newProvider(), migrationStore); err != nil {
					t.Fatal(err)
				}
				files["20261005120000_fixture.sql"] = migration
				provider = newProvider()
				wantVersion = 20261005120001
				previousVersion = wantVersion
			}
			if test.newerSchema {
				if _, _, err := provider.GetVersions(t.Context()); err != nil {
					t.Fatal(err)
				}
				if _, err := db.ExecContext(t.Context(), "INSERT INTO hub_schema_version(version_id, is_applied) VALUES (20261005120001, 1)"); err != nil {
					t.Fatal(err)
				}
				if _, err := applyHubMigrations(t.Context(), db, provider, migrationStore); !errors.Is(err, ErrUnsupportedSchema) {
					t.Fatalf("newer timestamp schema error = %v", err)
				}
				return
			}
			version, err := applyHubMigrations(t.Context(), db, provider, migrationStore)
			if !test.valid {
				if err == nil || !strings.Contains(err.Error(), "foreign key violations") {
					t.Fatalf("invalid migration error = %v", err)
				}
				var tables int
				if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM sqlite_schema WHERE name = 'migration_child'").Scan(&tables); err != nil || tables != 0 {
					t.Fatalf("failed migration retained its table: count=%d error=%v", tables, err)
				}
				current, currentErr := currentSchemaVersion(t.Context(), db)
				if currentErr != nil || current != previousVersion {
					t.Fatalf("failed migration advanced schema: version=%d error=%v", current, currentErr)
				}
				var foreignKeys int
				if err := db.QueryRowContext(t.Context(), "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil || foreignKeys != 1 {
					t.Fatalf("failed migration left foreign key enforcement=%d error=%v", foreignKeys, err)
				}
				migration.Data = append(migration.Data, []byte("INSERT INTO migration_parent VALUES (1);\n")...)
				provider = newProvider()
				version, err = applyHubMigrations(t.Context(), db, provider, migrationStore)
			}
			if err != nil || version != wantVersion {
				t.Fatalf("migration version=%d error=%v", version, err)
			}
			if test.missingHistory {
				if _, err := db.ExecContext(t.Context(), "DELETE FROM hub_schema_version WHERE version_id = 1"); err != nil {
					t.Fatal(err)
				}
				if _, err := applyHubMigrations(t.Context(), db, newProvider(), migrationStore); err == nil {
					t.Fatal("unchanged maximum version hid missing migration history")
				}
				return
			}
			if _, err := db.ExecContext(t.Context(), "CREATE TEMP VIEW pragma_foreign_key_check AS SELECT * FROM missing_scan_probe"); err != nil {
				t.Fatal(err)
			}
			if version, err := applyHubMigrations(t.Context(), db, newProvider(), migrationStore); err != nil || version != wantVersion {
				t.Fatalf("unchanged schema performed a data scan: version=%d error=%v", version, err)
			}
			var foreignKeys int
			if err := db.QueryRowContext(t.Context(), "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil || foreignKeys != 1 {
				t.Fatalf("foreign key enforcement=%d error=%v", foreignKeys, err)
			}
		})
	}
}

func TestHubTimestampMigrations(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		upgrade    bool
		outOfOrder bool
	}{
		{"fresh database", false, false},
		{"upgrade through 00071", true, false},
		{"older timestamp lands after newer timestamp", true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			db, err := sql.Open("sqlite", sqliteDSN(filepath.Join(t.TempDir(), "hub.db"), defaultBusyTimeout))
			if err != nil {
				t.Fatal(err)
			}
			db.SetMaxOpenConns(1)
			t.Cleanup(func() { _ = db.Close() })
			entries, err := migrationFiles.ReadDir("migrations")
			if err != nil {
				t.Fatal(err)
			}
			files := fstest.MapFS{}
			for _, entry := range entries {
				version, err := goose.NumericComponent(entry.Name())
				if err != nil {
					t.Fatal(err)
				}
				if version > 71 {
					continue
				}
				data, err := migrationFiles.ReadFile("migrations/" + entry.Name())
				if err != nil {
					t.Fatal(err)
				}
				files[entry.Name()] = &fstest.MapFile{Data: data}
			}
			if test.upgrade {
				if version, err := runMigrationsFromFS(t.Context(), db, files, discardLogger()); err != nil || version != 71 {
					t.Fatalf("historical migration version=%d error=%v", version, err)
				}
			}
			if _, err := db.ExecContext(t.Context(), "CREATE TABLE timestamp_marker (value TEXT); INSERT INTO timestamp_marker VALUES ('preserved')"); err != nil {
				t.Fatal(err)
			}
			first := &fstest.MapFile{Data: []byte("-- +goose Up\nCREATE TABLE timestamp_first (value TEXT);\nINSERT INTO timestamp_first SELECT value FROM timestamp_marker;\n")}
			files["20261005120001_second.sql"] = &fstest.MapFile{Data: []byte("-- +goose Up\nCREATE TABLE timestamp_second (value TEXT);\nINSERT INTO timestamp_second SELECT value FROM timestamp_marker;\n")}
			if test.outOfOrder {
				if _, err := runMigrationsFromFS(t.Context(), db, files, discardLogger()); err != nil {
					t.Fatal(err)
				}
			}
			files["20261005120000_first.sql"] = first
			if version, err := runMigrationsFromFS(t.Context(), db, files, discardLogger()); err != nil || version != 20261005120001 {
				t.Fatalf("timestamp migration version=%d error=%v", version, err)
			}
			for _, table := range []string{"timestamp_marker", "timestamp_first", "timestamp_second"} {
				var value string
				if err := db.QueryRowContext(t.Context(), "SELECT value FROM "+table).Scan(&value); err != nil || value != "preserved" {
					t.Fatalf("%s value=%q error=%v", table, value, err)
				}
			}
			rows, err := db.QueryContext(t.Context(), "SELECT version_id FROM hub_schema_version WHERE is_applied = 1 AND version_id > 0 ORDER BY id")
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var versions []int64
			for rows.Next() {
				var version int64
				if err := rows.Scan(&version); err != nil {
					t.Fatal(err)
				}
				versions = append(versions, version)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if want := len(files) + len(hubGoMigrations()); len(versions) != want {
				t.Fatalf("applied versions=%v, want %d entries", versions, want)
			}
			wantTail := []int64{71, 20261005120000, 20261005120001}
			if test.outOfOrder {
				wantTail = []int64{71, 20261005120001, 20261005120000}
			} else if !slices.IsSorted(versions) {
				t.Fatalf("migrations applied out of order: %v", versions)
			}
			if !slices.Equal(versions[len(versions)-3:], wantTail) {
				t.Fatalf("applied versions=%v, want tail %v", versions, wantTail)
			}
		})
	}
}

func supportedSchemaVersion(t *testing.T) int64 {
	t.Helper()
	version, err := latestHubSchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	return version
}
