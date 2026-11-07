package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/testenv/pgtest"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type boardSearchDeadline struct{ observed, deadline time.Time }

type boardSearchSessionTrace struct {
	conn      *pgx.Conn
	deadlines []boardSearchDeadline
}

func (p *boardSearchSessionTrace) TraceQueryStart(ctx context.Context, conn *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	p.conn = conn
	if deadline, ok := ctx.Deadline(); ok {
		p.deadlines = append(p.deadlines, boardSearchDeadline{time.Now(), deadline})
	}
	return ctx
}
func (*boardSearchSessionTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
func (p *boardSearchSessionTrace) TraceBatchStart(ctx context.Context, conn *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	return p.TraceQueryStart(ctx, conn, pgx.TraceQueryStartData{})
}
func (*boardSearchSessionTrace) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {
}
func (*boardSearchSessionTrace) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

func newBoardSearchFixture(t *testing.T) searchFixture {
	t.Helper()
	admin := template.Database(t)
	repository, pool := repositoryFor(t, admin, postgres.PoolConfig{MaxConnections: 4, ServingConnections: 2, BackgroundConnections: 1})
	objects := newHistoryObjects()
	f := searchFixture{repository: repository, pool: pool, admin: admin, objects: objects, content: searchstore.NewContent(objects)}
	ctx := t.Context()

	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedTenantAccess(t, repository, org)
		if err := repository.WriteTenant(ctx, org, corestore.Background, func(w tenantstore.Writer) error {
			_, err := w.CreateProjectRecord(ctx, projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: "prj_first", Name: "Example", Profile: "native"}, States: []tracker.NativeState{{Name: "Todo", Dispatchable: true}}})
			if err != nil {
				return err
			}
			for index, id := range []tracker.NativeWorkItemID{"wi_first_1", "wi_first_2"} {
				if _, err := w.CreateWorkItem(ctx, corestore.WorkItem{OrganizationID: org, ProjectID: "prj_first", NativeID: id, Number: int64(index + 1), Title: "Example", ActorKind: new("human"), ActorPrincipalID: new("example"), WorkflowStateName: new("Todo")}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		for index, id := range []tracker.NativeWorkItemID{"wi_first_1", "wi_first_2"} {
			f.put(t, searchIssue(org, string(id)), searchstore.Text{Title: "Example", Body: "abcabcabcabcabc"}, 0)
			ref, err := f.objects.StoreDetail(ctx, string(org), []byte(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			err = f.repository.WriteTenant(ctx, org, corestore.Background, func(w tenantstore.Writer) error {
				issue := &tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: "prj_first", WorkItemID: id, Revision: 1, Number: index + 1}, Title: "Example", State: "Todo", CreatedAt: historyAt(), UpdatedAt: historyAt(), Actor: tracker.Actor{Kind: "human", PrincipalID: "example"}}
				row := viewstore.BoardRecord{OrganizationID: org, ProjectID: "prj_first", WorkItemID: id, SourceRevision: 1, ActorPrincipalID: "example", Position: viewstore.BoardPosition{State: "Todo", Identifier: string(id)}, CreatedAt: historyAt(), UpdatedAt: historyAt(), Payload: ref, PageIssue: issue, ListIssue: issue, ReporterRelations: &viewstore.BoardReporterRelations{}, Display: viewstore.BoardDisplay{Ready: true, Exact: true, Title: issue.Title, Actor: issue.Actor}}
				if err := w.ReplaceBoardItem(ctx, viewstore.BoardUpdate{ProjectID: "prj_first", WorkItemID: id, ExpectedRevision: 1, Current: &row}); err != nil {
					return err
				}
				full := *issue
				full.Body = "abcabcabcabcabc"
				return publishSearchBody(ctx, f, w, full)
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		f.drain(t, org)
	}
	return f
}

func publishSearchBody(ctx context.Context, f searchFixture, w tenantstore.Writer, issue tracker.NativeIssue) error {
	candidate := viewstore.PageCandidate{OrganizationID: issue.OrganizationID, ProjectID: issue.ProjectID, WorkItemID: issue.WorkItemID, Kind: "item"}
	if err := f.admin.QueryRowContext(ctx, "SELECT record_id,generation,source_at FROM native_work_item_page_dirty WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3 AND kind='item'", issue.OrganizationID, issue.ProjectID, issue.WorkItemID).Scan(&candidate.RecordID, &candidate.Generation, &candidate.SourceAt); err != nil {
		return err
	}
	return w.(viewstore.WorkItemPageWriter).PublishWorkItemPage(ctx, candidate, &viewstore.ItemPageRecord{Value: issue, Generation: candidate.Generation, SourceAt: candidate.SourceAt, ProjectedAt: historyAt()}, nil)
}

func TestBoardSearchBoundsHostileReporterAcrossTenants(t *testing.T) {
	f := newBoardSearchFixture(t)
	ctx := t.Context()
	trace := &boardSearchSessionTrace{}
	f.pool = tracedFeaturePool(t, f.pool, trace)
	counted, err := postgres.NewRepository(ctx, f.pool, smallPool())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := counted.Close(); err != nil {
			t.Error(err)
		}
	})
	f.repository = counted
	timedOut := func(err error) bool {
		var pgErr *pgconn.PgError
		return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &pgErr) && pgErr.Code == "57014" && strings.Contains(pgErr.Message, "statement timeout"))
	}
	settleSearch := func(t *testing.T) {
		t.Helper()
		if _, err := f.admin.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('board-search:org_first',0))`); err != nil {
			t.Fatal(err)
		}
	}
	selection := viewstore.BoardSelection{ProjectID: "prj_first", SearchText: "abc", Limit: 1, Reporter: true, ReporterPrincipalID: "example"}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		err := f.repository.ReadTenant(ctx, org, corestore.Serving, func(r tenantstore.Reader) error {
			rows, err := r.BoardPage(ctx, selection)
			if err == nil && (len(rows) != 1 || rows[0].OrganizationID != org) {
				t.Fatalf("tenant %s normal page=%+v", org, rows)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Run("document readiness stops at one posting", func(t *testing.T) {
		f := newBoardSearchFixture(t)
		if _, err := f.admin.ExecContext(ctx, `INSERT INTO search_postings(organization_id,scope,term,document_id,impact,updated_at,updated_at_submicrosecond)
 SELECT p.organization_id,p.scope,'example-'||n,p.document_id,p.impact,p.updated_at,p.updated_at_submicrosecond FROM
 (SELECT DISTINCT ON(organization_id,document_id) * FROM search_postings ORDER BY organization_id,document_id,term) p CROSS JOIN generate_series(1,6000) n`); err != nil {
			t.Fatal(err)
		}
		if _, err := f.admin.ExecContext(ctx, "ANALYZE search_documents,search_postings,native_board_items,issues"); err != nil {
			t.Fatal(err)
		}
		probe := &boardQueryTracer{}
		counted, err := postgres.NewRepository(ctx, tracedFeaturePool(t, f.pool, probe), smallPool())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := counted.Close(); err != nil {
				t.Error(err)
			}
		})
		for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
			if err := counted.ReadTenant(ctx, org, corestore.Serving, func(r tenantstore.Reader) error {
				rows, err := r.BoardPage(ctx, selection)
				if err == nil && (len(rows) != 1 || rows[0].OrganizationID != org) {
					t.Fatalf("tenant %s dense page=%+v", org, rows)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			tx, err := f.admin.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			if _, err := tx.ExecContext(ctx, "SET LOCAL ROLE detent_runtime_v2"); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.ExecContext(ctx, "SELECT set_config('detent.organization_id',$1,true)", org); err != nil {
				t.Fatal(err)
			}
			var raw []byte
			err = tx.QueryRowContext(ctx, "EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) "+probe.page, probe.args...).Scan(&raw)
			if rollbackErr := tx.Rollback(); rollbackErr != nil {
				t.Fatal(rollbackErr)
			}
			if err != nil {
				t.Fatal(err)
			}
			var plans []struct{ Plan boardLanePlan }
			if err := json.Unmarshal(raw, &plans); err != nil {
				t.Fatal(err)
			}
			var visited float64
			var inspect func(boardLanePlan)
			inspect = func(node boardLanePlan) {
				if node.Relation == "search_postings" || strings.HasPrefix(node.Index, "search_postings") {
					visited += (node.Rows + node.Removed) * node.Loops
				}
				for _, child := range node.Plans {
					inspect(child)
				}
			}
			inspect(plans[0].Plan)
			if visited > 100 {
				t.Fatalf("tenant %s readiness scanned %.0f postings for two documents: %s", org, visited, raw)
			}
			t.Logf("tenant %s posting rows visited=%.0f", org, visited)
		}
	})
	if _, err := f.admin.ExecContext(ctx, `UPDATE search_postings SET literal_positions=ARRAY(SELECT generate_series(1,80000,2)::bigint) WHERE organization_id='org_first' AND document_id='issue/wi_first_1' AND cardinality(literal_positions)>0`); err != nil {
		t.Fatal(err)
	}
	t.Run("hostile positional expansion", func(t *testing.T) {
		bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		hostile := selection
		hostile.SearchText = "cabcab"
		err := f.repository.ReadTenant(bounded, "org_first", corestore.Serving, func(r tenantstore.Reader) error {
			_, err := r.BoardPage(bounded, hostile)
			return err
		})
		if !timedOut(err) {
			t.Fatalf("hostile search escaped server budget: %v", err)
		}
	})
	t.Run("sequential searches share the first deadline", func(t *testing.T) {
		settleSearch(t)
		first, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		defer cancel()
		firstDeadline, _ := first.Deadline()
		err := f.repository.ReadTenant(ctx, "org_first", corestore.Serving, func(r tenantstore.Reader) error {
			trace.deadlines = nil
			if _, err := r.BoardPage(first, selection); err != nil {
				t.Fatalf("initial search failed: %v", err)
			}
			hostile := selection
			hostile.SearchText = "cabcab"
			_, err := r.BoardPage(ctx, hostile)
			for _, sample := range trace.deadlines {
				if sample.deadline.After(firstDeadline) {
					t.Fatalf("second search extended first deadline: %v > %v", sample.deadline, firstDeadline)
				}
			}
			return err
		})
		if err == nil {
			t.Fatalf("second search reset first read budget: %v", err)
		}
	})
	for _, path := range []string{"page", "model", "lanes", "closed"} {
		t.Run(path, func(t *testing.T) {
			read := func(ctx context.Context, r tenantstore.Reader) error {
				var err error
				switch path {
				case "page":
					_, err = r.BoardPage(ctx, selection)
				case "model":
					_, err = r.(interface {
						BoardModelPages(context.Context, viewstore.BoardSelection, *viewstore.BoardSelection, accessstore.Authority) (viewstore.BoardModelPage, error)
					}).BoardModelPages(ctx, selection, nil, accessstore.Authority{})
				case "lanes":
					_, err = r.BoardLanes(ctx, selection, historyAt())
				case "closed":
					_, err = r.BoardClosedWindows(ctx, selection, []time.Time{historyAt().Add(-time.Hour)}, historyAt())
				}
				return err
			}
			settleSearch(t)
			if err := f.repository.ReadTenant(ctx, "org_first", corestore.Serving, func(r tenantstore.Reader) error {
				trace.deadlines = nil
				if err := read(ctx, r); err != nil {
					return fmt.Errorf("probe search: %w", err)
				}
				if len(trace.deadlines) == 0 {
					t.Fatal("search omitted its client deadline")
				}
				sample := trace.deadlines[len(trace.deadlines)-1]
				budget := sample.deadline.Sub(sample.observed)
				if budget <= 0 || budget > 750*time.Millisecond {
					t.Fatalf("search client budget=%v", budget)
				}
				var setting string
				if err := trace.conn.QueryRow(ctx, "SHOW statement_timeout").Scan(&setting); err != nil {
					return fmt.Errorf("probe server budget: %w", err)
				}
				serverBudget, err := time.ParseDuration(setting)
				if err != nil || serverBudget <= 0 || serverBudget > 750*time.Millisecond {
					t.Fatalf("search server budget=%q, %v", setting, err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			blocker, err := f.admin.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := blocker.Rollback(); err != nil {
					t.Error(err)
				}
			}()
			if _, err := blocker.ExecContext(ctx, "LOCK TABLE search_postings IN ACCESS EXCLUSIVE MODE"); err != nil {
				t.Fatal(err)
			}
			bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			err = f.repository.ReadTenant(bounded, "org_first", corestore.Serving, func(r tenantstore.Reader) error {
				return read(bounded, r)
			})
			if !timedOut(err) {
				t.Fatalf("search did not enforce a server statement timeout: %v", err)
			}
		})
	}
	t.Run("tenant concurrency cap", func(t *testing.T) {
		held, err := f.admin.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := held.Rollback(); err != nil {
				t.Error(err)
			}
		}()
		if _, err := held.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('board-search:' || $1,0))`, "org_first"); err != nil {
			t.Fatal(err)
		}
		err = f.repository.ReadTenant(ctx, "org_first", corestore.Serving, func(r tenantstore.Reader) error { _, err := r.BoardPage(ctx, selection); return err })
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("saturated tenant search error=%v", err)
		}
		err = f.repository.ReadTenant(ctx, "org_first", corestore.Serving, func(r tenantstore.Reader) error {
			_, err := r.(interface {
				BoardModelPages(context.Context, viewstore.BoardSelection, *viewstore.BoardSelection, accessstore.Authority) (viewstore.BoardModelPage, error)
			}).BoardModelPages(ctx, selection, nil, accessstore.Authority{})
			return err
		})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("saturated tenant model error=%v", err)
		}
		err = f.repository.ReadTenant(ctx, "org_second", corestore.Serving, func(r tenantstore.Reader) error { _, err := r.BoardPage(ctx, selection); return err })
		if err != nil {
			t.Fatalf("other tenant starved: %v", err)
		}
	})
	if _, err := f.admin.ExecContext(ctx, `UPDATE issues SET actor_principal_id='another-owner' WHERE organization_id='org_first' AND native_id='wi_first_1'`); err != nil {
		t.Fatal(err)
	}
	for _, ranked := range []bool{false, true} {
		visible := selection
		visible.SearchText, visible.SearchRanked, visible.Limit = "cabcab", ranked, 200
		if err := f.repository.ReadTenant(ctx, "org_first", corestore.Serving, func(r tenantstore.Reader) error {
			rows, err := r.BoardPage(ctx, visible)
			if err == nil && (len(rows) != 1 || rows[0].WorkItemID != "wi_first_2") {
				t.Fatalf("reporter page=%+v", rows)
			}
			return err
		}); err != nil {
			t.Fatalf("invisible hostile document consumed reporter budget: %v", err)
		}
	}
	var timeout string
	if err := f.pool.QueryRow(ctx, "SHOW statement_timeout").Scan(&timeout); err != nil {
		t.Fatal(err)
	}
	if timeout != "0" {
		t.Fatalf("search timeout leaked into pooled session: %s", timeout)
	}
	var admitted bool
	if err := f.admin.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended('board-search:org_first',0))`).Scan(&admitted); err != nil || !admitted {
		t.Fatalf("tenant cap was not released: admitted=%t error=%v", admitted, err)
	}

}

func TestBoardSearchEmptyDocumentsReturnsTitleBodyMatches(t *testing.T) {
	f := newBoardSearchFixture(t)
	ctx := t.Context()
	if _, err := f.admin.ExecContext(ctx, "DELETE FROM search_documents"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.admin.ExecContext(ctx, `UPDATE native_work_item_pages SET body=$1`, "bodyuniquemarker %_\\marker Café\nfingerprint: example-key\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.admin.ExecContext(ctx, `UPDATE native_board_items SET page_title='Example ÉCOLE'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.admin.ExecContext(ctx, "UPDATE issues SET actor_principal_id='another-owner' WHERE native_id='wi_first_1'"); err != nil {
		t.Fatal(err)
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		for _, reporter := range []bool{false, true} {
			for _, query := range []struct {
				text string
				want int
			}{{"Example", 2}, {"bodyuniquemarker", 2}, {"unmatched bodyuniq", 2}, {"%_\\marker", 2}, {"CAFÉ", 2}, {"café", 2}, {"cafe", 0}, {"école", 2}, {"ÉCOLE", 2}, {"ecole", 0}, {"absentuniqueterm", 0}, {"%%_", 0}, {"2", 1}} {
				selection := viewstore.BoardSelection{ProjectID: "prj_first", SearchText: query.text, SearchRanked: true, Limit: 200, Reporter: reporter, ReporterPrincipalID: "example"}
				want := query.want
				if reporter && want == 2 {
					want = 1
				}
				err := f.repository.ReadTenant(ctx, org, corestore.Serving, func(r tenantstore.Reader) error {
					rows, err := r.BoardPage(ctx, selection)
					if err != nil {
						return err
					}
					if len(rows) != want {
						t.Errorf("tenant=%s reporter=%t query=%q rows=%d want=%d", org, reporter, query.text, len(rows), want)
					}
					for _, row := range rows {
						if row.OrganizationID != org || reporter && row.WorkItemID == "wi_first_1" {
							t.Fatal("fallback leaked an unauthorized issue")
						}
					}
					lanes, err := r.BoardLanes(ctx, selection, historyAt())
					if err == nil {
						total := int64(0)
						for _, lane := range lanes {
							total += lane.Total
						}
						if total != int64(want) {
							t.Errorf("fallback count=%d want=%d", total, want)
						}
					}
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		f.put(t, searchIssue(org, "wi_first_1"), searchstore.Text{Title: "Example"}, 0)
		f.drain(t, org)
		for _, state := range []string{"partial", "stale"} {
			if state == "stale" {
				if _, err := f.admin.ExecContext(ctx, "UPDATE search_documents SET indexed_revision=0 WHERE organization_id=$1", org); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.repository.ReadTenant(ctx, org, corestore.Serving, func(r tenantstore.Reader) error {
				rows, err := r.BoardPage(ctx, viewstore.BoardSelection{ProjectID: "prj_first", SearchText: "bodyuniquemarker", Reporter: true, ReporterPrincipalID: "example", Limit: 10})
				if err == nil && (len(rows) != 1 || rows[0].WorkItemID != "wi_first_2") {
					t.Errorf("%s index hid missing document: rows=%+v", state, rows)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
	}

}

func TestBoardSearchEmptyIndexUsesTenantIndexes(t *testing.T) {
	f := newBoardSearchFixture(t)
	ctx := t.Context()
	if _, err := f.admin.ExecContext(ctx, "DELETE FROM search_documents"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.admin.ExecContext(ctx, `UPDATE native_work_item_pages SET body='Café needlebody' WHERE work_item_id='wi_first_2'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.admin.ExecContext(ctx, `UPDATE native_board_items SET page_title='ÉCOLE' WHERE work_item_id='wi_first_2'`); err != nil {
		t.Fatal(err)
	}
	counts := new(pageWireCounts)
	measured := pageMeasuredRepository(t, tracedFeaturePool(t, f.pool, &boardQueryTracer{}), counts)
	previous := 0
	for _, scale := range []int{2, 50} {
		for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
			if _, err := f.admin.ExecContext(ctx, `INSERT INTO issues(organization_id,project_id,native_id,number,title,actor_kind,actor_principal_id,workflow_state_name)
SELECT $1,'prj_first','wi_plan_'||n,n+10,'Unrelated title','human','example','Todo' FROM generate_series($2::integer,$3::integer) n`, org, previous+1, scale*24); err != nil {
				t.Fatal(err)
			}

			if _, err := f.admin.ExecContext(ctx, `INSERT INTO native_board_items(organization_id,project_id,work_item_id,source_revision,source_event_sequence,source_project_revision,state,archived,terminal,running,dispatchable,lane_rank,priority_rank,activity_missing,activity_at,identifier,closed_at,expires_at,execution_fence,actor_principal_id,issue_type,verification_state,priority,created_at,updated_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,page_title,page_body)
SELECT organization_id,project_id,'wi_plan_'||n,source_revision,source_event_sequence,source_project_revision,state,archived,terminal,running,dispatchable,lane_rank,priority_rank,activity_missing,activity_at,'plan#'||n,closed_at,expires_at,execution_fence,actor_principal_id,issue_type,verification_state,priority,created_at,updated_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,'Unrelated title','Unrelated body'
FROM native_board_items CROSS JOIN generate_series($2::integer,$3::integer) n WHERE organization_id=$1 AND work_item_id='wi_first_1'`, org, previous+1, scale*24); err != nil {
				t.Fatal(err)
			}
			if err := f.repository.WriteTenant(ctx, org, corestore.Background, func(w tenantstore.Writer) error {
				for n := previous + 1; n <= scale*24; n++ {
					issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: "prj_first", WorkItemID: tracker.NativeWorkItemID(fmt.Sprintf("wi_plan_%d", n)), Revision: 1, Number: n + 10}, Title: "Unrelated title", Body: "Unrelated body", State: "Todo", CreatedAt: historyAt(), UpdatedAt: historyAt(), Actor: tracker.Actor{Kind: "human", PrincipalID: "example"}}
					if err := publishSearchBody(ctx, f, w, issue); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		for _, table := range []string{"native_work_item_pages", "issues", "native_board_items"} {
			if _, err := f.admin.ExecContext(ctx, "VACUUM (ANALYZE) "+table); err != nil {
				t.Fatal(err)
			}
		}
		for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
			for _, reporter := range []bool{false, true} {
				for _, query := range []string{"needlebody", "CAFÉ", "école"} {
					counts.reset()
					err := measured.ReadTenant(ctx, org, corestore.Serving, func(r tenantstore.Reader) error {
						rows, err := r.BoardPage(ctx, viewstore.BoardSelection{ProjectID: "prj_first", SearchText: query, Limit: 200, Reporter: reporter, ReporterPrincipalID: "example"})
						if err == nil && (len(rows) != 1 || rows[0].OrganizationID != org || rows[0].WorkItemID != "wi_first_2") {
							t.Errorf("indexed fallback rows=%+v", rows)
						}
						return err
					})
					if err != nil {
						t.Fatal(err)
					}
					statements, exchanges := counts.read()
					t.Logf("tenant=%s scale=%d reporter=%t query=%q statements=%d exchanges=%d", org, scale, reporter, query, statements, exchanges)
					if statements != 6 || exchanges != 5 {
						t.Fatalf("statements=%d exchanges=%d, want 6/5", statements, exchanges)
					}
				}
			}
		}
		previous = scale * 24
	}
	probe := &boardQueryTracer{}
	counted, err := postgres.NewRepository(ctx, tracedFeaturePool(t, f.pool, probe), smallPool())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := counted.Close(); err != nil {
			t.Error(err)
		}
	})
	var statement string
	if err := f.admin.QueryRowContext(ctx, `SELECT prosrc FROM pg_proc WHERE oid='public.board_search_candidates(text,text[],boolean,text,boolean)'::regprocedure`).Scan(&statement); err != nil {
		t.Fatal(err)
	}
	statement = strings.NewReplacer("$1", "$1::text", "$2", "$2::text[]", "$3", "$3::boolean", "$4", "$4::text", "$5", "$5::boolean").Replace(statement)
	for _, query := range []struct {
		text     string
		patterns []string
	}{{"needlebody", []string{"%needlebody%"}}, {"CAFÉ", []string{"%CAFÉ%", "%cafe%"}}, {"école", []string{"%école%", "%ecole%"}}} {
		for _, reporter := range []bool{false, true} {
			selection := viewstore.BoardSelection{ProjectID: "prj_first", SearchText: query.text, Limit: 200, Reporter: reporter, ReporterPrincipalID: "example"}
			if err := counted.ReadTenant(ctx, "org_first", corestore.Serving, func(r tenantstore.Reader) error {
				rows, err := r.BoardPage(ctx, selection)
				if err == nil && len(rows) != 1 {
					t.Errorf("indexed fallback rows=%d", len(rows))
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			tx, err := f.admin.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = tx.ExecContext(ctx, "SET LOCAL ROLE detent_search_reader_v2"); err != nil {
				t.Fatal(err)
			}
			if _, err = tx.ExecContext(ctx, "SELECT set_config('detent.organization_id','org_first',true)"); err != nil {
				t.Fatal(err)
			}
			var plan []byte
			err = tx.QueryRowContext(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+statement, "prj_first", query.patterns, reporter, "example", false).Scan(&plan)
			if rollbackErr := tx.Rollback(); rollbackErr != nil {
				t.Fatal(rollbackErr)
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("query=%q reporter=%t plan=%s", query.text, reporter, plan)
			for _, index := range []string{"native_work_item_search_body_unicode", "native_board_search_title_unicode"} {
				if !strings.Contains(string(plan), index) {
					t.Fatalf("fallback did not use text index %s: %s", index, plan)
				}
			}
		}
	}
}

func TestBoardTextCandidateAuthorityStaysTenantScoped(t *testing.T) {
	f := newBoardSearchFixture(t)
	ctx := t.Context()
	if _, err := f.admin.ExecContext(ctx, "UPDATE native_work_item_pages SET body='foreignsentinel' WHERE organization_id='org_second'"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.admin.ExecContext(ctx, "UPDATE issues SET actor_principal_id='another-owner' WHERE organization_id='org_first' AND native_id='wi_first_1'"); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := f.pool.QueryRow(pgtest.UnboundProbe(ctx), "SELECT count(*) FROM public.board_search_candidates('prj_first',ARRAY['%abc%'],false,'',false)").Scan(&count); err != nil || count != 0 {
		t.Fatalf("unbound candidates=%d error=%v", count, err)
	}
	runtime, err := postgres.NewRuntime(ctx, f.pool, f.pool)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := runtime.Begin(ctx, "org_first", pgtransport.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		pattern   string
		reporter  bool
		principal string
		want      int
	}{{"%abc%", false, "", 2}, {"%abc%", true, "example", 1}, {"%abc%", true, "", 0}, {"%abc%", true, "missing-owner", 0}, {"%foreignsentinel%", false, "", 0}} {
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM public.board_search_candidates('prj_first',$1,$2,$3,false)", []string{test.pattern}, test.reporter, test.principal).Scan(&count); err != nil || count != test.want {
			t.Errorf("candidate authority=%+v count=%d error=%v", test, count, err)
		}
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"SELECT body FROM public.native_work_item_pages LIMIT 1", "SELECT * FROM public.board_search_candidates('prj_first',ARRAY['%abc%'],false,'',false)"} {
		probe, err := f.admin.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := probe.ExecContext(ctx, "SET LOCAL ROLE detent_platform_v2"); err != nil {
			t.Fatal(err)
		}
		_, err = probe.ExecContext(ctx, query)
		var denied *pgconn.PgError
		if !errors.As(err, &denied) || denied.Code != "42501" {
			t.Errorf("platform gained search authority: error=%v", err)
		}
		if err := probe.Rollback(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.admin.ExecContext(ctx, "ALTER FUNCTION public.board_search_candidates(text,text[],boolean,text,boolean) SECURITY INVOKER"); err != nil {
		t.Fatal(err)
	}
	_, roleErr := postgres.NewRuntime(ctx, f.pool, f.pool)
	if _, err := f.admin.ExecContext(ctx, "ALTER FUNCTION public.board_search_candidates(text,text[],boolean,text,boolean) SECURITY DEFINER"); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(roleErr, postgres.ErrRuntimeRole) {
		t.Fatalf("unsafe helper accepted: %v", roleErr)
	}
}
