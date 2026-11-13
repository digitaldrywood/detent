package hubserver

import (
	"net/http"
	"strconv"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/neutralimport"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/neutralimport/neutralimporttest"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestCoreWorkItemImportSupportsMutationsAndSearchAcrossTenants(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"native", "historical", "missing search"} {
		t.Run(scenario, func(t *testing.T) {
			repository, admin := coreImportPostgresWithAdmin(t, 4)
			objects := &memoryRunDetails{}
			service := openLocalPostgresHubTestService(t, repository, Config{RunDetails: objects, SecretKeys: secretTestKeys(t, "1", "1")})
			var fixtures []nativeFixture
			item := tracker.NativeWorkItemID("wi_core_import")
			if scenario == "historical" {
				item = "wi_original_41"
			}
			for _, org := range []tracker.OrganizationID{"org_import_first", "org_import_second"} {
				seedWorkItemRecordOrganization(t, repository, service, nativeOrganization{ID: org, Name: string(org), Slug: string(org)})
				var f nativeFixture
				var fixture *neutralimporttest.Fixture
				if scenario == "historical" {
					project := tracker.ProjectID("prj_import")
					if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
						record, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: project, Name: "Example project", Profile: "github_compatible"}, States: nativeFixtureStates()})
						if err != nil {
							return err
						}
						record.Settings.RepositoryEnabled, record.Settings.Repository = true, "example-org/example-project"
						_, err = w.UpdateProjectRecord(t.Context(), record)
						return err
					}); err != nil {
						t.Fatal(err)
					}
					f = nativeFixture{service: service, project: tracker.NativeProject{OrganizationID: org, ID: project}, base: "/api/v2/organizations/" + string(org) + "/projects/" + string(project), token: testHubAdminToken}
					seedNativeFixturePolicy(t, service, string(org)+"/"+string(project))
					fixture = neutralimporttest.HistoricalV7(t, org)
				} else {
					f = newNativeFixture(t, service, org, "Example project")
					fixture = coreWorkItemNeutralFixture(t, org, f.project.ID, false)
				}
				source := CoreWorkItemImportSource{BundlePath: fixture.Write(), OrganizationID: org, ProjectID: f.project.ID, Objects: objects}
				if scenario == "historical" {
					if err := neutralimport.Read(t.Context(), source.BundlePath, org, func(bundle *neutralimport.Bundle) error {
						for _, id := range []int64{41, 43} {
							suffix := strconv.FormatInt(id, 10)
							source.IdentityMappings = append(source.IdentityMappings, CoreWorkItemIdentityMapping{SourceSHA256: bundle.Identity().SourceSHA256, OrganizationID: org, SourceIssueID: id, SourceRepositoryID: 1, SourceRepositoryNodeID: "R_original", SourceGitHubNodeID: "I_original_" + suffix, TargetProjectID: f.project.ID, TargetWorkItemID: tracker.NativeWorkItemID("wi_original_" + suffix)})
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}
				for range 2 {
					if count, err := ImportCoreWorkItems(t.Context(), repository, source); err != nil || count != 2 {
						t.Fatalf("import count=%d error=%v", count, err)
					}
				}
				fixtures = append(fixtures, f)
			}
			for _, f := range fixtures {
				t.Run(string(f.project.OrganizationID), func(t *testing.T) {
					org := f.project.OrganizationID
					content := searchstore.NewContent(objects)
					assertSearch := func(title string) {
						t.Helper()
						for {
							more, err := content.RefreshOne(t.Context(), repository, org)
							if err != nil {
								t.Fatal(err)
							}
							if !more {
								break
							}
						}
						page, err := content.Find(t.Context(), repository, org, searchstore.Query{Text: title, Kind: "work", ProjectID: f.project.ID, WorkItemID: item, Limit: 10})
						if err != nil || len(page.Items) != 1 || page.Items[0].Document.WorkItemID != item || page.Items[0].Document.OrganizationID != org {
							t.Errorf("search page=%+v error=%v", page, err)
						}
					}
					path := f.base + "/work-items/" + string(item)
					response := performHubAPIRequest(t, service, http.MethodGet, path, f.token, nil)
					requireNativeStatus(t, response, http.StatusOK)
					var issue tracker.NativeIssue
					decodeHubResponse(t, response, &issue)
					assertSearch(issue.Title)
					if scenario == "historical" {
						return
					}
					if scenario == "missing search" {
						if _, err := admin.ExecContext(t.Context(), `DELETE FROM search_documents WHERE organization_id=$1 AND id=$2`, org, "issue:"+string(item)); err != nil {
							t.Fatal(err)
						}
					}
					title := "Searchable " + string(org)
					response = performHubAPIRequest(t, service, http.MethodPatch, path, f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "edit-import"}, ExpectedRevision: issue.Revision, Title: &title})
					requireNativeStatus(t, response, http.StatusOK)
					decodeHubResponse(t, response, &issue)
					if issue.Title != title {
						t.Fatalf("edited title=%q", issue.Title)
					}
					response = performHubAPIRequest(t, service, http.MethodPost, path+"/comments", f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "comment-import"}, Body: "Imported issue discussion"})
					requireNativeStatus(t, response, http.StatusOK)
					var comment tracker.NativeComment
					decodeHubResponse(t, response, &comment)
					if comment.Body != "Imported issue discussion" || comment.WorkItemID != item {
						t.Fatalf("created comment=%+v", comment)
					}
					response = performHubAPIRequest(t, service, http.MethodPost, path+"/workflow", f.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "move-import"}, ExpectedRevision: issue.Revision, State: "Done", Reason: "user_requested"})
					requireNativeStatus(t, response, http.StatusOK)
					decodeHubResponse(t, response, &issue)
					if issue.State != "Done" {
						t.Fatalf("moved state=%q", issue.State)
					}
					assertSearch(title)
				})
			}
		})
	}
}
