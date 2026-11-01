package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/issuecontract"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const workItemRecordBody = "## Acceptance criteria\nThe issue is served.\n## Must not break\nOther tenants.\n## How we know it worked\nRead the issue."

func TestWorkItemRecordBackendParity(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			cfg := Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), RunDetails: &memoryRunDetails{}}
			var repository tenantstore.Repository
			if backend == "postgres" {
				repository = projectRecordPostgres(t)
				cfg.TenantRepository = repository
			}
			service := openTestService(t, cfg)
			for _, name := range []string{"First organization", "Second organization"} {
				t.Run(name, func(t *testing.T) {
					org, err := service.createNativeOrganizationFor(t.Context(), name)
					if err != nil {
						t.Fatal(err)
					}
					key := string(org.ID) + ":"
					projectBase := "/api/v2/organizations/" + string(org.ID) + "/projects"
					response := performHubAPIRequest(t, service, http.MethodPost, projectBase, testHubAdminToken, map[string]any{"idempotency_key": key + "project", "name": "Example project", "states": nativeFixtureStates()})
					requireNativeStatus(t, response, http.StatusOK)
					var project tracker.NativeProject
					decodeHubResponse(t, response, &project)
					if repository == nil {
						if _, err := service.database.db.ExecContext(t.Context(), "UPDATE projects SET duplicate_challenge=0 WHERE id=?", project.ID); err != nil {
							t.Fatal(err)
						}
					} else {
						if err := repository.WriteTenant(t.Context(), org.ID, corestore.Serving, func(w tenantstore.Writer) error {
							record, err := w.ProjectRecord(t.Context(), project.ID)
							if err != nil {
								return err
							}
							record.Settings.DuplicateChallenge = false
							_, err = w.UpdateProjectRecord(t.Context(), record)
							return err
						}); err != nil {
							t.Fatal(err)
						}
					}
					base := projectBase + "/" + string(project.ID) + "/work-items"
					input := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: key + "issue"}, Title: "Example issue", Body: workItemRecordBody, Labels: []string{"example"}, Assignees: []string{"example-user"}}
					if repository != nil {
						machine := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: key + "machine"}, Title: "Machine report", Body: issueorigin.Stamp(workItemRecordBody, issueorigin.Origin{Kind: "worker", Fingerprint: "example-fingerprint"})}
						requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, base, testHubAdminToken, machine), http.StatusServiceUnavailable)
						challenged := input
						challenged.IdempotencyKey = key + "challenge"
						challenged.ChallengeToken = "example-challenge"
						challenged.NotDuplicateOf = []int{1}
						requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, base, testHubAdminToken, challenged), http.StatusServiceUnavailable)
					}
					response = performHubAPIRequest(t, service, http.MethodPost, base, testHubAdminToken, input)
					requireNativeStatus(t, response, http.StatusOK)
					var issue tracker.NativeIssue
					decodeHubResponse(t, response, &issue)
					if issue.Number != 1 || issue.Revision != 1 || issue.Body != input.Body || !slices.Equal(issue.Labels, input.Labels) || !slices.Equal(issue.Assignees, input.Assignees) {
						t.Fatalf("created=%+v", issue)
					}
					replay := performHubAPIRequest(t, service, http.MethodPost, base, testHubAdminToken, input)
					requireNativeStatus(t, replay, http.StatusOK)
					if response.Body.String() != replay.Body.String() {
						t.Fatal("issue replay changed response")
					}
					changed := input
					changed.Title = "Different"
					requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, base, testHubAdminToken, changed), http.StatusConflict)
					itemPath := base + "/" + string(issue.WorkItemID)
					read := performHubAPIRequest(t, service, http.MethodGet, itemPath, testHubAdminToken, nil)
					requireNativeStatus(t, read, http.StatusOK)
					var got tracker.NativeIssue
					decodeHubResponse(t, read, &got)
					if got.Body != issue.Body || got.Number != issue.Number {
						t.Fatalf("read=%+v", got)
					}
					title := "Edited title"
					patch := tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: key + "edit"}, ExpectedRevision: 1, Title: &title, Priority: tracker.SetPriority(new(2))}
					edited := performHubAPIRequest(t, service, http.MethodPatch, itemPath, testHubAdminToken, patch)
					requireNativeStatus(t, edited, http.StatusOK)
					requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPatch, itemPath, testHubAdminToken, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: key + "stale"}, ExpectedRevision: 1, Title: &title}), http.StatusConflict)
					repeat := performHubAPIRequest(t, service, http.MethodPatch, itemPath, testHubAdminToken, patch)
					requireNativeStatus(t, repeat, http.StatusOK)
					if edited.Body.String() != repeat.Body.String() {
						t.Fatal("edit replay changed response")
					}
					for index := range 3 {
						comment := tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("%scomment-%d", key, index)}, Body: fmt.Sprintf("Comment %d", index)}
						created := performHubAPIRequest(t, service, http.MethodPost, itemPath+"/comments", testHubAdminToken, comment)
						requireNativeStatus(t, created, http.StatusOK)
						replayed := performHubAPIRequest(t, service, http.MethodPost, itemPath+"/comments", testHubAdminToken, comment)
						requireNativeStatus(t, replayed, http.StatusOK)
						if created.Body.String() != replayed.Body.String() {
							t.Fatal("comment replay changed response")
						}
					}
					for _, direction := range []string{"oldest", "newest"} {
						cursor := ""
						var sequences []int64
						for {
							path := itemPath + "/comments?limit=1&direction=" + direction + "&cursor=" + url.QueryEscape(cursor)
							listed := performHubAPIRequest(t, service, http.MethodGet, path, testHubAdminToken, nil)
							requireNativeStatus(t, listed, http.StatusOK)
							var page tracker.Page[tracker.NativeComment]
							decodeHubResponse(t, listed, &page)
							if len(page.Items) != 1 {
								t.Fatalf("comment page=%+v", page)
							}
							sequences = append(sequences, page.Items[0].Sequence)
							cursor = page.NextCursor
							if cursor == "" {
								break
							}
						}
						want := []int64{3, 4, 5}
						if direction == "newest" {
							slices.Reverse(want)
						}
						if !slices.Equal(sequences, want) {
							t.Fatalf("sequences=%v want=%v", sequences, want)
						}
					}
					comments := performHubAPIRequest(t, service, http.MethodGet, itemPath+"/comments", testHubAdminToken, nil)
					requireNativeStatus(t, comments, http.StatusOK)
					var page tracker.Page[tracker.NativeComment]
					decodeHubResponse(t, comments, &page)
					original := page.Items[0]
					commentPath := itemPath + "/comments/" + original.ID
					update := tracker.UpdateComment{Mutation: tracker.Mutation{IdempotencyKey: key + "edit-comment"}, ExpectedRevision: 1, Body: "Edited comment"}
					updated := performHubAPIRequest(t, service, http.MethodPatch, commentPath, testHubAdminToken, update)
					requireNativeStatus(t, updated, http.StatusOK)
					stale := update
					stale.IdempotencyKey = key + "stale-comment"
					requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPatch, commentPath, testHubAdminToken, stale), http.StatusConflict)
					deletion := tracker.UpdateComment{Mutation: tracker.Mutation{IdempotencyKey: key + "delete-comment"}, ExpectedRevision: 2, Deleted: true}
					deleted := performHubAPIRequest(t, service, http.MethodPatch, commentPath, testHubAdminToken, deletion)
					requireNativeStatus(t, deleted, http.StatusOK)
					var tombstone tracker.NativeComment
					decodeHubResponse(t, deleted, &tombstone)
					if !tombstone.Deleted || tombstone.Body != "" || tombstone.Sequence != original.Sequence {
						t.Fatalf("deleted=%+v", tombstone)
					}
					archived := performHubAPIRequest(t, service, http.MethodPost, itemPath+"/archive", testHubAdminToken, archiveIssueRequest{Mutation: tracker.Mutation{IdempotencyKey: key + "archive"}, ExpectedRevision: 2})
					requireNativeStatus(t, archived, http.StatusOK)
					var archivedIssue tracker.NativeIssue
					decodeHubResponse(t, archived, &archivedIssue)
					if !archivedIssue.Archived || archivedIssue.Revision != 3 {
						t.Fatalf("archive=%+v", archivedIssue)
					}
					requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, itemPath+"/restore", testHubAdminToken, archiveIssueRequest{Mutation: tracker.Mutation{IdempotencyKey: key + "restore"}, ExpectedRevision: 3}), http.StatusOK)
					results := make(chan *httptest.ResponseRecorder, 4)
					var workers sync.WaitGroup
					for index := range 4 {
						workers.Go(func() {
							request := input
							request.IdempotencyKey = key + "concurrent-" + strconv.Itoa(index)
							request.Title = "Number allocation " + strconv.Itoa(index)
							results <- performHubAPIRequest(t, service, http.MethodPost, base, testHubAdminToken, request)
						})
					}
					workers.Wait()
					close(results)
					var numbers []int
					for response := range results {
						requireNativeStatus(t, response, http.StatusOK)
						var item tracker.NativeIssue
						decodeHubResponse(t, response, &item)
						numbers = append(numbers, item.Number)
					}
					slices.Sort(numbers)
					if !slices.Equal(numbers, []int{2, 3, 4, 5}) {
						t.Fatalf("concurrent numbers=%v", numbers)
					}
					repeated := input
					repeated.IdempotencyKey = key + "concurrent-replay"
					repeated.Title = "Repeated number allocation"
					replies := make(chan *httptest.ResponseRecorder, 4)
					for range 4 {
						workers.Go(func() {
							replies <- performHubAPIRequest(t, service, http.MethodPost, base, testHubAdminToken, repeated)
						})
					}
					workers.Wait()
					close(replies)
					var receipt string
					for response := range replies {
						requireNativeStatus(t, response, http.StatusOK)
						if receipt == "" {
							receipt = response.Body.String()
						} else if response.Body.String() != receipt {
							t.Fatal("concurrent replay changed response")
						}
					}
					foreign := projectBase + "/missing-project/work-items/" + string(issue.WorkItemID)
					requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, foreign, testHubAdminToken, nil), http.StatusNotFound)
					if repository != nil {
						scope := nativeScope{organization: org.ID, project: project.ID, sourceActor: &tracker.Actor{Kind: "human", PrincipalID: "different-principal"}, credential: apiCredential{HostedRole: "reporter"}}
						if _, err := service.readWorkItem(t.Context(), scope, string(issue.WorkItemID)); !isNativeNotFound(err) {
							t.Fatalf("reporter read error=%v", err)
						}
						var historicalVersion eventstore.CollaborationVersion
						if err := repository.ReadTenant(t.Context(), org.ID, corestore.Serving, func(r tenantstore.Reader) error {
							events, err := r.Events(t.Context(), project.ID, issue.WorkItemID, 0, corestore.MaxPageSize)
							if err != nil {
								return err
							}
							if len(events) != 9 {
								t.Errorf("events=%d want 9", len(events))
							}
							for index, event := range events {
								if event.Sequence != int64(index+1) {
									t.Errorf("event sequence=%d", event.Sequence)
								}
							}
							historicalVersion, err = r.CollaborationVersion(t.Context(), original.ID, 1)
							if err != nil {
								return err
							}
							row, err := r.WorkItem(t.Context(), project.ID, issue.WorkItemID)
							if err != nil {
								return err
							}
							encodedBody, err := json.Marshal(issue.Body)
							if err != nil {
								return err
							}
							expected, _, err := attachment.PrepareArchive(string(org.ID), 1, bytes.NewReader(encodedBody))
							if err != nil {
								return err
							}
							if row.BodyObjectKey == nil || *row.BodyObjectKey != expected.Key {
								t.Error("body reference does not identify the original text")
							}

							return nil
						}); err != nil {
							t.Fatal(err)
						}
						raw, err := eventstore.NewContent(service.runDetailStore()).CollaborationVersion(t.Context(), historicalVersion)
						if err != nil {
							t.Fatal(err)
						}
						var historical tracker.NativeComment
						if err := json.Unmarshal(raw, &historical); err != nil {
							t.Fatal(err)
						}
						if historical.Body != original.Body || historical.Deleted {
							t.Fatalf("history changed: %+v", historical)
						}
						if name == "First organization" {
							verifyWorkItemMaximumCommentPage(t, service, repository, issue, itemPath)
						}

					}
				})
			}
		})
	}
}

