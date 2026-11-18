package searchrecords_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestBoardMaintenanceDoesNotDeadlockItemWriterBoardDelta(t *testing.T) {
	t.Parallel()
	admin := pgstoretest.Template.Database(t)
	original, pool := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	if err := original.Close(); err != nil {
		t.Fatal(err)
	}
	barrier := &itemLockBarrier{}
	traced := pgstoretest.TracedFeaturePool(t, pool, barrier)
	repository, err := postgres.NewRepository(t.Context(), traced, pgstoretest.SmallPool())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
	var login string
	if err := traced.QueryRow(t.Context(), `SELECT session_user`).Scan(&login); err != nil {
		t.Fatal(err)
	}
	objects := pgstoretest.NewHistoryObjects()
	events, boards := eventstore.NewContent(objects), viewstore.NewContent(objects)
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		t.Run(string(org), func(t *testing.T) {
			pgstoretest.SeedTenantAccess(t, repository, org)
			var item corestore.WorkItem
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: "prj_example", Name: "Example", Profile: "native"}, States: []tracker.NativeState{{Name: "Todo", Dispatchable: true}, {Name: "Done", Terminal: true}}}); err != nil {
					return err
				}
				var err error
				item, err = w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: "prj_example", NativeID: "wi_example", Number: 1, Title: "Example", ActorKind: new("human"), ActorPrincipalID: new("example"), WorkflowStateName: new("Todo")})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			card := tracker.NativeBoardCard{Issue: tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: "prj_example", WorkItemID: "wi_example", Revision: tracker.Revision(item.Revision), Number: 1}, Title: "Example", State: "Todo", Actor: tracker.Actor{Kind: "human", PrincipalID: "example"}, CreatedAt: now, UpdatedAt: now}}
			ref, err := boards.PrepareBoard(t.Context(), card)
			if err != nil {
				t.Fatal(err)
			}
			record := viewstore.BoardRecord{OrganizationID: org, ProjectID: "prj_example", WorkItemID: "wi_example", SourceRevision: item.Revision, SourceEventSequence: item.EventSequence, Position: viewstore.BoardPosition{Lane: 1, State: "Todo", Activity: &now, Identifier: "prj_example#1"}, ActorPrincipalID: "example", CreatedAt: now, UpdatedAt: now, Payload: ref}
			event, err := events.PrepareEvent(t.Context(), org, pgstoretest.HistoryJSON(t, tracker.CollaborationEvent{ID: "event_lock_order", OrganizationID: org, ProjectID: "prj_example", AggregateType: "issue", AggregateID: "wi_example", Type: "issue.edited", SchemaVersion: 1, Actor: pgstoretest.HistoryActor(), RecordedAt: now, Data: tracker.CollaborationData{Revision: tracker.Revision(item.Revision), Fields: []string{"title"}}}))
			if err != nil {
				t.Fatal(err)
			}
			event.Sequence = 0

			itemLocked, release := barrier.arm()
			var releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(release) })
			writer := pgstoretest.StartRepositoryTask(t, func(ctx context.Context) error {
				return repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
					_, err := w.AppendEvent(ctx, event)
					return err
				})
			})
			pgstoretest.WaitRepositorySignal(t, itemLocked)
			var writerPID int
			if err := admin.QueryRowContext(t.Context(), `SELECT pid FROM pg_stat_activity WHERE datname=current_database() AND usename=$1 AND state='idle in transaction'`, login).Scan(&writerPID); err != nil {
				t.Fatal(err)
			}
			maintenance := pgstoretest.StartRepositoryTask(t, func(ctx context.Context) error {
				return repository.WriteTenant(ctx, org, corestore.Background, func(w tenantstore.Writer) error {
					return w.ReplaceBoardItem(ctx, viewstore.BoardUpdate{ProjectID: "prj_example", WorkItemID: "wi_example", ExpectedRevision: item.Revision, ExpectedEventSequence: item.EventSequence, Current: &record})
				})
			})
			testenv.WaitUntil(t, t.Context(), func() error {
				select {
				case <-maintenance.Done:
					return fmt.Errorf("board maintenance ended before waiting on the item: %v", maintenance.Err)
				default:
				}
				err := admin.QueryRowContext(t.Context(), `SELECT a.pid FROM pg_stat_activity a JOIN pg_locks l ON l.pid=a.pid AND l.locktype='tuple' AND l.relation='issues'::regclass WHERE a.datname=current_database() AND a.usename=$1 AND a.wait_event_type='Lock' AND $2=ANY(pg_blocking_pids(a.pid))`, login, writerPID).Scan(new(int))
				if err != nil {
					return fmt.Errorf("board maintenance is not waiting on the item locked by the writer: %w", err)
				}
				return nil
			})
			for _, probe := range []struct {
				mode    string
				blocked bool
			}{
				{mode: "FOR KEY SHARE", blocked: false},
				{mode: "FOR SHARE", blocked: true},
			} {
				var id string
				err := admin.QueryRowContext(t.Context(), `SELECT id FROM projects WHERE organization_id=$1 AND id='prj_example' `+probe.mode+` NOWAIT`, org).Scan(&id)
				if probe.blocked != (pgtransport.SQLState(err) == pgtransport.LockTimeout) || !probe.blocked && err != nil {
					t.Errorf("project %s while board maintenance waits on the item: blocked=%t want %t (err=%v)", probe.mode, err != nil, probe.blocked, err)
				}
			}
			releaseOnce.Do(func() { close(release) })
			writerErr, maintenanceErr := writer.Wait(t), maintenance.Wait(t)
			if writerErr != nil {
				t.Errorf("item writer (SQLSTATE=%s): %v", pgtransport.SQLState(writerErr), writerErr)
			}
			if !errors.Is(maintenanceErr, corestore.ErrRevisionConflict) {
				t.Errorf("board maintenance after the writer advanced the item (SQLSTATE=%s): %v, want revision conflict", pgtransport.SQLState(maintenanceErr), maintenanceErr)
			}
			var deltas int
			if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM native_board_deltas WHERE organization_id=$1 AND project_id='prj_example' AND topic='activity' AND entity_id=$2`, org, event.ID).Scan(&deltas); err != nil {
				t.Fatal(err)
			}
			if deltas != 1 {
				t.Errorf("activity board deltas=%d, want 1", deltas)
			}
		})
	}
}

type itemLockBarrier struct {
	mu       sync.Mutex
	locked   chan struct{}
	release  chan struct{}
	consumed bool
}

type itemLockQuery struct{}

const appendEventItemLock = `SELECT id FROM issues WHERE organization_id=$1 AND project_id=$2 AND native_id=$3 FOR NO KEY UPDATE`

func (b *itemLockBarrier) arm() (<-chan struct{}, chan struct{}) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.locked, b.release, b.consumed = make(chan struct{}), make(chan struct{}), false
	return b.locked, b.release
}

func (b *itemLockBarrier) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if data.SQL != appendEventItemLock {
		return ctx
	}
	return context.WithValue(ctx, itemLockQuery{}, true)
}

func (b *itemLockBarrier) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if ctx.Value(itemLockQuery{}) == nil || data.Err != nil {
		return
	}
	b.mu.Lock()
	if b.locked == nil || b.consumed {
		b.mu.Unlock()
		return
	}
	b.consumed = true
	locked, release := b.locked, b.release
	b.mu.Unlock()
	close(locked)
	select {
	case <-release:
	case <-ctx.Done():
	}
}
