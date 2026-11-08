package hubserver

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func gitHubWebhookRecordFixture(t *testing.T) (*Service, *time.Time, map[tracker.ProjectID]tracker.NativeWorkItemID) {
	t.Helper()
	repository := projectRecordPostgres(t)
	now := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	service := &Service{localOrganization: "org", database: &database{now: clock, hostedOrganization: "org"}, config: (Config{TenantRepository: repository, AccessRepository: repository, RunDetails: &memoryRunDetails{}, Clock: clock}).normalized()}
	seedPostgresHubAdmin(t, repository, service, nativeOrganization{ID: "org", Name: "Example", Slug: "example"})
	items := make(map[tracker.ProjectID]tracker.NativeWorkItemID)
	for _, project := range []tracker.ProjectID{"first", "second"} {
		if err := repository.WriteTenant(t.Context(), "org", corestore.Serving, func(w tenantstore.Writer) error {
			record, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: "org", ID: project, Name: string(project), Profile: "native"}, States: nativeTriageStates(nativeFixtureStates())})
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
		page, err := (gitHubLifecycleImporter{}).FetchImportPage(t.Context(), GitHubImportRequest{Repository: scope.integrationRepository, IssueNumber: 1, Stage: "issue"})
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

func gitHubWebhookEdit(t *testing.T, service *Service, event string) (projectstore.GitHubWebhook, githubWebhookPayload) {
	t.Helper()
	raw := json.RawMessage(`{"action":"edited","repository":{"full_name":"example-org/example-project"},"issue":{"node_id":"I_source","number":1,"title":"Updated source","body":"Updated source body","html_url":"https://github.com/example-org/example-project/issues/1","state":"open","user":{"login":"source-author"},"created_at":"2026-11-01T00:00:00Z","updated_at":"2026-11-02T00:00:00Z","labels":[],"assignees":[]},"comment":{"node_id":"C_source","body":"Source comment","user":{"login":"source-author"},"created_at":"2026-11-02T00:00:00Z","updated_at":"2026-11-02T00:00:00Z"}}`)
	var payload githubWebhookPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	ref, err := eventstore.NewContent(service.runDetailStore()).PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: "org"}, raw)
	if err != nil {
		t.Fatal(err)
	}
	row := projectstore.GitHubWebhook{DeliveryID: "delivery", Repository: "example-org/example-project", EventType: event, Action: "edited", Payload: ref, ReceivedAt: service.config.now(), NextAttemptAt: service.config.now(), ExpiresAt: service.config.now().Add(time.Hour), Status: "pending"}
	if event == "issue_comment" {
		row.Action = "created"
	}
	return row, payload
}

func TestGitHubWebhookReceiptsAcrossProjects(t *testing.T) {
	service, _, items := gitHubWebhookRecordFixture(t)
	row, payload := gitHubWebhookEdit(t, service, "issues")
	for _, project := range []tracker.ProjectID{"first", "second"} {
		scope, err := service.gitHubIntegrationScope(t.Context(), "org", project)
		if err != nil {
			t.Fatal(err)
		}
		if err := service.applyGitHubWebhookRecord(t.Context(), scope, row, payload); err != nil {
			t.Fatal(err)
		}
		issue, err := service.readWorkItem(t.Context(), scope, string(items[project]))
		if err != nil {
			t.Fatal(err)
		}
		if issue.Title != "Updated source" || issue.Body != "Updated source body" {
			t.Fatalf("project=%s title=%q body=%q", project, issue.Title, issue.Body)
		}
	}
}

func TestGitHubWebhookCursorRecoveryKeepsObservation(t *testing.T) {
	for _, event := range []string{"issues", "issue_comment"} {
		t.Run(event, func(t *testing.T) {
			service, now, items := gitHubWebhookRecordFixture(t)
			row, payload := gitHubWebhookEdit(t, service, event)
			if err := service.config.TenantRepository.WriteTenant(t.Context(), "org", corestore.Serving, func(w tenantstore.Writer) error {
				_, err := w.(projectstore.GitHubIntakeWriter).CreateGitHubWebhook(t.Context(), row)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			scope, err := service.gitHubIntegrationScope(t.Context(), "org", "first")
			if err != nil {
				t.Fatal(err)
			}
			if err := service.applyGitHubWebhookRecord(t.Context(), scope, row, payload); err != nil {
				t.Fatal(err)
			}
			*now = now.Add(time.Hour)
			if err := service.processGitHubWebhookRecord(t.Context(), row); err != nil {
				t.Fatalf("resume committed source with original pending cursor: %v", err)
			}
			if err := service.config.TenantRepository.ReadTenant(t.Context(), "org", corestore.Serving, func(r tenantstore.Reader) error {
				cursor, err := r.(projectstore.GitHubIntakeReader).GitHubWebhook(t.Context(), row.DeliveryID)
				if err != nil {
					return err
				}
				if cursor.AfterProject != "first" || cursor.ReceivedAt != row.ReceivedAt {
					t.Fatalf("cursor=%+v", cursor)
				}
				comments, err := r.SelectComments(t.Context(), "first", items["first"], 0, false, 10)
				if err == nil && event == "issue_comment" && len(comments) != 1 {
					t.Fatalf("replayed comments=%+v", comments)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
