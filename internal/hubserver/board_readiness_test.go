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
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestBoardReadinessBeforeAndDuringBackfill(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 6, 18, 0, 0, 0, time.UTC)
	service := newPostgresNativeService(t, Config{RunDetails: &memoryRunDetails{}, Clock: func() time.Time { return now }})
	repo := service.config.TenantRepository
	for _, org := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		t.Run(string(org), func(t *testing.T) {
			fixture := newNativeFixture(t, service, org, "Example backfill")
			scope := hubscope.NativeScope{Organization: org, Project: fixture.project.ID, Credential: hubapi.APICredential{ID: "test_hub_admin", Hash: apikey.HashToken(testHubAdminToken), Scope: hubapi.APIScopeAdmin}}
			records := []viewstore.BoardRecord{}
			for n := 1; n <= 2; n++ {
				id := tracker.NativeWorkItemID(fmt.Sprintf("wi_pending_%d", n))
				issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: scope.Project, WorkItemID: id, Number: n, Revision: 1}, Title: "Example", State: "Todo", Actor: tracker.Actor{Kind: "human", PrincipalID: "owner"}, CreatedAt: now, UpdatedAt: now}
				payload, err := viewstore.NewContent(service.runDetailStore()).PrepareBoard(t.Context(), tracker.NativeBoardCard{Issue: issue})
				if err != nil {
					t.Fatal(err)
				}
				if err := repo.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
					item, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: scope.Project, NativeID: id, Number: int64(n), Title: issue.Title, WorkflowStateName: new(issue.State), ActorPrincipalID: new("owner")})
					if err != nil {
						return err
					}
					project, err := w.Project(t.Context(), scope.Project)
					if err != nil {
						return err
					}
					records = append(records, viewstore.BoardRecord{OrganizationID: org, ProjectID: scope.Project, WorkItemID: id, SourceRevision: item.Revision, SourceEventSequence: item.EventSequence, SourceProjectRevision: project.ContentSettingsRevision, Position: viewstore.BoardPosition{State: "Todo", Identifier: fmt.Sprintf("example#%d", n)}, Payload: payload, PageIssue: &issue, ListIssue: &issue, ListBytes: 1000, ProjectedAt: now, CreatedAt: now, UpdatedAt: now, ActorPrincipalID: "owner", ReporterRelations: &viewstore.BoardReporterRelations{}})
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
					return w.ReplaceBoardItem(t.Context(), viewstore.BoardUpdate{ProjectID: scope.Project, WorkItemID: row.WorkItemID, ExpectedRevision: row.SourceRevision, ExpectedEventSequence: row.SourceEventSequence, ExpectedProjectRevision: row.SourceProjectRevision, Current: &row})
				}); err != nil {
					t.Fatal(err)
				}
			}
			check := func(pending bool) {
				t.Helper()
				for _, include := range []string{"board", "work", "summary"} {
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
					}).BoardSummaryModel(t.Context(), viewstore.BoardSelection{ProjectID: scope.Project})
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
					Pending bool              `json:"projection_pending"`
					Rows    []json.RawMessage `json:"rows"`
				}
				if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
					t.Errorf("missing counts readiness envelope: %v", err)
				}
				if pending && (recorder.Code != http.StatusOK || !envelope.Pending || envelope.Rows == nil || len(envelope.Rows) != 0) {
					t.Errorf("pending HTTP=%d %s", recorder.Code, recorder.Body.String())
				}
				if !pending && (recorder.Code != http.StatusOK || envelope.Pending) {
					t.Errorf("ready HTTP=%d %s", recorder.Code, recorder.Body.String())
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
			if _, err := usagePostgresAdmin(t, service).ExecContext(t.Context(), "UPDATE issues SET event_sequence=event_sequence+1,last_activity_at=greatest(last_activity_at,$3::timestamptz) WHERE organization_id=$1 AND project_id=$2 AND native_id=$4", org, scope.Project, now.Add(time.Minute), records[0].WorkItemID); err != nil {
				t.Fatal(err)
			}
			check(false)
			page, err := service.readBoardRecords(t.Context(), scope, url.Values{"include": {"board"}})
			if err != nil {
				t.Fatal(err)
			}
			if page.ProjectionOldestPendingAt == nil {
				t.Error("re-queued projected card does not report projection lag")
			}
			if err := repo.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
				_, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: scope.Project, NativeID: "wi_pending_3", Number: 3, Title: "Example", WorkflowStateName: new("Todo"), ActorPrincipalID: new("owner")})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			check(true)
		})
	}
	t.Run("dirty queue", func(t *testing.T) { testBoardReadinessDirtyQueue(t, service) })
}

