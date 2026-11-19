package coreimport_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/attachment"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/neutralimport/neutralimporttest"
	"github.com/digitaldrywood/detent/internal/hubserver/projectadmin"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestCloudWorkflowLanesUpgrade(t *testing.T) {
	t.Parallel()
	fixture := workflowMigrationFixture{repository: hubtest.Repository(t), objects: hubtest.NewDetails()}
	organization := tracker.OrganizationID("org_upgrade")
	source := workflowMigrationSource(t, organization)
	legacy := "---\ntracker:\n  kind: hub_native\n  observed_states: [Backlog, Blocked]\n  active_states: [Todo]\n  terminal_states: [Done]\n---\nKeep the Cloud prompt.\n"
	states := `[{"name":"Backlog"},{"name":"Todo","dispatchable":true},{"name":"Blocked"},{"name":"Done","terminal":true}]`
	original := historicalCloudWorkflowPolicy(t, legacy)
	raw := historyTestJSON(t, original)
	projects := []string{"prj_created_after_transition", "prj_edited_after_transition"}
	for _, id := range projects {
		source.Add("projects", map[string]any{"id": id, "organization_id": organization, "name": id, "profile": "native", "states_json": states, "created_at": testTimestamp, "workflow_markdown": legacy, "integration_revision": 7})
		source.Add("policy_revisions", map[string]any{"scope": string(organization) + "/" + id, "policy_id": original.ID, "metadata_json": string(raw), "approved_by": "human", "approved_at": testTimestamp})
		source.Add("project_policies", map[string]any{"scope": string(organization) + "/" + id, "policy_id": original.ID})
	}
	path := source.Write()
	for startup := range 2 {
		if err := fixture.importSource(t, organization, path); err != nil {
			t.Fatal(err)
		}
		for _, id := range projects {
			record, markdown := fixture.project(t, organization, tracker.ProjectID(id))
			migrated, err := workflowconfig.ParseProjectDefinition(workflowconfig.ProjectDefinitionSources{WorkflowPath: "Cloud WORKFLOW.md", Workflow: []byte(markdown)})
			if err != nil {
				t.Fatalf("startup%d project%s: %v", startup, id, err)
			}
			want := []workflowconfig.Lane{{Name: "Backlog", Role: workflowconfig.LaneHolding}, {Name: "Todo", Role: workflowconfig.LaneActive}, {Name: "Blocked", Role: workflowconfig.LaneHolding}, {Name: "Done", Role: workflowconfig.LaneTerminal}}
			archived := fixture.original(t, organization, tracker.ProjectID(id))
			if !slices.Equal(migrated.Config.Tracker.Lanes, want) || migrated.Prompt != "Keep the Cloud prompt.\n" || archived.States != states || archived.Workflow != legacy || record.Settings.Revision != 8 {
				t.Fatalf("startup%d project%s workflow=%#v original=%+v revision=%d", startup, id, migrated, archived, record.Settings.Revision)
			}
			approval := fixture.approval(t, organization, tracker.ProjectID(id))
			if approval.Policy.ID == original.ID || approval.ApprovedBy != "human" || approval.ApprovedAt != testTimestamp {
				t.Fatalf("project%s approval=%#v", id, approval)
			}
			if approval.Policy.Configuration == nil || approval.Policy.Configuration.DefinitionDigest != migrated.SourceHash {
				t.Fatalf("project%s approval does not match migrated Cloud workflow", id)
			}
			if _, err := workflowconfig.ApplyNativePolicy(migrated, approval.Policy); err != nil {
				t.Fatal(err)
			}
			historical := fixture.historicalPolicy(t, organization, tracker.ProjectID(id), original.ID)
			if string(historical) != string(raw) {
				t.Fatalf("project%s historical approval changed", id)
			}
		}
	}
}

