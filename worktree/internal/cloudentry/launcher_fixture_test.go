package cloudentry

import (
	"context"
	"sync"
)

type silentLauncher struct {
	mu       sync.Mutex
	err      error
	failure  error
	running  map[string]bool
	launches int
	stops    int
}

func (l *silentLauncher) Start(_ context.Context, spec TenantSpec) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil || l.running[spec.Organization.ID] {
		return l.err
	}
	l.running[spec.Organization.ID] = true
	l.launches++
	return nil
}

func (l *silentLauncher) Stop(id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.running[id] {
		l.stops++
	}
	delete(l.running, id)
	return nil
}

func (l *silentLauncher) Close() error { return nil }

func (l *silentLauncher) Failure(id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.running[id] {
		return nil
	}
	return l.failure
}

func (l *silentLauncher) counts() (launches, stops, running int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.launches, l.stops, len(l.running)
}
