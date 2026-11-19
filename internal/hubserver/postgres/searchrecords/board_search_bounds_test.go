package searchrecords_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func newBoardSearchFixture(t *testing.T) searchFixture {
	t.Helper()
	admin := pgstoretest.Template.Database(t)
	repository, pool := pgstoretest.RepositoryFor(t, admin, postgres.PoolConfig{MaxConnections: 4, ServingConnections: 2, BackgroundConnections: 1})
	objects := pgstoretest.NewHistoryObjects()
	f := searchFixture{repository: repository, pool: pool, admin: admin, objects: objects}

	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		pgstoretest.SeedTenantAccess(t, repository, org)
		seedBoardSearchProject(t, f, org, "prj_first", "example", []tracker.NativeWorkItemID{"wi_first_1", "wi_first_2"})
	}
	return f
}

func seedBoardSearchProject(t *testing.T, f searchFixture, org tracker.OrganizationID, project tracker.ProjectID, principal string, ids []tracker.NativeWorkItemID) {
	t.Helper()
	ctx := t.Context()
	if err := f.repository.WriteTenant(ctx, org, corestore.Background, func(w tenantstore.Writer) error {
		_, err := w.CreateProjectRecord(ctx, projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: project, Name: "Example " + string(project), Profile: "native"}, States: []tracker.NativeState{{Name: "Todo", Dispatchable: true}}})
		if err != nil {
			return err
		}
		for index, id := range ids {
			if _, err := w.CreateWorkItem(ctx, corestore.WorkItem{OrganizationID: org, ProjectID: project, NativeID: id, Number: int64(index + 1), Title: "Example", ActorKind: new("human"), ActorPrincipalID: new(principal), WorkflowStateName: new("Todo")}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for index, id := range ids {
		ref, err := f.objects.StoreDetail(ctx, string(org), []byte(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		err = f.repository.WriteTenant(ctx, org, corestore.Background, func(w tenantstore.Writer) error {
			issue := &tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: project, WorkItemID: id, Revision: 1, Number: index + 1}, Title: "Example", State: "Todo", CreatedAt: pgstoretest.HistoryAt(), UpdatedAt: pgstoretest.HistoryAt(), Actor: tracker.Actor{Kind: "human", PrincipalID: principal}}
			row := viewstore.BoardRecord{OrganizationID: org, ProjectID: project, WorkItemID: id, SourceRevision: 1, ActorPrincipalID: principal, Position: viewstore.BoardPosition{State: "Todo", Identifier: string(id)}, CreatedAt: pgstoretest.HistoryAt(), UpdatedAt: pgstoretest.HistoryAt(), Payload: ref, PageIssue: issue, ListIssue: issue, ReporterRelations: &viewstore.BoardReporterRelations{}, Display: viewstore.BoardDisplay{Ready: true, Exact: true, Title: issue.Title, Actor: issue.Actor}}
			if err := w.ReplaceBoardItem(ctx, viewstore.BoardUpdate{ProjectID: project, WorkItemID: id, ExpectedRevision: 1, Current: &row}); err != nil {
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
}

func publishSearchBody(ctx context.Context, f searchFixture, w tenantstore.Writer, issue tracker.NativeIssue) error {
	candidate := viewstore.PageCandidate{OrganizationID: issue.OrganizationID, ProjectID: issue.ProjectID, WorkItemID: issue.WorkItemID, Kind: "item"}
	if err := f.admin.QueryRowContext(ctx, "SELECT record_id,generation,source_at FROM native_work_item_page_dirty WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3 AND kind='item'", issue.OrganizationID, issue.ProjectID, issue.WorkItemID).Scan(&candidate.RecordID, &candidate.Generation, &candidate.SourceAt); err != nil {
		return err
	}
	return w.(viewstore.WorkItemPageWriter).PublishWorkItemPage(ctx, candidate, &viewstore.ItemPageRecord{Value: issue, Generation: candidate.Generation, SourceAt: candidate.SourceAt, ProjectedAt: pgstoretest.HistoryAt()}, nil)
}

func republishSearchText(t *testing.T, f searchFixture, id tracker.NativeWorkItemID, number int, title, body string) {
	t.Helper()
	ctx := t.Context()
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if _, err := f.admin.ExecContext(ctx, "INSERT INTO native_work_item_page_dirty(organization_id,project_id,work_item_id,record_id,kind) VALUES($1,'prj_first',$2,$2,'item') ON CONFLICT DO NOTHING", org, id); err != nil {
			t.Fatal(err)
		}
		if err := f.repository.WriteTenant(ctx, org, corestore.Background, func(w tenantstore.Writer) error {
			issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: "prj_first", WorkItemID: id, Revision: 1, Number: number}, Title: title, Body: body, State: "Todo", CreatedAt: pgstoretest.HistoryAt(), UpdatedAt: pgstoretest.HistoryAt(), Actor: tracker.Actor{Kind: "human", PrincipalID: "example"}}
			return publishSearchBody(ctx, f, w, issue)
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBoardSearchBoundsHostileReporterAcrossTenants(t *testing.T) {
	f := newBoardSearchFixture(t)
	ctx := t.Context()
	timedOut := func(err error) bool {
		var pgErr *pgconn.PgError
		return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &pgErr) && pgErr.Code == "57014" && strings.Contains(pgErr.Message, "statement timeout"))
	}
	settleSearch := func(t *testing.T) {
		t.Helper()
		if _, err := f.admin.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('board-search:"org_first":"example"',0))`); err != nil {
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
					_, err = r.BoardLanes(ctx, selection, pgstoretest.HistoryAt())
				case "closed":
					_, err = r.BoardClosedWindows(ctx, selection, []time.Time{pgstoretest.HistoryAt().Add(-time.Hour)}, pgstoretest.HistoryAt())
				}
				return err
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
			if _, err := blocker.ExecContext(ctx, "LOCK TABLE native_work_item_search IN ACCESS EXCLUSIVE MODE"); err != nil {
				t.Fatal(err)
			}
			bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			err = f.repository.ReadTenant(bounded, "org_first", corestore.Serving, func(r tenantstore.Reader) error {
				return read(bounded, r)
			})
			if !timedOut(err) || !errors.Is(err, searchstore.ErrBusy) {
				t.Fatalf("search did not enforce a server statement timeout as a busy search: %v", err)
			}
		})
	}
	t.Run("caller deadlines and cancellation are not busy searches", func(t *testing.T) {
		config := pgstoretest.SmallPool()
		config.SearchBudget = time.Hour
		patient, err := postgres.NewRepository(ctx, f.pool, config)
		if err != nil {
			t.Fatal(err)
		}
		settleSearch(t)
		blocker, err := f.admin.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := blocker.Rollback(); err != nil {
				t.Error(err)
			}
		}()
		if _, err := blocker.ExecContext(ctx, "LOCK TABLE native_work_item_search IN ACCESS EXCLUSIVE MODE"); err != nil {
			t.Fatal(err)
		}
		caller, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
		defer cancel()
		err = patient.ReadTenant(caller, "org_first", corestore.Serving, func(r tenantstore.Reader) error { _, err := r.BoardPage(caller, selection); return err })
		if !timedOut(err) || errors.Is(err, searchstore.ErrBusy) {
			t.Fatalf("caller deadline error=%v, want a timeout that is not a busy search", err)
		}
		finished := make(chan error, 1)
		go func() {
			finished <- patient.ReadTenant(ctx, "org_first", corestore.Serving, func(r tenantstore.Reader) error { _, err := r.BoardPage(ctx, selection); return err })
		}()
		testenv.WaitUntil(t, ctx, func() error {
			var cancelled bool
			if err := blocker.QueryRowContext(ctx, `SELECT coalesce(bool_or(pg_cancel_backend(pid)),false) FROM pg_stat_activity WHERE pid<>pg_backend_pid() AND wait_event_type='Lock' AND query LIKE '%FROM native_work_item_search%'`).Scan(&cancelled); err != nil {
				return err
			}
			if !cancelled {
				return errors.New("search is not waiting on the table lock yet")
			}
			return nil
		})
		err = <-finished
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "57014" || errors.Is(err, searchstore.ErrBusy) {
			t.Fatalf("administrative cancellation error=%v, want SQLSTATE 57014 that is not a busy search", err)
		}
	})
	t.Run("principal concurrency cap", func(t *testing.T) {
		held, err := f.admin.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := held.Rollback(); err != nil {
				t.Error(err)
			}
		}()
		if _, err := held.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, `board-search:"org_first":"example"`); err != nil {
			t.Fatal(err)
		}
		err = f.repository.ReadTenant(ctx, "org_first", corestore.Serving, func(r tenantstore.Reader) error { _, err := r.BoardPage(ctx, selection); return err })
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("saturated principal search error=%v", err)
		}
		err = f.repository.ReadTenant(ctx, "org_first", corestore.Serving, func(r tenantstore.Reader) error {
			_, err := r.(interface {
				BoardModelPages(context.Context, viewstore.BoardSelection, *viewstore.BoardSelection, accessstore.Authority) (viewstore.BoardModelPage, error)
			}).BoardModelPages(ctx, selection, nil, accessstore.Authority{})
			return err
		})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("saturated principal model error=%v", err)
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
		visible.SearchText, visible.SearchRanked, visible.Limit = "abcab", ranked, 200
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
	if err := f.admin.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended('board-search:"org_first":"example"',0))`).Scan(&admitted); err != nil || !admitted {
		t.Fatalf("principal cap was not released: admitted=%t error=%v", admitted, err)
	}

}

func TestBoardSearchMatchesTitleAndBodyWords(t *testing.T) {
	t.Parallel()
	f := newBoardSearchFixture(t)
	ctx := t.Context()
	for index, id := range []tracker.NativeWorkItemID{"wi_first_1", "wi_first_2"} {
		republishSearchText(t, f, id, index+1, "Example ÉCOLE", "bodyuniquemarker %_\\marker Café magazine counts reasons\nfingerprint: example-key\n")
	}
	if _, err := f.admin.ExecContext(ctx, "UPDATE issues SET actor_principal_id='another-owner' WHERE native_id='wi_first_1'"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.admin.ExecContext(ctx, `INSERT INTO native_work_item_fingerprints(organization_id,project_id,work_item_id,kind,record_id,fingerprint_digest,is_origin,origin_kind)
 SELECT organization_id,project_id,work_item_id,'item',work_item_id,$1,true,'worker' FROM native_work_item_pages WHERE project_id='prj_first' ON CONFLICT DO NOTHING`, searchstore.FingerprintDigest("example-key")); err != nil {
		t.Fatal(err)
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		for _, reporter := range []bool{false, true} {
			for _, query := range []struct {
				text string
				want int
			}{{"Example", 2}, {"bodyuniquemarker", 2}, {"unmatched bodyuniq", 2}, {"magazine count reason code", 2}, {"CAFÉ", 2}, {"café", 2}, {"école", 2}, {"ÉCOLE", 2}, {"exam", 2}, {"corns", 0}, {"uniquemarker", 0}, {"absentuniqueterm", 0}, {"the and of", 0}, {"%%_", 0}, {"\x00", 0}, {"2", 1}} {
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
							t.Fatal("search leaked an unauthorized issue")
						}
					}
					selection.SearchRanked = false
					lanes, err := r.BoardLanes(ctx, selection, pgstoretest.HistoryAt())
					if err == nil {
						total := int64(0)
						for _, lane := range lanes {
							total += lane.Total
						}
						if total != int64(want) {
							t.Errorf("search count=%d want=%d", total, want)
						}
					}
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			err := f.repository.ReadTenant(ctx, org, corestore.Serving, func(r tenantstore.Reader) error {
				rows, err := r.BoardPage(ctx, viewstore.BoardSelection{ProjectID: "prj_first", Fingerprint: "example-key", Limit: 200, Reporter: reporter, ReporterPrincipalID: "example"})
				want := 2
				if reporter {
					want = 1
				}
				if err == nil && len(rows) != want {
					t.Errorf("tenant=%s reporter=%t fingerprint rows=%d want=%d", org, reporter, len(rows), want)
				}
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	republishSearchText(t, f, "wi_first_1", 1, "Overlapmarker example", strings.Repeat("overlapmarker ", 50))
	republishSearchText(t, f, "wi_first_2", 2, "Overlapmarker example", "Unrelated description")
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := f.repository.ReadTenant(ctx, org, corestore.Serving, func(r tenantstore.Reader) error {
			rows, err := r.BoardPage(ctx, viewstore.BoardSelection{ProjectID: "prj_first", SearchText: "overlapmarker", SearchRanked: true, Limit: 10})
			if err == nil && (len(rows) != 2 || rows[0].WorkItemID != "wi_first_2" || rows[0].Position.SearchRank != rows[1].Position.SearchRank) {
				t.Errorf("tenant %s description repeats of a title word changed its rank: %+v", org, rows)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	republishSearchText(t, f, "wi_first_1", 1, "Rankingmarker example", "Unrelated description")
	republishSearchText(t, f, "wi_first_2", 2, "Example", strings.Repeat("rankingmarker ", 50))
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := f.repository.ReadTenant(ctx, org, corestore.Serving, func(r tenantstore.Reader) error {
			rows, err := r.BoardPage(ctx, viewstore.BoardSelection{ProjectID: "prj_first", SearchText: "rankingmark", SearchRanked: true, Limit: 10})
			if err != nil {
				return err
			}
			if len(rows) != 2 || rows[0].WorkItemID != "wi_first_1" || rows[0].Position.SearchRank <= rows[1].Position.SearchRank {
				t.Errorf("tenant %s title match did not outrank a repeated description match: %+v", org, rows)
			}
			if _, err := r.BoardPage(ctx, viewstore.BoardSelection{ProjectID: "prj_first", SearchText: "rankingmark", SearchRanked: true, WorkOrder: true, Limit: 10, After: &rows[0].Position}); !errors.Is(err, corestore.ErrInvalidPage) {
				t.Errorf("tenant %s work-order search accepted a continuation cursor: %v", org, err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tenant := range []string{"", "org_first"} {
		tx, err := f.admin.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, "SET LOCAL ROLE detent_runtime_v2"); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, "SELECT set_config('detent.organization_id',$1,true)", tenant); err != nil {
			t.Fatal(err)
		}
		var visible, foreign int
		if err := tx.QueryRowContext(ctx, "SELECT count(*),count(*) FILTER (WHERE organization_id<>$1) FROM native_work_item_search", tenant).Scan(&visible, &foreign); err != nil {
			t.Fatal(err)
		}
		_, insertErr := tx.ExecContext(ctx, "INSERT INTO native_work_item_search(organization_id,project_id,work_item_id,number,terms) VALUES('org_second','prj_first','wi_foreign',99,'example'::tsvector)")
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
		if want := map[string]int{"": 0, "org_first": 2}[tenant]; visible != want || foreign != 0 || insertErr == nil {
			t.Fatalf("tenant %q saw %d search rows (%d foreign), foreign insert error=%v", tenant, visible, foreign, insertErr)
		}
	}
}
