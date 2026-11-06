package hubserver

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresImportStarts(t *testing.T) {
	repository := projectRecordPostgres(t)
	service := &Service{config: Config{TenantRepository: repository, AccessRepository: repository, RunDetails: &memoryRunDetails{}, now: time.Now}}
	seedPostgresHubAdmin(t, repository, service, nativeOrganization{ID: "org", Name: "Example", Slug: "example"})
	for _, project := range []tracker.ProjectID{"first", "second"} {
		err := repository.WriteTenant(t.Context(), "org", corestore.Serving, func(w tenantstore.Writer) error {
			record, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: "org", ID: project, Name: string(project), Profile: "native"}, States: nativeFixtureStates()})
			if err != nil {
				return err
			}
			record.Settings.Repository = "example-org/" + string(project)
			record.Settings.Intake = "manual"
			_, err = w.UpdateProjectRecord(t.Context(), record)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		scope := nativeScope{organization: "org", project: project, credential: apiCredential{ID: "test_hub_admin", Hash: apikey.HashToken(testHubAdminToken), Scope: apiScopeAdmin}}
		request := startGitHubImportRequest{Mutation: tracker.Mutation{IdempotencyKey: "start"}, IssueNumber: 1}
		options := nativeCommandOptions{OperationID: projectOperationID(scope, "POST", "/imports")}
		raw, err := service.executeNativeMutation(t.Context(), scope, options, request.Mutation, request, service.startGitHubImportOperation(request))
		if err != nil {
			t.Fatal(err)
		}
		var job GitHubImport
		if err := json.Unmarshal(raw, &job); err != nil {
			t.Fatal(err)
		}
		got, err := service.readGitHubImportRecord(t.Context(), scope, job.ID)
		if err != nil || got.ID != job.ID || got.Revision != 1 {
			t.Fatalf("job=%+v err=%v", got, err)
		}
		replay, err := service.executeNativeMutation(t.Context(), scope, options, request.Mutation, request, service.startGitHubImportOperation(request))
		if err != nil || string(replay) != string(raw) {
			t.Fatalf("import replay=%s error=%v", replay, err)
		}
	}
}
