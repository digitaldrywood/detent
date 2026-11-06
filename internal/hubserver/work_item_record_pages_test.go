package hubserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"

	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type commentPageCountingRepository struct {
	tenantstore.Repository
	singles, batches int
}

func (r *commentPageCountingRepository) ReadTenant(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	return r.Repository.ReadTenant(ctx, org, workload, func(reader tenantstore.Reader) error {
		return read(commentPageCountingReader{Reader: reader, repository: r})
	})
}

type commentPageCountingReader struct {
	tenantstore.Reader
	repository *commentPageCountingRepository
}

func (r commentPageCountingReader) CollaborationVersion(ctx context.Context, id string, revision int64) (eventstore.CollaborationVersion, error) {
	r.repository.singles++
	return r.Reader.CollaborationVersion(ctx, id, revision)
}

func (r commentPageCountingReader) CollaborationVersions(ctx context.Context, keys []eventstore.CollaborationVersionKey) ([]eventstore.CollaborationVersion, error) {
	r.repository.batches++
	versions, err := r.Reader.(eventstore.CollaborationVersionsReader).CollaborationVersions(ctx, keys)
	for left, right := 0, len(versions)-1; left < right; left, right = left+1, right-1 {
		versions[left], versions[right] = versions[right], versions[left]
	}
	return versions, err
}

