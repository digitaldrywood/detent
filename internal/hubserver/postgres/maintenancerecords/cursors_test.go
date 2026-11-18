package maintenancerecords_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestMain(m *testing.M) { pgstoretest.Main(m) }

func TestHostedMaintenanceCursorPersistence(t *testing.T) {
	t.Run("registry revocation fences an admitted write", func(t *testing.T) {
		ctx, admin, _, ids, views := setupCursorViews(t)
		org := ids[0]
		barrier, err := admin.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := barrier.ExecContext(ctx, "LOCK TABLE hosted_maintenance_cursors IN SHARE MODE"); err != nil {
			t.Fatal(errors.Join(err, barrier.Rollback()))
		}
		done := make(chan struct{})
		var writeErr error
		go func() {
			defer close(done)
			writeErr = views[0].SetMaintenanceCursor(ctx, org, 1, "example", postgres.MaintenanceCursor{ID: 1})
		}()
		defer func() {
			if err := barrier.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
				t.Error(err)
			}
			<-done
		}()
		testenv.WaitUntil(t, ctx, func() error {
			select {
			case <-done:
				t.Fatalf("cursor write exited before deletion: %v", writeErr)
			default:
			}
			var waiting bool
			if err := barrier.QueryRowContext(ctx, `SELECT EXISTS(SELECT FROM pg_locks WHERE relation='hosted_maintenance_cursors'::regclass AND NOT granted AND pg_backend_pid()=ANY(pg_blocking_pids(pid)))`).Scan(&waiting); err != nil {
				t.Fatal(err)
			}
			if !waiting {
				return errors.New("waiting for the admitted cursor insert")
			}
			return nil
		})
		if _, err := barrier.ExecContext(ctx, "UPDATE platform.organizations SET managed=true,state='deleting' WHERE id=$1", org); err != nil {
			t.Fatal(err)
		}
		if err := barrier.Commit(); err != nil {
			t.Fatal(err)
		}
		<-done
		if !errors.Is(writeErr, corestore.ErrRevisionConflict) {
			t.Errorf("revoked cursor write returned %v", writeErr)
		}
		cursor, err := views[0].MaintenanceCursor(ctx, org, 1, "example")
		if !errors.Is(err, corestore.ErrRevisionConflict) || cursor.ID != 0 {
			t.Fatalf("revoked cursor persisted %d, error %v", cursor.ID, err)
		}
		if err := views[1].SetMaintenanceCursor(ctx, ids[1], 1, "example", postgres.MaintenanceCursor{ID: 2}); err != nil {
			t.Fatal(err)
		}
		if err := views[0].ReadBoardRequest(ctx, org, "fixture", func(context.Context) error {
			t.Fatal("retired view entered board request")
			return nil
		}); !errors.Is(err, corestore.ErrRevisionConflict) {
			t.Fatalf("retired board request: %v", err)
		}
		if _, err := admin.ExecContext(ctx, "UPDATE platform.organizations SET managed=false,state='ready' WHERE id=$1", org); err != nil {
			t.Fatal(err)
		}
		if cursor, err := views[0].MaintenanceCursor(ctx, org, 1, "example"); err != nil || cursor.ID != 0 {
			t.Fatalf("restored view read interrupted write: %+v %v", cursor, err)
		}
	})
	t.Run("ready generation changes before request binding", func(t *testing.T) {
		ctx, admin, _, ids, views := setupCursorViews(t)
		org := ids[0]
		if _, err := admin.ExecContext(ctx, "UPDATE platform.organizations SET generation=2 WHERE id=$1", org); err != nil {
			t.Fatal(err)
		}
		for i, view := range views {
			called := false
			err := view.ReadRequest(ctx, ids[i], func(context.Context) error {
				called = true
				return nil
			})
			if i == 0 && (!errors.Is(err, corestore.ErrRevisionConflict) || called) || i == 1 && (err != nil || !called) {
				t.Fatalf("request callback=%t error=%v", called, err)
			}
		}
	})
	t.Run("health cursor survives view retirement", func(t *testing.T) {
		ctx, _, repository, ids, views := setupCursorViews(t)
		for i, org := range ids {
			cursor := postgres.HealthScan{ItemProject: "prj_same", ItemID: int64(i + 17), Conversation: "conversation_" + string(org), StartedAt: time.Date(2026, 1, 5, 10, 20, 30, 0, time.UTC), Completed: [5]bool{true, false, true}}
			writer := views[i]
			if err := writer.SetHealthScan(ctx, org, 1, cursor); err != nil {
				t.Fatal(err)
			}
			if err := writer.SetHealthScan(ctx, org, 2, postgres.HealthScan{}); !errors.Is(err, corestore.ErrRevisionConflict) {
				t.Fatalf("unregistered generation wrote cursor: %v", err)
			}
			if _, err := writer.HealthScan(ctx, org, 2); !errors.Is(err, corestore.ErrRevisionConflict) {
				t.Fatalf("unregistered generation read cursor: %v", err)
			}
			reopened, err := repository.ForOrganization(org, 1)
			if err != nil {
				t.Fatal(err)
			}
			saved, err := reopened.HealthScan(ctx, org, 1)
			if err := errors.Join(err, reopened.Close()); err != nil {
				t.Fatal(err)
			}
			if !saved.StartedAt.Equal(cursor.StartedAt) {
				t.Fatalf("cursor start = %s, want %s", saved.StartedAt, cursor.StartedAt)
			}
			saved.StartedAt = cursor.StartedAt
			if saved != cursor {
				t.Fatalf("reopened %s cursor = %+v, want %+v", org, saved, cursor)
			}
		}
	})
}

func setupCursorViews(t *testing.T) (context.Context, *sql.DB, *postgres.Repository, []tracker.OrganizationID, []*postgres.Repository) {
	t.Helper()
	ctx := t.Context()
	admin := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	ids := []tracker.OrganizationID{"org_first", "org_second"}
	var views []*postgres.Repository
	for _, org := range ids {
		if err := repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(ctx, accessstore.Organization{OrganizationID: org, Name: string(org), CreatedAt: pgstoretest.HistoryAt()}); err != nil {
				return err
			}
			return w.CreateAccessTenant(ctx, accessstore.Tenant{OrganizationID: org, ProviderID: "provider_" + string(org), Deployment: "shared", AllocationGeneration: 1})
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(ctx, `INSERT INTO platform.organizations(id,provider_id,name,state,endpoint,generation,created_at,updated_at) VALUES ($1,'provider_'||$1,$1,'ready',$1,1,now(),now())`, org); err != nil {
			t.Fatal(err)
		}
		view, err := repository.ForOrganization(org, 1)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := view.Close(); err != nil {
				t.Error(err)
			}
		})
		views = append(views, view)
	}
	return ctx, admin, repository, ids, views
}
