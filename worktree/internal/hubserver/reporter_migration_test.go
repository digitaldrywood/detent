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

func reporterPreviousHostedStates() []tracker.NativeState {
	return []tracker.NativeState{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Todo", "Blocked", "Human Review", "Merging", "Done"}},
		{Name: "Blocked", Transitions: []string{"Todo", "In Progress", "Done"}},
		{Name: "Human Review", Transitions: []string{"Done", "In Progress", "Blocked", "Merging"}},
		{Name: "Merging", Dispatchable: true, Transitions: []string{"Done", "Blocked", "Human Review", "In Progress"}},
		{Name: "Done", Terminal: true, Transitions: []string{"Todo"}},
	}
}

func reporterUpgradedHostedStates() []tracker.NativeState {
	return []tracker.NativeState{
		{Name: "Triage", Transitions: []string{"Todo", "Backlog", "Cancelled"}},
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done", "Cancelled"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Todo", "Blocked", "Human Review", "Merging", "Done", "Cancelled"}},
		{Name: "Blocked", Transitions: []string{"Todo", "In Progress", "Done", "Cancelled"}},
		{Name: "Human Review", Transitions: []string{"Done", "In Progress", "Blocked", "Merging", "Cancelled"}},
		{Name: "Merging", Dispatchable: true, Transitions: []string{"Done", "Blocked", "Human Review", "In Progress", "Cancelled"}},
		{Name: "Done", Terminal: true, Transitions: []string{"Todo"}},
		{Name: "Backlog", Transitions: []string{"Todo", "Cancelled"}},
		{Name: "Cancelled", Terminal: true, Transitions: []string{"Todo"}},
	}
}

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
		"prj_without":       {{Name: "Backlog", Transitions: []string{"Todo"}}, {Name: "Todo", Dispatchable: true}, {Name: "Cancelled", Terminal: true}},
		"prj_existing":      {{Name: "Custom", Dispatchable: true}, {Name: "Triage", Transitions: []string{"Custom"}}},
		"prj_dispatchable":  {{Name: "Ready", Dispatchable: true}, {Name: "Triage", Dispatchable: true, OperatorOnly: true, Transitions: []string{"Ready"}}},
		"prj_terminal":      {{Name: "Ready", Dispatchable: true}, {Name: "Triage", Terminal: true, Transitions: []string{"Ready"}}},
		"prj_custom":        {{Name: "Ready", Transitions: []string{"Working"}}, {Name: "Working", Dispatchable: true}},
		"prj_empty_triage":  {{Name: "Ready", Transitions: []string{"Working"}}, {Name: "Working", Dispatchable: true}, {Name: "Triage"}},
		"prj_custom_triage": {{Name: "Ready"}, {Name: "Todo", Dispatchable: true}, {Name: "Cancelled", Terminal: true}, {Name: "Triage", Transitions: []string{"Ready"}}},
	}
	before["prj_default_one"] = reporterPreviousHostedStates()
	before["prj_default_two"] = append([]tracker.NativeState{{Name: "Triage", Transitions: []string{"Todo"}}}, reporterPreviousHostedStates()...)
	custom := reporterPreviousHostedStates()
	custom[0].Transitions = []string{"In Progress"}
	before["prj_custom_default"] = custom
	before["prj_repository_default"] = reporterPreviousHostedStates()

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
	if _, err := db.ExecContext(t.Context(), "UPDATE projects SET workflow_source='detent.yaml' WHERE id='prj_repository_default'"); err != nil {
		t.Fatal(err)
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
	for _, name := range []string{"20261013050000_reporter_role.sql", "20261013050001_native_project_triage.sql", "20261013085000_hosted_reporter_destinations.sql"} {
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
		switch id {
		case "prj_dispatchable", "prj_terminal":
			want = []tracker.NativeState{{Name: "Ready", Dispatchable: true}, {Name: "Triage", Transitions: []string{"Ready"}}}
		case "prj_custom":
			want = []tracker.NativeState{{Name: "Triage", Transitions: []string{"Ready"}}, {Name: "Ready", Transitions: []string{"Working"}}, {Name: "Working", Dispatchable: true}}
		case "prj_empty_triage":
			want = []tracker.NativeState{{Name: "Ready", Transitions: []string{"Working"}}, {Name: "Working", Dispatchable: true}, {Name: "Triage", Transitions: []string{"Ready"}}}
		case "prj_custom_triage":
			want = []tracker.NativeState{{Name: "Ready"}, {Name: "Todo", Dispatchable: true}, {Name: "Cancelled", Terminal: true}, {Name: "Triage", Transitions: []string{"Ready", "Todo", "Cancelled"}}}
		}
		if id == "prj_without" || id == "prj_custom_default" || id == "prj_repository_default" {
			transitions := []string{"Backlog", "Todo", "Cancelled"}
			if id != "prj_without" {
				transitions = []string{"Todo"}
			}
			want = append([]tracker.NativeState{{Name: "Triage", Transitions: transitions}}, states...)
		}
		if id == "prj_default_one" || id == "prj_default_two" {
			want = reporterUpgradedHostedStates()
		}

		if !reflect.DeepEqual(actual, want) {
			t.Fatalf("%s: got %#v want %#v", id, actual, want)
		}
		var count int
		if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM workflow_states WHERE project_id=? AND detent_state='Triage' AND dispatchable=0 AND terminal=0", id).Scan(&count); err != nil || count != 1 {
			t.Fatalf("triage rows=%d err=%v", count, err)
		}
		if id == "prj_default_one" || id == "prj_default_two" {
			for _, target := range []string{"Backlog", "Todo", "Cancelled"} {
				if err := validateNativeWorkflowTransition(actual, "Triage", target, apiScopeOperator); err != nil {
					t.Fatalf("%s Triage to %s: %v", id, target, err)
				}
				var terminal, dispatchable bool
				if err := db.QueryRowContext(t.Context(), "SELECT terminal,dispatchable FROM workflow_states WHERE project_id=? AND source_name=?", id, target).Scan(&terminal, &dispatchable); err != nil {
					t.Fatal(err)
				}
				if terminal != (target == "Cancelled") || dispatchable != (target == "Todo") {
					t.Fatalf("%s %s flags: terminal=%v dispatchable=%v", id, target, terminal, dispatchable)
				}
			}
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
