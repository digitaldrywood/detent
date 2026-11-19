package hubserver

import "time"

func newRelayAuthorityTicker(interval time.Duration) *time.Ticker {
	return time.NewTicker(interval)
}
