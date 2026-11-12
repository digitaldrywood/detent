package hubtest

import (
	"sync"
	"time"
)

type Clock struct {
	mu  sync.Mutex
	now time.Time
}

func NewClock(start time.Time) *Clock {
	return &Clock{now: start.UTC().Truncate(time.Microsecond)}
}

func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *Clock) Set(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now.UTC().Truncate(time.Microsecond)
}

func (c *Clock) Advance(step time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(step)
	return c.now
}
