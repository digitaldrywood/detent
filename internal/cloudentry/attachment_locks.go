package cloudentry

import (
	"context"
	"sync"

	"github.com/digitaldrywood/detent/internal/cloudentry/platformstore"
)

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

func (s *Service) lockAttachmentObject(ctx context.Context, key string) (*platformstore.EntryTransaction, error) {
	return s.registry.store.db.postgres.LockAttachment(ctx, entryWorkload(ctx), key)
}

func (s *Service) unlockAttachmentObject(ctx context.Context, g *platformstore.EntryTransaction) {
	if err := g.Rollback(ctx); err != nil {
		s.config.Logger.Error("release attachment object lock", "error", err)
	}
}
