package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"

	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type commentPageReverseRepository struct {
	tenantstore.Repository
}

func (r *commentPageReverseRepository) ReadTenant(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	return r.Repository.ReadTenant(ctx, org, workload, func(reader tenantstore.Reader) error {
		return read(commentPageReverseReader{Reader: reader})
	})
}

type commentPageReverseReader struct {
	tenantstore.Reader
}

func (r commentPageReverseReader) CollaborationVersions(ctx context.Context, keys []eventstore.CollaborationVersionKey) ([]eventstore.CollaborationVersion, error) {
	versions, err := r.Reader.(eventstore.CollaborationVersionsReader).CollaborationVersions(ctx, keys)
	for left, right := 0, len(versions)-1; left < right; left, right = left+1, right-1 {
		versions[left], versions[right] = versions[right], versions[left]
	}
	return versions, err
}

func TestOversizedCommentReportsBulkFallback(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "comment-fallback")
	for _, title := range []string{"First comment owner", "Second comment owner"} {
		t.Run(title, func(t *testing.T) {
			item := f.create(t, title)
			path := f.base + "/work-items/" + string(item.WorkItemID) + "/comments"
			created := performHubAPIRequest(t, f.service, http.MethodPost, path, f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: string(item.WorkItemID)}, Body: strings.Repeat("\x00", 64<<10)})
			requireNativeStatus(t, created, http.StatusOK)
			response := performHubAPIRequest(t, f.service, http.MethodGet, path, f.token, nil)
			requireNativeStatus(t, response, http.StatusUnprocessableEntity)
			if body := response.Body.String(); !strings.Contains(body, "single comment") || !strings.Contains(body, operatortool.WorkBulkExport) {
				t.Fatalf("oversized comment omitted its existing bulk fallback: %s", body)
			}
		})
	}
}

