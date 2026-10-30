package hubhealth

import "time"

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func formatHubTime(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }
