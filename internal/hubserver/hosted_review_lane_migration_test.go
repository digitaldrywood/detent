package hubserver

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/pressly/goose/v3"

	"github.com/digitaldrywood/detent/internal/tracker"
)

// TestHostedReviewLaneMigration moves projects created from the old hosted
// template onto the template with Human Review, and leaves every other
// workflow as it was. Applying it again changes nothing.
func TestHostedReviewLaneMigration(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := db.ExecContext(t.Context(), fmt.Sprintf("PRAGMA application_id = %d", hubApplicationID)); err != nil {
		t.Fatal(err)
	}
	files, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	migrations := fstest.MapFS{}
	for _, file := range files {
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
	if _, err := provider.UpTo(t.Context(), 35); err != nil {
		t.Fatal(err)
	}
	oldTemplate := []tracker.NativeState{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Todo", "Done"}},
		{Name: "Done", Terminal: true, Transitions: []string{"Todo"}},
	}
	customized := append([]tracker.NativeState(nil), oldTemplate...)
	customized[1].Transitions = []string{"Todo", "Done", "Todo"}
	encoded := func(states []tracker.NativeState) string {
		raw, err := marshalNative(states)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	if _, err := db.ExecContext(t.Context(), "INSERT INTO organizations (id, name, created_at) VALUES ('org_migrate', 'Migrate', ?)", testTimestamp); err != nil {
		t.Fatal(err)
	}
	projects := []struct {
		id     string
		states string
		want   []tracker.NativeState
	}{
		{id: "prj_old_template", states: encoded(oldTemplate), want: HostedProjectStates()},
		{id: "prj_old_template_spaced", states: strings.ReplaceAll(encoded(oldTemplate), ",", ", "), want: HostedProjectStates()},
		{id: "prj_customized", states: encoded(customized), want: customized},
		{id: "prj_current", states: encoded(HostedProjectStates()), want: HostedProjectStates()},
	}
	for _, project := range projects {
		if _, err := db.ExecContext(t.Context(), "INSERT INTO projects (id, organization_id, name, profile, states_json, created_at, github_repository_enabled) VALUES (?, 'org_migrate', ?, 'native', ?, ?, 0)", project.id, project.id, project.states, testTimestamp); err != nil {
			t.Fatal(err)
		}
		var states []tracker.NativeState
		if err := json.Unmarshal([]byte(project.states), &states); err != nil {
			t.Fatal(err)
		}
		for _, state := range states {
			if _, err := db.ExecContext(t.Context(), "INSERT INTO workflow_states (project_id, source_name, detent_state, terminal, dispatchable, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)", project.id, state.Name, state.Name, state.Terminal, state.Dispatchable, testTimestamp, testTimestamp); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := provider.UpTo(t.Context(), 36); err != nil {
		t.Fatal(err)
	}
	check := func(t *testing.T) {
		t.Helper()
		for _, project := range projects {
			var raw string
			if err := db.QueryRowContext(t.Context(), "SELECT states_json FROM projects WHERE id = ?", project.id).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var states []tracker.NativeState
			if err := json.Unmarshal([]byte(raw), &states); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(states, project.want) {
				t.Fatalf("%s states = %#v, want %#v", project.id, states, project.want)
			}
			var lanes, review int
			if err := db.QueryRowContext(t.Context(), "SELECT count(*), coalesce(sum(source_name = 'Human Review' AND terminal = 0 AND dispatchable = 0), 0) FROM workflow_states WHERE project_id = ?", project.id).Scan(&lanes, &review); err != nil {
				t.Fatal(err)
			}
			wantReview := 0
			for _, state := range project.want {
				if state.Name == "Human Review" {
					wantReview = 1
				}
			}
			if lanes != len(project.want) || review != wantReview {
				t.Fatalf("%s workflow lanes = %d (review %d), want %d (review %d)", project.id, lanes, review, len(project.want), wantReview)
			}
		}
	}
	t.Run("migrated", check)
	up, err := migrationFiles.ReadFile("migrations/00036_hosted_review_lane.sql")
	if err != nil {
		t.Fatal(err)
	}
	statements, _, _ := strings.Cut(string(up), "-- +goose Down")
	if _, err := db.ExecContext(t.Context(), strings.TrimPrefix(statements, "-- +goose Up")); err != nil {
		t.Fatal(err)
	}
	t.Run("applied again", check)
}