func TestCloudWorkflowLanesMigration(t *testing.T) {
	t.Parallel()
	legacy := "---\ntracker:\n  kind: hub_native\n  observed_states: [Backlog, Blocked]\n  active_states: [Todo]\n  terminal_states: [Done]\n---\nKeep the Cloud prompt.\n"
	fixture := workflowMigrationFixture{repository: hubtest.Repository(t), objects: hubtest.NewDetails()}
	for index, test := range []struct {
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
			organization := tracker.OrganizationID(fmt.Sprintf("org_cloud_migration_%d", index))
			source := workflowMigrationSource(t, organization)
			id := tracker.ProjectID("project_first")
			source.Add("projects", map[string]any{"id": id, "organization_id": organization, "name": string(id), "profile": "native", "states_json": test.states, "workflow_source": test.source, "workflow_markdown": test.markdown, "created_at": testTimestamp, "integration_revision": 7})
			var original policy.Descriptor
			if test.changed {
				approved := test.markdown
				if test.stale {
					approved = strings.Replace(approved, "Keep the Cloud prompt.\n", "Previously approved prompt.\n", 1)
				}
				original = historicalCloudWorkflowPolicy(t, approved)
				source.Add("policy_revisions", map[string]any{"scope": string(organization) + "/" + string(id), "policy_id": original.ID, "metadata_json": string(historyTestJSON(t, original)), "approved_by": "human", "approved_at": "2026-10-05T22:00:00Z"})
				source.Add("project_policies", map[string]any{"scope": string(organization) + "/" + string(id), "policy_id": original.ID})
			}
			path := source.Write()
			err := fixture.importSource(t, organization, path)
			if (err != nil) != test.fails {
				t.Fatalf("migration error=%v", err)
			}
			if test.fails {
				if err := fixture.repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
					_, err := r.ProjectRecord(t.Context(), id)
					return err
				}); !errors.Is(err, corestore.ErrNotFound) {
					t.Fatalf("failed historical workflow admitted project %s: %v", id, err)
				}
				return
			}
			record, markdown := fixture.project(t, organization, id)
			archived := fixture.original(t, organization, id)
			if archived.States != test.states || archived.Workflow != test.markdown {
				t.Fatal("migration changed historical source states or markdown")
			}
			if !test.changed {
				if markdown != test.markdown || record.Settings.Revision != 7 {
					t.Fatal("migration changed untouched project")
				}
				return
			}
			workflow, err := workflowconfig.ParseWorkflow([]byte(markdown))
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(workflow.Config.KanbanStateNames(), []string{"Backlog", "Todo", "Blocked", "Done"}) || workflow.Prompt != "Keep the Cloud prompt.\n" || record.Settings.Revision != 8 {
				t.Fatalf("migrated workflow=%#v revision=%d", workflow, record.Settings.Revision)
			}
			approval := fixture.approval(t, organization, id)
			if (approval.Policy.ID == original.ID) != test.stale || approval.ApprovedBy != "human" || approval.ApprovedAt != "2026-10-05T22:00:00Z" {
				t.Fatalf("migrated approval=%#v", approval)
			}
			if approval.Policy.Configuration == nil || (approval.Policy.Configuration.DefinitionDigest != workflow.SourceHash) != test.stale {
				t.Fatal("migration changed current/stale policy binding")
			}
			if _, err := workflowconfig.ApplyNativePolicy(workflow, approval.Policy); err != nil {
				t.Fatal(err)
			}
			raw := fixture.historicalPolicy(t, organization, id, original.ID)
			var historical policy.Descriptor
			if err := json.Unmarshal(raw, &historical); err != nil {
				t.Fatal(err)
			}
			if historical.ID != original.ID || historical.Configuration.DefinitionDigest != original.Configuration.DefinitionDigest || string(raw) != string(historyTestJSON(t, original)) {
				t.Fatal("migration changed approval history")
			}
			if err := fixture.importSource(t, organization, path); err != nil {
				t.Fatal(err)
			}
			second, again := fixture.project(t, organization, id)
			if again != markdown || second.Settings.Revision != 8 {
				t.Fatal("migration is not idempotent")
			}
		})
	}
}

func historicalCloudWorkflowPolicy(t *testing.T, markdown string) policy.Descriptor {
	t.Helper()
	workflow, err := workflowconfig.ParseHistoricalProjectDefinition(workflowconfig.ProjectDefinitionSources{WorkflowPath: "Cloud WORKFLOW.md", Workflow: []byte(markdown)})
	if err != nil {
		t.Fatal(err)
	}
	workflow.Definition.Layout = workflowconfig.ProjectDefinitionCloud
	descriptor, err := workflowconfig.ResolvePolicy(workflow)
	if err != nil {
		t.Fatal(err)
	}
	return descriptor
}

func (f workflowMigrationFixture) historicalPolicy(t *testing.T, organization tracker.OrganizationID, project tracker.ProjectID, id string) json.RawMessage {
	t.Helper()
	var revision eventstore.Receipt
	if err := f.repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
		row, err := r.ProjectPolicyRevision(t.Context(), project, id)
		revision = eventstore.Receipt{OrganizationID: organization, PayloadObjectKey: row.PayloadObjectKey, PayloadSHA256: row.PayloadSHA256, PayloadBytes: row.PayloadBytes, PayloadContentBytes: row.PayloadContentBytes}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := eventstore.NewContent(f.objects).Receipt(t.Context(), revision)
	if err != nil {
		t.Fatal(fmt.Errorf("read historical policy: %w", err))
	}
	return raw
}

