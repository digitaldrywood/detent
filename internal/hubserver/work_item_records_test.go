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
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/issuecontract"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const workItemRecordBody = "## Acceptance criteria\nThe issue is served.\n## Must not break\nOther tenants.\n## How we know it worked\nRead the issue."

type missingDetailReferenceStore struct {
	attachment.DetailStore
	key   string
	reads *int
}

func (s missingDetailReferenceStore) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	if ref.Key == s.key {
		*s.reads++
		return nil, artifact.ErrMissing
	}
	return s.DetailStore.ReadDetail(ctx, organization, ref)
}

func TestWorkItemRecordBackendParity(t *testing.T) {
	for _, backend := range []string{"postgres"} {
		t.Run(backend, func(t *testing.T) {
			cfg := Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), RunDetails: &memoryRunDetails{}, SecretKeys: secretTestKeys(t, "1", "1")}
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
					seedWorkItemRecordOrganization(t, repository, service, org)
					key := string(org.ID) + ":"
					projectBase := "/api/v2/organizations/" + string(org.ID) + "/projects"
					response := performHubAPIRequest(t, service, http.MethodPost, projectBase, testHubAdminToken, map[string]any{"idempotency_key": key + "project", "name": "Example project", "states": nativeFixtureStates(), "grant_access": true})
					requireNativeStatus(t, response, http.StatusCreated)
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
						challenged := input
						challenged.IdempotencyKey = key + "challenge"
						challenged.ChallengeToken = "example-challenge"
						challenged.NotDuplicateOf = []int{1}
						requireNativeCode(t, performHubAPIRequest(t, service, http.MethodPost, base, testHubAdminToken, challenged), http.StatusGone, "idempotency_expired")
					}
					response = performHubAPIRequest(t, service, http.MethodPost, base, testHubAdminToken, input)
					requireNativeStatus(t, response, http.StatusOK)
					var issue tracker.NativeIssue
					decodeHubResponse(t, response, &issue)
					if issue.Number != 1 || issue.Revision != 1 || issue.Body != input.Body || !slices.Equal(issue.Labels, input.Labels) || !slices.Equal(issue.Assignees, input.Assignees) {
						t.Fatalf("created=%+v", issue)
					}
					for _, field := range []string{"dependencies", "blockers", "dependents", "related", "dependency_ready", "blocked_reason", "unblocks_reason"} {
						if !slices.Contains(issue.OmittedFields, field) {
							t.Fatalf("create response did not omit %s", field)
						}
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
					restored := performHubAPIRequest(t, service, http.MethodPost, itemPath+"/restore", testHubAdminToken, archiveIssueRequest{Mutation: tracker.Mutation{IdempotencyKey: key + "restore"}, ExpectedRevision: 3})
					requireNativeStatus(t, restored, http.StatusOK)
					for _, response := range []*httptest.ResponseRecorder{response, edited, archived, restored} {
						var issue tracker.NativeIssue
						decodeHubResponse(t, response, &issue)
						for _, field := range []string{"dependencies", "blockers", "dependents", "related", "dependency_ready", "blocked_reason", "unblocks_reason"} {
							if !slices.Contains(issue.OmittedFields, field) {
								t.Fatalf("mutation response did not omit %s: %s", field, response.Body.String())
							}
						}
					}
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
						machine := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: key + "machine"}, Title: "Machine report", Body: issueorigin.Stamp(workItemRecordBody, issueorigin.Origin{Kind: "worker", Fingerprint: "example-fingerprint"})}
						machineReply := performHubAPIRequest(t, service, http.MethodPost, base, testHubAdminToken, machine)
						requireNativeStatus(t, machineReply, http.StatusOK)
						var machineIssue tracker.NativeIssue
						decodeHubResponse(t, machineReply, &machineIssue)
						if machineIssue.OrganizationID != org.ID || machineIssue.ProjectID != project.ID || machineIssue.WorkItemID == "" || machineIssue.WorkItemID == issue.WorkItemID || machineIssue.Number != 7 || machineIssue.Revision != 1 || machineIssue.PublicationReused || machineIssue.Title != machine.Title || machineIssue.Body != machine.Body {
							t.Fatalf("machine report did not preserve a new scoped issue: %+v", machineIssue)
						}
						if machineIssue.Actor.Kind != "human" || machineIssue.Actor.PrincipalID != "test_hub_admin" || machineIssue.IssueContract == nil || len(machineIssue.IssueContract.ConfirmedSections) != 0 {
							t.Fatal("machine origin changed the authenticated actor or fabricated human confirmation")
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
	service := openLocalPostgresHubTestService(t, repository, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), RunDetails: &memoryRunDetails{}, SecretKeys: secretTestKeys(t, "1", "1")})
	if err := service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	createProject := func(name, key string) (nativeOrganization, tracker.NativeProject) {
		t.Helper()
		organization := nativeOrganization{ID: tracker.OrganizationID("org_" + key), Name: name, Slug: key}
		seedWorkItemRecordOrganization(t, repository, service, organization)
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
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, firstBase+"/labels", testHubAdminToken, nil), http.StatusOK)
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
	credential, _, err := service.authenticateTenantAPIHash(t.Context(), firstOrganization.ID, apikey.HashToken(testHubAdminToken), "", "")
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
	if linked.Revision != 3 {
		t.Fatalf("dependency mutation=%+v", linked)
	}
	t.Run("mutation response projection", func(t *testing.T) {
		for _, field := range []string{"dependencies", "blockers", "dependents", "related", "dependency_ready", "blocked_reason", "unblocks_reason"} {
			if !slices.Contains(linked.OmittedFields, field) {
				t.Fatalf("dependency mutation response does not declare %s omitted: %+v", field, linked.OmittedFields)
			}
		}
		if linked.Dependencies == nil || len(linked.Dependencies) != 0 || linked.Blockers == nil || len(linked.Blockers) != 0 || linked.Dependents == nil || len(linked.Dependents) != 0 || linked.DependencyReady {
			t.Fatalf("dependency mutation response carries relationship facts it declares omitted: %+v", linked)
		}
	})
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
	targetSearch, err := searchstore.NewContent(service.runDetailStore()).Prepare(t.Context(), searchstore.Document{OrganizationID: firstOrganization.ID, ProjectID: firstProject.ID, WorkItemID: target.WorkItemID, ID: "issue:" + string(target.WorkItemID), SourceID: string(target.WorkItemID), Kind: "issue", Scope: "issue", Revision: int64(targetRead.Revision), UpdatedAt: targetRead.UpdatedAt}, searchstore.Text{Title: targetRead.Title, Body: targetRead.Body, Labels: targetRead.Labels})
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.WriteTenant(t.Context(), firstOrganization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.PutSearch(t.Context(), targetSearch, targetRow.Revision); err != nil {
			return err
		}
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
	if refreshedSource.DependencyReady || !slices.Contains(refreshedSource.OmittedFields, "dependency_ready") {
		t.Fatalf("basic item read did not omit readiness: %+v", refreshedSource)
	}
	refreshedSource, err = service.readWorkItemSourcePage(t.Context(), nativeScope{organization: firstOrganization.ID, project: firstProject.ID, credential: credential}, string(source.WorkItemID))
	if err != nil {
		t.Fatal(err)
	}
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
	scope := nativeScope{organization: firstOrganization.ID, project: firstProject.ID, credential: credential}
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
	if sourceAfterRemove.DependencyReady || !slices.Contains(sourceAfterRemove.OmittedFields, "dependency_ready") {
		t.Fatalf("basic item read did not omit readiness: %+v", sourceAfterRemove)
	}
	sourceAfterRemove, err = service.readWorkItemSourcePage(t.Context(), scope, string(source.WorkItemID))
	if err != nil {
		t.Fatal(err)
	}
	if len(sourceAfterRemove.Dependencies) != 0 || len(sourceAfterRemove.Blockers) != 0 || !sourceAfterRemove.DependencyReady {
		t.Fatalf("source relationship removal=%+v", sourceAfterRemove)
	}
	replayAfterRemove := performHubAPIRequest(t, service, http.MethodPost, itemPath+"/dependencies", testHubAdminToken, dependency)
	requireNativeStatus(t, replayAfterRemove, http.StatusOK)
	if response.Body.String() != replayAfterRemove.Body.String() {
		t.Fatal("dependency replay after a later removal changed the original response")
	}
	afterReplay, err := service.readWorkItemSourcePage(t.Context(), scope, string(source.WorkItemID))
	if err != nil {
		t.Fatal(err)
	}
	if afterReplay.Revision != sourceAfterRemove.Revision || len(afterReplay.Dependencies) != 0 || len(afterReplay.Blockers) != 0 {
		t.Fatalf("dependency replay wrote a new mutation: before=%d after=%+v", sourceAfterRemove.Revision, afterReplay)
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
	if err := repository.WriteTenant(t.Context(), firstOrganization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		now := service.config.now()
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: firstOrganization.ID, ID: "source-only", Name: "Source-only reader", Hash: apikey.HashToken("source-only-token"), Scope: string(apiScopeOperator), NativeOnly: true, KeyScope: "read", ProjectAccess: "selected", Kind: "service", CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		return w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: firstOrganization.ID, TokenID: "source-only", ProjectID: firstProject.ID})
	}); err != nil {
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
	for _, organization := range []nativeOrganization{firstOrganization, secondOrganization} {
		t.Run(string(organization.ID)+"/legacy_receipt_privacy", func(t *testing.T) {
			f := newNativeFixture(t, service, organization.ID, "Legacy response source")
			other := newNativeFixture(t, service, organization.ID, "Legacy response related")
			item := f.create(t, "Legacy response item")
			visible := f.create(t, "Visible dependency")
			hidden := other.create(t, "Private dependency")
			path := f.base + "/work-items/" + string(item.WorkItemID)
			for _, dependency := range []tracker.NativeIssue{visible, hidden} {
				request := tracker.DependencyMutation{Mutation: tracker.Mutation{IdempotencyKey: "link-" + string(dependency.WorkItemID)}, ExpectedRevision: item.Revision, RelatedWorkItemID: dependency.WorkItemID, Operation: "add"}
				response := performHubAPIRequest(t, service, http.MethodPost, path+"/dependencies", testHubAdminToken, request)
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &item)
			}
			credential, _, err := service.authenticateTenantAPIHash(t.Context(), organization.ID, apikey.HashToken(f.token), "", "")
			if err != nil {
				t.Fatal(err)
			}
			legacyScope := nativeScope{organization: organization.ID, project: f.project.ID, credential: credential}
			if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
				return w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization.ID, TokenID: credential.ID, ProjectID: other.project.ID})
			}); err != nil {
				t.Fatal(err)
			}
			var granted tracker.NativeIssue
			response := performHubAPIRequest(t, service, http.MethodGet, path, f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &granted)
			if !slices.Contains(granted.Dependencies, visible.WorkItemID) || !slices.Contains(granted.Dependencies, hidden.WorkItemID) {
				t.Fatalf("initial authorized dependency read=%+v", granted.Dependencies)
			}
			content := eventstore.NewContent(service.runDetailStore())
			grantRevoked := false
			var finalMethod, finalPath string
			var finalInput any
			for index, test := range []struct {
				name, projection string
				create           bool
				status           int
			}{
				{"lost create with absent facts", "absent", true, http.StatusOK},
				{"fact empty legacy edit", "empty", false, http.StatusOK},
				{"legacy private identifiers", "original", false, http.StatusGone},
				{"six field omission retains private identifiers", "six", false, http.StatusGone},
				{"duplicate field conceals earlier private identifiers", "duplicate", false, http.StatusGone},
				{"case variant field conceals earlier private identifiers", "case duplicate", false, http.StatusGone},
				{"duplicate reason conceals earlier private text", "duplicate reason", false, http.StatusGone},
				{"unknown private field", "unknown", false, http.StatusGone},
				{"malformed relationship field", "malformed", false, http.StatusGone},
				{"missing issue identity", "missing identity", false, http.StatusGone},
			} {
				t.Run(test.name, func(t *testing.T) {
					key := "legacy-" + string(organization.ID) + "-" + strconv.Itoa(index)
					method, resource, selected := http.MethodPatch, path, string(item.WorkItemID)
					var command tracker.Mutation
					var input any
					if test.create {
						request := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: key}, Title: "Legacy created item", Body: workItemRecordBody, State: "Todo"}
						input, command = request, request.Mutation
						method, resource, selected = http.MethodPost, f.base+"/work-items", ""
					} else {
						row, _, err := service.readWorkItemRecord(t.Context(), legacyScope, selected)
						if err != nil {
							t.Fatal(err)
						}
						request := tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: key}, ExpectedRevision: tracker.Revision(row.Revision), Title: new("Committed legacy edit " + strconv.Itoa(index))}
						input, command = request, request.Mutation
					}
					plan, err := service.loadWorkItemPlan(t.Context(), legacyScope, selected, "", input)
					if err != nil {
						t.Fatal(err)
					}
					plan, err = service.planWorkItemRecord(t.Context(), legacyScope, plan, input)
					if err != nil {
						t.Fatal(err)
					}
					canonical, err := json.Marshal(plan.issue)
					if err != nil {
						t.Fatal(err)
					}
					var body map[string]json.RawMessage
					if err := json.Unmarshal(canonical, &body); err != nil {
						t.Fatal(err)
					}
					if test.projection != "original" {
						for _, field := range []string{"dependencies", "blockers", "dependents", "related", "dependency_ready", "blocked_reason", "unblocks_reason", "omitted_fields"} {
							delete(body, field)
						}
					}
					if test.projection != "original" && test.projection != "absent" {
						body["dependencies"], body["blockers"], body["dependents"] = json.RawMessage(`[]`), json.RawMessage(`[]`), json.RawMessage(`[]`)
						body["dependency_ready"], body["blocked_reason"], body["unblocks_reason"] = json.RawMessage(`false`), json.RawMessage(`{}`), json.RawMessage(`{}`)
					}
					switch test.projection {
					case "six", "duplicate", "case duplicate":
						body["dependencies"], err = json.Marshal(plan.issue.Dependencies)
						if err != nil {
							t.Fatal(err)
						}
						if test.projection == "six" {
							body["omitted_fields"] = json.RawMessage(`["blockers","dependents","related","dependency_ready","blocked_reason","unblocks_reason"]`)
						}
					case "duplicate reason":
						private, err := json.Marshal(string(hidden.WorkItemID))
						if err != nil {
							t.Fatal(err)
						}
						body["blocked_reason"] = json.RawMessage(`{"text":` + string(private) + `,"text":""}`)
					case "unknown":
						body["unknown_relationship"], err = json.Marshal(hidden.WorkItemID)
						if err != nil {
							t.Fatal(err)
						}
					case "malformed":
						body["dependencies"] = json.RawMessage(`{}`)
					case "missing identity":
						delete(body, "work_item_id")
					}
					legacy, err := json.Marshal(body)
					if err != nil {
						t.Fatal(err)
					}
					if test.projection == "duplicate" {
						legacy = append(legacy[:len(legacy)-1], []byte(`,"dependencies":[]}`)...)
					}
					if test.projection == "case duplicate" {
						legacy = append(legacy[:len(legacy)-1], []byte(`,"DEPENDENCIES":[]}`)...)
					}
					version, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: organization.ID, ProjectID: f.project.ID, WorkItemID: plan.issue.WorkItemID, RecordID: string(plan.issue.WorkItemID), Revision: int64(plan.issue.Revision)}, canonical)
					if err != nil {
						t.Fatal(err)
					}
					eventRaw, err := json.Marshal(tracker.CollaborationEvent{ID: newNativeID("evt"), OrganizationID: organization.ID, ProjectID: f.project.ID, AggregateType: "issue", AggregateID: plan.issue.WorkItemID, Type: plan.eventType, SchemaVersion: 1, Actor: legacyScope.actor(), RecordedAt: plan.issue.UpdatedAt, Data: tracker.CollaborationData{Revision: plan.issue.Revision, Fields: plan.fields}})
					if err != nil {
						t.Fatal(err)
					}
					event, err := content.PrepareEvent(t.Context(), organization.ID, eventRaw)
					if err != nil {
						t.Fatal(err)
					}
					fields, err := service.prepareWorkItemFields(t.Context(), organization.ID, plan)
					if err != nil {
						t.Fatal(err)
					}
					searchDocument, err := searchstore.NewContent(service.runDetailStore()).Prepare(t.Context(), searchstore.Document{OrganizationID: organization.ID, ProjectID: plan.issue.ProjectID, WorkItemID: plan.issue.WorkItemID, ID: "issue:" + string(plan.issue.WorkItemID), SourceID: string(plan.issue.WorkItemID), Kind: "issue", Scope: "issue", Revision: int64(plan.issue.Revision), UpdatedAt: plan.issue.UpdatedAt}, searchstore.Text{Title: plan.issue.Title, Body: plan.issue.Body, Labels: plan.issue.Labels})
					if err != nil {
						t.Fatal(err)
					}
					hash, err := nativeCommandHash(input)
					if err != nil {
						t.Fatal(err)
					}
					receipt, err := content.PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: organization.ID, ActorID: credential.ID, Operation: method + " " + resource, CommandKey: command.IdempotencyKey, RequestHash: hash, CreatedAt: plan.issue.UpdatedAt, ResponseReplayUntilNs: plan.issue.UpdatedAt.Add(receiptstore.ReplayRetention).UnixNano()}, legacy)
					if err != nil {
						t.Fatal(err)
					}
					if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
						if err := writeWorkItemRecord(t.Context(), w, plan, fields); err != nil {
							return err
						}
						if err := w.PutSearch(t.Context(), searchDocument, plan.current.Revision); err != nil {
							return err
						}
						if _, err := w.CreateCollaborationVersion(t.Context(), version); err != nil {
							return err
						}
						if _, err := w.AppendEvent(t.Context(), event); err != nil {
							return err
						}
						var err error
						receipt, err = w.PutReceipt(t.Context(), receipt)
						return err
					}); err != nil {
						t.Fatal(err)
					}
					if !grantRevoked && test.status == http.StatusGone {
						if !bytes.Contains(legacy, []byte(hidden.WorkItemID)) {
							t.Fatal("legacy response did not retain the formerly authorized dependency")
						}
						if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
							return w.DeleteAccessTokenGrant(t.Context(), credential.ID, other.project.ID)
						}); err != nil {
							t.Fatal(err)
						}
						grantRevoked = true
					}
					var before corestore.WorkItem
					if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
						var err error
						before, err = r.WorkItem(t.Context(), f.project.ID, plan.issue.WorkItemID)
						return err
					}); err != nil {
						t.Fatal(err)
					}
					for range 2 {
						replay := performHubAPIRequest(t, service, method, resource, f.token, input)
						requireNativeStatus(t, replay, test.status)
						if test.status == http.StatusOK {
							if replay.Body.String() != string(legacy) {
								t.Fatal("safe legacy response was not replayed byte for byte")
							}
						} else if !strings.Contains(replay.Body.String(), "idempotency_expired") || !strings.Contains(replay.Body.String(), "completed") || !strings.Contains(replay.Body.String(), "will not be repeated") || strings.Contains(replay.Body.String(), string(hidden.WorkItemID)) {
							t.Fatalf("unsafe legacy response did not preserve a private, committed refusal: %s", replay.Body.String())
						}
					}
					if test.status == http.StatusGone {
						_, readErr := service.readWorkItemRecordReceipt(t.Context(), legacyScope, receipt, input, selected)
						if !errors.Is(readErr, receiptstore.ErrExpired) {
							t.Fatalf("unsafe legacy reader error=%v", readErr)
						}
						var refusal *operatortool.RequestError
						mapped := safeNativeWorkError(fmt.Errorf("execute work command: %w", readErr))
						if !errors.As(mapped, &refusal) || refusal.Code != "idempotency_expired" || !strings.Contains(refusal.Message, "completed") || !strings.Contains(refusal.Message, "will not be repeated") {
							t.Fatalf("generic operator legacy refusal lost committed outcome: %v", mapped)
						}
						if !errors.Is(workflowSafeError(readErr), receiptstore.ErrExpired) {
							t.Fatal("workflow legacy refusal lost its existing sentinel")
						}
					}
					var changed any
					switch request := input.(type) {
					case tracker.CreateIssue:
						request.Title = "Different input"
						changed = request
					case tracker.UpdateIssue:
						request.Title = new("Different input")
						changed = request
					}
					requireNativeStatus(t, performHubAPIRequest(t, service, method, resource, f.token, changed), http.StatusConflict)
					if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
						after, err := r.WorkItem(t.Context(), f.project.ID, plan.issue.WorkItemID)
						if err != nil {
							return err
						}
						stored, err := r.Receipt(t.Context(), credential.ID, receipt.Operation, command.IdempotencyKey)
						if err != nil {
							return err
						}
						if after.Revision != before.Revision || after.EventSequence != before.EventSequence || after.Title != plan.issue.Title || !reflect.DeepEqual(stored, receipt) {
							t.Fatalf("legacy replay changed its durable result: before=%+v after=%+v receipt=%+v", before, after, stored)
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					original, err := content.Receipt(t.Context(), receipt)
					if err != nil || !bytes.Equal(original, legacy) {
						t.Fatalf("legacy receipt bytes changed: %v", err)
					}
					finalMethod, finalPath, finalInput = method, resource, input
				})
			}
			if t.Failed() {
				return
			}
			var authorized tracker.NativeIssue
			response = performHubAPIRequest(t, service, http.MethodGet, path, f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &authorized)
			if !slices.Contains(authorized.Dependencies, visible.WorkItemID) || slices.Contains(authorized.Dependencies, hidden.WorkItemID) {
				t.Fatalf("authoritative read lost visible dependency or exposed revoked dependency: %+v", authorized.Dependencies)
			}
			update := tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "safe-new-" + string(organization.ID)}, ExpectedRevision: authorized.Revision, Title: new("Safe current response")}
			response = performHubAPIRequest(t, service, http.MethodPatch, path, f.token, update)
			requireNativeStatus(t, response, http.StatusOK)
			var updated tracker.NativeIssue
			decodeHubResponse(t, response, &updated)
			if strings.Contains(response.Body.String(), string(hidden.WorkItemID)) || strings.Contains(response.Body.String(), hidden.Title) || len(updated.Dependencies) != 0 {
				t.Fatalf("current mutation exposed private relationship data: %s", response.Body.String())
			}
			for _, field := range []string{"dependencies", "blockers", "dependents", "related", "dependency_ready", "blocked_reason", "unblocks_reason"} {
				if !slices.Contains(updated.OmittedFields, field) {
					t.Fatalf("current mutation did not declare %s omitted", field)
				}
			}
			replay := performHubAPIRequest(t, service, http.MethodPatch, path, f.token, update)
			requireNativeStatus(t, replay, http.StatusOK)
			if response.Body.String() != replay.Body.String() {
				t.Fatal("safe current mutation replay changed bytes")
			}
			transition := tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "safe-workflow-" + string(organization.ID)}, ExpectedRevision: updated.Revision, State: "In Progress", Reason: "user_requested"}
			workflow := performHubAPIRequest(t, service, http.MethodPost, path+"/workflow", f.token, transition)
			requireNativeStatus(t, workflow, http.StatusOK)
			var transitioned tracker.NativeIssue
			decodeHubResponse(t, workflow, &transitioned)
			for _, field := range []string{"dependencies", "blockers", "dependents", "related", "dependency_ready", "blocked_reason", "unblocks_reason"} {
				if !slices.Contains(transitioned.OmittedFields, field) {
					t.Fatalf("workflow response did not declare %s omitted", field)
				}
			}
			workflowReplay := performHubAPIRequest(t, service, http.MethodPost, path+"/workflow", f.token, transition)
			requireNativeStatus(t, workflowReplay, http.StatusOK)
			if workflow.Body.String() != workflowReplay.Body.String() || strings.Contains(workflow.Body.String(), string(hidden.WorkItemID)) {
				t.Fatal("workflow response lost safe byte replay")
			}
			preflight, found, err := service.workflowCommandReplay(t.Context(), legacyScope, string(item.WorkItemID), transition)
			if err != nil || !found || string(preflight) != workflow.Body.String() {
				t.Fatalf("human workflow replay bypassed the stored safe response: found=%v error=%v", found, err)
			}
			if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
				return w.DeleteAccessTokenGrant(t.Context(), credential.ID, f.project.ID)
			}); err != nil {
				t.Fatal(err)
			}
			requireNativeStatus(t, performHubAPIRequest(t, service, finalMethod, finalPath, f.token, finalInput), http.StatusForbidden)
		})
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
	if workload != corestore.Serving {
		return r.Repository.WriteTenant(ctx, organization, workload, write)
	}
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
	now := time.Now().UTC().Truncate(time.Microsecond)
	service := openLocalPostgresHubTestService(t, repository, Config{now: func() time.Time { return now }, DatabasePath: filepath.Join(t.TempDir(), "hub.db"), RunDetails: &memoryRunDetails{}, SecretKeys: secretTestKeys(t, "1", "1"), TenantRepository: repository})
	for _, id := range []tracker.OrganizationID{"org_first", "org_second"} {
		t.Run(string(id), func(t *testing.T) {
			service.config.Hosted = nil
			service.config.TenantRepository = repository
			organization := nativeOrganization{ID: id, Name: string(id), Slug: string(id)}
			seedWorkItemRecordOrganization(t, repository, service, organization)
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
		})
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
	scope := nativeScope{organization: tracker.OrganizationID("org_security"), project: tracker.ProjectID("prj_security")}
	fixtureNow := time.Now().UTC()
	if err := repository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: scope.organization, Name: "Example organization", CreatedAt: fixtureNow}); err != nil {
			return err
		}
		return w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: scope.organization, ProviderID: "org_provider"})
	}); err != nil {
		t.Fatal(err)
	}
	f := newHostedSecurityFixture(t, func(cfg *Config) {
		cfg.AccessRepository = repository
		cfg.TenantRepository = repository
		cfg.RunDetails = &memoryRunDetails{}
	})
	states := append(nativeFixtureStates(), tracker.NativeState{Name: "Triage", Transitions: []string{"Todo"}})
	project := projectstore.Record{Core: corestore.Project{OrganizationID: scope.organization, ID: scope.project, Name: "Example project", Slug: "example-project", Profile: "native", RequireDependencies: true}, States: states}
	if err := repository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		created, err := w.ProjectRecord(t.Context(), scope.project)
		if err != nil {
			return err
		}
		created.Core.Name, created.Core.Slug, created.Core.RequireDependencies = project.Core.Name, project.Core.Slug, project.Core.RequireDependencies
		created.States = project.States
		_, err = w.UpdateProjectRecord(t.Context(), created)
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
	identical := f.request(t, owner, http.MethodPost, f.base+"/work-items", request)
	requireNativeStatus(t, identical, http.StatusOK)
	if created.Body.String() != identical.Body.String() {
		t.Fatal("reporter issue replay changed its safe stored response")
	}
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
	requireNativeCode(t, f.request(t, owner, http.MethodPost, f.base+"/work-items", challenged), http.StatusUnprocessableEntity, "invalid_request")
	foreign := request
	foreign.IdempotencyKey = "other-reporter-file"
	response := f.request(t, other, http.MethodPost, f.base+"/work-items", foreign)
	requireNativeStatus(t, response, http.StatusNotFound)
}