func testBoardReadinessDirtyQueue(t *testing.T, service *Service) {
	admin := usagePostgresAdmin(t, service)
	ctx := t.Context()
	exec := func(t *testing.T, query string, args ...any) {
		t.Helper()
		if _, err := admin.ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		exec(t, `INSERT INTO projects(organization_id,id,name,slug,profile) VALUES ($1,'shared','shared','shared','native'),($1,'other','other','other','native')`, org)
		exec(t, `INSERT INTO project_workflow_states(organization_id,project_id,name,terminal,dispatchable,board_rank) VALUES ($1,'shared','Todo',false,true,0)`, org)
	}
	var cards []boardSeedCard
	for n := 1; n <= 257; n++ {
		cards = append(cards, boardSeedCard{id: tracker.NativeWorkItemID(fmt.Sprintf("wi_%d", n)), number: n, state: "Todo"})
	}
	seedBoardCards(t, service, "org_first", "shared", cards...)
	seedBoardCards(t, service, "org_first_second", "shared", cards[0])
	exec(t, `UPDATE native_board_sequences SET sequence=12345 WHERE organization_id='org_first' AND project_id='shared'`)
	exec(t, `UPDATE native_board_sequences SET sequence=54321 WHERE organization_id='org_first_second' AND project_id='shared'`)
	exec(t, `UPDATE native_board_items SET actor_principal_id='wrong' WHERE organization_id='org_first_second' AND project_id='shared' AND work_item_id='wi_1'`)
	epoch := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	foreignOldest := epoch.AddDate(-1, 0, 0)
	exec(t, `INSERT INTO native_board_dirty(organization_id,project_id,work_item_id,queued_at)
 VALUES ('org_first_second','shared','wi_1',$1),('org_first','other','wi_1',$1)`, foreignOldest)

	check := func(t *testing.T, org tracker.OrganizationID, project tracker.ProjectID, pending bool, sequence int64, oldest *time.Time) {
		t.Helper()
		for _, mode := range []string{"metadata", "work+summary"} {
			selection := viewstore.BoardSelection{ProjectID: project, Limit: 1}
			var work *viewstore.BoardSelection
			if mode == "work+summary" {
				selection.Compact, selection.Summary = true, true
				w := selection
				w.WorkOrder, w.Terminal = true, new(false)
				work = &w
			}
			if err := service.config.TenantRepository.ReadTenant(ctx, org, corestore.Serving, func(r tenantstore.Reader) error {
				page, err := r.(interface {
					BoardModelPages(context.Context, viewstore.BoardSelection, *viewstore.BoardSelection, accessstore.Authority) (viewstore.BoardModelPage, error)
				}).BoardModelPages(ctx, selection, work, accessstore.Authority{})
				if err != nil {
					return err
				}
				sameTime := page.OldestPendingAt == oldest || (page.OldestPendingAt != nil && oldest != nil && page.OldestPendingAt.Equal(*oldest))
				if page.Pending != pending || page.Sequence != sequence || !sameTime {
					t.Errorf("%s %s/%s pending=%t sequence=%d oldest=%v, want %t %d %v", mode, org, project, page.Pending, page.Sequence, page.OldestPendingAt, pending, sequence, oldest)
				}
				if selection.Summary {
					if pending && len(page.Summaries) != 0 {
						t.Errorf("pending page has %d summary rows", len(page.Summaries))
					}
					if !pending && org == "org_first" && project == "shared" {
						var total int64
						for _, row := range page.Summaries {
							if row.Kind == "total" && row.Sequence == sequence {
								total = row.Total
							}
						}
						if total != 257 {
							t.Errorf("ready summary total=%d, want 257 at sequence %d", total, sequence)
						}
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	check(t, "org_first_second", "shared", true, 54321, &foreignOldest)
	check(t, "org_first", "other", true, 0, &foreignOldest)
	check(t, "org_first", "shared", false, 12345, nil)
	exec(t, `WITH unrelated AS (
 INSERT INTO issues(organization_id,project_id,number,title)
 SELECT 'org_first_second','other',n,'Queued item' FROM generate_series(1,2048) AS n
 RETURNING organization_id,project_id,native_id
) INSERT INTO native_board_dirty(organization_id,project_id,work_item_id,queued_at)
 SELECT organization_id,project_id,native_id,$1 FROM unrelated`, epoch)
	queue := func(t *testing.T, first, last int) {
		t.Helper()
		exec(t, `INSERT INTO native_board_dirty(organization_id,project_id,work_item_id,queued_at)
 SELECT 'org_first','shared','wi_'||n,$1::timestamptz - n * interval '1 second' FROM generate_series($2::int,$3::int) n`, epoch, first, last)
	}
	for _, size := range []int{255, 256, 257} {
		t.Run(fmt.Sprintf("depth_%d", size), func(t *testing.T) {
			exec(t, `DELETE FROM native_board_dirty WHERE organization_id='org_first' AND project_id='shared'`)
			queue(t, 1, size)
			oldest := epoch.Add(-time.Duration(size) * time.Second)
			check(t, "org_first", "shared", size > 256, 12345, &oldest)
		})
	}
	exec(t, `DELETE FROM native_board_dirty WHERE organization_id='org_first' AND project_id='shared'`)
	queue(t, 1, 256)
	for _, change := range []struct{ name, query string }{
		{"wrong principal", `UPDATE native_board_items SET actor_principal_id='wrong' WHERE organization_id='org_first' AND project_id='shared' AND work_item_id='wi_1'`},
		{"missing board row", `DELETE FROM native_board_items WHERE organization_id='org_first' AND project_id='shared' AND work_item_id='wi_1'`},
	} {
		t.Run(change.name, func(t *testing.T) {
			exec(t, change.query)
			oldest := epoch.Add(-256 * time.Second)
			check(t, "org_first", "shared", true, 12345, &oldest)
		})
	}
	exec(t, `DELETE FROM native_board_dirty WHERE organization_id='org_first' AND project_id='shared' AND work_item_id='wi_1'`)
	exec(t, `DELETE FROM issues WHERE organization_id='org_first' AND project_id='shared' AND native_id='wi_2'`)
	oldest := epoch.Add(-256 * time.Second)
	check(t, "org_first", "shared", true, 12345, &oldest)
}