type projectUpdateBeforeWriteRepository struct {
	tenantstore.Repository
	mu     sync.Mutex
	before func() error
	skip   int
}

func (r *projectUpdateBeforeWriteRepository) WriteTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, write func(tenantstore.Writer) error) error {
	r.mu.Lock()
	before := r.before
	if r.skip > 0 {
		r.skip--
		before = nil
	} else {
		r.before = nil
	}
	r.mu.Unlock()
	if before != nil {
		if err := before(); err != nil {
			return err
		}
	}
	return r.Repository.WriteTenant(ctx, organization, workload, write)
}

func TestWorkItemCreationReplansAfterProjectWorkflowChanges(t *testing.T) {
	repository := projectRecordPostgres(t)
	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), RunDetails: &memoryRunDetails{}, TenantRepository: repository})
	organization, err := service.createNativeOrganizationFor(t.Context(), "Example organization")
	if err != nil {
		t.Fatal(err)
	}
	projects := "/api/v2/organizations/" + string(organization.ID) + "/projects"
	response := performHubAPIRequest(t, service, http.MethodPost, projects, testHubAdminToken, map[string]any{"idempotency_key": "project", "name": "Example project", "require_dependencies": true, "states": nativeFixtureStates()})
	requireNativeStatus(t, response, http.StatusOK)
	var project tracker.NativeProject
	decodeHubResponse(t, response, &project)

	intercepted := &projectUpdateBeforeWriteRepository{Repository: repository}
	var injectedErr error
	intercepted.before = func() error {
		injectedErr = repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
			record, err := w.ProjectRecord(t.Context(), project.ID)
			if err != nil {
				return err
			}
			record.Core.RequireDependencies = false
			_, err = w.UpdateProjectRecord(t.Context(), record)
			return err
		})
		return injectedErr
	}
	service.config.TenantRepository = intercepted

	path := projects + "/" + string(project.ID) + "/work-items"
	request := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "stale-workflow"}, Title: "Example issue", Body: workItemRecordBody}
	response = performHubAPIRequest(t, service, http.MethodPost, path, testHubAdminToken, request)
	if injectedErr != nil {
		t.Fatalf("update project workflow: %v", injectedErr)
	}
	requireNativeStatus(t, response, http.StatusOK)
	var issue tracker.NativeIssue
	decodeHubResponse(t, response, &issue)
	if !issue.IgnoreDependencies {
		t.Fatal("issue used the pre-update require_dependencies setting")
	}
	if intercepted.before != nil {
		t.Fatal("project update did not run before the issue write")
	}
	if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
		items, err := r.WorkItems(t.Context(), project.ID, 0, 10)
		if err != nil {
			return err
		}
		if len(items) != 1 {
			t.Errorf("issues=%d want 1", len(items))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestWorkItemRecordReplayRechecksAccessInTenantWrite(t *testing.T) {
	repository := projectRecordPostgres(t)
	f := newHostedSecurityFixture(t, func(cfg *Config) {
		cfg.AccessRepository = repository
		cfg.TenantRepository = repository
		cfg.RunDetails = &memoryRunDetails{}
	})
	if err := repository.WriteTenant(t.Context(), tracker.OrganizationID("org_security"), corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: "org_security", Name: "Example organization", CreatedAt: f.service.config.now()}); err != nil {
			return err
		}
		return w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: "org_security", ProviderID: "org_provider"})
	}); err != nil {
		t.Fatal(err)
	}
	member := f.user(t, "writer", "member", "writer@example.test", "", "")
	scope := nativeScope{organization: tracker.OrganizationID("org_security"), project: f.project}
	project := projectstore.Record{Core: corestore.Project{OrganizationID: scope.organization, ID: scope.project, Name: "Example project", Slug: "example-project", Profile: "native", RequireDependencies: true}, States: nativeFixtureStates()}
	if err := repository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.CreateProjectRecord(t.Context(), project)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		member, err := w.AccessMember(t.Context(), member.identity.Subject)
		if err != nil {
			return err
		}
		if err := w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: scope.organization, UserID: member.UserID, ProjectID: scope.project, CanWrite: true}); err != nil {
			return err
		}
		return w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: scope.organization, TokenID: member.PrincipalID, ProjectID: scope.project})
	}); err != nil {
		t.Fatal(err)
	}
	request := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "revoked-during-replay"}, Title: "Example issue", Body: workItemRecordBody}
	created := f.request(t, member, http.MethodPost, f.base+"/work-items", request)
	requireNativeStatus(t, created, http.StatusOK)
	intercepted := &projectUpdateBeforeWriteRepository{Repository: repository}
	var revokeErr error
	intercepted.before = func() error {
		revokeErr = f.service.revokeHostedMemberLocally(t.Context(), member.identity.Subject)
		return revokeErr
	}
	f.service.config.TenantRepository = intercepted
	response := f.request(t, member, http.MethodPost, f.base+"/work-items", request)
	if revokeErr != nil {
		t.Fatalf("revoke member before tenant write: %v", revokeErr)
	}
	requireNativeStatus(t, response, http.StatusForbidden)
	if err := repository.ReadTenant(t.Context(), scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		items, err := r.WorkItems(t.Context(), scope.project, 0, 10)
		if err != nil {
			return err
		}
		if len(items) != 1 {
			t.Errorf("work items after rejected replay = %d, want 1", len(items))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestWorkItemProvenanceDedupRespectsReporterVisibility(t *testing.T) {
	repository := projectRecordPostgres(t)
	f := newHostedSecurityFixture(t, func(cfg *Config) {
		cfg.AccessRepository = repository
		cfg.TenantRepository = repository
		cfg.RunDetails = &memoryRunDetails{}
	})
	scope := nativeScope{organization: tracker.OrganizationID("org_security"), project: f.project}
	if err := repository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: scope.organization, Name: "Example organization", CreatedAt: f.service.config.now()}); err != nil {
			return err
		}
		return w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: scope.organization, ProviderID: "org_provider"})
	}); err != nil {
		t.Fatal(err)
	}
	states := append(nativeFixtureStates(), tracker.NativeState{Name: "Triage", Transitions: []string{"Todo"}})
	project := projectstore.Record{Core: corestore.Project{OrganizationID: scope.organization, ID: scope.project, Name: "Example project", Slug: "example-project", Profile: "native", RequireDependencies: true}, States: states}
	if err := repository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.CreateProjectRecord(t.Context(), project)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	owner := f.user(t, "owner", "reporter", "owner@example.test", "", "")
	other := f.user(t, "other", "reporter", "other@example.test", "", "")
	for _, reporter := range []hostedSecurityUser{owner, other} {
		if err := repository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
			member, err := w.AccessMember(t.Context(), reporter.identity.Subject)
			if err != nil {
				return err
			}
			if err := w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: scope.organization, UserID: member.UserID, ProjectID: scope.project, CanWrite: true}); err != nil {
				return err
			}
			return w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: scope.organization, TokenID: member.PrincipalID, ProjectID: scope.project})
		}); err != nil {
			t.Fatal(err)
		}
	}
	now := f.service.config.now()
	request := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "owner-file"}, Type: "bug", Title: "Example report", Body: reporterBugSections + issueContractTestSections, Provenance: &tracker.Provenance{Provider: "github", ExternalID: "issue-42", AuthorID: "external-user", CreatedAt: now, UpdatedAt: now, ObservedAt: now}}
	created := f.request(t, owner, http.MethodPost, f.base+"/work-items", request)
	requireNativeStatus(t, created, http.StatusOK)
	var original tracker.NativeIssue
	decodeHubResponse(t, created, &original)
	retry := request
	retry.IdempotencyKey = "owner-retry"
	replayed := f.request(t, owner, http.MethodPost, f.base+"/work-items", retry)
	requireNativeStatus(t, replayed, http.StatusOK)
	var duplicate tracker.NativeIssue
	decodeHubResponse(t, replayed, &duplicate)
	if duplicate.WorkItemID != original.WorkItemID || duplicate.Body != original.Body {
		t.Fatalf("same-reporter provenance retry changed issue: got=%+v original=%+v", duplicate, original)
	}
	invalid := request
	invalid.IdempotencyKey = "owner-invalid-provenance"
	invalid.Provenance = new(tracker.Provenance)
	*invalid.Provenance = *request.Provenance
	invalid.Provenance.AuthorID = ""
	requireNativeStatus(t, f.request(t, owner, http.MethodPost, f.base+"/work-items", invalid), http.StatusUnprocessableEntity)
	challenged := request
	challenged.IdempotencyKey = "owner-unsupported-challenge"
	challenged.ChallengeToken = "example-challenge"
	challenged.NotDuplicateOf = []int{original.Number}
	requireNativeStatus(t, f.request(t, owner, http.MethodPost, f.base+"/work-items", challenged), http.StatusServiceUnavailable)
	foreign := request
	foreign.IdempotencyKey = "other-reporter-file"
	response := f.request(t, other, http.MethodPost, f.base+"/work-items", foreign)
	requireNativeStatus(t, response, http.StatusNotFound)
}

