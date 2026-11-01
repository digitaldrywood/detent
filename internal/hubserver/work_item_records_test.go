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

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/issuecontract"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/operatortool"
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

func TestPostgresWorkItemRelationshipsAndLabels(t *testing.T) {
	repository := projectRecordPostgres(t)
	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), RunDetails: &memoryRunDetails{}, TenantRepository: repository})
	createProject := func(name, key string) (nativeOrganization, tracker.NativeProject) {
		t.Helper()
		organization, err := service.createNativeOrganizationFor(t.Context(), name)
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
			return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization.ID, Name: organization.Name, Slug: organization.Slug, Local: true, CreatedAt: service.config.now()})
		}); err != nil {
			t.Fatal(err)
		}
		projects := "/api/v2/organizations/" + string(organization.ID) + "/projects"
		response := performHubAPIRequest(t, service, http.MethodPost, projects, testHubAdminToken, map[string]any{"idempotency_key": key, "name": name + " project", "states": nativeFixtureStates()})
		requireNativeStatus(t, response, http.StatusOK)
		var project tracker.NativeProject
		decodeHubResponse(t, response, &project)
		if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
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
		return organization, project
	}
	firstOrganization, firstProject := createProject("First organization", "first-project")
	secondOrganization, secondProject := createProject("Second organization", "second-project")
	createIssue := func(organization nativeOrganization, project tracker.NativeProject, key, title string, labels []string) tracker.NativeIssue {
		t.Helper()
		path := "/api/v2/organizations/" + string(organization.ID) + "/projects/" + string(project.ID) + "/work-items"
		response := performHubAPIRequest(t, service, http.MethodPost, path, testHubAdminToken, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: key}, Title: title, Body: workItemRecordBody, Labels: labels})
		requireNativeStatus(t, response, http.StatusOK)
		var issue tracker.NativeIssue
		decodeHubResponse(t, response, &issue)
		return issue
	}
	source := createIssue(firstOrganization, firstProject, "source", "Source issue", []string{"feature"})
	target := createIssue(firstOrganization, firstProject, "target", "Target issue", nil)
	foreign := createIssue(secondOrganization, secondProject, "foreign", "Foreign issue", nil)
	if err := repository.ReadTenant(t.Context(), firstOrganization.ID, corestore.Serving, func(r tenantstore.Reader) error {
		labels, err := r.LabelNames(t.Context(), firstProject.ID, corestore.LabelCursor{}, 10)
		if err != nil {
			return err
		}
		if !slices.Equal(labels.Items, []string{"feature"}) {
			t.Fatalf("label membership=%+v", labels)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	firstBase := "/api/v2/organizations/" + string(firstOrganization.ID) + "/projects/" + string(firstProject.ID)
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, firstBase+"/labels", testHubAdminToken, nil), http.StatusServiceUnavailable)
	updatedLabels := []string{"planning"}
	labelUpdate := tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "update-labels"}, ExpectedRevision: 1, Labels: &updatedLabels}
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPatch, itemPathFor(firstOrganization.ID, firstProject.ID, source.WorkItemID), testHubAdminToken, labelUpdate), http.StatusOK)
	if err := repository.ReadTenant(t.Context(), firstOrganization.ID, corestore.Serving, func(r tenantstore.Reader) error {
		labels, err := r.LabelNames(t.Context(), firstProject.ID, corestore.LabelCursor{}, 10)
		if err != nil {
			return err
		}
		if !slices.Equal(labels.Items, []string{"planning"}) {
			t.Fatalf("updated label membership=%+v", labels)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	itemPath := itemPathFor(firstOrganization.ID, firstProject.ID, source.WorkItemID)
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, itemPath, testHubAdminToken, nil), http.StatusOK)
	if err := repository.ReadTenant(t.Context(), firstOrganization.ID, corestore.Serving, func(r tenantstore.Reader) error {
		if _, err := r.WorkItemByNativeID(t.Context(), source.WorkItemID); err != nil {
			return err
		}
		if _, err := r.WorkItemByNativeID(t.Context(), target.WorkItemID); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatalf("read relationship endpoints: %v", err)
	}
	dependency := tracker.DependencyMutation{Mutation: tracker.Mutation{IdempotencyKey: "add-dependency"}, ExpectedRevision: 2, RelatedWorkItemID: target.WorkItemID, Operation: "add"}
	var rejectedItem tracker.NativeWorkItemID
	service.config.TenantRepository = rejectedWorkItemRepository{Repository: repository, err: errors.New("reject relationship event"), phase: "event", item: &rejectedItem}
	rejected := performHubAPIRequest(t, service, http.MethodPost, itemPath+"/dependencies", testHubAdminToken, dependency)
	requireNativeStatus(t, rejected, http.StatusInternalServerError)
	service.config.TenantRepository = repository
	credential, _, err := service.authenticateAPIToken(t.Context(), testHubAdminToken, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if rejectedItem != source.WorkItemID {
		t.Fatalf("rejected event item=%q want=%q", rejectedItem, source.WorkItemID)
	}
	if err := repository.ReadTenant(t.Context(), firstOrganization.ID, corestore.Serving, func(r tenantstore.Reader) error {
		stored, err := r.WorkItem(t.Context(), firstProject.ID, source.WorkItemID)
		if err != nil {
			return err
		}
		if stored.Revision != 2 {
			t.Fatalf("revision after rejected event=%d want=2", stored.Revision)
		}
		relations, err := r.Relationships(t.Context(), firstProject.ID, source.WorkItemID, corestore.RelationshipCursor{}, 10)
		if err != nil {
			return err
		}
		if len(relations.Items) != 0 {
			t.Fatalf("relationships after rejected event=%+v", relations.Items)
		}
		events, err := r.Events(t.Context(), firstProject.ID, source.WorkItemID, 0, 10)
		if err != nil {
			return err
		}
		if len(events) != 2 || events[0].Type != "issue.created" || events[1].Type != "issue.edited" {
			t.Fatalf("events after rejected event=%+v", events)
		}
		if _, err := r.CollaborationVersion(t.Context(), string(source.WorkItemID), 3); !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("version after rejected event=%v", err)
		}
		if _, err := r.Receipt(t.Context(), credential.ID, "POST "+itemPath+"/dependencies", dependency.IdempotencyKey); !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("receipt after rejected event=%v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	response := performHubAPIRequest(t, service, http.MethodPost, itemPath+"/dependencies", testHubAdminToken, dependency)
	requireNativeStatus(t, response, http.StatusOK)
	var linked tracker.NativeIssue
	decodeHubResponse(t, response, &linked)
	if linked.Revision != 3 || !slices.Equal(linked.Dependencies, []tracker.NativeWorkItemID{target.WorkItemID}) {
		t.Fatalf("dependency mutation=%+v", linked)
	}
	var sourceRead tracker.NativeIssue
	decodeHubResponse(t, performHubAPIRequest(t, service, http.MethodGet, itemPath, testHubAdminToken, nil), &sourceRead)
	if !slices.Equal(sourceRead.Dependencies, []tracker.NativeWorkItemID{target.WorkItemID}) || len(sourceRead.Blockers) != 1 || sourceRead.Blockers[0].ID != target.WorkItemID || sourceRead.DependencyReady {
		t.Fatalf("source relationship hydration=%+v", sourceRead)
	}
	targetPath := itemPathFor(firstOrganization.ID, firstProject.ID, target.WorkItemID)
	var targetRead tracker.NativeIssue
	decodeHubResponse(t, performHubAPIRequest(t, service, http.MethodGet, targetPath, testHubAdminToken, nil), &targetRead)
	if len(targetRead.Dependents) != 1 || targetRead.Dependents[0].ID != source.WorkItemID {
		t.Fatalf("target relationship hydration=%+v", targetRead)
	}
	var targetRow corestore.WorkItem
	var targetVersion eventstore.CollaborationVersion
	if err := repository.ReadTenant(t.Context(), firstOrganization.ID, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		targetRow, err = r.WorkItem(t.Context(), firstProject.ID, target.WorkItemID)
		if err != nil {
			return err
		}
		targetVersion, err = r.CollaborationVersion(t.Context(), string(target.WorkItemID), targetRow.Revision)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	content := eventstore.NewContent(service.runDetailStore())
	targetRaw, err := content.CollaborationVersion(t.Context(), targetVersion)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(targetRaw, &targetRead); err != nil {
		t.Fatal(err)
	}
	targetRead.State = "Done"
	targetRead.Revision++
	targetRead.UpdatedAt = service.config.now()
	targetRead.LastActivityAt = targetRead.UpdatedAt
	targetRaw, err = json.Marshal(targetRead)
	if err != nil {
		t.Fatal(err)
	}
	targetVersion.Revision = int64(targetRead.Revision)
	targetVersion, err = content.PrepareCollaborationVersion(t.Context(), targetVersion, targetRaw)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.WriteTenant(t.Context(), firstOrganization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		current, err := w.LockWorkItem(t.Context(), firstProject.ID, target.WorkItemID)
		if err != nil {
			return err
		}
		state := "Done"
		current.WorkflowStateName = &state
		if _, err := w.UpdateWorkItem(t.Context(), current); err != nil {
			return err
		}
		_, err = w.CreateCollaborationVersion(t.Context(), targetVersion)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var refreshedSource tracker.NativeIssue
	decodeHubResponse(t, performHubAPIRequest(t, service, http.MethodGet, itemPath, testHubAdminToken, nil), &refreshedSource)
	if !refreshedSource.DependencyReady || len(refreshedSource.Blockers) != 1 || refreshedSource.Blockers[0].State != "Done" || !refreshedSource.Blockers[0].Terminal {
		t.Fatalf("refreshed relationship facts=%+v", refreshedSource)
	}
	replay := performHubAPIRequest(t, service, http.MethodPost, itemPath+"/dependencies", testHubAdminToken, dependency)
	requireNativeStatus(t, replay, http.StatusOK)
	if response.Body.String() != replay.Body.String() {
		t.Fatal("dependency replay changed response")
	}
	related := tracker.DependencyMutation{Mutation: tracker.Mutation{IdempotencyKey: "add-related"}, ExpectedRevision: 3, RelatedWorkItemID: target.WorkItemID, RelationshipType: "related", Operation: "add"}
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, itemPath+"/dependencies", testHubAdminToken, related), http.StatusOK)
	scope := nativeScope{organization: firstOrganization.ID, project: firstProject.ID, credential: apiCredential{ID: "example-operator", Scope: apiScopeAdmin}}
	firstPage, err := service.readWorkItemRelationshipPage(t.Context(), scope, operatortool.WorkReadRequest{Reference: string(source.WorkItemID), Limit: 1}, linked)
	if err != nil || firstPage.NextCursor == "" || !slices.Equal(firstPage.Dependencies, []tracker.NativeWorkItemID{target.WorkItemID}) {
		t.Fatalf("first relationship page=%+v error=%v", firstPage, err)
	}
	secondPage, err := service.readWorkItemRelationshipPage(t.Context(), scope, operatortool.WorkReadRequest{Reference: string(source.WorkItemID), Limit: 1, Cursor: firstPage.NextCursor}, linked)
	if err != nil || secondPage.NextCursor != "" || len(secondPage.Related) != 1 || secondPage.Related[0].ID != target.WorkItemID {
		t.Fatalf("second relationship page=%+v error=%v", secondPage, err)
	}
	remove := tracker.DependencyMutation{Mutation: tracker.Mutation{IdempotencyKey: "remove-dependency"}, ExpectedRevision: 4, RelatedWorkItemID: target.WorkItemID, Operation: "remove"}
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, itemPath+"/dependencies", testHubAdminToken, remove), http.StatusOK)
	var sourceAfterRemove tracker.NativeIssue
	decodeHubResponse(t, performHubAPIRequest(t, service, http.MethodGet, itemPath, testHubAdminToken, nil), &sourceAfterRemove)
	if len(sourceAfterRemove.Dependencies) != 0 || len(sourceAfterRemove.Blockers) != 0 || !sourceAfterRemove.DependencyReady {
		t.Fatalf("source relationship removal=%+v", sourceAfterRemove)
	}
	reverse := tracker.DependencyMutation{Mutation: tracker.Mutation{IdempotencyKey: "reverse-dependency"}, ExpectedRevision: 2, RelatedWorkItemID: source.WorkItemID, Operation: "add"}
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, targetPath+"/dependencies", testHubAdminToken, reverse), http.StatusOK)
	var reversedTarget tracker.NativeIssue
	decodeHubResponse(t, performHubAPIRequest(t, service, http.MethodGet, targetPath, testHubAdminToken, nil), &reversedTarget)
	if !slices.Equal(reversedTarget.Dependencies, []tracker.NativeWorkItemID{source.WorkItemID}) || len(reversedTarget.Blockers) != 1 || reversedTarget.DependencyReady {
		t.Fatalf("reverse endpoint hydration=%+v", reversedTarget)
	}
	var reversedSource tracker.NativeIssue
	decodeHubResponse(t, performHubAPIRequest(t, service, http.MethodGet, itemPath, testHubAdminToken, nil), &reversedSource)
	if len(reversedSource.Dependents) != 1 || reversedSource.Dependents[0].ID != target.WorkItemID {
		t.Fatalf("reverse endpoint dependent hydration=%+v", reversedSource)
	}
	cycle := tracker.DependencyMutation{Mutation: tracker.Mutation{IdempotencyKey: "dependency-cycle"}, ExpectedRevision: 5, RelatedWorkItemID: target.WorkItemID, Operation: "add"}
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, itemPath+"/dependencies", testHubAdminToken, cycle), http.StatusUnprocessableEntity)
	removeReverse := tracker.DependencyMutation{Mutation: tracker.Mutation{IdempotencyKey: "remove-reverse-dependency"}, ExpectedRevision: 3, RelatedWorkItemID: source.WorkItemID, Operation: "remove"}
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, targetPath+"/dependencies", testHubAdminToken, removeReverse), http.StatusOK)
	crossTenant := tracker.DependencyMutation{Mutation: tracker.Mutation{IdempotencyKey: "foreign-dependency"}, ExpectedRevision: 5, RelatedWorkItemID: foreign.WorkItemID, Operation: "add"}
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, itemPath+"/dependencies", testHubAdminToken, crossTenant), http.StatusNotFound)
	projectsPath := "/api/v2/organizations/" + string(firstOrganization.ID) + "/projects"
	response = performHubAPIRequest(t, service, http.MethodPost, projectsPath, testHubAdminToken, map[string]any{"idempotency_key": "sibling-project", "name": "Sibling project", "states": nativeFixtureStates()})
	requireNativeStatus(t, response, http.StatusOK)
	var siblingProject tracker.NativeProject
	decodeHubResponse(t, response, &siblingProject)
	if err := repository.WriteTenant(t.Context(), firstOrganization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		record, err := w.ProjectRecord(t.Context(), siblingProject.ID)
		if err != nil {
			return err
		}
		record.Settings.DuplicateChallenge = false
		_, err = w.UpdateProjectRecord(t.Context(), record)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	siblingTarget := createIssue(firstOrganization, siblingProject, "sibling-target", "Sibling target", nil)
	var sourceForGrant tracker.NativeIssue
	decodeHubResponse(t, performHubAPIRequest(t, service, http.MethodGet, itemPath, testHubAdminToken, nil), &sourceForGrant)
	siblingDependency := tracker.DependencyMutation{Mutation: tracker.Mutation{IdempotencyKey: "sibling-dependency"}, ExpectedRevision: sourceForGrant.Revision, RelatedWorkItemID: siblingTarget.WorkItemID, Operation: "add"}
	response = performHubAPIRequest(t, service, http.MethodPost, itemPath+"/dependencies", testHubAdminToken, siblingDependency)
	requireNativeStatus(t, response, http.StatusOK)
	var siblingLinked tracker.NativeIssue
	decodeHubResponse(t, response, &siblingLinked)
	siblingRelated := tracker.DependencyMutation{Mutation: tracker.Mutation{IdempotencyKey: "sibling-related"}, ExpectedRevision: siblingLinked.Revision, RelatedWorkItemID: siblingTarget.WorkItemID, RelationshipType: "related", Operation: "add"}
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, itemPath+"/dependencies", testHubAdminToken, siblingRelated), http.StatusOK)
	seedHubAPIToken(t, service, "source-only", "source-only-token", apiScopeOperator)
	for _, project := range []tracker.NativeProject{firstProject, siblingProject} {
		if _, err := service.database.db.ExecContext(t.Context(), `INSERT INTO projects (id, organization_id, name, profile, states_json, created_at) VALUES (?, ?, ?, 'native', '[]', ?) ON CONFLICT(id) DO NOTHING`, project.ID, firstOrganization.ID, "Authorization mirror "+string(project.ID), testTimestamp); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := service.database.db.ExecContext(t.Context(), "INSERT INTO token_grants (token_id, organization_id, project_id) VALUES (?, ?, ?)", "source-only", firstOrganization.ID, firstProject.ID); err != nil {
		t.Fatal(err)
	}
	var sourceOnlyRead tracker.NativeIssue
	response = performHubAPIRequest(t, service, http.MethodGet, itemPath, "source-only-token", nil)
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &sourceOnlyRead)
	if slices.Contains(sourceOnlyRead.Dependencies, siblingTarget.WorkItemID) || slices.ContainsFunc(sourceOnlyRead.Blockers, func(blocker tracker.NativeDependency) bool { return blocker.ID == siblingTarget.WorkItemID }) || slices.ContainsFunc(sourceOnlyRead.Dependents, func(dependent tracker.NativeDependency) bool { return dependent.ID == siblingTarget.WorkItemID }) || slices.ContainsFunc(sourceOnlyRead.Related, func(related tracker.NativeDependency) bool { return related.ID == siblingTarget.WorkItemID }) {
		t.Fatalf("source-only credential exposed ungranted target: %+v", sourceOnlyRead)
	}
	if err := repository.WriteTenant(t.Context(), firstOrganization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		current, err := w.LockWorkItem(t.Context(), firstProject.ID, target.WorkItemID)
		if err != nil {
			return err
		}
		current.ActorPrincipalID = nil
		_, err = w.UpdateWorkItem(t.Context(), current)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	reporterScope := scope
	reporterScope.credential.HostedRole = "reporter"
	reporterScope.sourceActor = &tracker.Actor{Kind: "human", PrincipalID: "example-reporter"}
	reporterRead := sourceOnlyRead
	if err := service.hydrateWorkItemRelationshipFacts(t.Context(), reporterScope, &reporterRead); err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(reporterRead.Related, func(related tracker.NativeDependency) bool { return related.ID == target.WorkItemID }) {
		t.Fatalf("reporter relationship read exposed unowned target: %+v", reporterRead.Related)
	}
	bulkCount := corestore.MaxPageSize + 1
	now := service.config.now()
	if err := repository.WriteTenant(t.Context(), firstOrganization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		for index := range bulkCount {
			id := tracker.NativeWorkItemID(fmt.Sprintf("wi_bulk_%03d", index))
			principal := source.Actor.PrincipalID
			state := "Todo"
			if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{
				OrganizationID:    firstOrganization.ID,
				ProjectID:         firstProject.ID,
				NativeID:          id,
				Number:            int64(index + 10),
				Revision:          1,
				Title:             fmt.Sprintf("Bulk related issue %03d", index),
				WorkflowStateName: &state,
				ActorKind:         new("human"),
				ActorPrincipalID:  &principal,
			}); err != nil {
				return err
			}
			if err := w.SetRelationship(t.Context(), corestore.Relationship{
				OrganizationID:    firstOrganization.ID,
				ProjectID:         firstProject.ID,
				WorkItemID:        source.WorkItemID,
				RelatedProjectID:  firstProject.ID,
				RelatedWorkItemID: id,
				Type:              "related",
			}, "add", now); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var allRelated tracker.NativeIssue
	decodeHubResponse(t, performHubAPIRequest(t, service, http.MethodGet, itemPath, testHubAdminToken, nil), &allRelated)
	if len(allRelated.Related) != bulkCount+2 {
		t.Fatalf("relationship hydration truncated at one storage page: got=%d want=%d", len(allRelated.Related), bulkCount+2)
	}
}

func itemPathFor(organization tracker.OrganizationID, project tracker.ProjectID, item tracker.NativeWorkItemID) string {
	return "/api/v2/organizations/" + string(organization) + "/projects/" + string(project) + "/work-items/" + string(item)
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
	service := openPostgresHubTestService(t, repository, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), RunDetails: &memoryRunDetails{}, TenantRepository: repository})
	organization, err := service.createNativeOrganizationFor(t.Context(), "Example organization")
	if err != nil {
		t.Fatal(err)
	}
	seedPostgresHubAdmin(t, repository, service, organization)
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
	free := plans.Plans[0]
	freePG := billingstore.Plan{OrganizationID: scope.organization, PlanReference: billingstore.PlanReference{ID: free.ID, Version: free.Version}, Name: free.Name, MonthlyUSDCents: free.MonthlyUSDCents, RuntimeID: free.ID, Allowances: free.Allowances, Features: free.Features}
	if err := repository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateBillingPlan(t.Context(), freePG); err != nil {
			return err
		}
		return w.SetBillingAssignment(t.Context(), billingstore.Assignment{OrganizationID: scope.organization, Base: freePG.PlanReference}, 0)
	}); err != nil {
		t.Fatal(err)
	}
	f.service.database.billingRepository = repository
	withoutCollaboration := freePG
	withoutCollaboration.PlanReference = billingstore.PlanReference{ID: "test_free_without_collaboration", Version: 1}
	withoutCollaboration.Features = slices.DeleteFunc(slices.Clone(freePG.Features), func(feature string) bool { return feature == "collaboration" })
	f.service.config.TenantRepository = &downgradeBillingPlanRepository{Repository: repository, plan: withoutCollaboration}
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

