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
	scheduler.wait = func(ctx context.Context, duration time.Duration) (bool, error) {
		select {
		case wakes <- duration:
		case <-ctx.Done():
			return false, ctx.Err()
		}
		select {
		case <-advance:
			return false, nil
		case <-ctx.Done():
			return false, ctx.Err()
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

func TestEmptyRotationsBackOffAndResumeOnWork(t *testing.T) {
	worked := false
	feature := Feature{Name: "example", Bytes: 1, Step: func(ctx context.Context, tenant Tenant, _ Budget) (Outcome, error) {
		return Outcome{Progress: true, Items: 1, CursorOnly: !worked || tenant.Organization != "org_00000"}, nil
	}}
	scheduler, err := New(registry(2), []Feature{feature})
	if err != nil {
		t.Fatal(err)
	}
	var waits []time.Duration
	scheduler.wait = func(_ context.Context, d time.Duration) (bool, error) {
		waits = append(waits, d)
		worked = len(waits) == 3
		if len(waits) == 4 {
			return false, context.Canceled
		}
		return false, nil
	}
	if err := scheduler.Run(t.Context(), nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	for i, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, time.Second} {
		if waits[i] < want*9/10 || waits[i] > want*11/10 {
			t.Fatalf("wait %d = %v, want jitter around %v", i, waits[i], want)
		}
	}
}

func TestWakeServesWholeRotationAtBaseCadence(t *testing.T) {
	var visited map[string]bool
	var features []Feature
	for _, name := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"} {
		features = append(features, Feature{Name: name, Bytes: 1, Step: func(context.Context, Tenant, Budget) (Outcome, error) {
			if visited != nil {
				visited[name] = true
			}
			return Outcome{}, nil
		}})
	}
	s, err := New(registry(1), features)
	if err != nil {
		t.Fatal(err)
	}
	s.wait = func(ctx context.Context, d time.Duration) (bool, error) {
		if visited == nil && d > 50*time.Second {
			visited = map[string]bool{}
			s.Wake()
			return s.waitForWake(ctx, d)
		}
		if visited != nil {
			if len(visited) == len(features) {
				return false, context.Canceled
			}
			if d > 1100*time.Millisecond {
				t.Fatalf("wait %v after wake with %d/%d families served", d, len(visited), len(features))
			}
		}
		return false, nil
	}
	if err := s.Run(t.Context(), nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
