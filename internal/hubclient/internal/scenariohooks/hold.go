package scenariohooks

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
)

type Hold struct {
	ready                 func() bool
	armed                 atomic.Bool
	held, release         chan struct{}
	heldOnce, releaseOnce sync.Once
}

func New(ready func() bool) *Hold {
	return &Hold{ready: ready, held: make(chan struct{}), release: make(chan struct{})}
}

func (h *Hold) Arm() { h.armed.Store(true) }

func (h *Hold) Release() { h.releaseOnce.Do(func() { close(h.release) }) }

func (h *Hold) Wait(ctx context.Context) {
	if h == nil || !h.armed.Load() || !h.ready() {
		return
	}
	holding := false
	h.heldOnce.Do(func() {
		holding = true
		close(h.held)
	})
	if !holding {
		return
	}
	select {
	case <-h.release:
	case <-ctx.Done():
	}
}

func (h *Hold) AwaitHeld(t *testing.T, ctx context.Context) {
	t.Helper()
	select {
	case <-h.held:
	case <-ctx.Done():
		t.Fatal("completion never refilled its released slot")
	}
}