func TestWorkItemRecordBodyEditClearsReturnedIssueAction(t *testing.T) {
	repository := projectRecordPostgres(t)
	service := openLocalPostgresHubTestService(t, repository, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), RunDetails: &memoryRunDetails{}, SecretKeys: secretTestKeys(t, "1", "1")})
	organization := nativeOrganization{ID: "org_example", Name: "Example organization", Slug: "example-organization"}
	seedWorkItemRecordOrganization(t, repository, service, organization)
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
	searchDocument, err := searchstore.NewContent(service.runDetailStore()).Prepare(t.Context(), searchstore.Document{OrganizationID: organization.ID, ProjectID: issue.ProjectID, WorkItemID: issue.WorkItemID, ID: "issue:" + string(issue.WorkItemID), SourceID: string(issue.WorkItemID), Kind: "issue", Scope: "issue", Revision: int64(issue.Revision), UpdatedAt: issue.UpdatedAt}, searchstore.Text{Title: issue.Title, Body: issue.Body, Labels: issue.Labels})
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.PutSearch(t.Context(), searchDocument, int64(issue.Revision)-1); err != nil {
			return err
		}
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
	if err := errors.Join(service.stopGitHubWebhookMaintenance(), service.stopGitHubReconciliation()); err != nil {
		t.Fatal(err)
	}
	service.stopHostedBilling()
	service.stopIssueMailer()
	if service.outbox != nil {
		service.outbox.stop()
	}
	if service.dashboard != nil {
		service.dashboard.stop()
	}
	legacyReader := service.database.reader
	service.database.reader = nil
	t.Cleanup(func() { service.database.reader = legacyReader })
	fixed := nativeContractTestBody
	response = performHubAPIRequest(t, service, http.MethodPatch, path+"/"+string(issue.WorkItemID), testHubAdminToken, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "complete-contract"}, ExpectedRevision: issue.Revision, Body: &fixed})
	requireNativeStatus(t, response, http.StatusOK)
	var edited tracker.NativeIssue
	decodeHubResponse(t, response, &edited)
	if edited.State != "Blocked" || edited.IssueContract == nil || edited.IssueContract.ReturnState != "Todo" || edited.IssueContract.HumanAction != "" {
		t.Fatalf("body edit did not clear recovery action while preserving return state: %+v", edited.IssueContract)
	}
	row, storedVersion, err := service.readWorkItemRecord(t.Context(), nativeScope{organization: organization.ID, project: issue.ProjectID}, string(issue.WorkItemID))
	if err != nil {
		t.Fatal(err)
	}
	stored, err := service.hydrateWorkItemRecord(t.Context(), row, storedVersion)
	if err != nil {
		t.Fatal(err)
	}
	if stored.IssueContract == nil || stored.IssueContract.ReturnState != "Todo" || stored.IssueContract.HumanAction != "" {
		t.Errorf("stored recovery contract = %+v", stored.IssueContract)
	}
}

