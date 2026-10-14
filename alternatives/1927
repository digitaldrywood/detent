package hubserver

import (
	"database/sql"
	"fmt"
	"io/fs"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/pressly/goose/v3"
)

func TestRunnerLocalChecksMigrationPreservesArchive(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	t.Parallel()
	for _, version := range []int64{43, 44, 51} {
		t.Run(strconv.FormatInt(version, 10), func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "hub.db")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			if _, err := db.ExecContext(t.Context(), fmt.Sprintf("PRAGMA application_id = %d", hubApplicationID)); err != nil {
				t.Fatal(err)
			}
			migrations, err := fs.Sub(migrationFiles, "migrations")
			if err != nil {
				t.Fatal(err)
			}
			provider, err := goose.NewProvider(goose.DialectSQLite3, db, migrations,
				goose.WithDisableGlobalRegistry(true), goose.WithTableName(hubSchemaTable),
				goose.WithSlog(discardLogger()), goose.WithGoMigrations(hubGoMigrations()...))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := provider.UpTo(t.Context(), 7); err != nil {
				t.Fatal(err)
			}
			_, issue := seedProjection(t, db)
			if _, err := provider.UpTo(t.Context(), version); err != nil {
				t.Fatal(err)
			}
			wantArchived := "0"
			if version >= 44 {
				if _, err := db.ExecContext(t.Context(), "UPDATE issues SET archived = 1 WHERE id = ?", issue); err != nil {
					t.Fatal(err)
				}
				wantArchived = "1"
			}
			if version == 51 {
				if _, err := db.ExecContext(t.Context(), "INSERT INTO onboarding_issue_intake(project_id, request_json) SELECT project_id, '{\"status\":\"preview\"}' FROM issues WHERE id = ?", issue); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			cfg := Config{DatabasePath: path}
			service := openTestService(t, cfg)
			if err := service.Close(); err != nil {
				t.Fatal(err)
			}
			service = openTestService(t, cfg)
			if version == 51 {
				var got string
				if err := service.database.db.QueryRowContext(t.Context(), "SELECT request_json FROM onboarding_issue_intake WHERE project_id = (SELECT project_id FROM issues WHERE id = ?)", issue).Scan(&got); err != nil || got != `{"status":"preview"}` {
					t.Fatalf("intake after upgrade/reopen = %q, %v", got, err)
				}
			}
			for _, check := range []struct{ name, query, want string }{
				{"schema", "SELECT max(version_id) FROM hub_schema_version WHERE is_applied = 1", strconv.FormatInt(supportedSchemaVersion(t), 10)},
				{"archive state", fmt.Sprintf("SELECT archived FROM issues WHERE id = %d", issue), wantArchived},
				{"archive index", "SELECT count(*) FROM sqlite_schema WHERE name = 'issues_unarchived_organization'", "1"},
				{"local checks column", "SELECT count(*) FROM pragma_table_info('runner_identities') WHERE name = 'local_checks_json' AND dflt_value = '''{}'''", "1"},
				{"foreign keys", "SELECT count(*) FROM pragma_foreign_key_check", "0"},
				{"foreign key enforcement", "PRAGMA foreign_keys", "1"},
				{"integrity", "PRAGMA integrity_check", "ok"},
			} {
				t.Run(check.name, func(t *testing.T) {
					var got string
					if err := service.database.db.QueryRowContext(t.Context(), check.query).Scan(&got); err != nil {
						t.Fatal(err)
					}
					if got != check.want {
						t.Fatalf("got %q, want %q", got, check.want)
					}
				})
			}
		})
	}
}