func TestWorkItemRecordBodyEditClearsReturnedIssueAction(t *testing.T) {
	repository := projectRecordPostgres(t)
	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), RunDetails: &memoryRunDetails{}, TenantRepository: repository})
	organization, err := service.createNativeOrganizationFor(t.Context(), "Example organization")
	if err != nil {
		t.Fatal(err)
	}
	projects := "/api/v2/organizations/" + string(organization.ID) + "/projects"
	response := performHubAPIRequest(t, service, http.MethodPost, projects, testHubAdminToken, map[string]any{"idempotency_key": "project", "name": "Example project", "states": nativeFixtureStates()})
	requireNativeStatus(t, response, http.StatusOK)
	var project tracker.NativeProject
	decodeHubResponse(t, response, &project)
	path := projects + "/" + string(project.ID) + "/work-items"
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		projectRecord, err := w.ProjectRecord(t.Context(), project.ID)
		if err != nil {
			return err
		}
		projectRecord.States = append(projectRecord.States, tracker.NativeState{Name: "Blocked", Transitions: []string{"Todo"}})
		_, err = w.UpdateProjectRecord(t.Context(), projectRecord)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	incomplete := "## Acceptance criteria\nInitial description."
	response = performHubAPIRequest(t, service, http.MethodPost, path, testHubAdminToken, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "issue"}, Title: "Example issue", Body: incomplete, State: "Blocked"})
	requireNativeStatus(t, response, http.StatusOK)
	var issue tracker.NativeIssue
	decodeHubResponse(t, response, &issue)
	issue.IssueContract = &issuecontract.State{HumanAction: "Complete the issue contract", ReturnState: "Todo"}
	issue.Revision++
	plan := workItemRecordPlan{issue: issue}
	fields, err := service.prepareWorkItemFields(t.Context(), organization.ID, plan)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(issue)
	if err != nil {
		t.Fatal(err)
	}
	version, err := eventstore.NewContent(service.runDetailStore()).PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: issue.OrganizationID, ProjectID: issue.ProjectID, WorkItemID: issue.WorkItemID, RecordID: string(issue.WorkItemID), Revision: int64(issue.Revision)}, raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		row, err := w.WorkItem(t.Context(), issue.ProjectID, issue.WorkItemID)
		if err != nil {
			return err
		}
		row.BodyObjectKey = &fields.Body
		row.IssueContractObjectKey = fields.Contract
		row.WorkflowStateName = &issue.State
		if _, err := w.UpdateWorkItem(t.Context(), row); err != nil {
			return err
		}
		_, err = w.CreateCollaborationVersion(t.Context(), version)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	fixed := nativeContractTestBody
	response = performHubAPIRequest(t, service, http.MethodPatch, path+"/"+string(issue.WorkItemID), testHubAdminToken, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "complete-contract"}, ExpectedRevision: issue.Revision, Body: &fixed})
	requireNativeStatus(t, response, http.StatusOK)
	var edited tracker.NativeIssue
	decodeHubResponse(t, response, &edited)
	if edited.State != "Blocked" || edited.IssueContract == nil || edited.IssueContract.ReturnState != "Todo" || edited.IssueContract.HumanAction != "" {
		t.Fatalf("body edit did not clear recovery action while preserving return state: %+v", edited.IssueContract)
	}
	if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
		row, err := r.WorkItem(t.Context(), issue.ProjectID, issue.WorkItemID)
		if err != nil {
			return err
		}
		storedVersion, err := r.CollaborationVersion(t.Context(), string(issue.WorkItemID), row.Revision)
		if err != nil {
			return err
		}
		stored, err := service.hydrateWorkItemRecord(t.Context(), row, storedVersion)
		if err != nil {
			return err
		}
		if stored.IssueContract == nil || stored.IssueContract.ReturnState != "Todo" || stored.IssueContract.HumanAction != "" {
			t.Errorf("stored recovery contract = %+v", stored.IssueContract)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestWorkItemRecordCommentEditConflictAfterConcurrentWrite(t *testing.T) {
	repository := projectRecordPostgres(t)
	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), RunDetails: &memoryRunDetails{}, TenantRepository: repository})
	organization, err := service.createNativeOrganizationFor(t.Context(), "Example organization")
	if err != nil {
		t.Fatal(err)
	}
	projects := "/api/v2/organizations/" + string(organization.ID) + "/projects"
	response := performHubAPIRequest(t, service, http.MethodPost, projects, testHubAdminToken, map[string]any{"idempotency_key": "project", "name": "Example project", "states": nativeFixtureStates()})
	requireNativeStatus(t, response, http.StatusOK)
	var project tracker.NativeProject
	decodeHubResponse(t, response, &project)
	items := projects + "/" + string(project.ID) + "/work-items"
	response = performHubAPIRequest(t, service, http.MethodPost, items, testHubAdminToken, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "issue"}, Title: "Example issue", Body: nativeContractTestBody})
	requireNativeStatus(t, response, http.StatusOK)
	var issue tracker.NativeIssue
	decodeHubResponse(t, response, &issue)
	itemPath := items + "/" + string(issue.WorkItemID)
	response = performHubAPIRequest(t, service, http.MethodPost, itemPath+"/comments", testHubAdminToken, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "comment"}, Body: "Original comment"})
	requireNativeStatus(t, response, http.StatusOK)
	var comment tracker.NativeComment
	decodeHubResponse(t, response, &comment)
	commentPath := itemPath + "/comments/" + comment.ID
	intercepted := &projectUpdateBeforeWriteRepository{Repository: repository}
	intercepted.before = func() (writeErr error) {
		body, err := service.prepareWorkItemField(t.Context(), organization.ID, []byte(`"Competing comment"`))
		if err != nil {
			return err
		}
		competing := comment
		competing.Body = "Competing comment"
		competing.Revision++
		competing.UpdatedAt = service.config.now()
		raw, err := json.Marshal(competing)
		if err != nil {
			return err
		}
		version, err := eventstore.NewContent(service.runDetailStore()).PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: competing.OrganizationID, ProjectID: competing.ProjectID, WorkItemID: competing.WorkItemID, RecordID: competing.ID, Revision: int64(competing.Revision)}, raw)
		if err != nil {
			return err
		}
		return repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
			row, err := w.Comment(t.Context(), issue.ProjectID, issue.WorkItemID, comment.ID)
			if err != nil {
				return err
			}
			row.BodyObjectKey = body
			row.UpdatedAt = competing.UpdatedAt
			if _, err = w.UpdateComment(t.Context(), row); err != nil {
				return err
			}
			_, err = w.CreateCollaborationVersion(t.Context(), version)
			return err
		})
	}
	service.config.TenantRepository = intercepted
	outer := tracker.UpdateComment{Mutation: tracker.Mutation{IdempotencyKey: "stale-edit"}, ExpectedRevision: comment.Revision, Body: "Stale comment"}
	response = performHubAPIRequest(t, service, http.MethodPatch, commentPath, testHubAdminToken, outer)
	requireNativeStatus(t, response, http.StatusConflict)
	service.config.TenantRepository = repository
	var current tracker.NativeComment
	if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
		row, err := r.Comment(t.Context(), issue.ProjectID, issue.WorkItemID, comment.ID)
		if err != nil {
			return err
		}
		version, err := r.CollaborationVersion(t.Context(), comment.ID, row.Revision)
		if err != nil {
			return err
		}
		current, err = service.hydrateCommentRecord(t.Context(), row, version)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if current.Revision != comment.Revision+1 || current.Body != "Competing comment" {
		t.Fatalf("concurrent comment state = %+v", current)
	}
}