func TestWorkItemRecordCommentEditConflictAfterConcurrentWrite(t *testing.T) {
	repository := projectRecordPostgres(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	service := openLocalPostgresHubTestService(t, repository, Config{now: func() time.Time { return now }, DatabasePath: filepath.Join(t.TempDir(), "hub.db"), RunDetails: &memoryRunDetails{}, SecretKeys: secretTestKeys(t, "1", "1"), TenantRepository: repository})
	for _, id := range []tracker.OrganizationID{"org_first", "org_second"} {
		t.Run(string(id), func(t *testing.T) {
			organization := nativeOrganization{ID: id, Name: string(id), Slug: string(id)}
			seedWorkItemRecordOrganization(t, repository, service, organization)
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
			originalDetails := service.config.RunDetails
			var itemVersion eventstore.CollaborationVersion
			if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
				row, err := r.WorkItem(t.Context(), project.ID, issue.WorkItemID)
				if err != nil {
					return err
				}
				itemVersion, err = r.CollaborationVersion(t.Context(), string(row.NativeID), row.Revision)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			missingParentReads := 0
			service.config.RunDetails = missingDetailReferenceStore{DetailStore: originalDetails, key: itemVersion.PayloadObjectKey, reads: &missingParentReads}
			commentScope := nativeScope{organization: organization.ID, project: project.ID}
			if _, err := service.readWorkItem(t.Context(), commentScope, string(issue.WorkItemID)); !errors.Is(err, artifact.ErrMissing) {
				t.Fatalf("parent read error = %v, want missing archive", err)
			}
			if missingParentReads != 1 {
				t.Fatalf("parent archive reads = %d, want one", missingParentReads)
			}
			missingParentReads = 0
			if _, err := service.readCommentRecords(t.Context(), commentScope, string(issue.WorkItemID), url.Values{}); err != nil {
				t.Fatalf("list comments with missing parent archive: %v", err)
			}
			if missingParentReads != 0 {
				t.Fatalf("comment list read parent archive %d times, want zero", missingParentReads)
			}
			unauthorizedScope := commentScope
			unauthorizedScope.credential.HostedRole = "reporter"
			unauthorizedScope.sourceActor = &tracker.Actor{Kind: "human", PrincipalID: "example-other-reporter"}
			_, unauthorizedErr := service.readCommentRecords(t.Context(), unauthorizedScope, string(issue.WorkItemID), url.Values{})
			var unauthorizedResponse *nativeError
			if !errors.As(unauthorizedErr, &unauthorizedResponse) || unauthorizedResponse.status != http.StatusNotFound {
				t.Fatalf("unauthorized comment list error = %v, want not found", unauthorizedErr)
			}
			listed := performHubAPIRequest(t, service, http.MethodGet, itemPath+"/comments", testHubAdminToken, nil)
			service.config.RunDetails = originalDetails
			if missingParentReads != 0 {
				t.Fatalf("comments GET read parent archive %d times, want zero", missingParentReads)
			}
			requireNativeStatus(t, listed, http.StatusOK)
			var commentPage tracker.Page[tracker.NativeComment]
			decodeHubResponse(t, listed, &commentPage)
			if len(commentPage.Items) != 1 || commentPage.Items[0].ID != comment.ID || commentPage.Items[0].Body != comment.Body {
				t.Fatalf("listed comments = %+v, want created comment %+v", commentPage.Items, comment)
			}
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
		})
	}
}

func TestWorkItemRecordMutationRequiresHostedFeature(t *testing.T) {
	repository, f, owner, _ := newChangePolicyPostgresFixtureWithAdmin(t, 8)
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	scope := nativeScope{organization: tracker.OrganizationID("org_security"), project: f.project}

	free := normalizeHostedPlan(pilotHostedPlans().Plans[0])
	freePG := billingstore.Plan{OrganizationID: scope.organization, PlanReference: billingstore.PlanReference{ID: free.ID, Version: free.Version}, Name: free.Name, MonthlyUSDCents: free.MonthlyUSDCents, RuntimeID: free.ID, Allowances: free.Allowances, Features: free.Features}
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
	corestore.DuplicateChallengeWriter
	searchstore.DuplicateSourceWriter
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
		if err := write(rejectedWorkItemWriter{Writer: w, DuplicateChallengeWriter: w.(corestore.DuplicateChallengeWriter), DuplicateSourceWriter: w.(searchstore.DuplicateSourceWriter), reject: r}); err != nil {
			return err
		}
		return r.err
	})
}

