package cloudentry

import "sync"

type attachmentLocks struct {
	mu      sync.Mutex
	entries map[string]*attachmentLock
}

type attachmentLock struct {
	sync.RWMutex
	users int
}

func (l *attachmentLocks) lock(organization string, write bool) func() {
	l.mu.Lock()
	if l.entries == nil {
		l.entries = make(map[string]*attachmentLock)
	}
	entry := l.entries[organization]
	if entry == nil {
		entry = &attachmentLock{}
		l.entries[organization] = entry
	}
	entry.users++
	l.mu.Unlock()
	if write {
		entry.Lock()
	} else {
		entry.RLock()
	}
	return func() {
		if write {
			entry.Unlock()
		} else {
			entry.RUnlock()
		}
		l.mu.Lock()
		entry.users--
		if entry.users == 0 {
			delete(l.entries, organization)
		}
		l.mu.Unlock()
	}
}