func TestCommentPageUsesSetBasedVersions(t *testing.T) {
	repository := projectRecordPostgres(t)
	objects := &pageObjectProbe{DetailStore: &memoryRunDetails{}, t: t, repository: repository}
	service := openLocalPostgresHubTestService(t, repository, Config{RunDetails: hubmaintenance.LimitDetails(objects)})
	service.echo.GET(nativeBase+"/work-items/:item/relationships", service.GetWorkItemRelationships, service.requireNativeScope(apiScopeWorker, apiScopeOperator))
	service.echo.GET(nativeBase+"/work-items/:item/references", service.GetWorkItemReferences, service.requireNativeScope(apiScopeWorker, apiScopeOperator))
	for _, name := range []string{"First organization", "Second organization"} {
		objects.forbidden = false
		org, err := service.createNativeOrganizationFor(t.Context(), name)
		if err != nil {
			t.Fatal(err)
		}
		seedPostgresHubAdmin(t, repository, service, org)
		service.config.SecretKeys = secretTestKeys(t, "1", "1")
		envelope, err := service.config.SecretKeys.Seal(bytes.Repeat([]byte{1}, 32), accessstore.SecretAAD(org.ID, "", accessstore.NativeCursorKeyKind))
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), org.ID, corestore.Serving, func(w tenantstore.Writer) error {
			return w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: org.ID, Kind: accessstore.NativeCursorKeyKind, Envelope: envelope, UpdatedAt: service.config.now()}, accessstore.SecretAudit{OrganizationID: org.ID, Kind: accessstore.NativeCursorKeyKind, Actor: "migration", Event: "import", KeyVersion: envelope.Version, RecordedAt: service.config.now()})
		}); err != nil {
			t.Fatal(err)
		}
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
		for _, size := range []int{2, 50} {
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

			scope := nativeScope{organization: org.ID, project: project.ID}
			if size == 2 {
				if got, err := service.workItemResource(t.Context(), scope, string(issue.WorkItemID), url.Values{}); err != nil || got.Body != workItemRecordBody {
					t.Fatalf("cold detail: %v", err)
				}
				response := performHubAPIRequest(t, service, http.MethodGet, strings.TrimSuffix(path, "/comments"), testHubAdminToken, nil)
				requireNativeStatus(t, response, http.StatusOK)
				response = performHubAPIRequest(t, service, http.MethodGet, path, testHubAdminToken, nil)
				requireNativeStatus(t, response, http.StatusOK)
			}
			_, pendingErr := service.readCommentPageModel(t.Context(), scope, string(issue.WorkItemID), url.Values{"limit": {"50"}})
			var publicErr *nativeError
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
			for steps := 0; steps < 256; steps++ {
				if err := scheduler.Tick(t.Context()); err != nil {
					t.Fatal(err)
				}

				partial, partialErr := service.readCommentPageModel(t.Context(), scope, string(issue.WorkItemID), url.Values{"limit": {"50"}})
				if partialErr == nil && len(partial.Items) != size {
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
				if steps == 255 {
					t.Fatal("page projector did not drain bounded fixture")
				}
			}

			objects.forbidden = true
			corePage, err := service.workItemResource(t.Context(), nativeScope{organization: org.ID, project: project.ID}, string(issue.WorkItemID), url.Values{})
			if err != nil || corePage.Body != workItemRecordBody || corePage.OrganizationID != org.ID || corePage.ProjectID != project.ID || corePage.WorkItemID != issue.WorkItemID {
				t.Fatalf("typed item page: err=%v body=%q", err, corePage.Body)
			}

			for _, part := range []string{"", "/relationships", "/references"} {
				durations := []time.Duration{}
				for sample := 0; sample < 20; sample++ {
					started := time.Now()
					response := performHubAPIRequest(t, service, http.MethodGet, strings.TrimSuffix(path, "/comments")+part, testHubAdminToken, nil)
					durations = append(durations, time.Since(started))
					requireNativeStatus(t, response, http.StatusOK)
				}
				slices.Sort(durations)
				t.Logf("HTTP part=%q tenant=%s history=%d samples=%d p95=%s", part, org.ID, size, len(durations), durations[18])
			}
			counted := &commentPageCountingRepository{Repository: repository}
			service.config.TenantRepository = counted
			for _, limit := range []int{1, 2, 50} {
				durations := []time.Duration{}
				for _, direction := range []string{"oldest", "newest"} {
					for sample := 0; sample < 10; sample++ {
						counted.singles, counted.batches = 0, 0
						statements, transactions := 0, 0
						readCtx := postgres.WithTransactionTiming(t.Context(), func(name string, _ time.Time) {
							if name == "pg_begin" {
								transactions++
							}
						}, func(_ time.Time, _ string) { statements++ })
						started := time.Now()
						page, err := service.readCommentRecords(readCtx, nativeScope{organization: org.ID, project: project.ID}, string(issue.WorkItemID), url.Values{"limit": {strconv.Itoa(limit)}, "direction": {direction}})
						durations = append(durations, time.Since(started))
						if err != nil {
							t.Fatal(err)
						}
						if statements != 6 || transactions != 1 {
							t.Errorf("limit=%d: observed statements=%d (+2 setup), transactions=%d, want 6/1", limit, statements, transactions)
						}
						if counted.singles != 0 || counted.batches != 0 {
							t.Errorf("%s limit=%d direction=%s: single versions=%d batches=%d, want 0/0", name, limit, direction, counted.singles, counted.batches)
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
							if size == 50 && want < 2 {
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
				httpDurations := []time.Duration{}
				httpStatements, httpTransactions := 0, 0
				for sample := 0; sample < 20; sample++ {
					httpStatements, httpTransactions = 0, 0
					ctx := postgres.WithTransactionTiming(t.Context(), func(name string, _ time.Time) {
						if name == "pg_begin" {
							httpTransactions++
						}
					}, func(_ time.Time, _ string) { httpStatements++ })
					request := newHubAPIRequest(t, http.MethodGet, path+"?limit="+strconv.Itoa(limit), testHubAdminToken, nil).WithContext(ctx)
					response := httptest.NewRecorder()
					started := time.Now()
					service.Handler().ServeHTTP(response, request)
					httpDurations = append(httpDurations, time.Since(started))
					requireNativeStatus(t, response, http.StatusOK)
					if httpStatements > 18 || httpTransactions > 3 {
						t.Errorf("HTTP statement budget: SQL=%d transactions=%d", httpStatements, httpTransactions)
					}
				}
				slices.Sort(httpDurations)
				t.Logf("HTTP tenant=%s rows=%d page=%d samples=%d p95=%s observed_sql=%d transactions=%d", org.ID, size, limit, len(httpDurations), httpDurations[18], httpStatements, httpTransactions)

				slices.Sort(durations)
				p95 := durations[(len(durations)*95+99)/100-1]
				t.Logf("tenant=%s rows=%d page=%d samples=%d p95=%s sql=8 transactions=1", org.ID, size, limit, len(durations), p95)

			}
		}
		service.config.TenantRepository = repository
	}
}

func (r commentPageCountingReader) WorkItemDetailPage(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID) (viewstore.ItemPageRecord, error) {
	return r.Reader.(viewstore.WorkItemPageReader).WorkItemDetailPage(ctx, project, item)
}
func (r commentPageCountingReader) CommentPageRecords(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, after int64, newest bool, limit int) ([]viewstore.CommentPageRecord, error) {
	return r.Reader.(viewstore.WorkItemPageReader).CommentPageRecords(ctx, project, item, after, newest, limit)
}
func (r commentPageCountingReader) NextWorkItemPageCandidate(ctx context.Context) (viewstore.PageCandidate, error) {
	return r.Reader.(viewstore.WorkItemPageReader).NextWorkItemPageCandidate(ctx)
}

func (r *commentPageCountingRepository) ReadRequest(ctx context.Context, org tracker.OrganizationID, run func(context.Context) error) error {
	return r.Repository.(tenantstore.RequestReader).ReadRequest(ctx, org, run)
}

type pageObjectProbe struct {
	attachment.DetailStore
	t          *testing.T
	repository *postgres.Repository
	forbidden  bool
}

func (p *pageObjectProbe) ReadDetail(ctx context.Context, org string, ref attachment.ArchiveReference) ([]byte, error) {
	if p.forbidden {
		return nil, errors.New("page handler read object storage")
	}
	if p.repository.Stats().AcquiredConns() != 0 {
		p.t.Error("object read held a database connection")
	}
	return p.DetailStore.ReadDetail(ctx, org, ref)
}

func (r commentPageCountingReader) WorkItemPageReady(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, kind string) (bool, error) {
	return r.Reader.(viewstore.WorkItemPageReadinessReader).WorkItemPageReady(ctx, project, item, kind)
}
