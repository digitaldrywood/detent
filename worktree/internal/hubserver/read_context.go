package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

const hubReadTimeout = 15 * time.Second

func readTimeoutCause(stats sql.DBStats) string {
	if stats.MaxOpenConnections > 0 && stats.InUse >= stats.MaxOpenConnections {
		return "reader_pool_exhausted"
	}
	return "slow_read"
}

func (d *database) readTimeoutAttributes(err error) []any {
	if d == nil || d.reader == nil || !errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	stats := d.reader.Stats()
	return []any{"read_timeout_cause", readTimeoutCause(stats), "reader_in_use", stats.InUse, "reader_max", stats.MaxOpenConnections,
		"reader_wait_count", stats.WaitCount, "reader_wait_ms", stats.WaitDuration.Milliseconds()}
}
