package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRepositorySerializationFailure(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	seedRepository(t, repository, "org_first")
	admin.SetMaxOpenConns(2)
	err := repository.Write(t.Context(), "org_first", corestore.Serving, func(corestore.Writer) error {
		first, err := admin.BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelSerializable})
		if err != nil {
			return err
		}
		defer first.Rollback()
		second, err := admin.BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelSerializable})
		if err != nil {
			return err
		}
		defer second.Rollback()
		for _, tx := range []*sql.Tx{first, second} {
			var name string
			if err := tx.QueryRowContext(t.Context(), `SELECT name FROM projects WHERE organization_id='org_first' AND id='prj_first'`).Scan(&name); err != nil {
				return err
			}
		}
		if _, err := first.ExecContext(t.Context(), `UPDATE projects SET name='Changed' WHERE organization_id='org_first' AND id='prj_first'`); err != nil {
			return err
		}
		if err := first.Commit(); err != nil {
			return err
		}
		_, err = second.ExecContext(t.Context(), `UPDATE projects SET name='Rejected' WHERE organization_id='org_first' AND id='prj_first'`)
		return fmt.Errorf("update core project: %w", err)
	})
	if !errors.Is(err, corestore.ErrSerializationFailure) || !errors.Is(err, mutation.ErrConflict) {
		t.Fatalf("serialization error = %v", err)
	}
	requireSQLState(t, err, "40001")
}

