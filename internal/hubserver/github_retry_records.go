package hubserver

import "time"

func gitHubRetryDelay(attempt int64, base, maximum time.Duration, jitter float64) time.Duration {
	delay := min(base, maximum)
	for range min(max(attempt-1, 0), 63) {
		if delay >= maximum/2 {
			delay = maximum
			break
		}
		delay *= 2
	}
	return time.Duration(float64(delay) * (0.5 + 0.5*min(1, max(0, jitter))))
}