type workflowMigrationFixture struct {
	repository *postgres.Repository
	objects    attachment.DetailStore
}

func workflowMigrationSource(t *testing.T, organization tracker.OrganizationID) *neutralimporttest.Fixture {
	t.Helper()
	source := newCoreNeutralFixture(t, organization, "organizations", "hosted_tenant", "api_tokens", "hosted_members", "hosted_release_preferences", "daily_summary_preferences", "daily_summary_receipts", "hosted_project_grants", "token_grants", "hosted_sessions", "hosted_transactions", "hosted_invitations", "hosted_member_reservations", "hosted_binding_migrations", "organization_secrets", "organization_secret_audit", "project_secrets", "project_secret_audit", "hosted_audit", "hosted_audit_times", "hosted_work_view_preferences", "projects", "repositories", "policy_revisions", "project_policies", "change_review_policies", "monthly_budget_policies")
	source.Add("organizations", map[string]any{"id": organization, "name": string(organization), "slug": string(organization), "created_at": testTimestamp})
	return source
}

func (f workflowMigrationFixture) importSource(t *testing.T, organization tracker.OrganizationID, path string) error {
	t.Helper()
	catalog, err := storagecatalog.Read(os.DirFS("../../../.."), "tools/storageinventory/mappings/v1.json")
	if err != nil {
		return err
	}
	access := postgres.AccessCopyImportSource{BundlePath: path, OrganizationID: organization, Catalog: catalog, Objects: f.objects}
	if err := f.repository.InitializeAccessCopyOrganization(t.Context(), access); err != nil {
		return err
	}
	if _, err := f.repository.ImportCoreProjects(t.Context(), postgres.CoreProjectImportSource{BundlePath: path, OrganizationID: organization, Objects: f.objects}); err != nil {
		return err
	}
	if _, err := f.repository.ImportAccessCopy(t.Context(), access); err != nil {
		return err
	}
	_, err = f.repository.ImportCorePolicies(t.Context(), postgres.CorePolicyImportSource{BundlePath: path, OrganizationID: organization, Objects: f.objects})
	return err
}

func (f workflowMigrationFixture) project(t *testing.T, organization tracker.OrganizationID, id tracker.ProjectID) (projectstore.Record, string) {
	t.Helper()
	var record projectstore.Record
	if err := f.repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		record, err = r.ProjectRecord(t.Context(), id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	markdown, err := projectadmin.WorkflowMarkdown(t.Context(), f.objects, record.Settings)
	if err != nil {
		t.Fatal(err)
	}
	return record, markdown
}

func (f workflowMigrationFixture) original(t *testing.T, organization tracker.OrganizationID, id tracker.ProjectID) struct{ States, Workflow string } {
	t.Helper()
	var receipt eventstore.Receipt
	if err := f.repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		receipt, err = r.Receipt(t.Context(), "production-import", "core-project", string(id))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := eventstore.NewContent(f.objects).Receipt(t.Context(), receipt)
	if err != nil {
		t.Fatal(err)
	}
	var original struct{ States, Workflow string }
	if err := json.Unmarshal(raw, &original); err != nil {
		t.Fatal(err)
	}
	return original
}

func (f workflowMigrationFixture) approval(t *testing.T, organization tracker.OrganizationID, project tracker.ProjectID) policy.Approval {
	t.Helper()
	var revision policystore.Revision
	if err := f.repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		revision, err = r.ProjectPolicy(t.Context(), project)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := eventstore.NewContent(f.objects).Receipt(t.Context(), eventstore.Receipt{OrganizationID: organization, PayloadObjectKey: revision.PayloadObjectKey, PayloadSHA256: revision.PayloadSHA256, PayloadBytes: revision.PayloadBytes, PayloadContentBytes: revision.PayloadContentBytes})
	if err != nil {
		t.Fatal(err)
	}
	approval, err := hubrecords.DecodeProjectPolicy(t.Context(), string(organization)+"/"+string(project), revision.PolicyID, string(raw), policy.Approval{ApprovedBy: revision.ApprovedBy, ApprovedAt: hubapi.FormatHubTime(revision.ApprovedAt)})
	if err != nil {
		t.Fatal(err)
	}
	return approval
}
