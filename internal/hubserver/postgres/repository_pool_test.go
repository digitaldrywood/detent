package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type repositoryTask struct {
	done   chan struct{}
	err    error
	cancel context.CancelFunc
}

func startRepositoryTask(t *testing.T, run func(context.Context) error) *repositoryTask {
	t.Helper()
	ctx, cancel := context.WithCancel(testenv.WaitContext(t, t.Context()))
	task := &repositoryTask{done: make(chan struct{}), cancel: cancel}
	go func() { defer close(task.done); task.err = run(ctx) }()
	t.Cleanup(func() {
		cancel()
		cleanup := testenv.WaitContext(t, context.WithoutCancel(t.Context()))
		select {
		case <-task.done:
		case <-cleanup.Done():
			t.Error("repository task did not exit during cleanup")
		}
	})
	return task
}

func (task *repositoryTask) wait(t *testing.T) error {
	t.Helper()
	ctx := testenv.WaitContext(t, t.Context())
	select {
	case <-task.done:
		return task.err
	case <-ctx.Done():
		t.Fatal("repository task did not finish")
		return ctx.Err()
	}
}

func waitRepositorySignal(t *testing.T, ready <-chan struct{}) {
	t.Helper()
	ctx := testenv.WaitContext(t, t.Context())
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatal("repository signal did not arrive")
	}
}

type admissionContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (ctx *admissionContext) Done() <-chan struct{} {
	ctx.once.Do(func() { close(ctx.waiting) })
	return ctx.Context.Done()
}

