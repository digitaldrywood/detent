package hubserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestCloudWorkflowLanesUpgrade(t *testing.T) {
	t.Parallel()
	service := newPostgresNativeService(t, Config{})
	organization := tracker.OrganizationID("org_upgrade")
	source := historicalWorkflowSource(t, organization)
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
		if err := importHistoricalWorkflowSource(t, service, organization, path); err != nil {
			t.Fatal(err)
		}
		for _, id := range projects {
			record, markdown := readHistoricalWorkflowProject(t, service, organization, tracker.ProjectID(id))
			migrated, err := workflowconfig.ParseProjectDefinition(workflowconfig.ProjectDefinitionSources{WorkflowPath: "Cloud WORKFLOW.md", Workflow: []byte(markdown)})
			if err != nil {
				t.Fatalf("startup%d project%s: %v", startup, id, err)
			}
			want := []workflowconfig.Lane{{Name: "Backlog", Role: workflowconfig.LaneHolding}, {Name: "Todo", Role: workflowconfig.LaneActive}, {Name: "Blocked", Role: workflowconfig.LaneHolding}, {Name: "Done", Role: workflowconfig.LaneTerminal}}
			archived := readHistoricalWorkflowOriginal(t, service, organization, tracker.ProjectID(id))
			if !slices.Equal(migrated.Config.Tracker.Lanes, want) || migrated.Prompt != "Keep the Cloud prompt.\n" || archived.States != states || archived.Workflow != legacy || record.Settings.Revision != 8 {
				t.Fatalf("startup%d project%s workflow=%#v original=%+v revision=%d", startup, id, migrated, archived, record.Settings.Revision)
			}
			approval, err := service.readTenantProjectPolicy(t.Context(), organization, tracker.ProjectID(id))
			if err != nil {
				t.Fatal(err)
			}
			if approval.Policy.ID == original.ID || approval.ApprovedBy != "human" || approval.ApprovedAt != testTimestamp {
				t.Fatalf("project%s approval=%#v", id, approval)
			}
			if approval.Policy.Configuration == nil || approval.Policy.Configuration.DefinitionDigest != migrated.SourceHash {
				t.Fatalf("project%s approval does not match migrated Cloud workflow", id)
			}
			if _, err := workflowconfig.ApplyNativePolicy(migrated, approval.Policy); err != nil {
				t.Fatal(err)
			}
			historical := readHistoricalWorkflowPolicy(t, service, organization, tracker.ProjectID(id), original.ID)
			if string(historical) != string(raw) {
				t.Fatalf("project%s historical approval changed", id)
			}
		}
	}
}

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
			service := newPostgresNativeService(t, Config{})
			organization := tracker.OrganizationID("org_cloud_migration")
			source := historicalWorkflowSource(t, organization)
			var original policy.Descriptor
			if test.changed {
				approved := test.markdown
				if test.stale {
					approved = strings.Replace(approved, "Keep the Cloud prompt.", "Previously approved prompt.", 1)
				}
				original = historicalCloudWorkflowPolicy(t, approved)
			}
			ids := []tracker.ProjectID{"project_first", "project_second"}
			for _, id := range ids {
				source.Add("projects", map[string]any{"id": id, "organization_id": organization, "name": string(id), "profile": "native", "states_json": test.states, "workflow_source": test.source, "workflow_markdown": test.markdown, "created_at": testTimestamp, "integration_revision": 7})
				if test.changed {
					source.Add("policy_revisions", map[string]any{"scope": string(organization) + "/" + string(id), "policy_id": original.ID, "metadata_json": string(historyTestJSON(t, original)), "approved_by": "human", "approved_at": "2026-10-05T22:00:00Z"})
					source.Add("project_policies", map[string]any{"scope": string(organization) + "/" + string(id), "policy_id": original.ID})
				}
			}
			path := source.Write()
			err := importHistoricalWorkflowSource(t, service, organization, path)
			if (err != nil) != test.fails {
				t.Fatalf("migration error=%v", err)
			}
			if test.fails {
				for _, id := range ids {
					if err := service.config.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error { _, err := r.ProjectRecord(t.Context(), id); return err }); !errors.Is(err, corestore.ErrNotFound) {
						t.Fatalf("failed historical workflow admitted project%s: %v", id, err)
					}
				}
				return
			}
			for _, id := range ids {
				record, markdown := readHistoricalWorkflowProject(t, service, organization, id)
				archived := readHistoricalWorkflowOriginal(t, service, organization, id)
				if archived.States != test.states || archived.Workflow != test.markdown {
					t.Fatal("migration changed historical source states or markdown")
				}
				if !test.changed {
					if markdown != test.markdown || record.Settings.Revision != 7 {
						t.Fatal("migration changed untouched project")
					}
					continue
				}
				workflow, err := workflowconfig.ParseWorkflow([]byte(markdown))
				if err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(workflow.Config.KanbanStateNames(), []string{"Backlog", "Todo", "Blocked", "Done"}) || workflow.Prompt != "Keep the Cloud prompt.\n" || record.Settings.Revision != 8 {
					t.Fatalf("migrated workflow=%#v revision=%d", workflow, record.Settings.Revision)
				}
				approval, err := service.readTenantProjectPolicy(t.Context(), organization, id)
				if err != nil {
					t.Fatal(err)
				}
				if (approval.Policy.ID == original.ID) != test.stale || approval.ApprovedBy != "human" || approval.ApprovedAt != "2026-10-05T22:00:00Z" {
					t.Fatalf("migrated approval=%#v", approval)
				}
				if approval.Policy.Configuration == nil || (approval.Policy.Configuration.DefinitionDigest != workflow.SourceHash) != test.stale {
					t.Fatal("migration changed current/stale policy binding")
				}
				if _, err := workflowconfig.ApplyNativePolicy(workflow, approval.Policy); err != nil {
					t.Fatal(err)
				}
				raw := readHistoricalWorkflowPolicy(t, service, organization, id, original.ID)
				var historical policy.Descriptor
				if err := json.Unmarshal(raw, &historical); err != nil {
					t.Fatal(err)
				}
				if historical.ID != original.ID || historical.Configuration.DefinitionDigest != original.Configuration.DefinitionDigest || string(raw) != string(historyTestJSON(t, original)) {
					t.Fatal("migration changed approval history")
				}
				if err := importHistoricalWorkflowSource(t, service, organization, path); err != nil {
					t.Fatal(err)
				}
				second, again := readHistoricalWorkflowProject(t, service, organization, id)
				if again != markdown || second.Settings.Revision != 8 {
					t.Fatal("migration is not idempotent")
				}
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

func readHistoricalWorkflowPolicy(t *testing.T, service *Service, organization tracker.OrganizationID, project tracker.ProjectID, id string) json.RawMessage {
	t.Helper()
	var revision eventstore.Receipt
	if err := service.config.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
		row, err := r.ProjectPolicyRevision(t.Context(), project, id)
		revision = eventstore.Receipt{OrganizationID: organization, PayloadObjectKey: row.PayloadObjectKey, PayloadSHA256: row.PayloadSHA256, PayloadBytes: row.PayloadBytes, PayloadContentBytes: row.PayloadContentBytes}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := eventstore.NewContent(service.runDetailStore()).Receipt(t.Context(), revision)
	if err != nil {
		t.Fatal(fmt.Errorf("read historical policy: %w", err))
	}
	return raw
}
