package connector

import "time"

type GraphQLRateLimit struct {
	Limit      int64
	Used       int64
	Remaining  int64
	Cost       int64
	ResetAt    time.Time
	RetryAfter time.Duration
	UpdatedAt  time.Time
}

type RateLimitReporter interface {
	GraphQLRateLimit() (GraphQLRateLimit, bool)
}
