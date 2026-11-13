package hubserver

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPolicyRevisionInputsMigrationPreservesHistory(t *testing.T) {
	t.Parallel()
	admin := postgresStartupTemplate.Database(t)
	config := postgres.Config{Connection: startupConnection(t, admin, "detent_runtime_v2", ""), Pool: postgres.PoolConfig{MaxConnections: 3, ServingConnections: 1, BackgroundConnections: 1}}
	store, err := openPostgresDatabase(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	const organization tracker.OrganizationID = "org_policy_history"
	at := parseTestTime(t, testTimestamp)
	projects := []tracker.ProjectID{"prj_policy_first", "prj_policy_second"}
	if err := store.postgres.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: "Policy history", Slug: "policy-history", CreatedAt: at}); err != nil {
			return err
		}
		if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: organization, ProviderID: "provider_policy_history"}); err != nil {
			return err
		}
		for index, project := range projects {
			record, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: project, Name: string(project), Slug: projectstore.Slug(string(project)), Profile: "native"}, States: nativeFixtureStates()})
			if err != nil {
				return err
			}
			record.Settings.Repository = []string{"example-org/first", "example-org/second"}[index]
			if _, err := w.UpdateProjectRecord(t.Context(), record); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	type revision struct {
		project    tracker.ProjectID
		descriptor policy.Descriptor
		raw        string
	}
	var revisions []revision
	fixture := newCoreNeutralFixture(t, organization, "policy_revisions", "project_policies", "change_review_policies", "monthly_budget_policies")
	for _, project := range projects {
		for _, commit := range []string{"", strings.Repeat("a", 40), strings.Repeat("b", 40)} {
			descriptor := hubTestPolicy()
			if commit != "" {
				descriptor.Workflow = &policy.Workflow{Source: "detent.yaml", Revision: commit, States: nativeFixtureStates()}
			}
			descriptor = descriptor.WithID()
			raw, err := json.Marshal(descriptor)
			if err != nil {
				t.Fatal(err)
			}
			fixture.add("policy_revisions", map[string]any{"scope": string(organization) + "/" + string(project), "policy_id": descriptor.ID, "metadata_json": string(raw), "approved_by": "administrator", "approved_at": testTimestamp, "source_digest": "", "workflow_revision": "", "gate_github_pull_request": descriptor.Gates.GitHubPullRequest})
			revisions = append(revisions, revision{project, descriptor, string(raw)})
		}
		fixture.add("project_policies", map[string]any{"scope": string(organization) + "/" + string(project), "policy_id": revisions[len(revisions)-1].descriptor.ID})
	}
	objects := &memoryRunDetails{}
	source := postgres.CorePolicyImportSource{BundlePath: fixture.write(), OrganizationID: organization, Objects: objects}
	for range 2 {
		result, err := store.postgres.ImportCorePolicies(t.Context(), source)
		if err != nil || result.PolicyRevisions != 6 || result.CurrentPolicies != 2 {
			t.Fatalf("historical policy import=%+v error=%v", result, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = openPostgresDatabase(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	for _, revision := range revisions {
		var saved policystore.Revision
		if err := store.postgres.ReadTenant(t.Context(), organization, corestore.Serving, func(reader tenantstore.Reader) error {
			row, err := reader.ProjectPolicyRevision(t.Context(), revision.project, revision.descriptor.ID)
			if err != nil {
				return err
			}
			if row.ApprovedBy != "administrator" || !row.ApprovedAt.Equal(at) {
				return fmt.Errorf("migration changed approval audit: %+v", row)
			}
			saved = row
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		encoded, err := objects.ReadDetail(t.Context(), string(organization), eventstore.Reference(saved.PayloadObjectKey, saved.PayloadSHA256, saved.PayloadBytes, saved.PayloadContentBytes))
		if err != nil {
			t.Fatal(err)
		}
		var resolved policy.Descriptor
		if err := json.Unmarshal(encoded, &resolved); err != nil {
			t.Fatal(err)
		}
		wantCommit, gotCommit := "", ""
		if revision.descriptor.Workflow != nil {
			wantCommit = revision.descriptor.Workflow.Revision
		}
		if resolved.Workflow != nil {
			gotCommit = resolved.Workflow.Revision
		}
		if string(encoded) != revision.raw || resolved.SourceDigest != revision.descriptor.SourceDigest || gotCommit != wantCommit || !reflect.DeepEqual(resolved, revision.descriptor) {
			t.Fatalf("migration changed audit record or lost exact policy inputs: raw=%s descriptor=%+v", encoded, resolved)
		}
	}
	service := &Service{config: Config{TenantRepository: store.postgres, RunDetails: objects}}
	for projectIndex, project := range projects {
		scoped := revisions[projectIndex*3 : projectIndex*3+3]
		if err := store.postgres.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			record, err := w.ProjectRecord(t.Context(), project)
			if err != nil {
				return err
			}
			for index, revision := range scoped {
				previous := scoped[(index+2)%3].descriptor
				if err := service.createTenantWorkflowApply(t.Context(), w, hubscope.NativeScope{Organization: organization, Project: project}, record, previous.ID, previous, revision.descriptor, "administrator", "example-runner", at, eventstore.PolicyLanding{}, false); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		history, next, err := service.tenantProjectWorkflowHistory(t.Context(), organization, project, 10, "")
		if err != nil || next != "" || len(history) != 3 {
			t.Fatalf("typed workflow history=%+v next=%q error=%v", history, next, err)
		}
		for index, entry := range history {
			currentIndex := 2 - index
			current, previous := scoped[currentIndex].descriptor, scoped[(currentIndex+2)%3].descriptor
			commit := ""
			if current.Workflow != nil {
				commit = current.Workflow.Revision
			}
			if entry.Commit != commit || entry.DefinitionDigest != current.SourceDigest || entry.PreviousDefinitionDigest != previous.SourceDigest || entry.AppliedBy != "administrator" || entry.AppliedAt != testTimestamp || !reflect.DeepEqual(entry.Definition, &current) || !reflect.DeepEqual(entry.PreviousDefinition, &previous) {
				t.Fatalf("typed history selected wrong source/workflow revision: %+v want=%+v previous=%+v", entry, current, previous)
			}
		}
	}
}
