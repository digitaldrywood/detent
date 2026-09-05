package hubserver

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/pressly/goose/v3"

	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func TestRunnerRoutingMigrationPreservesIdentitiesAndLeases(t *testing.T) {
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
}
