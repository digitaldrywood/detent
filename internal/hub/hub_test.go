package hub_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hub"
)

func TestHubPublishFansOut(t *testing.T) {
	t.Parallel()

	events := hub.New[string]()

	first, err := events.Subscribe(context.Background())
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	second, err := events.Subscribe(context.Background())
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	if err := events.Publish("ready"); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	for name, sub := range map[string]*hub.Subscription[string]{
		"first":  first,
		"second": second,
	} {
		t.Run(name, func(t *testing.T) {
			if got := receive(t, sub.C()); got != "ready" {
				t.Fatalf("received %q, want ready", got)
			}
		})
	}
}

func TestHubSubscribeReplaysLastEvent(t *testing.T) {
	t.Parallel()

	events := hub.New[int]()

	if err := events.Publish(42); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	sub, err := events.Subscribe(context.Background())
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	if got := receive(t, sub.C()); got != 42 {
		t.Fatalf("replayed event = %d, want 42", got)
	}
}

func TestHubLatestReturnsLastEvent(t *testing.T) {
	t.Parallel()

	events := hub.New[string]()
	if got, ok := events.Latest(); ok || got != "" {
		t.Fatalf("Latest() before publish = %q, %v; want empty, false", got, ok)
	}

	if err := events.Publish("ready"); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	got, ok := events.Latest()
	if !ok || got != "ready" {
		t.Fatalf("Latest() = %q, %v; want ready, true", got, ok)
	}

	events.Close()
	got, ok = events.Latest()
	if !ok || got != "ready" {
		t.Fatalf("Latest() after Close() = %q, %v; want ready, true", got, ok)
	}
}

func TestHubDropsOldestForSlowSubscriber(t *testing.T) {
	t.Parallel()

	events := hub.New[string]()
	sub, err := events.Subscribe(context.Background())
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	if err := events.Publish("old"); err != nil {
		t.Fatalf("Publish(old) error = %v", err)
	}
	if err := events.Publish("new"); err != nil {
		t.Fatalf("Publish(new) error = %v", err)
	}

	if got := receive(t, sub.C()); got != "new" {
		t.Fatalf("received %q, want new", got)
	}
}

func TestHubWithBufferKeepsNewestBufferedEvents(t *testing.T) {
	t.Parallel()

	events := hub.New[string](hub.WithBuffer(2))
	sub, err := events.Subscribe(context.Background())
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	for _, event := range []string{"old", "middle", "new"} {
		if err := events.Publish(event); err != nil {
			t.Fatalf("Publish(%q) error = %v", event, err)
		}
	}

	if got := receive(t, sub.C()); got != "middle" {
		t.Fatalf("first buffered event = %q, want middle", got)
	}
	if got := receive(t, sub.C()); got != "new" {
		t.Fatalf("second buffered event = %q, want new", got)
	}
}

func TestHubConcurrentPublishAndConsumeIsRaceFree(t *testing.T) {
	t.Parallel()

	events := hub.New[int]()
	sub, err := events.Subscribe(context.Background())
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case _, ok := <-sub.C():
				if !ok {
					return
				}
			case <-done:
				return
			}
		}
	})

	for value := range 1000 {
		if err := events.Publish(value); err != nil {
			t.Fatalf("Publish(%d) error = %v", value, err)
		}
	}
	close(done)
	wg.Wait()
	sub.Close()
}

func TestHubCloseClosesSubscribersAndRejectsOperations(t *testing.T) {
	t.Parallel()

	events := hub.New[string]()
	sub, err := events.Subscribe(context.Background())
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	events.Close()

	if _, ok := <-sub.C(); ok {
		t.Fatal("subscription channel is open after Close()")
	}
	if err := events.Publish("ignored"); !errors.Is(err, hub.ErrClosed) {
		t.Fatalf("Publish() error = %v, want %v", err, hub.ErrClosed)
	}
	if _, err := events.Subscribe(context.Background()); !errors.Is(err, hub.ErrClosed) {
		t.Fatalf("Subscribe() error = %v, want %v", err, hub.ErrClosed)
	}
}

func TestHubSubscribeRejectsCanceledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	events := hub.New[string]()

	if _, err := events.Subscribe(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Subscribe() error = %v, want %v", err, context.Canceled)
	}
}

func TestSubscriptionClosesWhenContextIsCanceled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	events := hub.New[string]()
	sub, err := events.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	cancel()

	select {
	case _, ok := <-sub.C():
		if ok {
			t.Fatal("subscription channel is open after context cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for subscription channel to close")
	}
}

func TestSubscriptionCloseRemovesSubscriber(t *testing.T) {
	t.Parallel()

	events := hub.New[string]()
	sub, err := events.Subscribe(context.Background())
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	sub.Close()

	if _, ok := <-sub.C(); ok {
		t.Fatal("subscription channel is open after subscription Close()")
	}
	if err := events.Publish("ready"); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
}

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()

	select {
	case got := <-ch:
		return got
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
	}

	var zero T
	return zero
}
