package hubmaintenance

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestCancellationJoinsExactlyTwoWorkersAndRejectsOverlap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		started := make(chan struct{}, TenantLimit)
		exited := make(chan struct{}, TenantLimit)
		scheduler, err := New(registry(100), []Feature{{Name: "board", Bytes: 1, Step: func(ctx context.Context, _ Tenant, _ Budget) (Outcome, error) {
			started <- struct{}{}
			<-ctx.Done()
			exited <- struct{}{}
			return Outcome{}, ctx.Err()
		}}})
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- scheduler.Tick(ctx) }()
		synctest.Wait()
		if len(started) != WorkerLimit {
			t.Fatalf("active tasks=%d want=%d", len(started), WorkerLimit)
		}
		if err := scheduler.Tick(ctx); err == nil {
			t.Fatal("overlapping tick accepted")
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation lost: %v", err)
		}
		if len(exited) != WorkerLimit {
			t.Fatalf("workers not joined: %d", len(exited))
		}
	})
}

func TestRunUsesOneJitteredWakeAndKeepsWorkingAfterErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	failure := errors.New("candidate unavailable")
	scheduler, err := New(registry(1), []Feature{{Name: "board", Bytes: 1, Step: func(context.Context, Tenant, Budget) (Outcome, error) { return Outcome{}, failure }}})
	if err != nil {
		t.Fatal(err)
	}
	wakes := make(chan time.Duration)
	advance := make(chan struct{})
	scheduler.wait = func(ctx context.Context, duration time.Duration) error {
		select {
		case wakes <- duration:
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case <-advance:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	reported := make(chan error, 2)
	done := make(chan error, 1)
	go func() { done <- scheduler.Run(ctx, func(err error) { reported <- err }) }()
	defer func() {
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Errorf("shutdown: %v", err)
		}
	}()
	for range 2 {
		if duration := <-wakes; duration < 900*time.Millisecond || duration > 1100*time.Millisecond {
			t.Fatalf("unbounded wake jitter: %s", duration)
		}
		advance <- struct{}{}
		if err := <-reported; !errors.Is(err, failure) {
			t.Fatalf("failure not reported: %v", err)
		}
	}
}

func TestRunDispatchesFullDuePagesWithoutWaitingBetweenThem(t *testing.T) {
	for _, fail := range []bool{false, true} {
		ctx, cancel := context.WithCancel(t.Context())
		waits, pages, remaining := 0, 0, 2*TenantLimit+1
		billing := Feature{Name: "billing", Lead: true, Bytes: 1, Due: func(ctx context.Context, limit int) ([]Tenant, error) {
			pages++
			n := min(remaining, limit)
			remaining -= n
			return registry(n).Page(ctx, "", limit)
		}, Step: func(context.Context, Tenant, Budget) (Outcome, error) {
			if fail {
				return Outcome{}, errors.New("billing unavailable")
			}
			return Outcome{Progress: true, Items: 1}, nil
		}}
		scheduler, err := New(registry(1), []Feature{billing, {Name: "ordinary", Bytes: 1, Step: func(context.Context, Tenant, Budget) (Outcome, error) { return Outcome{Progress: true, Items: 1}, nil }}})
		if err != nil {
			t.Fatal(err)
		}
		scheduler.wait = func(context.Context, time.Duration) error {
			waits++
			if waits == 2 {
				cancel()
				return context.Canceled
			}
			return nil
		}
		err = scheduler.Run(ctx, nil)
		cancel()
		want := 3
		if fail {
			want = 1
		}
		if !errors.Is(err, context.Canceled) || pages != want {
			t.Fatalf("failure=%v due pages before next wait=%d, want %d, err=%v", fail, pages, want, err)
		}
	}
}

func TestRunBoundsWebhookContinuationAndRotatesAtFleetScale(t *testing.T) {
	for _, due := range []int{0, 3, 10_000} {
		t.Run(fmt.Sprint(due), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			pages, processed := 0, 0
			pending := false
			lead := Feature{Name: "webhook", Lead: true, Bytes: 1, Due: func(context.Context, int) ([]Tenant, error) {
				pages++
				pending = processed < due
				if pending {
					processed++
				}
				return nil, nil
			}, Continue: func() bool { return pending }, Step: func(context.Context, Tenant, Budget) (Outcome, error) {
				t.Fatal("platform page dispatched a tenant worker")
				return Outcome{}, nil
			}}
			var rotations atomic.Int32
			scheduler, err := NewShared(registry(10_000), []Feature{lead, {Name: "ordinary", Bytes: 1, Step: func(context.Context, Tenant, Budget) (Outcome, error) {
				rotations.Add(1)
				return Outcome{Progress: true, Items: 1}, nil
			}}})
			if err != nil {
				t.Fatal(err)
			}
			scheduler.wait = func(context.Context, time.Duration) error {
				if pages != 0 {
					cancel()
					return ctx.Err()
				}
				return nil
			}
			if err := scheduler.Run(ctx, nil); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if pages != min(due+1, TenantLimit) || processed != min(due, TenantLimit) || int(rotations.Load()) != pages*TenantLimit {
				t.Fatalf("pages=%d processed=%d ordinary=%d", pages, processed, rotations.Load())
			}
		})
	}
}

func TestSlowPageYieldsToOtherFamiliesAfterActiveJobs(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		batch, stop := context.WithCancel(t.Context())
		defer stop()
		started := make(chan struct{}, TenantLimit)
		release := make(chan struct{})
		var served atomic.Int32
		slow := Feature{Name: "due", Lead: true, Bytes: 1, Due: func(ctx context.Context, limit int) ([]Tenant, error) { return registry(limit).Page(ctx, "", limit) }, Step: func(ctx context.Context, _ Tenant, _ Budget) (Outcome, error) {
			started <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
				return Outcome{}, ctx.Err()
			}
			return Outcome{Items: 1, Progress: true}, nil
		}}
		ordinary := Feature{Name: "ordinary", Bytes: 1, Step: func(context.Context, Tenant, Budget) (Outcome, error) {
			served.Add(1)
			return Outcome{Items: 1, Progress: true}, nil
		}}
		scheduler, err := New(registry(1), []Feature{slow, ordinary})
		if err != nil {
			t.Fatal(err)
		}
		first := true
		scheduler.dispatchWindow = func(ctx context.Context) (context.Context, context.CancelFunc) {
			if first {
				first = false
				return batch, stop
			}
			return context.WithCancel(ctx)
		}
		done := make(chan error, 1)
		go func() { done <- scheduler.Tick(t.Context()) }()
		synctest.Wait()
		active := len(started)
		stop()
		synctest.Wait()
		close(release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if active != WorkerLimit || len(started) != WorkerLimit || served.Load() == 0 {
			t.Fatalf("active=%d total slow=%d ordinary=%d", active, len(started), served.Load())
		}
	})
}