type downgradeBillingPlanRepository struct {
	tenantstore.Repository
	plan       billingstore.Plan
	downgraded bool
}

func (r *downgradeBillingPlanRepository) WriteTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, write func(tenantstore.Writer) error) error {
	return r.Repository.WriteTenant(ctx, organization, workload, func(w tenantstore.Writer) error {
		if !r.downgraded {
			assignment, err := w.BillingAssignment(ctx)
			if err != nil {
				return err
			}
			if err := w.CreateBillingPlan(ctx, r.plan); err != nil {
				return err
			}
			if err := w.SetBillingAssignment(ctx, billingstore.Assignment{OrganizationID: organization, Base: r.plan.PlanReference}, assignment.Revision); err != nil {
				return err
			}
			r.downgraded = true
		}
		return write(w)
	})
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
	service := openPostgresHubTestService(t, repository, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), RunDetails: &memoryRunDetails{}, TenantRepository: repository})
	org, err := service.createNativeOrganizationFor(t.Context(), "Example organization")
	if err != nil {
		t.Fatal(err)
	}
	seedPostgresHubAdmin(t, repository, service, org)
	base := "/api/v2/organizations/" + string(org.ID) + "/projects"
	created := performHubAPIRequest(t, service, http.MethodPost, base, testHubAdminToken, map[string]any{"idempotency_key": "project", "name": "Example project", "states": nativeFixtureStates()})
	requireNativeStatus(t, created, http.StatusOK)
	var project tracker.NativeProject
	decodeHubResponse(t, created, &project)
	base += "/" + string(project.ID) + "/work-items"
	credential, _, err := service.authenticateTenantAPIHash(t.Context(), org.ID, apikey.HashToken(testHubAdminToken), "", "")
	if err != nil {
		t.Fatal(err)
	}
	request := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "rollback"}, Title: "Example issue", Body: workItemRecordBody}
	service.config.AccessRepository = nil
	_, err = service.executeWorkItemRecordMutation(t.Context(), nativeScope{organization: org.ID, project: project.ID, credential: credential}, nativeCommandOptions{}, request.Mutation, request, "")
	if err == nil {
		t.Fatal("PostgreSQL mutation accepted missing access authority")
	}
	service.config.AccessRepository = repository
	for _, phase := range []string{"event", "receipt"} {
		t.Run(phase, func(t *testing.T) {
			var failedItem tracker.NativeWorkItemID
			service.config.TenantRepository = rejectedWorkItemRepository{Repository: repository, err: errors.New("reject command"), phase: phase, item: &failedItem}
			rejected := performHubAPIRequest(t, service, http.MethodPost, base, testHubAdminToken, request)
			requireNativeStatus(t, rejected, http.StatusInternalServerError)
			if err := repository.ReadTenant(t.Context(), org.ID, corestore.Serving, func(r tenantstore.Reader) error {
				hits, err := r.Search(t.Context(), searchstore.Query{Kind: "work", ProjectID: project.ID, Text: "Example", Limit: 10})
				if err != nil {
					return err
				}
				for _, hit := range hits {
					if hit.Document.ID != "" {
						t.Errorf("search document survived rollback: %s", hit.Document.ID)
					}
				}
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
	service.config.Hosted = &HostedConfig{OrganizationID: string(org.ID)}
	service.searchRefresh = func(context.Context) (bool, error) {
		t.Fatal("PostgreSQL search maintenance called SQLite")
		return false, nil
	}
	for _, indexed := range []bool{false, true} {
		if indexed {
			service.maintainNativeSearch(t.Context())
		}
		page, err := searchstore.NewContent(service.runDetailStore()).Find(t.Context(), tenantSearchRepository{repository}, org.ID, searchstore.Query{Kind: "work", ProjectID: project.ID, Text: "Example", Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != 1 || page.Items[0].Document.WorkItemID != issue.WorkItemID {
			t.Fatalf("committed search result indexed=%t: %+v", indexed, page.Items)
		}
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