func TestCommentPagesPreserveOrderingAndEdits(t *testing.T) {
	t.Parallel()
	repository, _ := projectRecordPostgresWithAdmin(t, 3)
	objects := &pageObjectProbe{DetailStore: &memoryRunDetails{}, t: t, repository: repository}
	service := openLocalPostgresHubTestService(t, repository, Config{RunDetails: hubmaintenance.LimitDetails(objects)})
	service.echo.GET(hubapi.NativeBase+"/work-items/:item/relationships", service.GetWorkItemRelationships, service.requireNativeScope(hubapi.APIScopeWorker, hubapi.APIScopeOperator))
	service.echo.GET(hubapi.NativeBase+"/work-items/:item/references", service.GetWorkItemReferences, service.requireNativeScope(hubapi.APIScopeWorker, hubapi.APIScopeOperator))
	for _, name := range []string{"First organization", "Second organization"} {
		objects.forbidden = false
		org, err := service.createNativeOrganizationFor(t.Context(), name)
		if err != nil {
			t.Fatal(err)
		}
		seedPostgresHubAdmin(t, repository, service, org)
		projectPath := "/api/v2/organizations/" + string(org.ID) + "/projects"
		response := performHubAPIRequest(t, service, http.MethodPost, projectPath, testHubAdminToken, map[string]any{"idempotency_key": "project", "name": "Example", "states": nativeFixtureStates()})
		requireNativeStatus(t, response, http.StatusOK)
		var project tracker.NativeProject
		decodeHubResponse(t, response, &project)
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
		path := projectPath + "/" + string(project.ID) + "/work-items"
		response = performHubAPIRequest(t, service, http.MethodPost, path, testHubAdminToken, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "issue"}, Title: "Example", Body: workItemRecordBody})
		requireNativeStatus(t, response, http.StatusOK)
		var issue tracker.NativeIssue
		decodeHubResponse(t, response, &issue)
		path += "/" + string(issue.WorkItemID) + "/comments"
		var firstComment, secondComment tracker.NativeComment
		seeded := 0
		for _, size := range []int{2, 50, 201} {
			service.config.TenantRepository = repository
			objects.forbidden = false
			for index := seeded; index < size; index++ {
				response = performHubAPIRequest(t, service, http.MethodPost, path, testHubAdminToken, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("comment-%d", index)}, Body: fmt.Sprintf("Comment %d", index)})
				requireNativeStatus(t, response, http.StatusOK)
				if index == 0 {
					decodeHubResponse(t, response, &firstComment)
				}
				if index == 1 {
					decodeHubResponse(t, response, &secondComment)
				}
			}
			if size == 50 {
				requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPatch, path+"/"+firstComment.ID, testHubAdminToken, tracker.UpdateComment{Mutation: tracker.Mutation{IdempotencyKey: "edit-first"}, ExpectedRevision: 1, Body: "Edited comment"}), http.StatusOK)
				requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPatch, path+"/"+secondComment.ID, testHubAdminToken, tracker.UpdateComment{Mutation: tracker.Mutation{IdempotencyKey: "delete-second"}, ExpectedRevision: 1, Deleted: true}), http.StatusOK)
			}

			countedSource := &commentPageReverseRepository{Repository: repository}
			service.config.TenantRepository = countedSource
			for _, limit := range []int{1, 64, 65, 100, 200} {
				for _, direction := range []string{"oldest", "newest"} {
					params := url.Values{"limit": {strconv.Itoa(limit)}, "direction": {direction}}
					seen := map[string]bool{}
					for pageNumber := 0; pageNumber <= size/limit; pageNumber++ {
						response := performHubAPIRequest(t, service, http.MethodGet, path+"?"+params.Encode(), testHubAdminToken, nil)
						if response.Code != http.StatusOK {
							t.Fatalf("source HTTP size=%d limit=%d direction=%s: status=%d body=%s", size, limit, direction, response.Code, response.Body.String())
						}
						var page tracker.Page[tracker.NativeComment]
						decodeHubResponse(t, response, &page)
						if len(page.Items) != min(limit, size-len(seen)) {
							t.Fatalf("source page rows=%d size=%d limit=%d seen=%d", len(page.Items), size, limit, len(seen))
						}
						for _, comment := range page.Items {
							want := len(seen)
							if direction == "newest" {
								want = size - 1 - want
							}
							body := fmt.Sprintf("Comment %d", want)
							if size >= 50 && want == 0 {
								body = "Edited comment"
							} else if size >= 50 && want == 1 {
								body = ""
							}
							if seen[comment.ID] || comment.Body != body || comment.OrganizationID != org.ID || comment.ProjectID != project.ID || comment.WorkItemID != issue.WorkItemID {
								t.Fatalf("invalid source comment at index %d: %+v", len(seen), comment)
							}
							seen[comment.ID] = true
						}
						if page.NextCursor == "" {
							break
						}
						params.Set("cursor", page.NextCursor)
					}
					if len(seen) != size {
						t.Fatalf("source cursor omitted comments: got=%d want=%d", len(seen), size)
					}
				}
			}
			if size == 201 {
				var version eventstore.CollaborationVersion
				if err := repository.ReadTenant(t.Context(), org.ID, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					version, err = r.CollaborationVersion(t.Context(), firstComment.ID, 2)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				details := objects.DetailStore.(*memoryRunDetails)
				original := details.data[version.PayloadObjectKey]
				details.data[version.PayloadObjectKey] = []byte("{}")
				_, err := service.readCommentSourceRecords(t.Context(), hubscope.NativeScope{Organization: org.ID, Project: project.ID}, string(issue.WorkItemID), url.Values{"limit": {"200"}})
				if !errors.Is(err, artifact.ErrIntegrity) {
					t.Fatalf("corrupt comment payload must fail closed: %v", err)
				}
				requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, path+"?limit=200", testHubAdminToken, nil), http.StatusInternalServerError)
				details.data[version.PayloadObjectKey] = original
				requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, path+"?limit=201", testHubAdminToken, nil), http.StatusUnprocessableEntity)
			}
			service.config.TenantRepository = repository

			credential, status, err := service.authenticateTenantAPIHash(t.Context(), org.ID, apikey.HashToken(testHubAdminToken), "", "")
			if err != nil || status != http.StatusOK {
				t.Fatalf("detail fixture credential status=%d: %v", status, err)
			}
			scope := hubscope.NativeScope{Organization: org.ID, Project: project.ID, Credential: credential}
			readHead := func() int64 {
				t.Helper()
				var head viewstore.BoardSequence
				if err := repository.ReadTenant(t.Context(), org.ID, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					head, err = r.BoardSequence(t.Context(), project.ID)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if head.Sequence == 0 {
					t.Fatal("detail fixture has no source head")
				}
				return head.Sequence
			}
			assertHead := func(value tracker.NativeIssue, head int64) {
				t.Helper()
				if value.ChangeSequence != head || slices.Contains(value.OmittedFields, "change_sequence") {
					t.Fatalf("detail head=%d want=%d omitted=%v", value.ChangeSequence, head, value.OmittedFields)
				}
			}
			assertReadinessOmitted := func(issue tracker.NativeIssue) {
				t.Helper()
				if issue.DependencyReady || issue.SchedulingEligible != nil || !slices.Contains(issue.OmittedFields, "dependency_ready") || !slices.Contains(issue.OmittedFields, "scheduling_eligible") {
					t.Fatalf("basic item read exposed scheduling readiness: ready=%t eligible=%v omitted=%v", issue.DependencyReady, issue.SchedulingEligible, issue.OmittedFields)
				}
			}
			coldPage, err := service.workItemResource(t.Context(), scope, string(issue.WorkItemID), url.Values{})
			if err != nil {
				t.Fatal(err)
			}
			assertReadinessOmitted(coldPage)
			assertHead(coldPage, readHead())
			detailPath := strings.TrimSuffix(path, "/comments")
			response = performHubAPIRequest(t, service, http.MethodGet, detailPath, testHubAdminToken, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var coldREST tracker.NativeIssue
			decodeHubResponse(t, response, &coldREST)
			assertHead(coldREST, readHead())
			assertReadinessOmitted(coldREST)
			if size == 2 {
				if got, err := service.workItemResource(t.Context(), scope, string(issue.WorkItemID), url.Values{}); err != nil || got.Body != workItemRecordBody {
					t.Fatalf("cold detail: %v", err)
				}
				response := performHubAPIRequest(t, service, http.MethodGet, strings.TrimSuffix(path, "/comments"), testHubAdminToken, nil)
				requireNativeStatus(t, response, http.StatusOK)
				response = performHubAPIRequest(t, service, http.MethodGet, path, testHubAdminToken, nil)
				requireNativeStatus(t, response, http.StatusOK)
				changed := false
				var currentIssue tracker.NativeIssue
				objects.afterRead = func() {
					changed = true
					title := "Changed during immutable object hydration"
					updated := performHubAPIRequest(t, service, http.MethodPatch, detailPath, testHubAdminToken, tracker.UpdateIssue{
						Mutation: tracker.Mutation{IdempotencyKey: "detail-source-change"}, ExpectedRevision: issue.Revision, Title: &title,
					})
					requireNativeStatus(t, updated, http.StatusOK)
					decodeHubResponse(t, updated, &currentIssue)
				}
				response = performHubAPIRequest(t, service, http.MethodGet, detailPath, testHubAdminToken, nil)
				requireNativeStatus(t, response, http.StatusConflict)
				if !changed || objects.afterRead != nil {
					t.Fatal("cold detail did not exercise the object-read source change")
				}
				var conflict hubapi.NativeError
				decodeHubResponse(t, response, &conflict)
				if conflict.Code != "revision_conflict" || conflict.CurrentRevision != currentIssue.Revision || currentIssue.Revision != issue.Revision+1 {
					t.Fatalf("content change refusal: %+v current=%v original=%v", conflict, currentIssue.Revision, issue.Revision)
				}
				issue = currentIssue
				beforeHead := readHead()
				var beforeEvent, afterEvent corestore.WorkItem
				if err := repository.ReadTenant(t.Context(), org.ID, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					beforeEvent, err = r.WorkItem(t.Context(), project.ID, issue.WorkItemID)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				activityAt := beforeEvent.LastActivityAt.Add(time.Minute)
				eventRaw, err := json.Marshal(tracker.CollaborationEvent{
					ID: hubapi.NewID("evt"), OrganizationID: org.ID, ProjectID: project.ID, AggregateType: "issue", AggregateID: issue.WorkItemID,
					Type: "run.checkpointed", SchemaVersion: 1, Actor: scope.Actor(), RecordedAt: activityAt,
				})
				if err != nil {
					t.Fatal(err)
				}
				event, err := eventstore.NewContent(service.runDetailStore()).PrepareEvent(t.Context(), org.ID, eventRaw)
				if err != nil {
					t.Fatal(err)
				}
				changed = false
				objects.afterRead = func() {
					changed = true
					if err := repository.WriteTenant(t.Context(), org.ID, corestore.Serving, func(w tenantstore.Writer) error {
						_, err := w.AppendEvent(t.Context(), event)
						return err
					}); err != nil {
						t.Fatal(err)
					}
					if err := repository.ReadTenant(t.Context(), org.ID, corestore.Serving, func(r tenantstore.Reader) error {
						var err error
						afterEvent, err = r.WorkItem(t.Context(), project.ID, issue.WorkItemID)
						return err
					}); err != nil {
						t.Fatal(err)
					}
				}
				response = performHubAPIRequest(t, service, http.MethodGet, detailPath, testHubAdminToken, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var afterEventREST tracker.NativeIssue
				decodeHubResponse(t, response, &afterEventREST)
				if !changed || objects.afterRead != nil || afterEvent.Revision != beforeEvent.Revision || afterEvent.EventSequence != beforeEvent.EventSequence+1 || !afterEvent.LastActivityAt.Equal(activityAt) {
					t.Fatalf("event-only fixture did not preserve content revision: before=%+v after=%+v fired=%t", beforeEvent, afterEvent, changed)
				}
				if afterEventREST.Revision != issue.Revision || afterEventREST.Title != issue.Title || afterEventREST.Body != workItemRecordBody || !afterEventREST.LastActivityAt.Equal(afterEvent.LastActivityAt) || afterEventREST.ChangeSequence <= beforeHead {
					t.Fatalf("event-only detail lost current metadata: %+v headBefore=%d", afterEventREST, beforeHead)
				}
				assertHead(afterEventREST, readHead())
				assertReadinessOmitted(afterEventREST)

				readerID, readerToken := "detail_reader", "example-detail-reader-"+string(org.ID)
				now := service.config.Clock()
				if err := repository.WriteTenant(t.Context(), org.ID, corestore.Serving, func(w tenantstore.Writer) error {
					return w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org.ID, ID: readerID, Name: "Example detail reader", Hash: apikey.HashToken(readerToken), Scope: "operator", NativeOnly: true, Kind: "service", ProjectAccess: "all", CreatedAt: now, UpdatedAt: now})
				}); err != nil {
					t.Fatal(err)
				}
				readerCredential, readerStatus, err := service.authenticateTenantAPIHash(t.Context(), org.ID, apikey.HashToken(readerToken), "", "")
				if err != nil || readerStatus != http.StatusOK {
					t.Fatalf("detail reader credential status=%d: %v", readerStatus, err)
				}
				requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, detailPath, readerToken, nil), http.StatusOK)
				changed = false
				objects.afterRead = func() {
					changed = true
					if err := repository.WriteTenant(t.Context(), org.ID, corestore.Serving, func(w tenantstore.Writer) error {
						return w.RevokeAccessCredential(t.Context(), readerID, service.config.Clock())
					}); err != nil {
						t.Fatal(err)
					}
				}
				response = performHubAPIRequest(t, service, http.MethodGet, detailPath, readerToken, nil)
				requireNativeStatus(t, response, http.StatusForbidden)
				var denied hubapi.APIErrorResponse
				decodeHubResponse(t, response, &denied)
				if !changed || objects.afterRead != nil || denied.Code != "access_denied" {
					t.Fatalf("detail did not refuse revocation between snapshots: changed=%t response=%+v", changed, denied)
				}
				changed = false
				objects.afterRead = func() { changed = true }
				_, err = service.readWorkItemSourcePage(t.Context(), hubscope.NativeScope{Organization: org.ID, Project: project.ID, Credential: readerCredential}, string(issue.WorkItemID))
				objects.afterRead = nil
				if !errors.Is(err, accessstore.ErrDenied) || changed {
					t.Fatalf("revoked authority reached immutable content: objectRead=%t err=%v", changed, err)
				}
			}
			_, pendingErr := service.readCommentPageModel(t.Context(), scope, string(issue.WorkItemID), url.Values{"limit": {"50"}})
			var publicErr *hubapi.NativeError
			if !errors.As(pendingErr, &publicErr) || publicErr.Code != "not_ready" {
				t.Fatalf("cold or dirty comments must be pending, got %v", pendingErr)
			}
			if size == 2 {
				for _, part := range []string{"/relationships", "/references"} {
					response := performHubAPIRequest(t, service, http.MethodGet, strings.TrimSuffix(path, "/comments")+part, testHubAdminToken, nil)
					requireNativeStatus(t, response, http.StatusServiceUnavailable)
					response = performHubAPIRequest(t, service, http.MethodGet, strings.TrimSuffix(strings.TrimSuffix(path, "/comments"), string(issue.WorkItemID))+"missing"+part, testHubAdminToken, nil)
					requireNativeStatus(t, response, http.StatusNotFound)
				}
			}
			seeded = size
			tenant := hubmaintenance.Tenant{Organization: string(org.ID), Generation: 1}

			scheduler, err := hubmaintenance.New(maintenanceTenantRegistry{tenant}, []hubmaintenance.Feature{service.WorkItemPageMaintenanceFeature()})
			if err != nil {
				t.Fatal(err)
			}
			for steps := 0; steps < 512; steps++ {
				if err := scheduler.Tick(t.Context()); err != nil {
					t.Fatal(err)
				}

				partial, partialErr := service.readCommentPageModel(t.Context(), scope, string(issue.WorkItemID), url.Values{"limit": {"50"}})
				if partialErr == nil && len(partial.Items) != min(50, size) {
					t.Fatalf("partial projection returned complete success: %d/%d", len(partial.Items), size)
				}
				if partialErr != nil && (!errors.As(partialErr, &publicErr) || publicErr.Code != "not_ready") {
					t.Fatal(partialErr)
				}
				err := repository.ReadTenant(t.Context(), org.ID, corestore.Background, func(r tenantstore.Reader) error {
					_, err := r.(viewstore.WorkItemPageReader).NextWorkItemPageCandidate(t.Context())
					return err
				})
				if errors.Is(err, corestore.ErrNotFound) {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if steps == 511 {
					t.Fatal("page projector did not drain bounded fixture")
				}
			}

			objects.forbidden = true
			corePage, err := service.workItemResource(t.Context(), scope, string(issue.WorkItemID), url.Values{})
			if err != nil || corePage.Body != workItemRecordBody || corePage.OrganizationID != org.ID || corePage.ProjectID != project.ID || corePage.WorkItemID != issue.WorkItemID {
				t.Fatalf("typed item page: err=%v body=%q", err, corePage.Body)
			}
			assertReadinessOmitted(corePage)
			assertHead(corePage, readHead())
			response = performHubAPIRequest(t, service, http.MethodGet, detailPath, testHubAdminToken, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var warmREST tracker.NativeIssue
			decodeHubResponse(t, response, &warmREST)
			assertHead(warmREST, readHead())
			assertReadinessOmitted(warmREST)

			service.config.TenantRepository = repository
			for _, limit := range []int{1, 2, 50, 100, 200} {
				for _, direction := range []string{"oldest", "newest"} {
					page, err := service.readCommentRecords(t.Context(), hubscope.NativeScope{Organization: org.ID, Project: project.ID}, string(issue.WorkItemID), url.Values{"limit": {strconv.Itoa(limit)}, "direction": {direction}})
					if err != nil {
						t.Fatal(err)
					}
					if len(page.Items) != min(limit, size) || (page.NextCursor != "") != (limit < size) {
						t.Fatalf("page len=%d next=%q", len(page.Items), page.NextCursor)
					}
					for index, comment := range page.Items {
						if comment.OrganizationID != org.ID || comment.ProjectID != project.ID || comment.WorkItemID != issue.WorkItemID {
							t.Fatalf("comment page crossed its authorized scope: %+v", comment)
						}
						want := index
						if direction == "newest" {
							want = size - 1 - index
						}
						body := fmt.Sprintf("Comment %d", want)
						if size >= 50 && want < 2 {
							if comment.Revision != 2 || comment.EditedBy == nil {
								t.Fatalf("projected edit metadata=%+v", comment)
							}
							if want == 0 {
								body = "Edited comment"
							} else {
								body = ""
								if !comment.Deleted {
									t.Fatal("projected deletion lost its tombstone")
								}
							}
						}
						if comment.Body != body {
							t.Fatalf("body=%q want %q", comment.Body, body)
						}
					}
				}
			}
		}
		service.config.TenantRepository = repository
	}
}

