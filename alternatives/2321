package hubserver

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestProjectGitHubBatchUsesPostgresRecords(t *testing.T) {
	repository := projectRecordPostgres(t)
	details := &memoryRunDetails{}
	now := time.Now().UTC()
	service := &Service{database: &database{hostedOrganization: "org_example"}, config: Config{TenantRepository: repository, AccessRepository: repository, RunDetails: details, now: func() time.Time { return now }}}
	if err := repository.WriteTenant(t.Context(), "org_example", corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: "org_example", Name: "Example", CreatedAt: now}); err != nil {
			return err
		}
		return w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: "org_example", ID: "admin", Name: "Example", Hash: apikey.HashToken("example-token"), Scope: "admin", CreatedAt: now, UpdatedAt: now, KeyScope: "admin", ProjectAccess: "all", Kind: "service"})
	}); err != nil {
		t.Fatal(err)
	}
	for _, project := range []tracker.ProjectID{"prj_first", "prj_second"} {
		t.Run(string(project), func(t *testing.T) {
			scope := nativeScope{organization: "org_example", project: project, credential: apiCredential{ID: "admin", Scope: apiScopeAdmin, Hash: apikey.HashToken("example-token")}}
			if err := repository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
				_, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: scope.organization, ID: project, Name: string(project), Profile: "native"}, States: nativeFixtureStates()})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			empty, err := service.projectGitHubBatch(t.Context(), scope)
			if err != nil || empty.Batch != nil || len(empty.Lanes) == 0 {
				t.Fatalf("empty project batch = %+v, %v", empty, err)
			}
			batch := tracker.GitHubBatch{ID: "batch_" + string(project), Revision: 1, Status: "ready", HubOwned: true, Page: tracker.GitHubDiscoveryPage{Total: 2, Issues: []tracker.GitHubIssuePreview{{Number: 1, Body: "private body", Snapshot: &tracker.GitHubIssueSnapshot{}}}}, Items: []tracker.GitHubBatchItem{{Number: 1, Status: "completed"}, {Number: 2, Status: "pending"}}}
			raw, err := json.Marshal(batch)
			if err != nil {
				t.Fatal(err)
			}
			payload, err := eventstore.NewContent(details).PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: scope.organization}, raw)
			if err != nil {
				t.Fatal(err)
			}
			record := projectstore.IntakeBatch{ProjectID: project, ID: batch.ID, Revision: batch.Revision, Status: batch.Status, HubOwned: batch.HubOwned, Payload: payload}
			if err := repository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
				return w.(projectstore.IntakeBatchWriter).SaveProjectIntakeBatch(t.Context(), record, 0)
			}); err != nil {
				t.Fatal(err)
			}
			if err := repository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
				return w.(projectstore.IntakeBatchWriter).SaveProjectIntakeBatch(t.Context(), record, 0)
			}); !errors.Is(err, corestore.ErrRevisionConflict) {
				t.Fatalf("stale batch write = %v", err)
			}
			actual, err := service.projectGitHubBatch(t.Context(), scope)
			if err != nil {
				t.Fatal(err)
			}
			if actual.Batch == nil || actual.Batch.ID != batch.ID || actual.Batch.Page.Issues[0].Body != "" || actual.Batch.Page.Issues[0].Snapshot != nil || actual.Progress.Discovered != 1 || actual.Progress.Total != 2 || actual.Progress.Completed != 1 || actual.Progress.Pending != 1 {
				t.Fatalf("batch view = %+v", actual)
			}
		})
	}
	for _, project := range []tracker.ProjectID{"prj_first", "prj_second"} {
		scope := nativeScope{organization: "org_example", project: project, credential: apiCredential{ID: "admin", Scope: apiScopeAdmin, Hash: apikey.HashToken("example-token")}}
		actual, err := service.projectGitHubBatch(t.Context(), scope)
		if err != nil || actual.Batch == nil || actual.Batch.ID != "batch_"+string(project) {
			t.Fatalf("independent project batch = %+v, %v", actual, err)
		}
	}
}
