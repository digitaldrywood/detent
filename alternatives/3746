package hubserver

import (
	"context"
	"errors"
)

func (d *database) readTimeoutAttributes(err error) []any {
	if d == nil || d.postgres == nil || !errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	stats := d.postgres.Stats()
	return []any{"read_timeout_cause", "read_deadline", "pool_full", stats.AcquiredConns() == stats.MaxConns(),
		"pool_in_use", stats.AcquiredConns(), "pool_max", stats.MaxConns(),
		"pool_wait_count", stats.EmptyAcquireCount(), "pool_wait_ms", stats.EmptyAcquireWaitTime().Milliseconds(),
		"pool_canceled_acquires", stats.CanceledAcquireCount()}
}