func (r commentPageReverseReader) WorkItemDetailPage(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID) (viewstore.ItemPageRecord, error) {
	return r.Reader.(viewstore.WorkItemPageReader).WorkItemDetailPage(ctx, project, item)
}
func (r commentPageReverseReader) CommentPageRecords(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, after int64, newest bool, limit int) ([]viewstore.CommentPageRecord, error) {
	return r.Reader.(viewstore.WorkItemPageReader).CommentPageRecords(ctx, project, item, after, newest, limit)
}
func (r commentPageReverseReader) NextWorkItemPageCandidate(ctx context.Context) (viewstore.PageCandidate, error) {
	return r.Reader.(viewstore.WorkItemPageReader).NextWorkItemPageCandidate(ctx)
}

func (r *commentPageReverseRepository) ReadRequest(ctx context.Context, org tracker.OrganizationID, run func(context.Context) error) error {
	return r.Repository.(tenantstore.RequestReader).ReadRequest(ctx, org, run)
}

type pageObjectProbe struct {
	attachment.DetailStore
	t          *testing.T
	repository *postgres.Repository
	forbidden  bool
	afterRead  func()
}

func (p *pageObjectProbe) ReadDetail(ctx context.Context, org string, ref attachment.ArchiveReference) ([]byte, error) {
	if p.forbidden {
		return nil, errors.New("page handler read object storage")
	}
	if p.repository.Stats().AcquiredConns() != 0 {
		p.t.Error("object read held a database connection")
	}
	raw, err := p.DetailStore.ReadDetail(ctx, org, ref)
	if err == nil && p.afterRead != nil {
		afterRead := p.afterRead
		p.afterRead = nil
		afterRead()
	}
	return raw, err
}

func (r commentPageReverseReader) WorkItemPageReady(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, kind string) (bool, error) {
	return r.Reader.(viewstore.WorkItemPageReadinessReader).WorkItemPageReady(ctx, project, item, kind)
}
