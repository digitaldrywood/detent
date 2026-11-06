package postgres_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestTenantTransactionConcurrentRevisions(t *testing.T) {
	for _, isolated := range []bool{false, true} {
		t.Run(map[bool]string{false: "same tenant", true: "two tenants"}[isolated], func(t *testing.T) {
			r, _ := repositoryFor(t, template.Database(t), smallPool())
			seedRepository(t, r, "org_first")
			seedRepository(t, r, "org_second")
			organizations := []tracker.OrganizationID{"org_first", "org_first"}
			if isolated {
				organizations[1] = "org_second"
			}
			ready := make(chan struct{}, 2)
			proceed := make(chan struct{})
			var once sync.Once
			t.Cleanup(func() { once.Do(func() { close(proceed) }) })
			var tasks []*repositoryTask
			var mutations []tenantMutation
			for i, org := range organizations {
				m := prepareTenantMutation(t, org, []string{"first", "second"}[i])
				mutations = append(mutations, m)
				workload := []corestore.Workload{corestore.Serving, corestore.Background}[i]
				tasks = append(tasks, startRepositoryTask(t, func(ctx context.Context) error {
					return r.WriteTenant(ctx, org, workload, func(w tenantstore.Writer) error {
						item, err := w.WorkItem(ctx, m.event.ProjectID, m.event.WorkItemID)
						if err != nil {
							return err
						}
						ready <- struct{}{}
						select {
						case <-proceed:
						case <-ctx.Done():
							return ctx.Err()
						}
						item.Title = "Updated atomically"
						if _, err := w.UpdateWorkItem(ctx, item); err != nil {
							return err
						}
						if _, err := w.AppendEvent(ctx, m.event); err != nil {
							return err
						}
						_, err = w.PutReceipt(ctx, m.receipt)
						return err
					})
				}))
			}
			for range tasks {
				waitRepositorySignal(t, ready)
			}
			once.Do(func() { close(proceed) })
			conflicts := 0
			for i, task := range tasks {
				err := task.wait(t)
				if err == nil {
					mutations[i].check(t, r, true)
					continue
				}
				if !errors.Is(err, corestore.ErrRevisionConflict) {
					t.Fatal(err)
				}
				conflicts++
				m := mutations[i]
				if err := r.ReadTenant(t.Context(), m.event.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
					_, err := reader.Receipt(t.Context(), m.receipt.ActorID, m.receipt.Operation, m.receipt.CommandKey)
					if !errors.Is(err, corestore.ErrNotFound) {
						t.Errorf("losing command published receipt: %v", err)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			want := 1
			if isolated {
				want = 0
			}
			if conflicts != want {
				t.Errorf("conflicts = %d, want %d", conflicts, want)
			}
		})
	}
}

func TestTenantTransactionReadScope(t *testing.T) {
	admin := template.Database(t)
	r, pool := repositoryFor(t, admin, smallPool())
	seedRepository(t, r, "org_first")
	if _, err := pool.Exec(t.Context(), `SET default_transaction_isolation='serializable'`); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"read", "write"} {
		t.Run(mode, func(t *testing.T) {
			read := func(reader tenantstore.Reader) error {
				first, err := reader.WorkItem(t.Context(), "prj_first", "wi_first_1")
				if err != nil {
					return err
				}
				if _, err := admin.ExecContext(t.Context(), `UPDATE issues SET title=title || ' changed' WHERE organization_id='org_first' AND native_id='wi_first_1'`); err != nil {
					return err
				}
				second, err := reader.WorkItem(t.Context(), "prj_first", "wi_first_1")
				if err == nil && second.Title != first.Title+" changed" {
					t.Error("tenant transaction did not refresh its read-committed snapshot")
				}
				return err
			}
			var err error
			if mode == "read" {
				err = r.ReadTenant(t.Context(), "org_first", corestore.Serving, read)
			} else {
				err = r.WriteTenant(t.Context(), "org_first", corestore.Serving, func(w tenantstore.Writer) error { return read(w) })
			}
			if err != nil {
				t.Fatal(err)
			}
		})
	}
	err := r.ReadTenant(t.Context(), "org_first", corestore.Serving, func(reader tenantstore.Reader) error {
		writer, ok := reader.(tenantstore.Writer)
		if !ok {
			return nil
		}
		err := writer.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: "org_first", ID: "machine", Hostname: "example", Capacity: 1})
		if err == nil {
			t.Error("read-only tenant scope permitted a write")
		}
		return err
	})
	if err != nil {
		requireSQLState(t, err, "25006")
	}
}
