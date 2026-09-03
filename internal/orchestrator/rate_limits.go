package orchestrator

import (
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func (o *Orchestrator) captureConnectorAuthHealth(state *State) {
	if reporter, ok := o.connector.(connector.AuthHealthReporter); ok {
		if health, ok := reporter.AuthHealth(); ok {
			state.Auth = health
			return
		}
	}
	state.Auth = connector.AuthHealth{}
}

type graphQLRateLimitCycle struct {
	Bucket     *telemetry.RateLimitBucket
	Cost       *telemetry.GraphQLCost
	HasSummary bool
}

type restRateLimitCycle struct {
	Bucket     *telemetry.RateLimitBucket
	Budgets    []telemetry.RESTBudget
	Usage      *telemetry.RESTUsage
	HasSummary bool
}

func (o *Orchestrator) captureConnectorRateLimits(state *State, now time.Time) graphQLRateLimitCycle {
	var usage connector.GraphQLRateLimitUsage
	if reporter, ok := o.connector.(connector.GraphQLRateLimitUsageReporter); ok {
		usage = reporter.FlushGraphQLRateLimitUsage()
	}

	var rateLimit connector.GraphQLRateLimit
	hasRateLimit := usage.HasRateLimit
	status := graphQLRateLimitTelemetryStatus(usage.RateLimitStatus)
	if hasRateLimit {
		rateLimit = usage.RateLimit
	} else if status == "" {
		reporter, ok := o.connector.(connector.RateLimitReporter)
		if !ok {
			return graphQLRateLimitCycle{}
		} else {
			var okRateLimit bool
			rateLimit, okRateLimit = reporter.GraphQLRateLimit()
			if !okRateLimit {
				return graphQLRateLimitCycle{}
			}
			hasRateLimit = okRateLimit
		}
	}

	cost := graphQLCostSummary(usage)
	bucket := gitHubGraphQLBucket(rateLimit, now, status)
	if cost != nil {
		bucket.Cost = cost.TotalCost
	}
	if state.RateLimits == nil {
		state.RateLimits = &telemetry.RateLimits{}
	}
	state.RateLimits.GitHubGraphQL = bucket
	state.RateLimits.GraphQLCost = cost
	return graphQLRateLimitCycle{
		Bucket:     bucket,
		Cost:       cost,
		HasSummary: hasRateLimit,
	}
}

func (o *Orchestrator) captureConnectorRESTRateLimits(state *State, now time.Time) restRateLimitCycle {
	reporter, ok := o.connector.(connector.RESTRateLimitUsageReporter)
	if !ok {
		return restRateLimitCycle{}
	}
	usage := reporter.FlushRESTRateLimitUsage()
	if !usage.HasRateLimit && usage.TotalRequests == 0 && len(usage.Requests) == 0 {
		return restRateLimitCycle{}
	}

	bucket := gitHubRESTBucket(usage, now)
	budgets := restBudgetSummaries(usage.Budgets)
	summary := restUsageSummary(usage)
	if state.RateLimits == nil {
		state.RateLimits = &telemetry.RateLimits{}
	}
	state.RateLimits.GitHubREST = bucket
	state.RateLimits.GitHubRESTBudgets = replaceRESTConsumerBudgets(state.RateLimits.GitHubRESTBudgets, budgets, telemetry.RESTConsumerOrchestrator)
	state.RateLimits.RESTUsage = summary
	return restRateLimitCycle{
		Bucket:     bucket,
		Budgets:    budgets,
		Usage:      summary,
		HasSummary: true,
	}
}

func replaceRESTConsumerBudgets(current []telemetry.RESTBudget, replacement []telemetry.RESTBudget, consumer string) []telemetry.RESTBudget {
	out := make([]telemetry.RESTBudget, 0, len(current)+len(replacement))
	for _, budget := range current {
		budgetConsumer := strings.TrimSpace(budget.Consumer)
		if budgetConsumer == "" {
			budgetConsumer = telemetry.RESTConsumerOrchestrator
		}
		if budgetConsumer != consumer {
			out = append(out, budget)
		}
	}
	return append(out, replacement...)
}

func restUsageSummary(usage connector.RESTRateLimitUsage) *telemetry.RESTUsage {
	if usage.TotalRequests == 0 && len(usage.Requests) == 0 && !usage.RateLimited {
		return nil
	}

	out := &telemetry.RESTUsage{
		TotalRequests:       usage.TotalRequests,
		ConditionalRequests: usage.ConditionalRequests,
		NotModifiedRequests: usage.NotModifiedRequests,
		BillableRequests:    usage.BillableRequests,
		RateLimited:         usage.RateLimited,
		Contributors:        make([]telemetry.RESTUsageContributor, 0, len(usage.Requests)),
	}
	if !usage.BackoffUntil.IsZero() {
		backoffUntil := usage.BackoffUntil
		out.BackoffUntil = &backoffUntil
	}
	for _, request := range usage.Requests {
		contributor := telemetry.RESTUsageContributor{
			Consumer:           telemetry.RESTConsumerOrchestrator,
			CredentialIdentity: request.CredentialIdentity,
			EndpointFamily:     request.EndpointFamily,
			Count:              request.Count,
			Conditional:        request.Conditional,
			NotModified:        request.NotModified,
			Billable:           request.Billable,
			Remaining:          request.Remaining,
			Limit:              request.Limit,
			Resource:           request.Resource,
			RateLimited:        request.RateLimited,
			LastStatus:         request.LastStatus,
		}
		if !request.ResetAt.IsZero() {
			resetAt := request.ResetAt
			contributor.ResetAt = &resetAt
		}
		if request.RetryAfter > 0 {
			contributor.RetryAfterMS = request.RetryAfter.Milliseconds()
		}
		out.Contributors = append(out.Contributors, contributor)
	}
	return out
}

func restBudgetSummaries(budgets []connector.RESTRateLimitBudget) []telemetry.RESTBudget {
	if len(budgets) == 0 {
		return nil
	}
	out := make([]telemetry.RESTBudget, 0, len(budgets))
	for _, budget := range budgets {
		summary := telemetry.RESTBudget{
			Consumer:           telemetry.RESTConsumerOrchestrator,
			CredentialIdentity: budget.CredentialIdentity,
			EndpointFamily:     budget.EndpointFamily,
			Resource:           budget.RateLimit.Resource,
			Remaining:          budget.RateLimit.Remaining,
			Used:               budget.RateLimit.Used,
			Limit:              budget.RateLimit.Limit,
		}
		if !budget.RateLimit.ResetAt.IsZero() {
			resetAt := budget.RateLimit.ResetAt
			summary.ResetAt = &resetAt
		}
		if !budget.RateLimit.UpdatedAt.IsZero() {
			observedAt := budget.RateLimit.UpdatedAt
			summary.ObservedAt = &observedAt
		}
		out = append(out, summary)
	}
	return out
}

func gitHubRESTBucket(usage connector.RESTRateLimitUsage, now time.Time) *telemetry.RateLimitBucket {
	rateLimit := usage.RateLimit
	var resetAt *time.Time
	var observedAt *time.Time
	var resetInSeconds int64
	if !rateLimit.ResetAt.IsZero() {
		value := rateLimit.ResetAt
		resetAt = &value
	}
	if !rateLimit.UpdatedAt.IsZero() {
		value := rateLimit.UpdatedAt
		observedAt = &value
	}
	if rateLimit.RetryAfter > 0 {
		updatedAt := rateLimit.UpdatedAt
		if updatedAt.IsZero() {
			updatedAt = now
		}
		value := updatedAt.Add(rateLimit.RetryAfter)
		resetAt = &value
		resetInSeconds = int64(rateLimit.RetryAfter.Round(time.Second) / time.Second)
	}
	if !usage.BackoffUntil.IsZero() && usage.BackoffUntil.After(now) {
		value := usage.BackoffUntil
		resetAt = &value
		resetInSeconds = int64(usage.BackoffUntil.Sub(now).Round(time.Second) / time.Second)
	}

	return &telemetry.RateLimitBucket{
		Remaining:      rateLimit.Remaining,
		Used:           rateLimit.Used,
		Limit:          rateLimit.Limit,
		Cost:           usage.BillableRequests,
		ResetAt:        resetAt,
		ObservedAt:     observedAt,
		ResetInSeconds: resetInSeconds,
	}
}

func (o *Orchestrator) logRESTRateLimitCycle(cycle restRateLimitCycle) {
	if !cycle.HasSummary || cycle.Bucket == nil {
		return
	}

	var resetAt time.Time
	if cycle.Bucket.ResetAt != nil {
		resetAt = *cycle.Bucket.ResetAt
	}
	totalRequests := int64(0)
	billableRequests := int64(0)
	notModifiedRequests := int64(0)
	rateLimited := false
	var contributors []telemetry.RESTUsageContributor
	if cycle.Usage != nil {
		totalRequests = cycle.Usage.TotalRequests
		billableRequests = cycle.Usage.BillableRequests
		notModifiedRequests = cycle.Usage.NotModifiedRequests
		rateLimited = cycle.Usage.RateLimited
		contributors = cycle.Usage.Contributors
	}

	o.logger.Info(
		"github rest budget summary",
		"consumer", telemetry.RESTConsumerOrchestrator,
		"request_count", totalRequests,
		"billable_request_count", billableRequests,
		"not_modified_request_count", notModifiedRequests,
		"rate_limited", rateLimited,
		"remaining", cycle.Bucket.Remaining,
		"limit", cycle.Bucket.Limit,
		"reset_at", resetAt,
		"credential_budgets", cycle.Budgets,
		"contributors", contributors,
	)
}

func graphQLCostSummary(usage connector.GraphQLRateLimitUsage) *telemetry.GraphQLCost {
	if usage.TotalQueries == 0 && usage.TotalCost == 0 && len(usage.QueryCosts) == 0 {
		return nil
	}

	cost := &telemetry.GraphQLCost{
		TotalQueries: usage.TotalQueries,
		TotalCost:    usage.TotalCost,
		Contributors: make([]telemetry.GraphQLCostContributor, 0, len(usage.QueryCosts)),
	}
	for _, contributor := range usage.QueryCosts {
		cost.Contributors = append(cost.Contributors, telemetry.GraphQLCostContributor{
			QueryType: contributor.QueryType,
			Count:     contributor.Count,
			Cost:      contributor.Cost,
		})
	}
	return cost
}

func (o *Orchestrator) logGraphQLRateLimitCycle(cycle graphQLRateLimitCycle) {
	if !cycle.HasSummary || cycle.Bucket == nil {
		return
	}

	var resetAt time.Time
	if cycle.Bucket.ResetAt != nil {
		resetAt = *cycle.Bucket.ResetAt
	}
	cycleCost := int64(0)
	queryCount := int64(0)
	var contributors []telemetry.GraphQLCostContributor
	if cycle.Cost != nil {
		cycleCost = cycle.Cost.TotalCost
		queryCount = cycle.Cost.TotalQueries
		contributors = cycle.Cost.Contributors
	}

	o.logger.Debug(
		"github graphql budget summary",
		"cycle_cost", cycleCost,
		"query_count", queryCount,
		"remaining", cycle.Bucket.Remaining,
		"limit", cycle.Bucket.Limit,
		"reset_at", resetAt,
		"contributors", contributors,
	)

	if cycle.Bucket.Remaining < o.cfg.GitHubGraphQLWarnRemaining {
		o.logger.Warn(
			"github graphql budget below warning floor",
			"remaining", cycle.Bucket.Remaining,
			"warning_floor", o.cfg.GitHubGraphQLWarnRemaining,
			"limit", cycle.Bucket.Limit,
			"reset_at", resetAt,
		)
	}
}

func graphQLRateLimitTelemetryStatus(status string) string {
	switch strings.TrimSpace(status) {
	case connector.GraphQLRateLimitStatusUnknown:
		return telemetry.RateLimitStatusUnknown
	case connector.GraphQLRateLimitStatusExhausted:
		return telemetry.RateLimitStatusExhausted
	case connector.GraphQLRateLimitStatusBackoff:
		return telemetry.RateLimitStatusBackoff
	default:
		return ""
	}
}

func gitHubGraphQLBucket(rateLimit connector.GraphQLRateLimit, now time.Time, status string) *telemetry.RateLimitBucket {
	var resetAt *time.Time
	var observedAt *time.Time
	var resetInSeconds int64
	if !rateLimit.ResetAt.IsZero() {
		value := rateLimit.ResetAt
		resetAt = &value
	}
	if !rateLimit.UpdatedAt.IsZero() {
		value := rateLimit.UpdatedAt
		observedAt = &value
	}
	if rateLimit.RetryAfter > 0 {
		updatedAt := rateLimit.UpdatedAt
		if updatedAt.IsZero() {
			updatedAt = now
		}
		value := updatedAt.Add(rateLimit.RetryAfter)
		resetAt = &value
		resetInSeconds = int64(rateLimit.RetryAfter.Round(time.Second) / time.Second)
	}
	if status == "" {
		status = graphQLRateLimitStatusFromSnapshot(rateLimit)
	}

	return &telemetry.RateLimitBucket{
		Remaining:      rateLimit.Remaining,
		Used:           rateLimit.Used,
		Limit:          rateLimit.Limit,
		Cost:           rateLimit.Cost,
		Status:         status,
		ResetAt:        resetAt,
		ObservedAt:     observedAt,
		ResetInSeconds: resetInSeconds,
	}
}

func graphQLRateLimitStatusFromSnapshot(rateLimit connector.GraphQLRateLimit) string {
	if rateLimit.Limit > 0 && rateLimit.Remaining <= 0 {
		return telemetry.RateLimitStatusExhausted
	}
	if rateLimit.RetryAfter > 0 {
		return telemetry.RateLimitStatusBackoff
	}
	return ""
}

func (o *Orchestrator) adaptivePollInterval(state *State, now time.Time) time.Duration {
	base := o.cfg.PollInterval
	if base <= 0 {
		base = defaultPollInterval
	}
	if o.scheduling != nil {
		source := state.RefreshSources[telemetry.RefreshSourceCandidates]
		if source.Condition == schedulingUnavailableCondition && source.FailureStreak > 0 {
			return schedulingBackoffInterval(base, source.FailureStreak)
		}
		return dispatchRecoveryPollInterval(state, now, base)
	}

	if pause := o.gitHubGraphQLPause(state, now); pause > base {
		return pause
	}
	if pause := o.gitHubRESTPause(state, now); pause > base {
		return pause
	}
	bucket := gitHubGraphQLBucketFromState(state)
	if bucket == nil || bucket.Remaining <= 0 || bucket.Remaining >= gitHubGraphQLBackoffRemaining {
		return dispatchRecoveryPollInterval(state, now, base)
	}

	multiplier := int64(gitHubGraphQLBackoffRemaining) / bucket.Remaining
	if int64(gitHubGraphQLBackoffRemaining)%bucket.Remaining != 0 {
		multiplier++
	}
	if multiplier < 2 {
		multiplier = 2
	}
	return dispatchRecoveryPollInterval(state, now, base*time.Duration(multiplier))
}

func schedulingBackoffInterval(base time.Duration, failureStreak int) time.Duration {
	if base <= 0 || failureStreak <= 0 {
		return base
	}
	shift := min(failureStreak, 4)
	interval := base * time.Duration(1<<shift)
	maximum := 5 * time.Minute
	if maximum < base {
		maximum = base
	}
	if interval > maximum {
		return maximum
	}
	return interval
}

func (o *Orchestrator) gitHubRESTPause(state *State, now time.Time) time.Duration {
	bucket := gitHubRESTBucketFromState(state)
	if bucket == nil || bucket.ResetAt == nil {
		return 0
	}
	if bucket.ResetInSeconds > 0 && bucket.ResetAt.After(now) {
		return bucket.ResetAt.Sub(now)
	}
	if bucket.Remaining > 0 {
		return 0
	}
	if !bucket.ResetAt.After(now) {
		return 0
	}
	return bucket.ResetAt.Sub(now)
}

func gitHubRESTBucketFromState(state *State) *telemetry.RateLimitBucket {
	if state.RateLimits == nil {
		return nil
	}
	return state.RateLimits.GitHubREST
}

func gitHubRESTRemaining(state *State) int64 {
	bucket := gitHubRESTBucketFromState(state)
	if bucket == nil {
		return 0
	}
	return bucket.Remaining
}

func (o *Orchestrator) gitHubGraphQLPause(state *State, now time.Time) time.Duration {
	bucket := gitHubGraphQLBucketFromState(state)
	if bucket == nil || bucket.ResetAt == nil {
		return 0
	}
	if bucket.ResetInSeconds > 0 && bucket.ResetAt.After(now) {
		return bucket.ResetAt.Sub(now)
	}
	if bucket.Remaining >= gitHubGraphQLPauseRemaining {
		return 0
	}
	if !bucket.ResetAt.After(now) {
		return 0
	}
	return bucket.ResetAt.Sub(now)
}

func gitHubGraphQLBucketFromState(state *State) *telemetry.RateLimitBucket {
	if state.RateLimits == nil {
		return nil
	}
	return state.RateLimits.GitHubGraphQL
}

func gitHubGraphQLRemaining(state *State) int64 {
	bucket := gitHubGraphQLBucketFromState(state)
	if bucket == nil {
		return 0
	}
	return bucket.Remaining
}
