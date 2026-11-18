package keylock

import "sync"

type Mutex struct {
	mu    sync.Mutex
	locks map[string]*entryLock
}

type entryLock struct {
	sync.Mutex
	users int
}

func (r *Mutex) Lock(key string) func() {
	r.mu.Lock()
	if r.locks == nil {
		r.locks = make(map[string]*entryLock)
	}
	entry := r.locks[key]
	if entry == nil {
		entry = &entryLock{}
		r.locks[key] = entry
	}
	entry.users++
	r.mu.Unlock()
	entry.Lock()
	return func() {
		entry.Unlock()
		r.mu.Lock()
		entry.users--
		if entry.users == 0 {
			delete(r.locks, key)
		}
		r.mu.Unlock()
	}
}
