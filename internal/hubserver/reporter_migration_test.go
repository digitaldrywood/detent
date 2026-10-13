package hubserver

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
	"testing/fstest"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestReporterAndTriageMigration(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(t.Context(), "PRAGMA foreign_keys=ON"); err != nil {
		t.Fatal(err)
	}
	files, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	migrations := fstest.MapFS{}
	for _, file := range files {
		if file.Name() >= "20261013050000_" {
			continue
		}
		data, err := migrationFiles.ReadFile("migrations/" + file.Name())
		if err != nil {
			t.Fatal(err)
		}
		migrations[file.Name()] = &fstest.MapFile{Data: data}
	}
	migrate := func() {
		t.Helper()
		if _, err := runMigrationsFromFS(t.Context(), db, migrations, discardLogger(), hubGoMigrations(discardLogger())); err != nil {
			t.Fatal(err)
		}
	}
	migrate()
	if _, err := db.ExecContext(t.Context(), "INSERT INTO organizations(id,name,created_at) VALUES('org_reporter','Example organization','2026-10-12T00:00:00Z')"); err != nil {
		t.Fatal(err)
	}
	before := map[string][]tracker.NativeState{
		"prj_without":  {{Name: "Backlog", Transitions: []string{"Todo"}}, {Name: "Todo", Dispatchable: true}, {Name: "Cancelled", Terminal: true}},
		"prj_existing": {{Name: "Custom", Dispatchable: true}, {Name: "Triage", Transitions: []string{"Custom"}}},
	}
	for id, states := range before {
		raw, err := json.Marshal(states)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(t.Context(), "INSERT INTO projects(id,organization_id,name,profile,states_json,created_at) VALUES(?,'org_reporter',?,'native',?,'2026-10-12T00:00:00Z')", id, id, string(raw)); err != nil {
			t.Fatal(err)
		}
		for _, state := range states {
			if _, err := db.ExecContext(t.Context(), "INSERT INTO workflow_states(project_id,source_name,detent_state,terminal,dispatchable,created_at,updated_at) VALUES(?,?,?,?,?,'2026-10-12T00:00:00Z','2026-10-12T00:00:00Z')", id, state.Name, state.Name, state.Terminal, state.Dispatchable); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, id := range []string{"first", "second"} {
		for _, statement := range []struct {
			query string
			args  []any
		}{
			{"INSERT INTO api_tokens(id,name,token_hash,token_fingerprint,scope,created_at,updated_at) VALUES(?,?,?,'','operator','2026-10-12T00:00:00Z','2026-10-12T00:00:00Z')", []any{"token_" + id, id, apikey.HashToken("token_" + id)}},
			{"INSERT INTO hosted_members(user_id,email,membership_id,role,principal_id,created_at,updated_at) VALUES(?,?,?,'member',?,'2026-10-12T00:00:00Z','2026-10-12T00:00:00Z')", []any{id, id + "@example.test", "mem_" + id, "token_" + id}},
			{"INSERT INTO hosted_project_grants(user_id,organization_id,project_id,can_write) VALUES(?,'org_reporter','prj_without',1)", []any{id}},
			{"INSERT INTO hosted_work_view_preferences(organization_id,user_id,scope,view_query) VALUES('org_reporter',?,'project:prj_without','state:Todo')", []any{id}},
			{"INSERT INTO hosted_invitations(id,email,organization_id,role,created_at,expires_at,grants_json) VALUES(?,?,'org_reporter','member','2026-10-12T00:00:00Z','2026-11-12T00:00:00Z','[{\"project_id\":\"prj_without\",\"write\":true}]')", []any{"inv_" + id, id + "@example.test"}},
		} {
			if _, err := db.ExecContext(t.Context(), statement.query, statement.args...); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, name := range []string{"20261013050000_reporter_role.sql", "20261013050001_native_project_triage.sql"} {
		data, err := migrationFiles.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		migrations[name] = &fstest.MapFile{Data: data}
	}
	migrate()
	migrate()
	for id, states := range before {
		var raw string
		if err := db.QueryRowContext(t.Context(), "SELECT states_json FROM projects WHERE id=?", id).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var actual []tracker.NativeState
		if err := json.Unmarshal([]byte(raw), &actual); err != nil {
			t.Fatal(err)
		}
		want := states
		if id == "prj_without" {
			want = append([]tracker.NativeState{{Name: "Triage", Transitions: []string{"Backlog", "Todo", "Cancelled"}}}, states...)
		}
		if !reflect.DeepEqual(actual, want) {
			t.Fatalf("%s: got %#v want %#v", id, actual, want)
		}
		var count int
		if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM workflow_states WHERE project_id=? AND detent_state='Triage' AND dispatchable=0 AND terminal=0", id).Scan(&count); err != nil || count != 1 {
			t.Fatalf("triage rows=%d err=%v", count, err)
		}
	}
	for _, id := range []string{"first", "second"} {
		if _, err := db.ExecContext(t.Context(), "UPDATE hosted_members SET role='reporter' WHERE user_id=?", id); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(t.Context(), "UPDATE hosted_invitations SET role='reporter' WHERE id=?", "inv_"+id); err != nil {
			t.Fatal(err)
		}
		var count int
		if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM hosted_members m JOIN hosted_project_grants g ON g.user_id=m.user_id JOIN hosted_work_view_preferences v ON v.user_id=m.user_id JOIN hosted_invitations i ON i.email=m.email WHERE m.user_id=? AND m.role='reporter' AND i.role='reporter' AND g.can_write=1 AND v.view_query='state:Todo' AND i.expires_at='2026-11-12T00:00:00Z' AND json_array_length(i.grants_json)=1", id).Scan(&count); err != nil || count != 1 {
			t.Fatalf("preserved member=%s rows=%d err=%v", id, count, err)
		}
	}
	var foreignKeys int
	if err := db.QueryRowContext(t.Context(), "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil || foreignKeys != 1 {
		t.Fatalf("foreign keys=%d err=%v", foreignKeys, err)
	}
	rows, err := db.QueryContext(t.Context(), "PRAGMA foreign_key_check")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("migration broke a foreign key")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}
