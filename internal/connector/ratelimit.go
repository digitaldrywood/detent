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

type RESTRateLimit struct {
	Limit      int64
	Used       int64
	Remaining  int64
	Resource   string
	ResetAt    time.Time
	RetryAfter time.Duration
	UpdatedAt  time.Time
}

type RESTEndpointUsage struct {
	EndpointFamily string
	Count          int64
	Limit          int64
	Used           int64
	Remaining      int64
	Resource       string
	ResetAt        time.Time
	RetryAfter     time.Duration
	RateLimited    bool
	LastStatus     int
}

type RESTRateLimitUsage struct {
	RateLimit     RESTRateLimit
	HasRateLimit  bool
	Requests      []RESTEndpointUsage
	TotalRequests int64
	RateLimited   bool
	BackoffUntil  time.Time
}

type RESTRateLimitUsageReporter interface {
	FlushRESTRateLimitUsage() RESTRateLimitUsage
}