func poolPIDs(t *testing.T, admin, pool *sql.DB) []int {
	t.Helper()
	var login string
	if err := pool.QueryRowContext(t.Context(), `SELECT session_user`).Scan(&login); err != nil {
		t.Fatal(err)
	}
	rows, err := admin.QueryContext(t.Context(), `SELECT pid FROM pg_stat_activity WHERE datname=current_database() AND usename=$1 ORDER BY pid`, login)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var pids []int
	for rows.Next() {
		var pid int
		if err := rows.Scan(&pid); err != nil {
			t.Fatal(err)
		}
		pids = append(pids, pid)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return pids
}

func TestRepositoryPoolBudgetsAndReuse(t *testing.T) {
	for _, config := range []postgres.PoolConfig{
		smallPool(),
		{MaxConnections: 4, ServingConnections: 3, BackgroundConnections: 1},
		{MaxConnections: 4, ServingConnections: 1, BackgroundConnections: 3},
	} {
		t.Run(fmt.Sprintf("serving=%d/background=%d", config.ServingConnections, config.BackgroundConnections), func(t *testing.T) {
			admin := template.Database(t)
			repository, pool := repositoryFor(t, admin, config)
			seedRepository(t, repository, "org_first")
			seedRepository(t, repository, "org_second")
			release := make(chan struct{})
			var releaseOnce sync.Once
			t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
			hold := func(workload corestore.Workload, organization tracker.OrganizationID) *repositoryTask {
				ready := make(chan struct{})
				task := startRepositoryTask(t, func(ctx context.Context) error {
					return repository.ReadTenant(ctx, organization, workload, func(r tenantstore.Reader) error {
						projects, err := r.Projects(ctx, "", 2)
						if err != nil {
							return err
						}
						if len(projects) != 2 || projects[0].OrganizationID != organization || projects[1].OrganizationID != organization {
							return errors.New("pooled transaction escaped tenant")
						}
						close(ready)
						select {
						case <-release:
							return nil
						case <-ctx.Done():
							return ctx.Err()
						}
					})
				})
				select {
				case <-ready:
				case <-task.done:
					t.Fatalf("held transaction exited before readiness: %v", task.err)
				case <-testenv.WaitContext(t, t.Context()).Done():
					t.Fatal("held transaction did not start")
				}
				return task
			}
			var held []*repositoryTask
			for range config.BackgroundConnections {
				held = append(held, hold(corestore.Background, "org_first"))
			}
			for range config.ServingConnections {
				held = append(held, hold(corestore.Serving, "org_second"))
			}
			if stats := pool.Stats(); stats.MaxOpenConnections != config.MaxConnections || stats.InUse != config.ServingConnections+config.BackgroundConnections {
				t.Fatalf("pool stats = %+v", stats)
			}
			for _, workload := range []corestore.Workload{corestore.Serving, corestore.Background} {
				waiting := make(chan struct{})
				blocked := startRepositoryTask(t, func(ctx context.Context) error {
					observed := &admissionContext{Context: ctx, waiting: waiting}
					return repository.Read(observed, "org_first", workload, func(corestore.Reader) error { return errors.New("exceeded configured workload budget") })
				})
				waitRepositorySignal(t, waiting)
				blocked.cancel()
				if err := blocked.wait(t); !errors.Is(err, context.Canceled) {
					t.Fatalf("saturated admission error = %v", err)
				}
			}
			releaseOnce.Do(func() { close(release) })
			for _, task := range held {
				if err := task.wait(t); err != nil {
					t.Fatal(err)
				}
			}
			before := poolPIDs(t, admin, pool)
			for index := range 100 {
				organization := tracker.OrganizationID(fmt.Sprintf("org_example_%d", index))
				if err := repository.Read(t.Context(), organization, corestore.Serving, func(r corestore.Reader) error {
					projects, err := r.Projects(t.Context(), "", 1)
					if len(projects) != 0 {
						t.Error("new tenant inherited pooled scope")
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			if after := poolPIDs(t, admin, pool); !reflect.DeepEqual(after, before) {
				t.Errorf("tenant reads replaced shared connections: before=%v after=%v", before, after)
			}
			if stats := pool.Stats(); stats.InUse != 0 || stats.OpenConnections > config.MaxConnections {
				t.Errorf("pool leaked connections: %+v", stats)
			}
		})
	}
}

func TestRepositoryPoolConfiguration(t *testing.T) {
	pool := rolePool(t, template.Database(t), "detent_runtime", "")
	for _, tc := range []struct {
		name   string
		config postgres.PoolConfig
		pool   *sql.DB
	}{
		{"nil pool", smallPool(), nil},
		{"unbounded pool", postgres.PoolConfig{ServingConnections: 1, BackgroundConnections: 1}, pool},
		{"negative serving", postgres.PoolConfig{MaxConnections: 2, ServingConnections: -1, BackgroundConnections: 1}, pool},
		{"no background", postgres.PoolConfig{MaxConnections: 2, ServingConnections: 1}, pool},
		{"overcommitted", postgres.PoolConfig{MaxConnections: 2, ServingConnections: 2, BackgroundConnections: 1}, pool},
		{"background exceeds remainder", postgres.PoolConfig{MaxConnections: 3, ServingConnections: 1, BackgroundConnections: 3}, pool},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repository, err := postgres.NewRepository(t.Context(), tc.pool, tc.config)
			if repository != nil || !errors.Is(err, postgres.ErrPoolConfig) {
				t.Fatalf("repository=%v error=%v", repository, err)
			}
		})
	}
	repository, err := postgres.NewRepository(t.Context(), pool, smallPool())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name         string
		organization tracker.OrganizationID
		workload     corestore.Workload
		want         error
	}{
		{"missing tenant", "", corestore.Serving, postgres.ErrTenantRequired},
		{"blank tenant", " \t", corestore.Serving, postgres.ErrTenantRequired},
		{"unknown workload", "org_first", corestore.Workload(42), postgres.ErrWorkload},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := repository.Read(t.Context(), tc.organization, tc.workload, func(corestore.Reader) error { t.Error("invalid input invoked callback"); return nil })
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestRepositoryReadCommittedAndReadOnly(t *testing.T) {
	admin := template.Database(t)
	repository, pool := repositoryFor(t, admin, smallPool())
	seedRepository(t, repository, "org_first")
	if _, err := pool.ExecContext(t.Context(), `SET default_transaction_isolation='serializable'`); err != nil {
		t.Fatal(err)
	}
	for _, readOnly := range []bool{false, true} {
		t.Run(fmt.Sprintf("readOnly=%v", readOnly), func(t *testing.T) {
			read := func(r corestore.Reader) error {
				first, err := r.Project(t.Context(), "prj_first")
				if err != nil {
					return err
				}
				if _, err := admin.ExecContext(t.Context(), `UPDATE projects SET workflow_source_revision=workflow_source_revision || '_changed' WHERE organization_id='org_first' AND id='prj_first'`); err != nil {
					return err
				}
				second, err := r.Project(t.Context(), "prj_first")
				if err != nil {
					return err
				}
				if second.WorkflowSourceRevision != first.WorkflowSourceRevision+"_changed" {
					t.Error("transaction did not take a fresh READ COMMITTED snapshot")
				}
				return nil
			}
			var err error
			if readOnly {
				err = repository.Read(t.Context(), "org_first", corestore.Serving, read)
			} else {
				err = repository.Write(t.Context(), "org_first", corestore.Serving, func(w corestore.Writer) error { return read(w) })
			}
			if err != nil {
				t.Fatal(err)
			}
		})
	}
	var writerAvailable bool
	err := repository.Read(t.Context(), "org_first", corestore.Serving, func(r corestore.Reader) error {
		w, ok := r.(corestore.Writer)
		writerAvailable = ok
		if !ok {
			return nil
		}
		p, err := r.Project(t.Context(), "prj_first")
		if err != nil {
			return err
		}
		_, err = w.UpdateProject(t.Context(), p)
		return err
	})
	if writerAvailable {
		requireSQLState(t, err, "25006")
	} else if err != nil {
		t.Fatal(err)
	}
}
