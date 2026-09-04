package connector

import (
	"context"
	"time"
)

const (
	GraphQLRateLimitStatusUnknown   = "unknown"
	GraphQLRateLimitStatusBackoff   = "backoff"
	GraphQLRateLimitStatusExhausted = "exhausted"
)

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
	RateLimit       GraphQLRateLimit
	HasRateLimit    bool
	RateLimitStatus string
	QueryCosts      []GraphQLQueryCost
	TotalQueries    int64
	TotalCost       int64
}

type RateLimitReporter interface {
	GraphQLRateLimit() (GraphQLRateLimit, bool)
}

type GraphQLRateLimitUsageReporter interface {
	ResetGraphQLRateLimitUsage()
	FlushGraphQLRateLimitUsage() GraphQLRateLimitUsage
}

type GraphQLRateLimitStatusReporter interface {
	GraphQLRateLimitStatus() string
}

type GraphQLRateLimitProber interface {
	ProbeGraphQLRateLimit(context.Context) (GraphQLRateLimit, error)
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
	CredentialIdentity string
	EndpointFamily     string
	BudgetScope        string
	BudgetGate         string
	Count              int64
	Conditional        int64
	NotModified        int64
	Billable           int64
	Limit              int64
	Used               int64
	Remaining          int64
	Resource           string
	ResetAt            time.Time
	RetryAfter         time.Duration
	RateLimited        bool
	LastStatus         int
}

type RESTRateLimitBudget struct {
	CredentialIdentity string
	EndpointFamily     string
	RateLimit          RESTRateLimit
}

const (
	RESTDivergenceExpectedShared = "expected_shared_credential"
	RESTDivergenceUnattributed   = "unattributed"
)

type RESTUsageDivergence struct {
	CredentialIdentity   string
	Resource             string
	Attribution          string
	ObservedRequests     int64
	DetentRequests       int64
	AttributedRequests   int64
	UnattributedRequests int64
	WindowStartedAt      time.Time
	LastObservedAt       time.Time
	ResetAt              time.Time
	WarningEmitted       bool
}

type RESTRateLimitUsage struct {
	RateLimit           RESTRateLimit
	HasRateLimit        bool
	Requests            []RESTEndpointUsage
	Budgets             []RESTRateLimitBudget
	Divergences         []RESTUsageDivergence
	TotalRequests       int64
	ConditionalRequests int64
	NotModifiedRequests int64
	BillableRequests    int64
	RateLimited         bool
	ReserveHeld         bool
	FanoutDeferred      bool
	BackoffUntil        time.Time
}

type RESTRateLimitUsageReporter interface {
	FlushRESTRateLimitUsage() RESTRateLimitUsage
}

type RESTRateLimitStatusReporter interface {
	RESTRateLimitStatus() RESTRateLimitUsage
}

type RESTRateLimitProber interface {
	ProbeRESTRateLimit(context.Context, int64) (RESTRateLimit, error)
}
