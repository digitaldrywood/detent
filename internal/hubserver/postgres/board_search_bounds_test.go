package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestBoardSearchBoundsHostileReporterAcrossTenants(t *testing.T) {
	admin := template.Database(t)
	repository, pool := repositoryFor(t, admin, postgres.PoolConfig{MaxConnections: 4, ServingConnections: 2, BackgroundConnections: 1})
	objects := newHistoryObjects()
	f := searchFixture{repository: repository, pool: pool, admin: admin, objects: objects, content: searchstore.NewContent(objects)}
	ctx := t.Context()
	if _, err := f.admin.ExecContext(ctx, `DO $$ BEGIN
	 IF to_regprocedure('public.mark_integration_page_dirty()') IS NOT NULL THEN
	  ALTER FUNCTION public.mark_integration_page_dirty() SET search_path=pg_catalog,public;
	 END IF;
	END $$`); err != nil {
		t.Fatal(err)
	}
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
		for _, id := range []tracker.NativeWorkItemID{"wi_first_1", "wi_first_2"} {
			f.put(t, searchIssue(org, string(id)), searchstore.Text{Title: "Example", Body: "abcabcabcabcabc"}, 0)
			ref, err := f.objects.StoreDetail(ctx, string(org), []byte(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			err = f.repository.WriteTenant(ctx, org, corestore.Background, func(w tenantstore.Writer) error {
				row := viewstore.BoardRecord{OrganizationID: org, ProjectID: "prj_first", WorkItemID: id, SourceRevision: 1, ActorPrincipalID: "example", Position: viewstore.BoardPosition{State: "Todo", Identifier: string(id)}, CreatedAt: historyAt(), UpdatedAt: historyAt(), Payload: ref}
				return w.ReplaceBoardItem(ctx, viewstore.BoardUpdate{ProjectID: "prj_first", WorkItemID: id, ExpectedRevision: 1, Current: &row})
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		f.drain(t, org)
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
	if _, err := f.admin.ExecContext(ctx, `UPDATE search_postings SET literal_positions=ARRAY(SELECT generate_series(1,80000,2)::bigint) WHERE organization_id='org_first' AND document_id='issue/wi_first_1' AND cardinality(literal_positions)>0`); err != nil {
		t.Fatal(err)
	}
	t.Run("hostile positional expansion", func(t *testing.T) {
		bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		hostile := selection
		hostile.SearchText = "cabcab"
		started := time.Now()
		err := f.repository.ReadTenant(bounded, "org_first", corestore.Serving, func(r tenantstore.Reader) error {
			_, err := r.BoardPage(bounded, hostile)
			return err
		})
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "57014" || !strings.Contains(pgErr.Message, "statement timeout") {
			t.Fatalf("hostile search escaped server budget: %v", err)
		}
		if elapsed := time.Since(started); elapsed > time.Second {
			t.Fatalf("hostile search exceeded read budget: %v", elapsed)
		}
	})
	t.Run("sequential searches share the first deadline", func(t *testing.T) {
		first, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		defer cancel()
		started := time.Now()
		err := f.repository.ReadTenant(ctx, "org_first", corestore.Serving, func(r tenantstore.Reader) error {
			if _, err := r.BoardPage(first, selection); err != nil {
				return err
			}
			hostile := selection
			hostile.SearchText = "cabcab"
			_, err := r.BoardPage(ctx, hostile)
			return err
		})
		if err == nil || time.Since(started) > 500*time.Millisecond {
			t.Fatalf("second search reset first read budget: %v", err)
		}
	})
	for _, path := range []string{"page", "model", "lanes", "closed"} {
		t.Run(path, func(t *testing.T) {
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
			started := time.Now()
			err = f.repository.ReadTenant(bounded, "org_first", corestore.Serving, func(r tenantstore.Reader) error {
				switch path {
				case "page":
					_, err = r.BoardPage(bounded, selection)
				case "model":
					_, err = r.(interface {
						BoardModelPages(context.Context, viewstore.BoardSelection, *viewstore.BoardSelection, accessstore.Authority) (viewstore.BoardModelPage, error)
					}).BoardModelPages(bounded, selection, nil, accessstore.Authority{})
				case "lanes":
					_, err = r.BoardLanes(bounded, selection, historyAt())
				case "closed":
					_, err = r.BoardClosedWindows(bounded, selection, []time.Time{historyAt().Add(-time.Hour)}, historyAt())
				}
				return err
			})
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "57014" || !strings.Contains(pgErr.Message, "statement timeout") {
				t.Fatalf("search did not enforce a server statement timeout: %v", err)
			}
			if elapsed := time.Since(started); elapsed > time.Second {
				t.Fatalf("search exceeded read budget: %v", elapsed)
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
