package hubserver

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
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
	t.Parallel()
	service := newPostgresNativeService(t, Config{})
	organization := tracker.OrganizationID("org_reporter")
	source := historicalWorkflowSource(t, organization)
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
		workflowSource := ""
		if id == "prj_repository_default" {
			workflowSource = "detent.yaml"
		}
		source.add("projects", map[string]any{"id": id, "organization_id": organization, "name": id, "profile": "native", "states_json": string(historyTestJSON(t, states)), "workflow_source": workflowSource, "created_at": "2026-10-12T00:00:00Z", "integration_revision": 7})
	}
	for _, id := range []string{"first", "second"} {
		source.add("api_tokens", map[string]any{"id": "token_" + id, "name": id, "token_hash": apikey.HashToken("token_" + id), "scope": "operator", "created_at": "2026-10-12T00:00:00Z", "updated_at": "2026-10-12T00:00:00Z", "hosted_user_id": id, "hosted_membership_id": "mem_" + id, "operator_key_scope": "write", "operator_project_access": "selected", "key_kind": "personal"})
		source.add("hosted_members", map[string]any{"user_id": id, "email": id + "@example.test", "membership_id": "mem_" + id, "role": "member", "principal_id": "token_" + id, "created_at": "2026-10-12T00:00:00Z", "updated_at": "2026-10-12T00:00:00Z"})
		source.add("hosted_project_grants", map[string]any{"user_id": id, "organization_id": organization, "project_id": "prj_without", "can_write": 1})
		source.add("token_grants", map[string]any{"token_id": "token_" + id, "organization_id": organization, "project_id": "prj_without"})
		source.add("hosted_work_view_preferences", map[string]any{"organization_id": organization, "user_id": id, "scope": "project:prj_without", "view_query": "state:Todo"})
		source.add("hosted_invitations", map[string]any{"id": "inv_" + id, "email": id + "@example.test", "organization_id": organization, "role": "member", "created_at": "2026-10-12T00:00:00Z", "expires_at": "2026-11-12T00:00:00Z", "grants_json": `[{"project_id":"prj_without","write":true}]`})
	}
	path := source.write()
	for replay := range 2 {
		if err := importHistoricalWorkflowSource(t, service, organization, path); err != nil {
			t.Fatal(err)
		}
		for id, states := range before {
			record, _ := readHistoricalWorkflowProject(t, service, organization, tracker.ProjectID(id))
			actual := record.States
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

			if !historicalWorkflowStatesEqual(actual, want) {
				t.Fatalf("%s replay%d: got %#v want %#v", id, replay, actual, want)
			}
			triage := 0
			for _, state := range actual {
				if state.Name == "Triage" && !state.Dispatchable && !state.Terminal && !state.OperatorOnly {
					triage++
				}
			}
			if triage != 1 {
				t.Fatalf("%s triage rows=%d", id, triage)
			}
			if id == "prj_default_one" || id == "prj_default_two" {
				for _, target := range []string{"Backlog", "Todo", "Cancelled"} {
					if err := hubrecords.ValidateNativeWorkflowTransition(actual, "Triage", target, hubapi.APIScopeOperator); err != nil {
						t.Fatalf("%s Triage to %s: %v", id, target, err)
					}
					found := false
					for _, state := range actual {
						if state.Name == target {
							found = true
							if state.Terminal != (target == "Cancelled") || state.Dispatchable != (target == "Todo") {
								t.Fatalf("%s %s flags: %+v", id, target, state)
							}
						}
					}
					if !found {
						t.Fatalf("%s missing %s", id, target)
					}
				}
			}
			original := readHistoricalWorkflowOriginal(t, service, organization, tracker.ProjectID(id))
			if original.States != string(historyTestJSON(t, states)) {
				t.Fatalf("%s historical states changed", id)
			}
		}
	}
	for _, id := range []string{"first", "second"} {
		if err := service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			member, err := w.AccessMember(t.Context(), id)
			if err != nil {
				return err
			}
			member.Role = "reporter"
			return w.UpdateAccessMember(t.Context(), member)
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := hostedPostgresAdmin(t, service).ExecContext(t.Context(), `UPDATE hosted_invitations SET role='reporter' WHERE organization_id=$1 AND id=$2`, organization, "inv_"+id); err != nil {
			t.Fatal(err)
		}
		if err := service.config.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
			member, err := r.AccessMember(t.Context(), id)
			if err != nil {
				return err
			}
			credential, err := r.AccessCredential(t.Context(), "token_"+id)
			if err != nil {
				return err
			}
			grant, err := r.AccessProjectGrant(t.Context(), id, "prj_without")
			if err != nil {
				return err
			}
			preference, err := r.WorkViewPreference(t.Context(), id, "prj_without")
			if err != nil {
				return err
			}
			invitation, err := r.AccessInvitation(t.Context(), "inv_"+id)
			if err != nil {
				return err
			}
			invitationGrant, err := r.AccessInvitationGrants(t.Context(), "inv_"+id, "", 2)
			if err != nil {
				return err
			}
			expires := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
			if member.Role != "reporter" || member.PrincipalID != "token_"+id || member.MembershipID != "mem_"+id || credential.Hash != apikey.HashToken("token_"+id) || credential.UserID != id || !grant.CanWrite || preference != "state:Todo" || invitation.Role != "reporter" || invitation.Email != member.Email || !invitation.ExpiresAt.Equal(expires) || len(invitationGrant) != 1 || invitationGrant[0].ProjectID != "prj_without" || !invitationGrant[0].CanWrite {
				return fmt.Errorf("preserved member %s changed: member=%+v preference=%q invitation=%+v grants=%+v", id, member, preference, invitation, invitationGrant)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func historicalWorkflowSource(t *testing.T, organization tracker.OrganizationID) *coreNeutralFixture {
	t.Helper()
	source := newCoreNeutralFixture(t, organization, "organizations", "hosted_tenant", "api_tokens", "hosted_members", "hosted_release_preferences", "daily_summary_preferences", "daily_summary_receipts", "hosted_project_grants", "token_grants", "hosted_sessions", "hosted_transactions", "hosted_invitations", "hosted_member_reservations", "hosted_binding_migrations", "organization_secrets", "organization_secret_audit", "project_secrets", "project_secret_audit", "hosted_audit", "hosted_audit_times", "hosted_work_view_preferences", "projects", "repositories", "policy_revisions", "project_policies", "change_review_policies", "monthly_budget_policies")
	source.add("organizations", map[string]any{"id": organization, "name": string(organization), "slug": string(organization), "created_at": testTimestamp})
	return source
}

func importHistoricalWorkflowSource(t *testing.T, service *Service, organization tracker.OrganizationID, path string) error {
	t.Helper()
	catalog, err := storagecatalog.Read(os.DirFS("../.."), "tools/storageinventory/mappings/v1.json")
	if err != nil {
		return err
	}
	repository := service.config.TenantRepository.(*postgres.Repository)
	access := postgres.AccessCopyImportSource{BundlePath: path, OrganizationID: organization, Catalog: catalog, Objects: service.runDetailStore()}
	if err := repository.InitializeAccessCopyOrganization(t.Context(), access); err != nil {
		return err
	}
	if _, err := repository.ImportCoreProjects(t.Context(), postgres.CoreProjectImportSource{BundlePath: path, OrganizationID: organization, Objects: service.runDetailStore()}); err != nil {
		return err
	}
	if _, err := repository.ImportAccessCopy(t.Context(), access); err != nil {
		return err
	}
	_, err = repository.ImportCorePolicies(t.Context(), postgres.CorePolicyImportSource{BundlePath: path, OrganizationID: organization, Objects: service.runDetailStore()})
	return err
}

func readHistoricalWorkflowProject(t *testing.T, service *Service, organization tracker.OrganizationID, id tracker.ProjectID) (projectstore.Record, string) {
	t.Helper()
	var record projectstore.Record
	if err := service.config.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		record, err = r.ProjectRecord(t.Context(), id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	markdown, err := service.projectRecordWorkflow(t.Context(), record)
	if err != nil {
		t.Fatal(err)
	}
	return record, markdown
}

func readHistoricalWorkflowOriginal(t *testing.T, service *Service, organization tracker.OrganizationID, id tracker.ProjectID) struct{ States, Workflow string } {
	t.Helper()
	var receipt eventstore.Receipt
	if err := service.config.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		receipt, err = r.Receipt(t.Context(), "production-import", "core-project", string(id))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := eventstore.NewContent(service.runDetailStore()).Receipt(t.Context(), receipt)
	if err != nil {
		t.Fatal(err)
	}
	var original struct{ States, Workflow string }
	if err := json.Unmarshal(raw, &original); err != nil {
		t.Fatal(err)
	}
	return original
}

func historicalWorkflowStatesEqual(actual, want []tracker.NativeState) bool {
	if len(actual) != len(want) {
		return false
	}
	for index, state := range actual {
		expected := want[index]
		if state.Name != expected.Name || state.Terminal != expected.Terminal || state.Dispatchable != expected.Dispatchable || state.OperatorOnly != expected.OperatorOnly || !slices.Equal(state.Transitions, expected.Transitions) {
			return false
		}
	}
	return true
}
