package httpratelimit

import (
	"math"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

type Limiter struct {
	mu          sync.Mutex
	entries     map[string]*bucket
	limit       rate.Limit
	limitHeader int
	burst       int
	lastCleanup time.Time
	stopped     bool
}

type bucket struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

func New(requestsPerMinute int, burst int) *Limiter {
	if requestsPerMinute <= 0 {
		requestsPerMinute = 120
	}
	if burst <= 0 {
		burst = requestsPerMinute
	}
	return &Limiter{
		entries:     map[string]*bucket{},
		limit:       rate.Every(time.Minute / time.Duration(requestsPerMinute)),
		limitHeader: requestsPerMinute,
		burst:       burst,
	}
}

func (l *Limiter) Allow(key string, now time.Time) (bool, int) {
	if l == nil {
		return true, 0
	}
	key = strings.TrimSpace(key)
	if key == "" {
		key = "unknown"
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stopped {
		return true, l.burst
	}
	l.cleanupLocked(now)
	entry := l.entries[key]
	if entry == nil {
		entry = &bucket{limiter: rate.NewLimiter(l.limit, l.burst)}
		l.entries[key] = entry
	}
	entry.lastSeen = now
	allowed := entry.limiter.AllowN(now, 1)
	remaining := int(math.Floor(entry.limiter.TokensAt(now)))
	if remaining < 0 {
		remaining = 0
	}
	return allowed, remaining
}

func (l *Limiter) Limit() int {
	if l == nil {
		return 0
	}
	return l.limitHeader
}

func (l *Limiter) Stop() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.stopped = true
	l.entries = nil
}

func (l *Limiter) cleanupLocked(now time.Time) {
	if !l.lastCleanup.IsZero() && now.Sub(l.lastCleanup) < 5*time.Minute {
		return
	}
	l.lastCleanup = now
	cutoff := now.Add(-10 * time.Minute)
	for key, entry := range l.entries {
		if entry.lastSeen.Before(cutoff) {
			delete(l.entries, key)
		}
	}
}
