package postgres_test

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"testing"
	"testing/fstest"

	"github.com/pressly/goose/v3"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPageBackfillResumesWithoutSourceWriteLocks(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedTenantAccess(t, repository, org)
		seedRepository(t, repository, org)
		if _, err := admin.ExecContext(t.Context(), `DELETE FROM native_work_item_page_dirty WHERE organization_id=$1`, org); err != nil {
			t.Fatal(err)
		}
		interrupted := errors.New("interrupt backfill transaction")
		if err := repository.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
			if _, err := w.(interface {
				BackfillWorkItemPages(context.Context) (bool, error)
			}).BackfillWorkItemPages(t.Context()); err != nil {
				return err
			}
			return interrupted
		}); !errors.Is(err, interrupted) {
			t.Fatalf("interrupted backfill: %v", err)
		}
		var queuedAfterRollback, advancedAfterRollback int
		if err := admin.QueryRowContext(t.Context(), `SELECT
 (SELECT count(*) FROM native_work_item_page_dirty WHERE organization_id=$1),
 (SELECT count(*) FROM native_work_item_page_progress WHERE organization_id=$1 AND (backfill_phase<>0 OR backfill_issue_id<>0 OR backfill_comment_id<>''))`, org).Scan(&queuedAfterRollback, &advancedAfterRollback); err != nil {
			t.Fatal(err)
		}
		if queuedAfterRollback != 0 || advancedAfterRollback != 0 {
			t.Fatalf("interrupted backfill persisted queue=%d checkpoints=%d", queuedAfterRollback, advancedAfterRollback)
		}
		for step := 0; step < 14; step++ {
			done := false
			if err := repository.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
				backfill, ok := w.(interface {
					BackfillWorkItemPages(context.Context) (bool, error)
				})
				if !ok {
					t.Fatal("background page backfill is missing")
				}
				var err error
				done, err = backfill.BackfillWorkItemPages(t.Context())
				if err != nil {
					return err
				}
				tx, err := admin.BeginTx(t.Context(), nil)
				if err != nil {
					return err
				}
				defer tx.Rollback()
				if _, err := tx.ExecContext(t.Context(), `LOCK TABLE issues,native_comments IN ROW EXCLUSIVE MODE NOWAIT`); err != nil {
					t.Fatalf("backfill blocks source writers: %v", err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			var queued int
			if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM native_work_item_page_dirty WHERE organization_id=$1`, org).Scan(&queued); err != nil {
				t.Fatal(err)
			}
			if queued > (step+1)*2 {
				t.Fatalf("step %d queued unbounded rows: %d", step, queued)
			}
			if done {
				if queued != 16 {
					t.Fatalf("replayed seed rows=%d, want four items, four relationship heads and eight comments", queued)
				}
				var invalidPairs int
				if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM native_work_item_page_dirty item
 JOIN native_work_item_page_dirty relations USING(organization_id,project_id,work_item_id,record_id)
 WHERE item.organization_id=$1 AND item.kind='item' AND relations.kind='relationships'
 AND (item.generation>=relations.generation OR NOT relations.propagate)`, org).Scan(&invalidPairs); err != nil {
					t.Fatal(err)
				}
				if invalidPairs != 0 {
					t.Fatalf("backfill lost body-before-relationship ordering or propagation: %d", invalidPairs)
				}
				if err := repository.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
					complete, err := w.(interface {
						BackfillWorkItemPages(context.Context) (bool, error)
					}).BackfillWorkItemPages(t.Context())
					if !complete {
						t.Error("completed backfill restarted")
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				break
			}
			if step == 13 {
				t.Fatal("backfill did not finish")
			}
		}
	}
	data, err := os.ReadFile("migrations/20261106233256_work_item_page_backfill.sql")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, admin, fstest.MapFS{"20261106233256_work_item_page_backfill.sql": &fstest.MapFile{Data: data}}, goose.WithDisableGlobalRegistry(true), goose.WithDisableVersioning(true), goose.WithSlog(slog.New(slog.DiscardHandler)))
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := provider.Up(t.Context()); err != nil {
			t.Fatalf("rerun concurrent installation: %v", err)
		}
	}
	var complete int
	if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM native_work_item_page_progress WHERE backfill_phase=2`).Scan(&complete); err != nil {
		t.Fatal(err)
	}
	if complete != 2 {
		t.Fatalf("rerun reset tenant checkpoints: %d", complete)
	}
	var valid bool
	if err := admin.QueryRowContext(t.Context(), `SELECT indisvalid FROM pg_index WHERE indexrelid='native_work_item_page_dirty_pending'::regclass`).Scan(&valid); err != nil {
		t.Fatal(err)
	}
	if !valid {
		t.Fatal("concurrent installation left invalid index")
	}
}

func backfillPageFixture(t *testing.T, repository tenantstore.Repository, org tracker.OrganizationID) {
	t.Helper()
	for step := 0; step < 1000; step++ {
		complete := false
		if err := repository.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
			var err error
			complete, err = w.(interface {
				BackfillWorkItemPages(context.Context) (bool, error)
			}).BackfillWorkItemPages(t.Context())
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if complete {
			return
		}
	}
	t.Fatal("fixture backfill did not complete")
}
