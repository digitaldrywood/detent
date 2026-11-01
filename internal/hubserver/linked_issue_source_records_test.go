package hubserver

import (
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestLinkedIssueSourceRecordUsesTenantStorageAndArchivedSnapshot(t *testing.T) {
	repository := projectRecordPostgres(t)
	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), RunDetails: &memoryRunDetails{}, TenantRepository: repository})
	for _, name := range []string{"First source organization", "Second source organization"} {
		t.Run(name, func(t *testing.T) {
			org, err := service.createNativeOrganizationFor(t.Context(), name)
			if err != nil {
				t.Fatal(err)
			}
			projectsPath := "/api/v2/organizations/" + string(org.ID) + "/projects"
			createdProject := performHubAPIRequest(t, service, http.MethodPost, projectsPath, testHubAdminToken, map[string]any{"idempotency_key": "project", "name": "Linked source project", "states": nativeFixtureStates()})
			requireNativeStatus(t, createdProject, http.StatusOK)
			var project tracker.NativeProject
			decodeHubResponse(t, createdProject, &project)
			if err := repository.WriteTenant(t.Context(), org.ID, corestore.Serving, func(w tenantstore.Writer) error {
				record, err := w.ProjectRecord(t.Context(), project.ID)
				if err != nil {
					return err
				}
				record.Settings.RepositoryEnabled = true
				record.Settings.Repository = "example/repository"
				_, err = w.UpdateProjectRecord(t.Context(), record)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			itemsPath := projectsPath + "/" + string(project.ID) + "/work-items"
			request := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "linked-issue"}, GitHubIssueURL: "https://github.com/example/repository/issues/12"}
			created := performHubAPIRequest(t, service, http.MethodPost, itemsPath, testHubAdminToken, request)
			requireNativeStatus(t, created, http.StatusOK)
			var issue tracker.NativeIssue
			decodeHubResponse(t, created, &issue)
			if issue.State != "Triage" || issue.LinkedSource == nil || issue.LinkedSource.Status != "pending" || issue.LinkedSource.URL != "https://github.com/example/repository/issues/12" {
				t.Fatalf("linked issue=%+v", issue)
			}
			replay := performHubAPIRequest(t, service, http.MethodPost, itemsPath, testHubAdminToken, request)
			requireNativeStatus(t, replay, http.StatusOK)
			if replay.Body.String() != created.Body.String() {
				t.Fatal("linked issue replay changed the archived response")
			}
			duplicate := request
			duplicate.IdempotencyKey = "linked-issue-new-key"
			duplicateResponse := performHubAPIRequest(t, service, http.MethodPost, itemsPath, testHubAdminToken, duplicate)
			requireNativeStatus(t, duplicateResponse, http.StatusOK)
			var duplicateIssue tracker.NativeIssue
			decodeHubResponse(t, duplicateResponse, &duplicateIssue)
			if duplicateIssue.WorkItemID != issue.WorkItemID {
				t.Fatalf("duplicate source created another item: %s != %s", duplicateIssue.WorkItemID, issue.WorkItemID)
			}
			now := service.config.now()
			snapshot := tracker.GitHubIssueSnapshot{URL: request.GitHubIssueURL, Title: "Imported title", Body: "Imported body", Provenance: tracker.Provenance{Provider: "github", ExternalID: "node_12", AuthorID: "example-user", CreatedAt: now, UpdatedAt: now, ObservedAt: now}, Comments: []tracker.GitHubIssueComment{{Body: "Imported discussion", Provenance: tracker.Provenance{Provider: "github", ExternalID: "comment_12", AuthorID: "example-user", CreatedAt: now, UpdatedAt: now, ObservedAt: now}}}}
			intake := tracker.GitHubIntake{Mutation: tracker.Mutation{IdempotencyKey: "source-intake"}, Snapshot: snapshot}
			intakePath := itemsPath + "/" + string(issue.WorkItemID) + "/source-intake"
			intakeResponse := performHubAPIRequest(t, service, http.MethodPost, intakePath, testHubAdminToken, intake)
			requireNativeStatus(t, intakeResponse, http.StatusOK)
			intakeReplay := performHubAPIRequest(t, service, http.MethodPost, intakePath, testHubAdminToken, intake)
			requireNativeStatus(t, intakeReplay, http.StatusOK)
			if intakeReplay.Body.String() != intakeResponse.Body.String() {
				t.Fatal("linked source intake replay changed its archived response")
			}
			read := performHubAPIRequest(t, service, http.MethodGet, itemsPath+"/"+string(issue.WorkItemID), testHubAdminToken, nil)
			requireNativeStatus(t, read, http.StatusOK)
			var refreshed tracker.NativeIssue
			decodeHubResponse(t, read, &refreshed)
			if refreshed.Title != snapshot.Title || refreshed.Body != snapshot.Body || refreshed.LinkedSource == nil || refreshed.LinkedSource.Status != "complete" || refreshed.LinkedSource.Snapshot == nil || refreshed.LinkedSource.Snapshot.Title != snapshot.Title {
				t.Fatalf("refreshed source=%+v", refreshed.LinkedSource)
			}
			if err := repository.ReadTenant(t.Context(), org.ID, corestore.Serving, func(r tenantstore.Reader) error {
				comment, err := r.CommentBySource(t.Context(), project.ID, issue.WorkItemID, "github:comment_12")
				if err != nil {
					return err
				}
				if comment.ID == "" {
					t.Fatal("intake did not persist the imported comment")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			stale := intake
			stale.IdempotencyKey = "stale-source-intake"
			stale.Snapshot.Provenance.CreatedAt = now.Add(-2 * time.Second)
			stale.Snapshot.Provenance.UpdatedAt = now.Add(-time.Second)
			stale.Snapshot.Title = "Stale title"
			stale.Snapshot.Comments = append(stale.Snapshot.Comments, tracker.GitHubIssueComment{Body: "Later imported discussion", Provenance: tracker.Provenance{Provider: "github", ExternalID: "comment_13", AuthorID: "example-user", CreatedAt: now, UpdatedAt: now, ObservedAt: now}})
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, intakePath, testHubAdminToken, stale), http.StatusOK)
			read = performHubAPIRequest(t, service, http.MethodGet, itemsPath+"/"+string(issue.WorkItemID), testHubAdminToken, nil)
			requireNativeStatus(t, read, http.StatusOK)
			decodeHubResponse(t, read, &refreshed)
			if refreshed.Title != snapshot.Title || refreshed.LinkedSource.Snapshot.Title != snapshot.Title {
				t.Fatalf("stale intake replaced current source: %+v", refreshed)
			}
			var localCount int
			if err := service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM linked_issue_sources WHERE work_item_id=?", issue.WorkItemID).Scan(&localCount); err != nil {
				t.Fatal(err)
			}
			if localCount != 0 {
				t.Fatal("selected linked source wrote to SQLite")
			}
		})
	}
}