func TestWorkItemRecordMutationRequiresHostedFeature(t *testing.T) {
	f := newHostedSecurityFixture(t)
	f.service.config.RunDetails = &memoryRunDetails{}
	owner := f.user(t, "owner", "owner", "owner@example.test", "write", "")
	scope := nativeScope{organization: tracker.OrganizationID("org_security"), project: f.project}
	repository := projectRecordPostgres(t)
	project := projectstore.Record{Core: corestore.Project{OrganizationID: scope.organization, ID: scope.project, Name: "Example project", Slug: "example-project", Profile: "native", RequireDependencies: true}, States: nativeFixtureStates()}
	if err := repository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.CreateProjectRecord(t.Context(), project)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f.service.config.TenantRepository = repository

	plans := hostedTestPlans(t, f.service, nil)
	unentitled := plans.Plans[0]
	unentitled.Version++
	unentitled.Features = slices.DeleteFunc(slices.Clone(unentitled.Features), func(feature string) bool { return feature == "collaboration" })
	plans.Plans = append(plans.Plans, unentitled)
	plans.Base = unentitled.PlanReference
	hosted := *f.service.config.Hosted
	hosted.Plans = &plans
	if err := f.service.database.configureHostedPlans(t.Context(), &hosted); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE hosted_plan_assignments SET base_version=?", unentitled.Version); err != nil {
		t.Fatal(err)
	}
	path := f.base + "/work-items"
	request := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "issue"}, Title: "Example issue", Body: workItemRecordBody}
	response := f.request(t, owner, http.MethodPost, path, request)
	requireNativeStatus(t, response, http.StatusTooManyRequests)
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		items, err := r.WorkItems(t.Context(), scope.project, 0, 10)
		if err != nil {
			return err
		}
		if len(items) != 0 {
			t.Errorf("unentitled issue writes=%d", len(items))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

type rejectedWorkItemRepository struct {
	tenantstore.Repository
	err   error
	phase string
	item  *tracker.NativeWorkItemID
}

type rejectedWorkItemWriter struct {
	tenantstore.Writer
	reject rejectedWorkItemRepository
}

func (w rejectedWorkItemWriter) AppendEvent(ctx context.Context, event eventstore.Event) (eventstore.Event, error) {
	*w.reject.item = event.WorkItemID
	stored, err := w.Writer.AppendEvent(ctx, event)
	if err != nil {
		return stored, err
	}
	if w.reject.phase == "event" {
		return stored, w.reject.err
	}
	return stored, nil
}

func (r rejectedWorkItemRepository) WriteTenant(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, write func(tenantstore.Writer) error) error {
	return r.Repository.WriteTenant(ctx, org, workload, func(w tenantstore.Writer) error {
		if err := write(rejectedWorkItemWriter{Writer: w, reject: r}); err != nil {
			return err
		}
		return r.err
	})
}

func TestWorkItemRecordRollback(t *testing.T) {
	repository := projectRecordPostgres(t)
	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), RunDetails: &memoryRunDetails{}, TenantRepository: repository})
	org, err := service.createNativeOrganizationFor(t.Context(), "Example organization")
	if err != nil {
		t.Fatal(err)
	}
	base := "/api/v2/organizations/" + string(org.ID) + "/projects"
	created := performHubAPIRequest(t, service, http.MethodPost, base, testHubAdminToken, map[string]any{"idempotency_key": "project", "name": "Example project", "states": nativeFixtureStates()})
	requireNativeStatus(t, created, http.StatusOK)
	var project tracker.NativeProject
	decodeHubResponse(t, created, &project)
	base += "/" + string(project.ID) + "/work-items"
	credential, _, err := service.authenticateAPIToken(t.Context(), testHubAdminToken, "", "")
	if err != nil {
		t.Fatal(err)
	}
	request := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "rollback"}, Title: "Example issue", Body: workItemRecordBody}
	for _, phase := range []string{"event", "receipt"} {
		t.Run(phase, func(t *testing.T) {
			var failedItem tracker.NativeWorkItemID
			service.config.TenantRepository = rejectedWorkItemRepository{Repository: repository, err: errors.New("reject command"), phase: phase, item: &failedItem}
			rejected := performHubAPIRequest(t, service, http.MethodPost, base, testHubAdminToken, request)
			requireNativeStatus(t, rejected, http.StatusInternalServerError)
			if err := repository.ReadTenant(t.Context(), org.ID, corestore.Serving, func(r tenantstore.Reader) error {
				number, err := r.NextWorkItemNumber(t.Context(), project.ID)
				if err != nil {
					return err
				}
				if number != 1 {
					t.Errorf("rolled back number=%d", number)
				}
				items, err := r.WorkItems(t.Context(), project.ID, 0, 10)
				if err != nil {
					return err
				}
				if len(items) != 0 {
					t.Errorf("partial issues=%+v", items)
				}
				events, err := r.Events(t.Context(), project.ID, failedItem, 0, 10)
				if err != nil {
					return err
				}
				if len(events) != 0 {
					t.Errorf("events survived rollback: %+v", events)
				}
				_, err = r.CollaborationVersion(t.Context(), string(failedItem), 1)
				if !errors.Is(err, corestore.ErrNotFound) {
					t.Errorf("version after rollback: %v", err)
				}
				_, err = r.Receipt(t.Context(), credential.ID, "POST "+base, "rollback")
				if !errors.Is(err, corestore.ErrNotFound) {
					t.Errorf("receipt after rollback: %v", err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
	service.config.TenantRepository = repository
	committed := performHubAPIRequest(t, service, http.MethodPost, base, testHubAdminToken, request)
	requireNativeStatus(t, committed, http.StatusOK)
	var issue tracker.NativeIssue
	decodeHubResponse(t, committed, &issue)
	if issue.Number != 1 {
		t.Fatalf("committed number=%d", issue.Number)
	}
}

func verifyWorkItemMaximumCommentPage(t *testing.T, service *Service, repository tenantstore.Repository, issue tracker.NativeIssue, path string) {
	t.Helper()
	var comments []corestore.Comment
	var versions []eventstore.CollaborationVersion
	content := eventstore.NewContent(service.runDetailStore())
	for index := range 198 {
		comment := tracker.NativeComment{OrganizationID: issue.OrganizationID, ProjectID: issue.ProjectID, WorkItemID: issue.WorkItemID, ID: fmt.Sprintf("%s-page-boundary-%d", issue.WorkItemID, index), Revision: 1, Sequence: int64(index + 10), Body: "Boundary comment", Actor: issue.Actor, CreatedAt: issue.CreatedAt, UpdatedAt: issue.CreatedAt}
		raw, err := json.Marshal(comment)
		if err != nil {
			t.Fatal(err)
		}
		version, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: issue.OrganizationID, ProjectID: issue.ProjectID, WorkItemID: issue.WorkItemID, RecordID: comment.ID, Revision: 1}, raw)
		if err != nil {
			t.Fatal(err)
		}
		encodedBody, err := json.Marshal(comment.Body)
		if err != nil {
			t.Fatal(err)
		}
		body, err := service.prepareWorkItemField(t.Context(), issue.OrganizationID, encodedBody)
		if err != nil {
			t.Fatal(err)
		}
		comments = append(comments, corestore.Comment{OrganizationID: issue.OrganizationID, ProjectID: issue.ProjectID, WorkItemID: issue.WorkItemID, ID: comment.ID, Sequence: comment.Sequence, BodyObjectKey: body, ActorKind: comment.Actor.Kind, ActorPrincipalID: comment.Actor.PrincipalID, CreatedAt: comment.CreatedAt, UpdatedAt: comment.UpdatedAt})
		versions = append(versions, version)
	}
	if err := repository.WriteTenant(t.Context(), issue.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		for index, comment := range comments {
			if _, err := w.CreateComment(t.Context(), comment); err != nil {
				return err
			}
			if _, err := w.CreateCollaborationVersion(t.Context(), versions[index]); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	listed := performHubAPIRequest(t, service, http.MethodGet, path+"/comments?limit=200", testHubAdminToken, nil)
	requireNativeStatus(t, listed, http.StatusOK)
	var first tracker.Page[tracker.NativeComment]
	decodeHubResponse(t, listed, &first)
	if len(first.Items) != 200 || first.NextCursor == "" {
		t.Fatalf("maximum page: length=%d cursor=%q", len(first.Items), first.NextCursor)
	}
	listed = performHubAPIRequest(t, service, http.MethodGet, path+"/comments?limit=200&cursor="+url.QueryEscape(first.NextCursor), testHubAdminToken, nil)
	requireNativeStatus(t, listed, http.StatusOK)
	var last tracker.Page[tracker.NativeComment]
	decodeHubResponse(t, listed, &last)
	if len(last.Items) != 1 || last.NextCursor != "" || last.Items[0].Sequence <= first.Items[199].Sequence {
		t.Fatalf("boundary page=%+v", last)
	}
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, path+"/comments?limit=200&direction=newest&cursor="+url.QueryEscape(first.NextCursor), testHubAdminToken, nil), http.StatusUnprocessableEntity)
}
