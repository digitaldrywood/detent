package store

import (
	"context"
	"time"
)

// CardHistory contains durable facts that must not depend on runtime history limits.
// Days use UTC, consistently across the dashboard and API.
type CardHistory struct {
	AttemptsToday int64
	LaneReason    string
	LaneReasonAt  *time.Time
}

type CardHistoryStore interface {
	IssueCardHistory(context.Context, IssueIdentity, time.Time) (CardHistory, error)
}
