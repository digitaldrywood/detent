package activity

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestBrokerBackfillsAndFansOutByIssue(t *testing.T) {
	t.Parallel()

	broker := NewBrokerWithLimits(4, 1024)
	key := Key{ProjectID: "detent", IssueID: "issue-1156"}
	broker.Publish(key, Event{DetentSessionID: 1, Kind: "assistant", Content: "backfill"})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	first := broker.Subscribe(ctx, key)
	t.Cleanup(first.Close)
	second := broker.Subscribe(ctx, key)
	t.Cleanup(second.Close)

	if got := first.Backfill(); len(got) != 1 || got[0].Content != "backfill" {
		t.Fatalf("Backfill() = %#v, want backfill event", got)
	}
	if got := broker.SubscriberCount(key); got != 2 {
		t.Fatalf("SubscriberCount() = %d, want 2", got)
	}

	broker.Publish(key, Event{DetentSessionID: 1, Kind: "tool_output", Content: "live"})
	for name, subscription := range map[string]*Subscription{"first": first, "second": second} {
		t.Run(name, func(t *testing.T) {
			select {
			case event := <-subscription.C():
				if event.Content != "live" {
					t.Fatalf("event = %#v, want live output", event)
				}
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for live event")
			}
		})
	}

	first.Close()
	second.Close()
	if got := broker.SubscriberCount(key); got != 0 {
		t.Fatalf("SubscriberCount() after Close = %d, want 0", got)
	}
}

func TestBrokerRotatesAndBoundsSessionBackfill(t *testing.T) {
	t.Parallel()

	broker := NewBrokerWithLimits(2, 16)
	key := Key{IssueID: "issue-1156"}
	broker.Publish(key, Event{DetentSessionID: 1, Content: "old"})
	broker.Publish(key, Event{DetentSessionID: 2, Content: strings.Repeat("x", 32)})
	broker.Publish(key, Event{DetentSessionID: 2, Content: "latest"})

	subscription := broker.Subscribe(context.Background(), key)
	t.Cleanup(subscription.Close)
	backfill := subscription.Backfill()
	if len(backfill) != 1 {
		t.Fatalf("Backfill() len = %d, want 1 bounded current-session event: %#v", len(backfill), backfill)
	}
	if backfill[0].Content != "latest" {
		t.Fatalf("Backfill()[0] = %#v, want latest", backfill[0])
	}
}
