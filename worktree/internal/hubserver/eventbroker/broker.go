package eventbroker

import (
	"fmt"
	"sync"
)

type Broker struct {
	mu          sync.Mutex
	closed      bool
	epoch       string
	revisions   map[string]uint64
	subscribers map[string]map[*Subscription]struct{}
}

type Subscription struct {
	key    string
	wake   chan struct{}
	closed chan struct{}
	once   sync.Once
}

func New(epoch string) *Broker {
	return &Broker{epoch: epoch, revisions: make(map[string]uint64), subscribers: map[string]map[*Subscription]struct{}{}}
}

func (b *Broker) Subscribe(key string) (subscription *Subscription, cancel func()) {
	subscription = &Subscription{key: key, wake: make(chan struct{}, 1), closed: make(chan struct{})}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		subscription.close()
		return subscription, func() {}
	}
	set, ok := b.subscribers[key]
	if !ok {
		set = map[*Subscription]struct{}{}
		b.subscribers[key] = set
	}
	set[subscription] = struct{}{}
	return subscription, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if set, ok := b.subscribers[key]; ok {
			delete(set, subscription)
			if len(set) == 0 {
				delete(b.subscribers, key)
			}
		}
		subscription.close()
	}
}

func (s *Subscription) Wake() <-chan struct{} { return s.wake }

func (s *Subscription) Closed() <-chan struct{} { return s.closed }

func (s *Subscription) close() {
	s.once.Do(func() { close(s.closed) })
}

func (b *Broker) Notify(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, tracked := b.revisions[key]; tracked {
		b.revisions[key]++
	}
	for subscription := range b.subscribers[key] {
		select {
		case subscription.wake <- struct{}{}:
		default:
		}
	}
}

func (b *Broker) CloseAll() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	for _, set := range b.subscribers {
		for subscription := range set {
			subscription.close()
		}
	}
	b.subscribers = map[string]map[*Subscription]struct{}{}
}

func (b *Broker) Cursor(key string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	revision := b.revisions[key]
	b.revisions[key] = revision
	return fmt.Sprintf("%s:%d", b.epoch, revision)
}

func (b *Broker) Subscribers(key string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subscribers[key])
}
