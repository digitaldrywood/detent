package hubserver

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strconv"
	"testing"
	"testing/fstest"

	"github.com/pressly/goose/v3"
)

func TestNativeMigrationPreservesCompatibilityIdentity(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "hub.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), fmt.Sprintf("PRAGMA application_id = %d", hubApplicationID)); err != nil {
		t.Fatal(err)
	}
	files, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	migrations := fstest.MapFS{}
	for _, file := range files {
		if file.Name() >= "00008_" {
			continue
		}
		data, err := migrationFiles.ReadFile("migrations/" + file.Name())
		if err != nil {
			t.Fatal(err)
		}
		migrations[file.Name()] = &fstest.MapFile{Data: data}
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, migrations, goose.WithDisableGlobalRegistry(true), goose.WithTableName(hubSchemaTable), goose.WithSlog(discardLogger()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
	repositoryID, issueID := seedProjection(t, db)
	for _, statement := range []string{
		`INSERT INTO machines (id, hostname, capacity, version, last_heartbeat_at, registered_at, updated_at) VALUES ('legacy-machine', 'host', 1, 'test', '2026-09-01T12:00:00Z', '2026-09-01T12:00:00Z', '2026-09-01T12:00:00Z')`,
		fmt.Sprintf(`INSERT INTO leases (lease_id, issue_id, machine_id, session_id, expires_at, acquired_at, renewed_at, created_at, updated_at) VALUES ('legacy-lease', %d, 'legacy-machine', 'legacy-session', '2026-09-01T12:10:00Z', '2026-09-01T12:00:00Z', '2026-09-01T12:00:00Z', '2026-09-01T12:00:00Z', '2026-09-01T12:00:00Z')`, issueID),
		fmt.Sprintf(`INSERT INTO work_events (issue_id, fencing_token, kind, payload_json, occurred_at, recorded_at) VALUES (%d, 1, 'legacy-event', '{"reference":"preserved"}', '2026-09-01T12:00:00Z', '2026-09-01T12:00:00Z')`, issueID),
	} {
		if _, err := db.ExecContext(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	service := openTestService(t, Config{DatabasePath: path})
	var nativeID, organizationID, projectID string
	if err := service.database.db.QueryRowContext(t.Context(), "SELECT native_id, organization_id, project_id FROM issues WHERE id = ?", issueID).Scan(&nativeID, &organizationID, &projectID); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, query, want string }{
		{"repository", fmt.Sprintf("SELECT repository_id FROM issues WHERE id = %d", issueID), strconv.FormatInt(repositoryID, 10)},
		{"GitHub identity", fmt.Sprintf("SELECT github_node_id FROM issues WHERE id = %d", issueID), "I_issue"},
		{"number", fmt.Sprintf("SELECT number FROM issues WHERE id = %d", issueID), "1"},
		{"compatibility profile", "SELECT profile FROM projects WHERE id = '" + projectID + "'", "github_compatible"},
		{"lease", "SELECT issue_id FROM leases WHERE lease_id = 'legacy-lease'", strconv.FormatInt(issueID, 10)},
		{"fencing", "SELECT fencing_token FROM leases WHERE lease_id = 'legacy-lease'", "1"},
		{"history", "SELECT payload_json FROM work_events WHERE kind = 'legacy-event'", `{"reference":"preserved"}`},
		{"foreign keys", "SELECT count(*) FROM pragma_foreign_key_check", "0"},
		{"foreign key enforcement", "PRAGMA foreign_keys", "1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var got string
			if err := service.database.db.QueryRowContext(t.Context(), test.query).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("got %q, want %q", got, test.want)
			}
		})
	}
	if _, err := service.database.db.ExecContext(t.Context(), "UPDATE issues SET native_id = 'changed' WHERE id = ?", issueID); err == nil {
		t.Fatal("native identity was mutable")
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	service = openTestService(t, Config{DatabasePath: path})
	var restartedID string
	if err := service.database.db.QueryRowContext(t.Context(), "SELECT native_id FROM issues WHERE id = ?", issueID).Scan(&restartedID); err != nil {
		t.Fatal(err)
	}
	if restartedID != nativeID {
		t.Fatal("restart changed native identity")
	}
	fixture := newNativeFixture(t, service, "", "coexisting-native")
	fixture.create(t, "native work")
}
