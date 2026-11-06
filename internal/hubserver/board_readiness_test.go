package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestBoardReadinessBeforeAndDuringBackfill(t *testing.T) {
	now := time.Date(2026, 11, 6, 18, 0, 0, 0, time.UTC)
	service := newPostgresNativeService(t, Config{RunDetails: &memoryRunDetails{}, now: func() time.Time { return now }})
	repo := service.config.TenantRepository
	for _, org := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		t.Run(string(org), func(t *testing.T) {
			fixture := newNativeFixture(t, service, org, "Example backfill")
			scope := nativeScope{organization: org, project: fixture.project.ID, credential: apiCredential{ID: "test_hub_admin", Hash: apikey.HashToken(testHubAdminToken), Scope: apiScopeAdmin}}
			records := []viewstore.BoardRecord{}
			for n := 1; n <= 2; n++ {
				id := tracker.NativeWorkItemID(fmt.Sprintf("wi_pending_%d", n))
				issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: scope.project, WorkItemID: id, Number: n, Revision: 1}, Title: "Example", State: "Todo", Actor: tracker.Actor{Kind: "human", PrincipalID: "owner"}, CreatedAt: now, UpdatedAt: now}
				payload, err := viewstore.NewContent(service.runDetailStore()).PrepareBoard(t.Context(), tracker.NativeBoardCard{Issue: issue})
				if err != nil {
					t.Fatal(err)
				}
				if err := repo.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
					item, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: scope.project, NativeID: id, Number: int64(n), Title: issue.Title, WorkflowStateName: new(issue.State), ActorPrincipalID: new("owner")})
					if err != nil {
						return err
					}
					project, err := w.Project(t.Context(), scope.project)
					if err != nil {
						return err
					}
					records = append(records, viewstore.BoardRecord{OrganizationID: org, ProjectID: scope.project, WorkItemID: id, SourceRevision: item.Revision, SourceEventSequence: item.EventSequence, SourceProjectRevision: project.ContentSettingsRevision, Position: viewstore.BoardPosition{State: "Todo", Identifier: fmt.Sprintf("example#%d", n)}, Payload: payload, PageIssue: &issue, ListIssue: &issue, ListBytes: 1000, ProjectedAt: now, CreatedAt: now, UpdatedAt: now, ActorPrincipalID: "owner", ReporterRelations: &viewstore.BoardReporterRelations{}})
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			publish := func(index int, page, list bool) {
				t.Helper()
				row := records[index]
				if !page {
					row.PageIssue = nil
				}
				if !list {
					row.ListIssue = nil
				}
				if err := repo.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
					return w.ReplaceBoardItem(t.Context(), viewstore.BoardUpdate{ProjectID: scope.project, WorkItemID: row.WorkItemID, ExpectedRevision: row.SourceRevision, ExpectedEventSequence: row.SourceEventSequence, ExpectedProjectRevision: row.SourceProjectRevision, Current: &row})
				}); err != nil {
					t.Fatal(err)
				}
			}
			check := func(pending bool) {
				t.Helper()
				for _, include := range []string{"board", "work"} {
					page, err := service.readBoardRecords(t.Context(), scope, url.Values{"include": {include}})
					if err != nil {
						t.Fatal(err)
					}
					if page.ProjectionPending != pending {
						t.Errorf("%s pending=%v want=%v", include, page.ProjectionPending, pending)
					}
				}
				if err := repo.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
					rows, err := r.(interface {
						BoardSummaryModel(context.Context, viewstore.BoardSelection) ([]viewstore.BoardSummaryRecord, error)
					}).BoardSummaryModel(t.Context(), viewstore.BoardSelection{ProjectID: scope.project})
					if pending {
						if !errors.Is(err, searchstore.ErrIndexPending) || len(rows) != 0 {
							t.Errorf("incomplete counts=%+v error=%v", rows, err)
						}
						return nil
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				recorder := httptest.NewRecorder()
				c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/board/counts", nil), recorder)
				c.Set("native_scope", scope)
				if err := service.boardModelCounts(c); err != nil {
					t.Fatal(err)
				}
				var envelope struct {
					Pending bool   `json:"projection_pending"`
					Code    string `json:"code"`
				}
				if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
					t.Errorf("missing counts readiness envelope: %v", err)
				}
				if pending && (recorder.Code != http.StatusServiceUnavailable || !envelope.Pending || envelope.Code != "not_ready") {
					t.Errorf("pending HTTP=%d %s", recorder.Code, recorder.Body.String())
				}
			}
			check(true)
			publish(0, true, true)
			check(true)
			publish(1, false, false)
			check(true)
			publish(1, true, false)
			check(true)
			publish(1, true, true)
			check(false)
		})
	}
}
