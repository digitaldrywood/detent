package hubserver

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/pressly/goose/v3"

	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerRoutingMigrationPreservesIdentitiesAndLeases(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	t.Parallel()
	path := filepath.Join(t.TempDir(), "hub.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(t.Context(), fmt.Sprintf("PRAGMA application_id = %d", hubApplicationID)); err != nil {
		t.Fatal(err)
	}
	files, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	migrations := fstest.MapFS{}
	for _, file := range files {
		if file.Name() >= "00011_" {
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
	_, issue := seedProjection(t, db)
	binding := runnerauth.NewBinding()
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO api_tokens (id,name,token_hash,token_fingerprint,scope,created_at,updated_at,expires_at) VALUES ('seed-runner','Runner',?,'seed','worker','2026-09-05T12:00:00Z','2026-09-05T12:00:00Z','2026-09-06T12:00:00Z')`, []any{strings.Repeat("a", 64)}},
		{`INSERT INTO machines (id,hostname,display_name,capacity,version,last_heartbeat_at,registered_at,updated_at,organization_id,token_id) SELECT ?,'original-host','Original name',2,'test','2026-09-05T12:00:00Z','2026-09-05T12:00:00Z','2026-09-05T12:00:00Z',id,'seed-runner' FROM organizations WHERE local = 1`, []any{binding.MachineID}},
		{`INSERT INTO runner_enrollments (id,organization_id,runner_id,machine_id,token_hash,operations_json,created_at,expires_at,created_by) SELECT 'seed-enrollment',id,?,?,?,'["claim"]','2026-09-05T12:00:00Z','2026-09-05T12:01:00Z','seed-runner' FROM organizations WHERE local = 1`, []any{binding.RunnerID, binding.MachineID, strings.Repeat("b", 64)}},
		{`INSERT INTO runner_identities (id,organization_id,machine_id,token_id,enrollment_id,operations_json,created_at) SELECT ?,id,?,'seed-runner','seed-enrollment','["claim"]','2026-09-05T12:00:00Z' FROM organizations WHERE local = 1`, []any{binding.RunnerID, binding.MachineID}},
		{`INSERT INTO runner_identity_events (runner_id,actor_id,kind,occurred_at) VALUES (?,'seed-runner','enrolled','2026-09-05T12:00:00Z')`, []any{binding.RunnerID}},
		{`INSERT INTO leases (lease_id,issue_id,machine_id,session_id,expires_at,acquired_at,renewed_at,created_at,updated_at) VALUES ('seed-lease',?,?,'seed-session','2026-09-05T12:05:00Z','2026-09-05T12:00:00Z','2026-09-05T12:00:00Z','2026-09-05T12:00:00Z','2026-09-05T12:00:00Z')`, []any{issue, binding.MachineID}},
	} {
		if _, err := db.ExecContext(t.Context(), statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range files {
		if file.Name() >= "20261005233000_" {
			continue
		}
		data, err := migrationFiles.ReadFile("migrations/" + file.Name())
		if err != nil {
			t.Fatal(err)
		}
		migrations[file.Name()] = &fstest.MapFile{Data: data}
	}
	provider, err = goose.NewProvider(goose.DialectSQLite3, db, migrations, goose.WithDisableGlobalRegistry(true), goose.WithTableName(hubSchemaTable), goose.WithSlog(discardLogger()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO token_grants(token_id,organization_id,project_id) SELECT 'seed-runner',organization_id,project_id FROM issues WHERE id = ?`, issue); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `UPDATE runner_identities SET routing_settings_json = json_set(routing_settings_json, '$.home_project_ids', json('["legacy-home"]'), '$.spillover', json('{"mode":"after","after_seconds":60}')), home_dry_since = '2026-09-05T12:00:00Z' WHERE id = ?`, binding.RunnerID); err != nil {
		t.Fatal(err)
	}
	var organization tracker.OrganizationID
	if err := db.QueryRowContext(t.Context(), "SELECT id FROM organizations WHERE local = 1").Scan(&organization); err != nil {
		t.Fatal(err)
	}
	for _, project := range []struct {
		id, created string
	}{
		{"prj_seed_new", "2026-09-05T12:00:00Z"},
		{"prj_seed_old_b", "2026-09-04T12:00:00Z"},
		{"prj_seed_old_a", "2026-09-04T12:00:00Z"},
	} {
		if _, err := db.ExecContext(t.Context(), "INSERT INTO projects(id, organization_id, name, profile, states_json, created_at) VALUES (?, ?, ?, 'native', '[]', ?)", project.id, organization, project.id, project.created); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(t.Context(), "INSERT INTO organizations(id, name, created_at) VALUES ('org_seed_other', 'Other', '2020-01-01T00:00:00Z')"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), "INSERT INTO projects(id, organization_id, name, profile, states_json, created_at) VALUES ('prj_seed_other', 'org_seed_other', 'Other', 'native', '[]', '2020-01-01T00:00:00Z')"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	service := openTestService(t, Config{DatabasePath: path})
	var runner, machine, name, state, tags string
	var capacity, events int
	err = service.database.db.QueryRowContext(t.Context(), `SELECT r.id,r.machine_id,r.display_name,r.state,r.tags_json,r.capacity_limit,
(SELECT count(*) FROM runner_identity_events e WHERE e.runner_id = r.id)
FROM runner_identities r JOIN lease_runners lr ON lr.runner_id = r.id WHERE lr.lease_id = 'seed-lease'`).Scan(&runner, &machine, &name, &state, &tags, &capacity, &events)
	if err != nil {
		t.Fatal(err)
	}
	if runner != binding.RunnerID || machine != string(binding.MachineID) || name != "Original name" || state != "active" || tags != "[]" || capacity != 2 || events != 1 {
		t.Fatal("migration changed identity, routing defaults, ownership or audit history")
	}
	var grants, retiredFields int
	if err := service.database.db.QueryRowContext(t.Context(), `SELECT count(*) FROM token_grants WHERE token_id = 'seed-runner'`).Scan(&grants); err != nil {
		t.Fatal(err)
	}
	if err := service.database.db.QueryRowContext(t.Context(), `SELECT (SELECT count(*) FROM pragma_table_info('runner_identities') WHERE name = 'home_dry_since') + CASE WHEN json_type(routing_settings_json, '$.home_project_ids') IS NOT NULL OR json_type(routing_settings_json, '$.spillover') IS NOT NULL THEN 1 ELSE 0 END FROM runner_identities WHERE id = ?`, binding.RunnerID).Scan(&retiredFields); err != nil {
		t.Fatal(err)
	}
	rank, err := readOrganizationProjectRank(t.Context(), service.database.db, organization)
	if err != nil {
		t.Fatal(err)
	}
	var expected []tracker.ProjectID
	rows, err := service.database.db.QueryContext(t.Context(), "SELECT id, scheduling_rank FROM projects WHERE organization_id = ? ORDER BY created_at, id", organization)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id tracker.ProjectID
		var seeded int
		if err := rows.Scan(&id, &seeded); err != nil {
			t.Fatal(err)
		}
		if seeded != len(expected) {
			t.Fatalf("project %s seeded rank = %d, want %d", id, seeded, len(expected))
		}
		expected = append(expected, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rank.ProjectIDs, expected) || rank.Revision != 1 {
		t.Fatalf("seeded rank = %#v, want %v at revision 1", rank, expected)
	}
	if grants != 1 || retiredFields != 0 {
		t.Fatalf("migration grants = %d, retired routing fields = %d", grants, retiredFields)
	}
}