func TestLinkedIssueSourceRecordUsesSelectedOwnersWithoutSQLite(t *testing.T) {
	repository := projectRecordPostgres(t)
	f := newHostedSecurityFixture(t, func(cfg *Config) {
		cfg.AccessRepository = repository
		cfg.BillingRepository = repository
		cfg.TenantRepository = repository
		cfg.RunDetails = &memoryRunDetails{}
	})
	organization := tracker.OrganizationID("org_security")
	project := projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: f.project, Name: "Example project", Slug: "example-project", Profile: "native", RequireDependencies: true}, Settings: projectstore.Settings{OrganizationID: organization, ProjectID: f.project, RepositoryEnabled: true, Repository: "example/repository"}, States: nativeFixtureStates()}
	if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: "Example organization", CreatedAt: f.service.config.now()}); err != nil {
			return err
		}
		if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: organization, ProviderID: "org_provider"}); err != nil {
			return err
		}
		_, err := w.CreateProjectRecord(t.Context(), project)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	owner := f.user(t, "owner", "owner", "owner@example.test", "", "")
	if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		member, err := w.AccessMember(t.Context(), owner.identity.Subject)
		if err != nil {
			return err
		}
		if err := w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: organization, UserID: member.UserID, ProjectID: f.project, CanWrite: true}); err != nil {
			return err
		}
		return w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: member.PrincipalID, ProjectID: f.project})
	}); err != nil {
		t.Fatal(err)
	}
	created := f.request(t, owner, http.MethodPost, f.base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "selected-source-create"}, GitHubIssueURL: "https://github.com/example/repository/issues/12"})
	requireNativeStatus(t, created, http.StatusOK)
	var issue tracker.NativeIssue
	decodeHubResponse(t, created, &issue)
	if err := f.service.database.db.Close(); err != nil {
		t.Fatal(err)
	}
	now := f.service.config.now()
	snapshot := tracker.GitHubIssueSnapshot{URL: "https://github.com/example/repository/issues/12", Title: "Imported title", Body: "Imported body", Provenance: tracker.Provenance{Provider: "github", ExternalID: "node_12", AuthorID: "example-user", CreatedAt: now, UpdatedAt: now, ObservedAt: now}}
	request := tracker.GitHubIntake{Mutation: tracker.Mutation{IdempotencyKey: "selected-source-intake"}, Snapshot: snapshot}
	response := f.request(t, owner, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/source-intake", request)
	requireNativeStatus(t, response, http.StatusOK)
	var refreshed tracker.NativeIssue
	decodeHubResponse(t, response, &refreshed)
	if refreshed.Title != snapshot.Title || refreshed.Body != snapshot.Body || refreshed.LinkedSource == nil || refreshed.LinkedSource.Status != "complete" {
		t.Fatalf("source intake did not complete from selected owners: %+v", refreshed)
	}
}
