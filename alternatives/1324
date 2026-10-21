package hub

import (
	"context"
	"testing"
	"time"
)

func TestSubscriptionCloseSignalsDone(t *testing.T) {
	t.Parallel()

	events := New[string]()
	sub, err := events.Subscribe(context.Background())
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	sub.Close()

	assertClosed(t, sub.done)
}

func TestHubCloseSignalsSubscriptionDone(t *testing.T) {
	t.Parallel()

	events := New[string]()
	sub, err := events.Subscribe(context.Background())
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	events.Close()

	assertClosed(t, sub.done)
}

func assertClosed(t *testing.T, ch <-chan struct{}) {
	t.Helper()

	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for subscription done channel to close")
	}
}
