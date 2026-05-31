package hub_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/symphony-go/internal/hub"
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
