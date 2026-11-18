package hubserver

import (
	"errors"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
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
	t.Parallel()
	hostedLaneMigrationTest(t, hostedLaneMigration{
		from: 38, to: 39, file: "migrations/00039_hosted_direct_landing.sql", lane: "Merging", dispatchable: true,
		before: hostedLandingLaneStates(),
		after:  hostedDirectLandingStates(),
	})
}

func TestHostedBlockedLaneMigration(t *testing.T) {
	t.Parallel()
	hostedLaneMigrationTest(t, hostedLaneMigration{
		from: 47, to: 48, file: "migrations/00048_hosted_blocked_lane.sql", lane: "Blocked", dispatchable: false,
		before: hostedDirectLandingStates(),
		after: []tracker.NativeState{
			{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}},
			{Name: "In Progress", Dispatchable: true, Transitions: []string{"Todo", "Blocked", "Human Review", "Merging", "Done"}},
			{Name: "Blocked", Transitions: []string{"Todo", "In Progress", "Done"}},
			{Name: "Human Review", Transitions: []string{"Done", "In Progress", "Blocked", "Merging"}},
			{Name: "Merging", Dispatchable: true, Transitions: []string{"Done", "Blocked", "Human Review", "In Progress"}},
			{Name: "Done", Terminal: true, Transitions: []string{"Todo"}},
		},
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
	service := newPostgresNativeService(t, Config{})
	organization := tracker.OrganizationID("org_migrate")
	source := historicalWorkflowSource(t, organization)
	oldTemplate := migration.before
	customized := append([]tracker.NativeState(nil), oldTemplate...)
	customized[1].Transitions = append(append([]string(nil), customized[1].Transitions...), "Todo")
	collapsed := append([]tracker.NativeState(nil), oldTemplate...)
	encoded := func(states []tracker.NativeState) string { t.Helper(); return string(historyTestJSON(t, states)) }
	projects := []struct {
		id, states string
		want       []tracker.NativeState
	}{
		{id: "prj_old_template", states: encoded(oldTemplate), want: migration.after},
		{id: "prj_old_template_spaced", states: strings.ReplaceAll(encoded(oldTemplate), ",", ", "), want: migration.after},
		{id: "prj_customized", states: encoded(customized), want: collapsed},
		{id: "prj_current", states: encoded(migration.after), want: migration.after},
	}
	for _, project := range projects {
		if err := service.config.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
			_, err := r.ProjectRecord(t.Context(), tracker.ProjectID(project.id))
			return err
		}); !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("historical project%s already exists: %v", project.id, err)
		}
		source.Add("projects", map[string]any{"id": project.id, "organization_id": organization, "name": project.id, "profile": "native", "states_json": project.states, "created_at": testTimestamp, "github_repository_enabled": 0, "integration_revision": 7})
	}
	path := source.Write()
	for _, pass := range []string{"migrated", "applied again"} {
		if err := importHistoricalWorkflowSource(t, service, organization, path); err != nil {
			t.Fatal(err)
		}
		t.Run(pass, func(t *testing.T) {
			for _, project := range projects {
				record, _ := readHistoricalWorkflowProject(t, service, organization, tracker.ProjectID(project.id))
				want := reporterUpgradedHostedStates()
				if project.id == "prj_customized" {
					want = append([]tracker.NativeState{{Name: "Triage", Transitions: []string{"Todo"}}}, project.want...)
				}
				if !historicalWorkflowStatesEqual(record.States, want) || record.Settings.Revision != 7 {
					t.Fatalf("%s states=%#v want=%#v revision=%d", project.id, record.States, want, record.Settings.Revision)
				}
				original := readHistoricalWorkflowOriginal(t, service, organization, tracker.ProjectID(project.id))
				if original.States != project.states {
					t.Fatalf("%s historical state bytes changed", project.id)
				}
				added, wantAdded := 0, 0
				for _, state := range record.States {
					if state.Name == migration.lane && !state.Terminal && state.Dispatchable == migration.dispatchable {
						added++
					}
				}
				for _, state := range want {
					if state.Name == migration.lane {
						wantAdded = 1
					}
				}
				if len(record.States) != len(want) || added != wantAdded {
					t.Fatalf("%s workflow lanes=%d (%s %d), want%d (%s %d)", project.id, len(record.States), migration.lane, added, len(want), migration.lane, wantAdded)
				}
			}
		})
	}
}
