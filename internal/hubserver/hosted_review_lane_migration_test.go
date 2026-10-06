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

// hostedReviewLaneStates is the hosted template migration 36 produces: the
// review lane, before migration 37 added the landing lane.
func hostedReviewLaneStates() []tracker.NativeState {
	return []tracker.NativeState{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Todo", "Human Review", "Done"}},
		{Name: "Human Review", Transitions: []string{"Done", "In Progress"}},
		{Name: "Done", Terminal: true, Transitions: []string{"Todo"}},
	}
}

// TestHostedReviewLaneMigration moves projects created from the old hosted
// template onto the template with Human Review, and leaves every other
// workflow as it was. Applying it again changes nothing.
func TestHostedReviewLaneMigration(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	t.Parallel()
	hostedLaneMigrationTest(t, hostedLaneMigration{
		from: 35, to: 36, file: "migrations/00036_hosted_review_lane.sql", lane: "Human Review", dispatchable: false,
		before: []tracker.NativeState{
			{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}},
			{Name: "In Progress", Dispatchable: true, Transitions: []string{"Todo", "Done"}},
			{Name: "Done", Terminal: true, Transitions: []string{"Todo"}},
		},
		after: hostedReviewLaneStates(),
	})
}

// hostedLandingLaneStates is the hosted template migration 38 produces: the
// landing lane, before migration 39 let a run go straight to it.
func hostedLandingLaneStates() []tracker.NativeState {
	return []tracker.NativeState{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Todo", "Human Review", "Done"}},
		{Name: "Human Review", Transitions: []string{"Done", "In Progress", "Merging"}},
		{Name: "Merging", Dispatchable: true, Transitions: []string{"Done", "Human Review", "In Progress"}},
		{Name: "Done", Terminal: true, Transitions: []string{"Todo"}},
	}
}

func hostedDirectLandingStates() []tracker.NativeState {
	return []tracker.NativeState{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Todo", "Human Review", "Merging", "Done"}},
		{Name: "Human Review", Transitions: []string{"Done", "In Progress", "Merging"}},
		{Name: "Merging", Dispatchable: true, Transitions: []string{"Done", "Human Review", "In Progress"}},
		{Name: "Done", Terminal: true, Transitions: []string{"Todo"}},
	}
}

// TestHostedLandingLaneMigration moves projects on the review-lane template
// onto the template with Merging, where an approved Change Request waits for
// the runner that lands it, and leaves every other workflow as it was.
// Migration 37 (observed policies) lies between them and touches no lanes.
func TestHostedLandingLaneMigration(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	t.Parallel()
	hostedLaneMigrationTest(t, hostedLaneMigration{
		from: 37, to: 38, file: "migrations/00038_hosted_landing_lane.sql", lane: "Merging", dispatchable: true,
		before: hostedReviewLaneStates(),
		after:  hostedLandingLaneStates(),
	})
}

// TestHostedDirectLandingMigration lets a run on the landing-lane template
// move from In Progress straight to Merging when its version needs no
// review, and leaves every other workflow as it was.
func TestHostedDirectLandingMigration(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	t.Parallel()
	hostedLaneMigrationTest(t, hostedLaneMigration{
		from: 38, to: 39, file: "migrations/00039_hosted_direct_landing.sql", lane: "Merging", dispatchable: true,
		before: hostedLandingLaneStates(),
		after:  hostedDirectLandingStates(),
	})
}

func TestHostedBlockedLaneMigration(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	t.Parallel()
	hostedLaneMigrationTest(t, hostedLaneMigration{
		from: 47, to: 48, file: "migrations/00048_hosted_blocked_lane.sql", lane: "Blocked", dispatchable: false,
		before: hostedDirectLandingStates(),
		after:  HostedProjectStates(),
	})
}

type hostedLaneMigration struct {
	from, to     int64
	file         string
	lane         string
	dispatchable bool
	before       []tracker.NativeState
	after        []tracker.NativeState
}

func hostedLaneMigrationTest(t *testing.T, migration hostedLaneMigration) {
	t.Helper()
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
	if _, err := provider.UpTo(t.Context(), migration.from); err != nil {
		t.Fatal(err)
	}
	oldTemplate := migration.before
	customized := append([]tracker.NativeState(nil), oldTemplate...)
	customized[1].Transitions = append(append([]string(nil), customized[1].Transitions...), "Todo")
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
		{id: "prj_old_template", states: encoded(oldTemplate), want: migration.after},
		{id: "prj_old_template_spaced", states: strings.ReplaceAll(encoded(oldTemplate), ",", ", "), want: migration.after},
		{id: "prj_customized", states: encoded(customized), want: customized},
		{id: "prj_current", states: encoded(migration.after), want: migration.after},
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
	if _, err := provider.UpTo(t.Context(), migration.to); err != nil {
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
			var lanes, added int
			if err := db.QueryRowContext(t.Context(), "SELECT count(*), coalesce(sum(source_name = ? AND terminal = 0 AND dispatchable = ?), 0) FROM workflow_states WHERE project_id = ?", migration.lane, migration.dispatchable, project.id).Scan(&lanes, &added); err != nil {
				t.Fatal(err)
			}
			wantAdded := 0
			for _, state := range project.want {
				if state.Name == migration.lane {
					wantAdded = 1
				}
			}
			if lanes != len(project.want) || added != wantAdded {
				t.Fatalf("%s workflow lanes = %d (%s %d), want %d (%s %d)", project.id, lanes, migration.lane, added, len(project.want), migration.lane, wantAdded)
			}
		}
	}
	t.Run("migrated", check)
	up, err := migrationFiles.ReadFile(migration.file)
	if err != nil {
		t.Fatal(err)
	}
	statements, _, _ := strings.Cut(string(up), "-- +goose Down")
	if _, err := db.ExecContext(t.Context(), strings.TrimPrefix(statements, "-- +goose Up")); err != nil {
		t.Fatal(err)
	}
	t.Run("applied again", check)
}
