package coreimport_test

import (
	"net/http"
	"os"
	"strconv"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/coreimport"
	"github.com/digitaldrywood/detent/internal/hubserver/coreimport/coreimporttest"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
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
			objects := hubtest.NewDetails()
			h := hubfixture.Open(t, hubserver.Config{RunDetails: objects, SecretKeys: hubfixture.SecretKeys(t)})
			repository, admin := h.Repository, h.Admin
			var fixtures []hubfixture.Native
			item := tracker.NativeWorkItemID("wi_core_import")
			if scenario == "historical" {
				item = "wi_original_41"
			}
			for _, org := range []tracker.OrganizationID{"org_import_first", "org_import_second"} {
				h.EnsureOrganization(t, org)
				var f hubfixture.Native
				var fixture *neutralimporttest.Fixture
				if scenario == "historical" {
					project := tracker.ProjectID("prj_import")
					if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
						record, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: project, Name: "Example project", Profile: "github_compatible"}, States: hubtest.States()})
						if err != nil {
							return err
						}
						record.Settings.RepositoryEnabled, record.Settings.Repository = true, "example-org/example-project"
						_, err = w.UpdateProjectRecord(t.Context(), record)
						return err
					}); err != nil {
						t.Fatal(err)
					}
					f = hubfixture.Native{Hub: h, Project: tracker.NativeProject{OrganizationID: org, ID: project}, Base: "/api/v2/organizations/" + string(org) + "/projects/" + string(project), Token: h.AdminToken()}
					hubtest.Policy(t, repository, objects, f.Project, hubtest.PolicyOptions{Now: h.Now()})
					fixture = neutralimporttest.HistoricalV7(t, org)
				} else {
					f = hubfixture.NewNative(t, h, org, "Example project")
					fixture = coreimporttest.WorkItems(t, os.DirFS("../../../.."), org, f.Project.ID, false, coreimporttest.WorkItemBody())
				}
				source := coreimport.CoreWorkItemImportSource{BundlePath: fixture.Write(), OrganizationID: org, ProjectID: f.Project.ID, Objects: objects}
				if scenario == "historical" {
					if err := neutralimport.Read(t.Context(), source.BundlePath, org, func(bundle *neutralimport.Bundle) error {
						for _, id := range []int64{41, 43} {
							suffix := strconv.FormatInt(id, 10)
							source.IdentityMappings = append(source.IdentityMappings, coreimport.CoreWorkItemIdentityMapping{SourceSHA256: bundle.Identity().SourceSHA256, OrganizationID: org, SourceIssueID: id, SourceRepositoryID: 1, SourceRepositoryNodeID: "R_original", SourceGitHubNodeID: "I_original_" + suffix, TargetProjectID: f.Project.ID, TargetWorkItemID: tracker.NativeWorkItemID("wi_original_" + suffix)})
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}
				for range 2 {
					if count, err := coreimport.ImportCoreWorkItems(t.Context(), repository, source); err != nil || count != 2 {
						t.Fatalf("import count=%d error=%v", count, err)
					}
				}
				fixtures = append(fixtures, f)
			}
			for _, f := range fixtures {
				t.Run(string(f.Project.OrganizationID), func(t *testing.T) {
					org := f.Project.OrganizationID
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
						page, err := content.Find(t.Context(), repository, org, searchstore.Query{Text: title, Kind: "work", ProjectID: f.Project.ID, WorkItemID: item, Limit: 10})
						if err != nil || len(page.Items) != 1 || page.Items[0].Document.WorkItemID != item || page.Items[0].Document.OrganizationID != org {
							t.Errorf("search page=%+v error=%v", page, err)
						}
					}
					path := f.Base + "/work-items/" + string(item)
					response := h.Request(t, http.MethodGet, path, f.Token, nil)
					hubfixture.RequireStatus(t, response, http.StatusOK)
					var issue tracker.NativeIssue
					hubfixture.Decode(t, response, &issue)
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
					response = h.Request(t, http.MethodPatch, path, f.Token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "edit-import"}, ExpectedRevision: issue.Revision, Title: &title})
					hubfixture.RequireStatus(t, response, http.StatusOK)
					hubfixture.Decode(t, response, &issue)
					if issue.Title != title {
						t.Fatalf("edited title=%q", issue.Title)
					}
					response = h.Request(t, http.MethodPost, path+"/comments", f.Token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "comment-import"}, Body: "Imported issue discussion"})
					hubfixture.RequireStatus(t, response, http.StatusOK)
					var comment tracker.NativeComment
					hubfixture.Decode(t, response, &comment)
					if comment.Body != "Imported issue discussion" || comment.WorkItemID != item {
						t.Fatalf("created comment=%+v", comment)
					}
					response = h.Request(t, http.MethodPost, path+"/workflow", f.Token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "move-import"}, ExpectedRevision: issue.Revision, State: "Done", Reason: "user_requested"})
					hubfixture.RequireStatus(t, response, http.StatusOK)
					hubfixture.Decode(t, response, &issue)
					if issue.State != "Done" {
						t.Fatalf("moved state=%q", issue.State)
					}
					assertSearch(title)
				})
			}
		})
	}
}
