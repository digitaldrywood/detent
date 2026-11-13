package hubserver

import (
	"github.com/digitaldrywood/detent/internal/hubserver/eventbroker"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
)

type notificationBroker struct{ *eventbroker.Broker }

type notificationSubscription struct {
	wake   <-chan struct{}
	closed <-chan struct{}
}

func newNotificationBroker() *notificationBroker {
	return &notificationBroker{eventbroker.New(hubapi.NewID("wait"))}
}

func (b *notificationBroker) subscribe(key string) (*notificationSubscription, func()) {
	subscription, cancel := b.Subscribe(key)
	return &notificationSubscription{wake: subscription.Wake(), closed: subscription.Closed()}, cancel
}

func (b *notificationBroker) notify(key string) { b.Notify(key) }

func (b *notificationBroker) closeAll() { b.CloseAll() }
