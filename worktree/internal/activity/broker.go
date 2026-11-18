package activity

import (
	"context"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	defaultMaxEvents = 200
	defaultMaxBytes  = 512 * 1024
	defaultBuffer    = 64
)

type Key struct {
	ProjectID string
	IssueID   string
}

type Event struct {
	At                time.Time
	DetentSessionID   int64
	ProviderSessionID string
	TurnID            string
	ItemID            string
	Kind              string
	Title             string
	Content           string
	Status            string
	Model             string
	TotalTokens       int64
	Truncated         bool
}

type Broker struct {
	mu        sync.Mutex
	streams   map[Key]*stream
	maxEvents int
	maxBytes  int
}

type stream struct {
	sessionID   int64
	events      []Event
	bytes       int
	subscribers map[*Subscription]struct{}
}

type Subscription struct {
	broker   *Broker
	key      Key
	backfill []Event
	events   chan Event
	done     chan struct{}
	once     sync.Once
}

func NewBroker() *Broker {
	return NewBrokerWithLimits(defaultMaxEvents, defaultMaxBytes)
}

func NewBrokerWithLimits(maxEvents int, maxBytes int) *Broker {
	if maxEvents <= 0 {
		maxEvents = defaultMaxEvents
	}
	if maxBytes <= 0 {
		maxBytes = defaultMaxBytes
	}
	return &Broker{
		streams:   make(map[Key]*stream),
		maxEvents: maxEvents,
		maxBytes:  maxBytes,
	}
}

func (b *Broker) Publish(key Key, event Event) {
	if b == nil {
		return
	}
	key = normalizeKey(key)
	if key.IssueID == "" {
		return
	}
	event = normalizeEvent(event, b.maxBytes)

	b.mu.Lock()
	current := b.streams[key]
	if current == nil {
		current = &stream{subscribers: make(map[*Subscription]struct{})}
		b.streams[key] = current
	}
	if event.DetentSessionID > 0 && current.sessionID != 0 && current.sessionID != event.DetentSessionID {
		current.events = nil
		current.bytes = 0
	}
	if event.DetentSessionID > 0 {
		current.sessionID = event.DetentSessionID
	}
	current.events = append(current.events, event)
	current.bytes += eventBytes(event)
	for len(current.events) > b.maxEvents || current.bytes > b.maxBytes {
		current.bytes -= eventBytes(current.events[0])
		current.events = current.events[1:]
	}
	for subscriber := range current.subscribers {
		sendLatest(subscriber.events, event)
	}
	b.mu.Unlock()
}

func (b *Broker) Subscribe(ctx context.Context, key Key) *Subscription {
	if ctx == nil {
		ctx = context.Background()
	}
	key = normalizeKey(key)
	subscription := &Subscription{
		broker: b,
		key:    key,
		events: make(chan Event, defaultBuffer),
		done:   make(chan struct{}),
	}
	if b == nil || key.IssueID == "" || ctx.Err() != nil {
		subscription.Close()
		return subscription
	}

	b.mu.Lock()
	current := b.streams[key]
	if current == nil {
		current = &stream{subscribers: make(map[*Subscription]struct{})}
		b.streams[key] = current
	}
	subscription.backfill = append([]Event(nil), current.events...)
	current.subscribers[subscription] = struct{}{}
	b.mu.Unlock()

	go func() {
		select {
		case <-ctx.Done():
			subscription.Close()
		case <-subscription.done:
		}
	}()
	return subscription
}

func (b *Broker) SubscriberCount(key Key) int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	current := b.streams[normalizeKey(key)]
	if current == nil {
		return 0
	}
	return len(current.subscribers)
}

func (s *Subscription) Backfill() []Event {
	if s == nil {
		return nil
	}
	return append([]Event(nil), s.backfill...)
}

func (s *Subscription) C() <-chan Event {
	if s == nil {
		return nil
	}
	return s.events
}

func (s *Subscription) Close() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		if s.broker != nil {
			s.broker.mu.Lock()
			if current := s.broker.streams[s.key]; current != nil {
				delete(current.subscribers, s)
			}
			s.broker.mu.Unlock()
		}
		close(s.done)
		close(s.events)
	})
}

func normalizeKey(key Key) Key {
	return Key{
		ProjectID: strings.TrimSpace(key.ProjectID),
		IssueID:   strings.TrimSpace(key.IssueID),
	}
}

func normalizeEvent(event Event, maxBytes int) Event {
	event.At = event.At.UTC()
	if event.At.IsZero() {
		event.At = time.Now().UTC()
	}
	event.ProviderSessionID = strings.TrimSpace(event.ProviderSessionID)
	event.TurnID = strings.TrimSpace(event.TurnID)
	event.ItemID = strings.TrimSpace(event.ItemID)
	event.Kind = strings.TrimSpace(event.Kind)
	event.Title = strings.TrimSpace(event.Title)
	event.Status = strings.TrimSpace(event.Status)
	event.Model = strings.TrimSpace(event.Model)
	if len(event.Content) > maxBytes {
		event.Content = trimUTF8(event.Content, maxBytes)
		event.Truncated = true
	}
	return event
}

func eventBytes(event Event) int {
	return len(event.Kind) + len(event.Title) + len(event.Content) + len(event.Status) + len(event.Model)
}

func trimUTF8(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func sendLatest(ch chan Event, event Event) {
	select {
	case ch <- event:
	default:
		select {
		case <-ch:
		default:
		}
		select {
		case ch <- event:
		default:
		}
	}
}
