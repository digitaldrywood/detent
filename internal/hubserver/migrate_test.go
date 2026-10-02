package hubserver

import (
	"database/sql"
	"fmt"
	"path/filepath"
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
	}{
		{"valid migration", true, false},
		{"foreign key failure rolls back the version", false, false},
		{"missing earlier migration is refused", true, true},
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
				"00001_fixture.sql": {Data: []byte("-- +goose Up\nCREATE TABLE migration_seed (id INTEGER);\n")},
				fmt.Sprintf("%05d_fixture.sql", supportedSchemaVersion): migration,
			}
			newProvider := func() *goose.Provider {
				t.Helper()
				provider, err := goose.NewProvider(goose.DialectCustom, db, files,
					goose.WithStore(hubMigrationStore{Store: store}),
					goose.WithDisableGlobalRegistry(true), goose.WithSlog(discardLogger()))
				if err != nil {
					t.Fatal(err)
				}
				return provider
			}
			provider := newProvider()
			version, err := applyHubMigrations(t.Context(), db, provider)
			if !test.valid {
				if err == nil || !strings.Contains(err.Error(), "foreign key violations") {
					t.Fatalf("invalid migration error = %v", err)
				}
				var tables int
				if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM sqlite_schema WHERE name = 'migration_child'").Scan(&tables); err != nil || tables != 0 {
					t.Fatalf("failed migration retained its table: count=%d error=%v", tables, err)
				}
				current, currentErr := currentSchemaVersion(t.Context(), db)
				if currentErr != nil || current != 1 {
					t.Fatalf("failed migration advanced schema: version=%d error=%v", current, currentErr)
				}
				var foreignKeys int
				if err := db.QueryRowContext(t.Context(), "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil || foreignKeys != 1 {
					t.Fatalf("failed migration left foreign key enforcement=%d error=%v", foreignKeys, err)
				}
				migration.Data = append(migration.Data, []byte("INSERT INTO migration_parent VALUES (1);\n")...)
				provider = newProvider()
				version, err = applyHubMigrations(t.Context(), db, provider)
			}
			if err != nil || version != supportedSchemaVersion {
				t.Fatalf("migration version=%d error=%v", version, err)
			}
			if test.missingHistory {
				if _, err := db.ExecContext(t.Context(), "DELETE FROM hub_schema_version WHERE version_id = 1"); err != nil {
					t.Fatal(err)
				}
				if _, err := applyHubMigrations(t.Context(), db, newProvider()); err == nil {
					t.Fatal("unchanged maximum version hid missing migration history")
				}
				return
			}
			if _, err := db.ExecContext(t.Context(), "CREATE TEMP VIEW pragma_foreign_key_check AS SELECT * FROM missing_scan_probe"); err != nil {
				t.Fatal(err)
			}
			if version, err := applyHubMigrations(t.Context(), db, newProvider()); err != nil || version != supportedSchemaVersion {
				t.Fatalf("unchanged schema performed a data scan: version=%d error=%v", version, err)
			}
			var foreignKeys int
			if err := db.QueryRowContext(t.Context(), "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil || foreignKeys != 1 {
				t.Fatalf("foreign key enforcement=%d error=%v", foreignKeys, err)
			}
		})
	}
}
