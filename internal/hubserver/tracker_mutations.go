package hubserver

import (
	"time"
)

func (d *database) currentTime() (time.Time, error) {
	now := d.now().UTC()
	if now.IsZero() {
		return time.Time{}, ErrInvalidClock
	}
	return now, nil
}

func formatHubTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}
