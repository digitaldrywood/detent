package eventbroker

import "testing"

func TestConversationBrokerNotifiesSubscribersOnce(t *testing.T) {
	broker := New("wait")
	first, cancelFirst := broker.Subscribe("conv_a")
	defer cancelFirst()
	other, cancelOther := broker.Subscribe("conv_b")
	defer cancelOther()

	broker.Notify("conv_a")
	broker.Notify("conv_a")
	select {
	case <-first.wake:
	default:
		t.Fatal("expected a wake signal for conv_a")
	}
	select {
	case <-first.wake:
		t.Fatal("wake signals must coalesce to one pending signal")
	default:
	}
	select {
	case <-other.wake:
		t.Fatal("conv_b must not be woken by conv_a events")
	default:
	}
}

func TestConversationBrokerCancelAndClose(t *testing.T) {
	broker := New("wait")
	subscription, cancel := broker.Subscribe("conv_a")
	cancel()
	select {
	case <-subscription.closed:
	default:
		t.Fatal("cancel must close the subscription")
	}
	if len(broker.subscribers) != 0 {
		t.Fatalf("expected no subscribers after cancel, got %d", len(broker.subscribers))
	}
	broker.Notify("conv_a")

	live, cancelLive := broker.Subscribe("conv_a")
	defer cancelLive()
	broker.CloseAll()
	select {
	case <-live.closed:
	default:
		t.Fatal("closeAll must close live subscriptions")
	}
	late, cancelLate := broker.Subscribe("conv_a")
	defer cancelLate()
	select {
	case <-late.closed:
	default:
		t.Fatal("subscribing after close must return a closed subscription")
	}
}
