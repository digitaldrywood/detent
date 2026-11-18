package postgres

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"testing/synctest"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/poolbudget"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestWorkloadBudgetsReserveIndependentCapacity(t *testing.T) {
	t.Parallel()
	for _, workload := range []corestore.Workload{corestore.Serving, corestore.Background} {
		t.Run(map[corestore.Workload]string{corestore.Serving: "serving", corestore.Background: "background"}[workload], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				repository := &Repository{budget: poolbudget.New(1, 1)}
				release, err := repository.acquire(t.Context(), workload)
				if err != nil {
					t.Fatal(err)
				}
				defer release()
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				result := make(chan error, 1)
				go func() {
					jobContext := ctx
					queuedWorkload := workload
					if workload == corestore.Background {
						jobContext = WithBackgroundWork(ctx)
						queuedWorkload = corestore.Serving
					}
					release, err := repository.acquire(jobContext, queuedWorkload)
					if release != nil {
						release()
						result <- errors.New("workload exceeded its own budget")
						return
					}
					result <- err
				}()
				synctest.Wait()
				select {
				case err := <-result:
					t.Fatalf("saturated workload was admitted: %v", err)
				default:
				}
				other := corestore.Background
				if workload == other {
					other = corestore.Serving
				}
				releaseOther, err := repository.acquire(t.Context(), other)
				if err != nil {
					t.Fatal(err)
				}
				releaseOther()
				cancel()
				if err := <-result; !errors.Is(err, context.Canceled) {
					t.Fatalf("admission error = %v", err)
				}
				releaseAgain, err := repository.acquire(t.Context(), other)
				if err != nil {
					t.Fatal(err)
				}
				releaseAgain()
			})
		})
	}
}

func TestSharedServingAdmission(t *testing.T) {
	for _, slots := range []int{1, 2, 4} {
		t.Run(strconv.Itoa(slots), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				root := &Repository{budget: poolbudget.New(slots, 1), runtime: &Runtime{pools: new(pgtransport.Pools)}}
				view := func(id tracker.OrganizationID) *Repository {
					v, err := root.ForOrganization(id, 1)
					if err != nil {
						t.Fatal(err)
					}
					return v
				}
				a, b := view("org_alpha"), view("org_beta")
				var held []func()
				releaseAll := func() {
					for _, free := range held {
						free()
					}
					held = nil
				}
				defer releaseAll()
				for range max(1, slots-1) {
					free, err := a.acquire(t.Context(), corestore.Serving)
					if err != nil {
						t.Fatal(err)
					}
					held = append(held, free)
				}
				wait, cancel := context.WithCancel(t.Context())
				defer cancel()
				same := view("org_alpha")
				rejected := make(chan error, 1)
				go func() {
					free, err := same.acquire(wait, corestore.Serving)
					if free != nil {
						free()
					}
					rejected <- err
				}()
				synctest.Wait()
				select {
				case err := <-rejected:
					if !errors.Is(err, corestore.ErrServingCapacity) {
						t.Fatalf("second view bypassed tenant capacity, err=%v", err)
					}
				default:
					cancel()
					<-rejected
					t.Fatal("saturated tenant queued excess work")
				}
				done := make(chan error, 1)
				go func() {
					free, err := b.acquire(t.Context(), corestore.Serving)
					if free != nil {
						free()
					}
					done <- err
				}()
				synctest.Wait()
				if slots == 1 {
					releaseAll()
				}
				synctest.Wait()
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				default:
					t.Fatal("other tenant stalled")
				}
			})
		})
	}
}
