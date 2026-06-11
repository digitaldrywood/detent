package hub

import (
	"context"
	"errors"
	"sync"
)

var ErrClosed = errors.New("hub closed")

type Option func(*options)

type options struct {
	buffer int
}

type Hub[T any] struct {
	mu          sync.Mutex
	subscribers map[*Subscription[T]]struct{}
	buffer      int
	last        T
	hasLast     bool
	closed      bool
}

type Subscription[T any] struct {
	hub       *Hub[T]
	ch        chan T
	done      chan struct{}
	closeOnce sync.Once
}

func New[T any](opts ...Option) *Hub[T] {
	cfg := options{buffer: 1}
	for _, opt := range opts {
		opt(&cfg)
	}

	return &Hub[T]{
		subscribers: make(map[*Subscription[T]]struct{}),
		buffer:      cfg.buffer,
	}
}

func WithBuffer(size int) Option {
	return func(cfg *options) {
		if size > 0 {
			cfg.buffer = size
		}
	}
}

func (h *Hub[T]) Subscribe(ctx context.Context) (*Subscription[T], error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	sub := &Subscription[T]{
		hub:  h,
		ch:   make(chan T, h.buffer),
		done: make(chan struct{}),
	}

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil, ErrClosed
	}
	h.subscribers[sub] = struct{}{}
	if h.hasLast {
		sendLatest(sub.ch, h.last)
	}
	h.mu.Unlock()

	if done := ctx.Done(); done != nil {
		go func() {
			select {
			case <-done:
				sub.Close()
			case <-sub.done:
			}
		}()
	}

	return sub, nil
}

func (h *Hub[T]) Publish(value T) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.closed {
		return ErrClosed
	}

	h.last = value
	h.hasLast = true
	for sub := range h.subscribers {
		sendLatest(sub.ch, value)
	}

	return nil
}

func (h *Hub[T]) Latest() (T, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.last, h.hasLast
}

func (h *Hub[T]) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.closed {
		return
	}

	h.closed = true
	for sub := range h.subscribers {
		delete(h.subscribers, sub)
		sub.close()
	}
}

// C returns a lossy subscription stream. Slow subscribers may miss intermediate
// values when their buffer is full, and consumers racing with publishers should
// use Latest when they need the authoritative current value.
func (s *Subscription[T]) C() <-chan T {
	return s.ch
}

func (s *Subscription[T]) Close() {
	s.hub.unsubscribe(s)
}

func (h *Hub[T]) unsubscribe(sub *Subscription[T]) {
	h.mu.Lock()
	defer h.mu.Unlock()

	delete(h.subscribers, sub)

	sub.close()
}

func (s *Subscription[T]) close() {
	s.closeOnce.Do(func() {
		close(s.done)
		close(s.ch)
	})
}

func sendLatest[T any](ch chan T, value T) {
	select {
	case ch <- value:
	default:
		select {
		case <-ch:
		default:
		}
		select {
		case ch <- value:
		default:
		}
	}
}
