package hubserver

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func gitHubWebhookRecordFixture(t *testing.T) (*Service, *time.Time, map[tracker.ProjectID]tracker.NativeWorkItemID) {
	t.Helper()
	repository := projectRecordPostgres(t)
	now := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	service := &Service{localOrganization: "org", notifications: newNotificationBroker(), database: &database{now: clock, hostedOrganization: "org"}, config: (Config{TenantRepository: repository, AccessRepository: repository, RunDetails: &memoryRunDetails{}, Clock: clock}).normalized()}
	seedPostgresHubAdmin(t, repository, service, nativeOrganization{ID: "org", Name: "Example", Slug: "example"})
	items := make(map[tracker.ProjectID]tracker.NativeWorkItemID)
	for _, project := range []tracker.ProjectID{"first", "second"} {
		if err := repository.WriteTenant(t.Context(), "org", corestore.Serving, func(w tenantstore.Writer) error {
			record, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: "org", ID: project, Name: string(project), Profile: "native"}, States: hubrecords.NativeTriageStates(nativeFixtureStates())})
			if err != nil {
				return err
			}
			record.Settings.Repository, record.Settings.Intake = "example-org/example-project", "manual"
			_, err = w.UpdateProjectRecord(t.Context(), record)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		scope, err := service.gitHubIntegrationScope(t.Context(), "org", project)
		if err != nil {
			t.Fatal(err)
		}
		page, err := (gitHubLifecycleImporter{}).FetchImportPage(t.Context(), GitHubImportRequest{Repository: scope.IntegrationRepository, IssueNumber: 1, Stage: "issue"})
		if err != nil {
			t.Fatal(err)
		}
		snapshot := linkedSnapshotFromSource(*page.Issue, now.Add(-time.Hour))
		issue, err := service.importGitHubSnapshotRecord(t.Context(), scope, "seed:"+string(project), tracker.GitHubIssuePreview{URL: page.Issue.URL, Number: 1}, snapshot, "Triage", nil)
		if err != nil {
			t.Fatal(err)
		}
		items[project] = issue.WorkItemID
	}
	return service, &now, items
}