func TestWorkItemRecordRollback(t *testing.T) {
	repository := projectRecordPostgres(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	service := openLocalPostgresHubTestService(t, repository, Config{now: func() time.Time { return now }, DatabasePath: filepath.Join(t.TempDir(), "hub.db"), RunDetails: &memoryRunDetails{}, SecretKeys: secretTestKeys(t, "1", "1"), TenantRepository: repository})
	for _, id := range []tracker.OrganizationID{"org_first", "org_second"} {
		t.Run(string(id), func(t *testing.T) {
			service.config.Hosted = nil
			service.config.TenantRepository = repository
			org := nativeOrganization{ID: id, Name: string(id), Slug: string(id)}
			seedWorkItemRecordOrganization(t, repository, service, org)
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
		})
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

func seedWorkItemRecordOrganization(t *testing.T, repository tenantstore.Repository, service *Service, organization nativeOrganization) {
	t.Helper()
	seedPostgresHubAdmin(t, repository, service, organization)
	envelope, err := service.config.SecretKeys.Seal(bytes.Repeat([]byte{0x35}, 32), accessstore.SecretAAD(organization.ID, "", accessstore.NativeCursorKeyKind))
	if err != nil {
		t.Fatal(err)
	}
	now := service.config.now()
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		return w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: organization.ID, Kind: accessstore.NativeCursorKeyKind, Envelope: envelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: organization.ID, Kind: accessstore.NativeCursorKeyKind, Actor: "migration", Event: "import", KeyVersion: envelope.Version, RecordedAt: now})
	}); err != nil {
		t.Fatal(err)
	}
}
