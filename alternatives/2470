package postgres

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
)

func TestWorkloadBudgetsReserveIndependentCapacity(t *testing.T) {
	for _, workload := range []corestore.Workload{corestore.Serving, corestore.Background} {
		t.Run(map[corestore.Workload]string{corestore.Serving: "serving", corestore.Background: "background"}[workload], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				repository := &Repository{serving: make(chan struct{}, 1), background: make(chan struct{}, 1)}
				release, err := repository.acquire(t.Context(), workload)
				if err != nil {
					t.Fatal(err)
				}
				defer release()
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				result := make(chan error, 1)
				go func() {
					release, err := repository.acquire(ctx, workload)
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
