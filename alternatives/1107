package connector

import "time"

type AuthStatus string

const (
	AuthStatusStale     AuthStatus = "stale"
	AuthStatusRecovered AuthStatus = "recovered"
)

type AuthHealth struct {
	Status          AuthStatus
	LastError       string
	LastErrorAt     time.Time
	LastRecoveredAt time.Time
}

type AuthHealthReporter interface {
	AuthHealth() (AuthHealth, bool)
}
