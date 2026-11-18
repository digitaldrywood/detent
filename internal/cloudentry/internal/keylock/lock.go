package keylock

import (
	"context"
	"sync"

	"golang.org/x/sync/semaphore"
)

type Mutex struct {
	mu    sync.Mutex
	locks map[string]*entryLock
}

type entryLock struct {
	gate  *semaphore.Weighted
	users int
}

func (r *Mutex) Lock(key string) func() {
	unlock, _ := r.LockContext(context.Background(), key)
	return unlock
}

func (r *Mutex) LockContext(ctx context.Context, key string) (func(), error) {
	r.mu.Lock()
	if r.locks == nil {
		r.locks = make(map[string]*entryLock)
	}
	entry := r.locks[key]
	if entry == nil {
		entry = &entryLock{gate: semaphore.NewWeighted(1)}
		r.locks[key] = entry
	}
	entry.users++
	r.mu.Unlock()
	release := func() {
		r.mu.Lock()
		entry.users--
		if entry.users == 0 {
			delete(r.locks, key)
		}
		r.mu.Unlock()
	}
	if err := entry.gate.Acquire(ctx, 1); err != nil {
		release()
		return nil, err
	}
	return func() { entry.gate.Release(1); release() }, nil
}
