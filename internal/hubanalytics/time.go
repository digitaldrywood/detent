package hubanalytics

import "time"

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func formatHubTime(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }
