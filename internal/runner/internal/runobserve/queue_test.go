package runobserve

import (
	"context"
	"errors"
	"slices"
	"testing"
	"testing/synctest"
	"time"
)

func TestRequiredObservationsDrainAfterTurnCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		parent, cancelTurn := context.WithCancel(t.Context())
		defer cancelTurn()
		_, queue := StartRequired(parent, errors.New("lost authority"), func(err error) { t.Error(err) })
		var recorded []int
		release := make(chan struct{})
		queue.Observe(func(ctx context.Context) error {
			<-release
			if err := ctx.Err(); err != nil {
				return err
			}
			recorded = append(recorded, 1)
			return nil
		})
		synctest.Wait()
		queue.ObserveLatest(func(context.Context) error { recorded = append(recorded, 2); return nil })
		queue.Observe(func(context.Context) error { recorded = append(recorded, 3); return nil })
		queue.ObserveLatest(func(context.Context) error { recorded = append(recorded, 4); return nil })
		cancelTurn()
		drain, cancelDrain := context.WithTimeout(context.WithoutCancel(parent), time.Second)
		defer cancelDrain()
		finished := make(chan error, 1)
		go func() { finished <- queue.Finish(drain) }()
		synctest.Wait()
		close(release)
		if err := <-finished; err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(recorded, []int{1, 3, 4}) {
			t.Fatalf("recorded %v, want [1 3 4]", recorded)
		}
	})
}

func TestRequiredObservationsSurfaceFailedWrite(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		failure := errors.New("store unavailable")
		turn, queue := StartRequired(t.Context(), errors.New("lost authority"), func(err error) { t.Error(err) })
		queue.Observe(func(context.Context) error { return failure })
		queue.Observe(func(context.Context) error {
			t.Error("delivered a required record after an unacknowledged write")
			return nil
		})
		synctest.Wait()
		if turn.Err() != nil {
			t.Errorf("persistence failure interrupted provider dispatch: %v", context.Cause(turn))
		}
		if err := queue.Finish(t.Context()); !errors.Is(err, failure) {
			t.Fatalf("finish returned %v, want %v", err, failure)
		}
	})
}

func TestRequiredObservationsStopAtDrainDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, queue := StartRequired(t.Context(), errors.New("lost authority"), func(err error) { t.Error(err) })
		queue.Observe(func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		})
		drain, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		if err := queue.Finish(drain); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("finish returned %v, want drain deadline", err)
		}
	})
}
