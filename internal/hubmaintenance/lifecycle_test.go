package hubmaintenance

import (
	"context"
	"errors"
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
		scheduler, err := New(registry(100), []Feature{{Name: "board", Bytes: 1, Step: func(ctx context.Context, _ Tenant, _ Budget) error {
			started <- struct{}{}
			<-ctx.Done()
			exited <- struct{}{}
			return ctx.Err()
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
	scheduler, err := New(registry(1), []Feature{{Name: "board", Bytes: 1, Step: func(context.Context, Tenant, Budget) error { return failure }}})
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
