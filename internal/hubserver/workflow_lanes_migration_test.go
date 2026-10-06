package hubserver

import (
	"database/sql"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/policy"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
)

func TestCloudWorkflowLanesMigration(t *testing.T) {
	t.Parallel()
	legacy := "---\ntracker:\n  kind: hub_native\n  observed_states: [Backlog, Blocked]\n  active_states: [Todo]\n  terminal_states: [Done]\n---\nKeep the Cloud prompt.\n"
	for _, test := range []struct {
		name, source, markdown, states string
		changed, fails, stale          bool
	}{
		{"cloud current order", "", legacy, `[{"name":"Backlog"},{"name":"Todo","dispatchable":true},{"name":"Blocked"},{"name":"Done","terminal":true}]`, true, false, false},
		{"stale approval", "", legacy, `[{"name":"Backlog"},{"name":"Todo","dispatchable":true},{"name":"Blocked"},{"name":"Done","terminal":true}]`, true, false, true},
		{"repository authority", "detent.yaml", legacy, `[]`, false, false, false},
		{"already migrated", "", "---\ntracker:\n  lanes: [{name: Todo, role: active}]\n---\nPrompt.\n", `[{"name":"Todo","dispatchable":true}]`, false, false, false},
		{"no markdown", "", "", `[]`, false, false, false},
		{"inconsistent stored order", "", legacy, `[{"name":"Missing"}]`, false, true, false},
		{"mixed forms", "", strings.Replace(legacy, "  observed_states:", "  lanes: []\n  observed_states:", 1), `[]`, false, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, err := sql.Open("sqlite", ":memory:")
			if err != nil {
				t.Fatal(err)
			}
			db.SetMaxOpenConns(1)
			t.Cleanup(func() {
				if err := db.Close(); err != nil {
					t.Error(err)
				}
			})
			if _, err := db.ExecContext(t.Context(), "CREATE TABLE projects(id TEXT PRIMARY KEY, organization_id TEXT, profile TEXT, workflow_source TEXT, workflow_markdown TEXT, states_json TEXT, integration_revision INTEGER)"); err != nil {
				t.Fatal(err)
			}
			for _, ddl := range []string{
				"CREATE TABLE policy_revisions(scope TEXT, policy_id TEXT, metadata_json TEXT, approved_by TEXT, approved_at TEXT, PRIMARY KEY(scope,policy_id))",
				"CREATE TABLE project_policies(scope TEXT PRIMARY KEY, policy_id TEXT)",
			} {
				if _, err := db.ExecContext(t.Context(), ddl); err != nil {
					t.Fatal(err)
				}
			}
			var original policy.Descriptor
			if test.changed {
				approvedMarkdown := test.markdown
				if test.stale {
					approvedMarkdown = strings.Replace(approvedMarkdown, "Keep the Cloud prompt.", "Previously approved prompt.", 1)
				}
				workflow, err := workflowconfig.ParseProjectDefinition(workflowconfig.ProjectDefinitionSources{WorkflowPath: "Cloud WORKFLOW.md", Workflow: []byte(approvedMarkdown)})
				if err != nil {
					t.Fatal(err)
				}
				workflow.Definition.Layout = workflowconfig.ProjectDefinitionCloud
				original, err = workflowconfig.ResolvePolicy(workflow)
				if err != nil {
					t.Fatal(err)
				}
				raw, err := json.Marshal(original)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.ExecContext(t.Context(), "INSERT INTO policy_revisions VALUES ('org/project',?,?, 'human', '2026-10-05T22:00:00Z')", original.ID, string(raw)); err != nil {
					t.Fatal(err)
				}
				if _, err := db.ExecContext(t.Context(), "INSERT INTO project_policies VALUES ('org/project',?)", original.ID); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := db.ExecContext(t.Context(), "INSERT INTO projects VALUES ('project','org','native',?,?,?,7)", test.source, test.markdown, test.states); err != nil {
				t.Fatal(err)
			}
			apply := func() error {
				t.Helper()
				tx, err := db.BeginTx(t.Context(), nil)
				if err != nil {
					return err
				}
				if err := migrateCloudWorkflowLanes(t.Context(), tx); err != nil {
					if rollbackErr := tx.Rollback(); rollbackErr != nil {
						t.Fatal(rollbackErr)
					}
					return err
				}
				return tx.Commit()
			}
			err = apply()
			if (err != nil) != test.fails {
				t.Fatalf("migration error = %v", err)
			}
			var markdown, states string
			var revision int
			if err := db.QueryRowContext(t.Context(), "SELECT workflow_markdown, states_json, integration_revision FROM projects").Scan(&markdown, &states, &revision); err != nil {
				t.Fatal(err)
			}
			if states != test.states {
				t.Fatal("migration changed stored states")
			}
			if !test.changed {
				if markdown != test.markdown || revision != 7 {
					t.Fatal("migration changed untouched project")
				}
				return
			}
			workflow, err := workflowconfig.ParseWorkflow([]byte(markdown))
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(workflow.Config.KanbanStateNames(), []string{"Backlog", "Todo", "Blocked", "Done"}) || workflow.Prompt != "Keep the Cloud prompt.\n" || revision != 8 {
				t.Fatalf("migrated workflow = %#v revision=%d", workflow, revision)
			}
			approval, err := readProjectPolicy(t.Context(), db, "org/project")
			if err != nil {
				t.Fatal(err)
			}
			if (approval.Policy.ID == original.ID) != test.stale || approval.ApprovedBy != "human" || approval.ApprovedAt != "2026-10-05T22:00:00Z" {
				t.Fatalf("migrated approval = %#v", approval)
			}
			if err := validateWorkflowPolicy(t.Context(), db, "org/project", approval.Policy); (err != nil) != test.stale {
				t.Fatal(err)
			}
			if _, err := workflowconfig.ApplyNativePolicy(workflow, approval.Policy); err != nil {
				t.Fatal(err)
			}
			var history string
			if err := db.QueryRowContext(t.Context(), "SELECT metadata_json FROM policy_revisions WHERE policy_id=?", original.ID).Scan(&history); err != nil {
				t.Fatal(err)
			}
			var historical policy.Descriptor
			if err := json.Unmarshal([]byte(history), &historical); err != nil {
				t.Fatal(err)
			}
			if historical.ID != original.ID || historical.Configuration.DefinitionDigest != original.Configuration.DefinitionDigest {
				t.Fatal("migration changed approval history")
			}
			if err := apply(); err != nil {
				t.Fatal(err)
			}
			var second string
			if err := db.QueryRowContext(t.Context(), "SELECT workflow_markdown, integration_revision FROM projects").Scan(&second, &revision); err != nil {
				t.Fatal(err)
			}
			if second != markdown || revision != 8 {
				t.Fatal("migration is not idempotent")
			}
		})
	}
}