func TestRepositoryDeadlockFailure(t *testing.T) {
	admin := template.Database(t)
	repository, pool := repositoryFor(t, admin, smallPool())
	seedRepository(t, repository, "org_first")
	var login string
	if err := pool.QueryRowContext(t.Context(), `SELECT session_user`).Scan(&login); err != nil {
		t.Fatal(err)
	}
	firstLocked, secondLocked, finishSecond := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(finishSecond) }) })
	run := func(first, second string, locked, wait <-chan struct{}, signal chan<- struct{}) func(context.Context) error {
		return func(ctx context.Context) error {
			workload := corestore.Serving
			if first == "prj_second" {
				workload = corestore.Background
			}
			return repository.Write(ctx, "org_first", workload, func(w corestore.Writer) error {
				p, err := w.Project(ctx, tracker.ProjectID(first))
				if err != nil {
					return err
				}
				p.Name += "_changed"
				if _, err := w.UpdateProject(ctx, p); err != nil {
					return err
				}
				close(signal)
				for _, ready := range []<-chan struct{}{locked, wait} {
					select {
					case <-ready:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				p, err = w.Project(ctx, tracker.ProjectID(second))
				if err != nil {
					return err
				}
				p.Name += "_changed"
				_, err = w.UpdateProject(ctx, p)
				return err
			})
		}
	}
	ready := make(chan struct{})
	close(ready)
	first := startRepositoryTask(t, run("prj_first", "prj_second", secondLocked, ready, firstLocked))
	waitRepositorySignal(t, firstLocked)
	second := startRepositoryTask(t, run("prj_second", "prj_first", firstLocked, finishSecond, secondLocked))
	waitRepositorySignal(t, secondLocked)
	testenv.WaitUntil(t, t.Context(), func() error {
		var waiting bool
		err := admin.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND usename=$1 AND wait_event_type='Lock')`, login).Scan(&waiting)
		if err != nil {
			return err
		}
		if !waiting {
			return errors.New("first transaction has not reached the conflicting lock")
		}
		return nil
	})
	releaseOnce.Do(func() { close(finishSecond) })
	failures := 0
	for _, task := range []*repositoryTask{first, second} {
		if err := task.wait(t); err != nil {
			failures++
			if !errors.Is(err, corestore.ErrSerializationFailure) || !errors.Is(err, mutation.ErrConflict) {
				t.Errorf("deadlock error = %v", err)
			}
			requireSQLState(t, err, "40P01")
		}
	}
	if failures != 1 {
		t.Errorf("deadlock victims = %d, want 1", failures)
	}
}

func TestRepositoryCancellation(t *testing.T) {
	for _, mode := range []string{"before request", "expired deadline", "waiting for pool", "running statement", "server cancellation"} {
		t.Run(mode, func(t *testing.T) {
			admin := template.Database(t)
			repository, pool := repositoryFor(t, admin, smallPool())
			seedRepository(t, repository, "org_first")
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			want := error(context.Canceled)
			if mode == "before request" || mode == "expired deadline" {
				cancel()
				if mode == "expired deadline" {
					ctx, cancel = context.WithDeadline(t.Context(), time.Time{})
					defer cancel()
					want = context.DeadlineExceeded
				}
				err := repository.Read(ctx, "org_first", corestore.Serving, func(corestore.Reader) error { t.Error("canceled request invoked callback"); return nil })
				if !errors.Is(err, want) {
					t.Fatalf("error = %v, want %v", err, want)
				}
				return
			}
			var task *repositoryTask
			if mode == "waiting for pool" {
				var held []*sql.Conn
				for range smallPool().MaxConnections {
					conn, err := pool.Conn(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					held = append(held, conn)
					t.Cleanup(func() { conn.Close() })
				}
				waits := pool.Stats().WaitCount
				task = startRepositoryTask(t, func(taskCtx context.Context) error {
					return repository.Read(taskCtx, "org_first", corestore.Serving, func(corestore.Reader) error { return errors.New("canceled pool wait invoked callback") })
				})
				testenv.WaitUntil(t, t.Context(), func() error {
					if pool.Stats().WaitCount <= waits {
						return errors.New("request has not reached the shared pool")
					}
					return nil
				})
				task.cancel()
				if err := task.wait(t); !errors.Is(err, want) {
					t.Fatalf("pool cancellation error = %v", err)
				}
				for _, conn := range held {
					if err := conn.Close(); err != nil {
						t.Fatal(err)
					}
				}
			} else {
				admin.SetMaxOpenConns(2)
				blocker, err := admin.BeginTx(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}
				defer blocker.Rollback()
				var blockerPID int
				if err := blocker.QueryRowContext(t.Context(), `SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
					t.Fatal(err)
				}
				if _, err := blocker.ExecContext(t.Context(), `UPDATE projects SET name=name WHERE organization_id='org_first' AND id='prj_first'`); err != nil {
					t.Fatal(err)
				}
				task = startRepositoryTask(t, func(ctx context.Context) error {
					return repository.Write(ctx, "org_first", corestore.Serving, func(w corestore.Writer) error {
						p, err := w.Project(ctx, "prj_first")
						if err != nil {
							return err
						}
						p.Name = "Canceled change"
						_, err = w.UpdateProject(ctx, p)
						return err
					})
				})
				var waiterPID int
				testenv.WaitUntil(t, t.Context(), func() error {
					err := admin.QueryRowContext(t.Context(), `SELECT pid FROM pg_stat_activity WHERE datname=current_database() AND $1=ANY(pg_blocking_pids(pid))`, blockerPID).Scan(&waiterPID)
					if errors.Is(err, sql.ErrNoRows) {
						return errors.New("statement has not reached the conflicting lock")
					}
					return err
				})
				if mode == "server cancellation" {
					var canceled bool
					if err := admin.QueryRowContext(t.Context(), `SELECT pg_cancel_backend($1)`, waiterPID).Scan(&canceled); err != nil || !canceled {
						t.Fatalf("cancel fixture backend = %v: %v", canceled, err)
					}
				} else {
					task.cancel()
				}
				if err := task.wait(t); !errors.Is(err, want) {
					t.Fatalf("statement cancellation error = %v", err)
				}
				if err := blocker.Rollback(); err != nil {
					t.Fatal(err)
				}
			}
			if err := repository.Read(t.Context(), "org_first", corestore.Serving, func(r corestore.Reader) error {
				p, err := r.Project(t.Context(), "prj_first")
				if err == nil && p.Name != "first" {
					t.Errorf("canceled write committed: %+v", p)
				}
				return err
			}); err != nil {
				t.Fatalf("canceled request leaked capacity: %v", err)
			}
		})
	}
}
