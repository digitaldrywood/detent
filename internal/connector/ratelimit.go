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

type GraphQLQueryCost struct {
	QueryType string
	Count     int64
	Cost      int64
}

type GraphQLRateLimitUsage struct {
	RateLimit    GraphQLRateLimit
	HasRateLimit bool
	QueryCosts   []GraphQLQueryCost
	TotalQueries int64
	TotalCost    int64
}

type RateLimitReporter interface {
	GraphQLRateLimit() (GraphQLRateLimit, bool)
}

type GraphQLRateLimitUsageReporter interface {
	ResetGraphQLRateLimitUsage()
	FlushGraphQLRateLimitUsage() GraphQLRateLimitUsage
}
