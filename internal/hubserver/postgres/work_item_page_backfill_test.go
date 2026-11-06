package postgres_test

import (
	"context"
	"testing"

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
}
