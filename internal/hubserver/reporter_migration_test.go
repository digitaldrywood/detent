package hubserver

import (
	"encoding/json"
	"os"
	"slices"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/neutralimport/neutralimporttest"
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

func historicalWorkflowSource(t *testing.T, organization tracker.OrganizationID) *neutralimporttest.Fixture {
	t.Helper()
	source := newCoreNeutralFixture(t, organization, "organizations", "hosted_tenant", "api_tokens", "hosted_members", "hosted_release_preferences", "daily_summary_preferences", "daily_summary_receipts", "hosted_project_grants", "token_grants", "hosted_sessions", "hosted_transactions", "hosted_invitations", "hosted_member_reservations", "hosted_binding_migrations", "organization_secrets", "organization_secret_audit", "project_secrets", "project_secret_audit", "hosted_audit", "hosted_audit_times", "hosted_work_view_preferences", "projects", "repositories", "policy_revisions", "project_policies", "change_review_policies", "monthly_budget_policies")
	source.Add("organizations", map[string]any{"id": organization, "name": string(organization), "slug": string(organization), "created_at": testTimestamp})
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
