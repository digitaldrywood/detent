package orchestrator

import (
	"context"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/backendcapacity"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const (
	githubLookupBackoffKind            = "github_lookup_backoff"
	githubLookupTriggerGraphQL         = "github_graphql_rate_limit"
	githubLookupTriggerREST            = "github_rest_rate_limit"
	githubLookupTriggerProvider        = "provider_capacity_outage"
	githubLookupBackoffInitial         = 30 * time.Second
	githubLookupBackoffMaximum         = 15 * time.Minute
	githubLookupBackoffJitterDivisor   = 5
	githubLookupProbeResultBackingOff  = "backing_off"
	githubLookupProbeResultProviderDue = "provider_probe_due"
)

var githubLookupBackoffScope = backendcapacity.Scope{
	BackendID:   "github-lookups",
	BackendKind: "tracker",
	Provider:    "github",
}

type githubLookupSignal struct {
	trigger string
	reason  string
	resetAt time.Time
}

func (o *Orchestrator) githubLookupBackoffGate(ctx context.Context, state *State, now time.Time) bool {
	if state == nil {
		return false
	}
	if now.IsZero() {
		now = o.clockNow()
	}
	provider, providerActive := activeProviderCapacityOutage(state.BackendOutages)
	key, outage, active := githubLookupBackoff(state.BackendOutages)
	signal, signaled := o.currentGitHubLookupSignal(state, now)

	if !active {
		switch {
		case signaled:
			o.advanceGitHubLookupBackoff(state, BackendOutage{}, signal, now, time.Time{})
			return true
		case providerActive:
			o.advanceGitHubLookupBackoff(state, BackendOutage{}, providerGitHubLookupSignal(provider), now, providerCapacityProbeAt(provider))
			return true
		default:
			return false
		}
	}

	if signaled && outage.Trigger == githubLookupTriggerProvider {
		o.advanceGitHubLookupBackoff(state, outage, signal, now, time.Time{})
		return true
	}
	if outage.Trigger == githubLookupTriggerProvider {
		if !providerActive {
			o.recoverGitHubLookupBackoff(state, key, outage, now, "provider capacity recovered")
			return false
		}
		if now.Before(outage.NextProbeAt) {
			return true
		}
		providerProbeAt := providerCapacityProbeAt(provider)
		if strings.TrimSpace(provider.ProbeIssueID) != "" || providerProbeAt.IsZero() || now.Before(providerProbeAt) {
			o.advanceGitHubLookupBackoff(state, outage, providerGitHubLookupSignal(provider), now, providerProbeAt)
			return true
		}
		outage.LastProbeAt = now.UTC()
		outage.LastProbeResult = githubLookupProbeResultProviderDue
		outage.LastProbeDetail = "provider capacity canary is due"
		state.BackendOutages[key] = outage
		return false
	}

	if now.Before(outage.NextProbeAt) {
		return true
	}
	if outage.Trigger == githubLookupTriggerREST {
		return o.probeGitHubRESTLookupBackoff(ctx, state, key, outage, now)
	}
	prober, ok := o.connector.(connector.GraphQLRateLimitProber)
	if !ok {
		signal.reason = "GitHub GraphQL rate-limit probe is unavailable"
		if signal.trigger == "" {
			signal.trigger = outage.Trigger
		}
		o.advanceGitHubLookupBackoff(state, outage, signal, now, time.Time{})
		return true
	}

	outage.LastProbeAt = now.UTC()
	rateLimit, err := prober.ProbeGraphQLRateLimit(ctx)
	if err != nil {
		signal = githubLookupSignal{
			trigger: outage.Trigger,
			reason:  "GitHub GraphQL rate-limit probe failed: " + err.Error(),
			resetAt: outage.ResetAt,
		}
		o.advanceGitHubLookupBackoff(state, outage, signal, now, time.Time{})
		return true
	}
	o.captureGitHubLookupProbe(state, rateLimit, now)
	if rateLimit.Limit <= 0 || rateLimit.Remaining <= 0 {
		signal = githubLookupSignal{
			trigger: githubLookupTriggerGraphQL,
			reason: fmt.Sprintf(
				"GitHub GraphQL has no available capacity (remaining %d)",
				rateLimit.Remaining,
			),
			resetAt: rateLimit.ResetAt,
		}
		o.advanceGitHubLookupBackoff(state, outage, signal, now, time.Time{})
		return true
	}

	outage.LastProbeResult = "capacity_available"
	outage.LastProbeDetail = fmt.Sprintf("GitHub GraphQL probe reports %d remaining", rateLimit.Remaining)
	if signal, active := o.currentGitHubLookupSignal(state, now); active {
		o.advanceGitHubLookupBackoff(state, outage, signal, now, time.Time{})
		return true
	}
	o.recoverGitHubLookupBackoff(state, key, outage, now, outage.LastProbeDetail)
	return false
}

func (o *Orchestrator) probeGitHubRESTLookupBackoff(
	ctx context.Context,
	state *State,
	key string,
	outage BackendOutage,
	now time.Time,
) bool {
	prober, ok := o.connector.(connector.RESTRateLimitProber)
	if !ok {
		o.advanceGitHubLookupBackoff(state, outage, githubLookupSignal{
			trigger: githubLookupTriggerREST,
			reason:  "GitHub REST rate-limit probe is unavailable",
			resetAt: outage.ResetAt,
		}, now, time.Time{})
		return true
	}

	outage.LastProbeAt = now.UTC()
	rateLimit, err := prober.ProbeRESTRateLimit(ctx, 0)
	if err != nil {
		resetAt := outage.ResetAt
		if signal, active := o.currentGitHubLookupSignal(state, now); active && signal.trigger == githubLookupTriggerREST && signal.resetAt.After(resetAt) {
			resetAt = signal.resetAt
		}
		o.advanceGitHubLookupBackoff(state, outage, githubLookupSignal{
			trigger: githubLookupTriggerREST,
			reason:  "GitHub REST rate-limit probe failed: " + err.Error(),
			resetAt: resetAt,
		}, now, time.Time{})
		return true
	}
	o.captureGitHubRESTLookupProbe(state, rateLimit, now)
	if rateLimit.Limit <= 0 || rateLimit.Remaining <= 0 {
		o.advanceGitHubLookupBackoff(state, outage, githubLookupSignal{
			trigger: githubLookupTriggerREST,
			reason: fmt.Sprintf(
				"GitHub REST has no available capacity (remaining %d)",
				rateLimit.Remaining,
			),
			resetAt: rateLimit.ResetAt,
		}, now, time.Time{})
		return true
	}

	outage.LastProbeResult = "capacity_available"
	outage.LastProbeDetail = fmt.Sprintf("GitHub REST recovery read confirms %d remaining", rateLimit.Remaining)
	if signal, active := o.currentGitHubLookupSignal(state, now); active {
		o.advanceGitHubLookupBackoff(state, outage, signal, now, time.Time{})
		return true
	}
	o.recoverGitHubLookupBackoff(state, key, outage, now, outage.LastProbeDetail)
	return false
}

func (o *Orchestrator) syncGitHubLookupBackoff(state *State, now time.Time) {
	if state == nil {
		return
	}
	if _, _, active := githubLookupBackoff(state.BackendOutages); active {
		return
	}
	if signal, ok := o.currentGitHubLookupSignal(state, now); ok {
		o.advanceGitHubLookupBackoff(state, BackendOutage{}, signal, now, time.Time{})
		return
	}
	if provider, ok := activeProviderCapacityOutage(state.BackendOutages); ok {
		o.advanceGitHubLookupBackoff(state, BackendOutage{}, providerGitHubLookupSignal(provider), now, providerCapacityProbeAt(provider))
	}
}

func (o *Orchestrator) observeGitHubLookupBackoff(state *State, now time.Time) bool {
	_, outage, active := githubLookupBackoff(state.BackendOutages)
	if active {
		if signal, signaled := o.currentGitHubLookupSignal(state, now); signaled && outage.Trigger == githubLookupTriggerProvider {
			o.advanceGitHubLookupBackoff(state, outage, signal, now, time.Time{})
			return true
		}
		return outage.Trigger != githubLookupTriggerProvider || outage.LastProbeResult != githubLookupProbeResultProviderDue
	}
	signal, ok := o.currentGitHubLookupSignal(state, now)
	if !ok {
		return false
	}
	o.advanceGitHubLookupBackoff(state, BackendOutage{}, signal, now, time.Time{})
	return true
}

func (o *Orchestrator) finalizeGitHubLookupProviderProbe(state *State, now time.Time) {
	key, outage, active := githubLookupBackoff(state.BackendOutages)
	if !active || outage.Trigger != githubLookupTriggerProvider || outage.LastProbeResult != githubLookupProbeResultProviderDue {
		return
	}
	provider, providerActive := activeProviderCapacityOutage(state.BackendOutages)
	if !providerActive {
		o.recoverGitHubLookupBackoff(state, key, outage, now, "provider capacity recovered")
		return
	}
	o.advanceGitHubLookupBackoff(state, outage, providerGitHubLookupSignal(provider), now, providerCapacityProbeAt(provider))
}

func (o *Orchestrator) currentGitHubLookupSignal(state *State, now time.Time) (githubLookupSignal, bool) {
	if reporter, ok := o.connector.(connector.GraphQLRateLimitStatusReporter); ok {
		switch reporter.GraphQLRateLimitStatus() {
		case connector.GraphQLRateLimitStatusBackoff, connector.GraphQLRateLimitStatusExhausted:
			return githubLookupSignal{
				trigger: githubLookupTriggerGraphQL,
				reason:  "GitHub GraphQL returned a rate-limit response",
			}, true
		}
	}
	if reporter, ok := o.connector.(connector.RateLimitReporter); ok {
		if rateLimit, exists := reporter.GraphQLRateLimit(); exists {
			if graphQLLookupExhausted(rateLimit, now) {
				o.captureGitHubLookupProbe(state, rateLimit, now)
				return githubLookupSignal{
					trigger: githubLookupTriggerGraphQL,
					reason: fmt.Sprintf(
						"GitHub GraphQL exhausted (remaining %d)",
						rateLimit.Remaining,
					),
					resetAt: rateLimit.ResetAt,
				}, true
			}
		}
	}
	if reporter, ok := o.connector.(connector.RESTRateLimitStatusReporter); ok {
		usage := reporter.RESTRateLimitStatus()
		if usage.BackoffUntil.After(now) {
			return githubLookupSignal{
				trigger: githubLookupTriggerREST,
				reason:  "GitHub REST returned a rate-limit response",
				resetAt: usage.BackoffUntil,
			}, true
		}
		if resetAt, rateLimited, hasFamilyRequests := restLookupRateLimitedForUsage(usage.Requests, now); rateLimited {
			return githubLookupSignal{
				trigger: githubLookupTriggerREST,
				reason:  "GitHub REST returned a rate-limit response for candidate lookup",
				resetAt: resetAt,
			}, true
		} else if usage.RateLimited && !hasFamilyRequests {
			return githubLookupSignal{
				trigger: githubLookupTriggerREST,
				reason:  "GitHub REST returned a rate-limit response",
			}, true
		}
		if rateLimit, exceeded := restLookupExhaustedForUsage(usage, now); exceeded {
			return githubLookupSignal{
				trigger: githubLookupTriggerREST,
				reason: fmt.Sprintf(
					"GitHub REST exhausted (remaining %d)",
					rateLimit.Remaining,
				),
				resetAt: rateLimit.ResetAt,
			}, true
		}
	}
	if state != nil && state.RateLimits != nil {
		bucket := state.RateLimits.GitHubGraphQL
		if bucket != nil && (bucket.Status == telemetry.RateLimitStatusBackoff || bucket.Status == telemetry.RateLimitStatusExhausted) {
			return githubLookupSignal{
				trigger: githubLookupTriggerGraphQL,
				reason:  "GitHub GraphQL returned a rate-limit response",
				resetAt: rateLimitBucketResetAt(bucket),
			}, true
		}
		if lookupBucketExhausted(bucket, now) {
			return githubLookupSignal{
				trigger: githubLookupTriggerGraphQL,
				reason: fmt.Sprintf(
					"GitHub GraphQL exhausted (remaining %d)",
					bucket.Remaining,
				),
				resetAt: rateLimitBucketResetAt(bucket),
			}, true
		}
		usage := state.RateLimits.RESTUsage
		if usage != nil && usage.BackoffUntil != nil && usage.BackoffUntil.After(now) {
			return githubLookupSignal{
				trigger: githubLookupTriggerREST,
				reason:  "GitHub REST returned a rate-limit response",
				resetAt: *usage.BackoffUntil,
			}, true
		}
		if resetAt, rateLimited, hasFamilyRequests := restLookupRateLimitedForContributors(usage, now); rateLimited {
			return githubLookupSignal{
				trigger: githubLookupTriggerREST,
				reason:  "GitHub REST returned a rate-limit response for candidate lookup",
				resetAt: resetAt,
			}, true
		} else if usage != nil && usage.RateLimited && !hasFamilyRequests {
			return githubLookupSignal{
				trigger: githubLookupTriggerREST,
				reason:  "GitHub REST returned a rate-limit response",
				resetAt: rateLimitBucketResetAt(state.RateLimits.GitHubREST),
			}, true
		}
		budget, exceeded, hasFamilyBudgets := restLookupBudgetExhausted(state.RateLimits.GitHubRESTBudgets, now)
		rest := state.RateLimits.GitHubREST
		if exceeded ||
			(!hasFamilyBudgets && lookupBucketExhausted(rest, now)) {
			resetAt := rateLimitBucketResetAt(rest)
			if exceeded && budget.ResetAt != nil {
				resetAt = *budget.ResetAt
			}
			return githubLookupSignal{
				trigger: githubLookupTriggerREST,
				reason:  "GitHub REST exhausted",
				resetAt: resetAt,
			}, true
		}
	}
	return githubLookupSignal{}, false
}

func restLookupRateLimitedForUsage(requests []connector.RESTEndpointUsage, now time.Time) (time.Time, bool, bool) {
	if len(requests) == 0 {
		return time.Time{}, false, false
	}
	for _, request := range requests {
		if !request.RateLimited || !githubRESTCandidateLookupEndpointFamily(request.EndpointFamily) {
			continue
		}
		resetAt := request.ResetAt
		if request.RetryAfter > 0 {
			retryAt := now.Add(request.RetryAfter)
			if retryAt.After(resetAt) {
				resetAt = retryAt
			}
		}
		return resetAt, true, true
	}
	return time.Time{}, false, true
}

func restLookupRateLimitedForContributors(usage *telemetry.RESTUsage, now time.Time) (time.Time, bool, bool) {
	if usage == nil || len(usage.Contributors) == 0 {
		return time.Time{}, false, false
	}
	for _, contributor := range usage.Contributors {
		consumer := strings.TrimSpace(contributor.Consumer)
		if consumer != "" && consumer != telemetry.RESTConsumerOrchestrator {
			continue
		}
		if !contributor.RateLimited || !githubRESTCandidateLookupEndpointFamily(contributor.EndpointFamily) {
			continue
		}
		var resetAt time.Time
		if contributor.ResetAt != nil {
			resetAt = *contributor.ResetAt
		}
		if contributor.RetryAfterMS > 0 {
			retryAt := now.Add(time.Duration(contributor.RetryAfterMS) * time.Millisecond)
			if retryAt.After(resetAt) {
				resetAt = retryAt
			}
		}
		return resetAt, true, true
	}
	return time.Time{}, false, true
}

func restLookupExhaustedForUsage(usage connector.RESTRateLimitUsage, now time.Time) (connector.RESTRateLimit, bool) {
	if len(usage.Budgets) == 0 {
		return usage.RateLimit, restLookupExhausted(usage.RateLimit, usage.HasRateLimit, now)
	}
	for _, budget := range usage.Budgets {
		if !githubRESTCandidateLookupEndpointFamily(budget.EndpointFamily) {
			continue
		}
		if restLookupExhausted(budget.RateLimit, true, now) {
			return budget.RateLimit, true
		}
	}
	return connector.RESTRateLimit{}, false
}

func restLookupBudgetExhausted(budgets []telemetry.RESTBudget, now time.Time) (telemetry.RESTBudget, bool, bool) {
	hasOrchestratorBudgets := false
	for _, budget := range budgets {
		consumer := strings.TrimSpace(budget.Consumer)
		if consumer != "" && consumer != telemetry.RESTConsumerOrchestrator {
			continue
		}
		hasOrchestratorBudgets = true
		if !githubRESTCandidateLookupEndpointFamily(budget.EndpointFamily) {
			continue
		}
		if budget.Limit > 0 && budget.Remaining <= 0 && (budget.ResetAt == nil || !now.After(budget.ResetAt.Add(githubRateLimitResetSkew))) {
			return budget, true, true
		}
	}
	return telemetry.RESTBudget{}, false, hasOrchestratorBudgets
}

func githubRESTCandidateLookupEndpointFamily(family string) bool {
	switch strings.TrimSpace(family) {
	case "label issues", "repository issues", "search":
		return true
	default:
		return false
	}
}

func graphQLLookupExhausted(rateLimit connector.GraphQLRateLimit, now time.Time) bool {
	return rateLimit.Limit > 0 &&
		rateLimit.Remaining <= 0 &&
		(rateLimit.ResetAt.IsZero() || !now.After(rateLimit.ResetAt.Add(githubRateLimitResetSkew)))
}

func restLookupExhausted(rateLimit connector.RESTRateLimit, hasRateLimit bool, now time.Time) bool {
	return hasRateLimit &&
		rateLimit.Limit > 0 &&
		rateLimit.Remaining <= 0 &&
		(rateLimit.ResetAt.IsZero() || !now.After(rateLimit.ResetAt.Add(githubRateLimitResetSkew)))
}

func rateLimitBucketResetAt(bucket *telemetry.RateLimitBucket) time.Time {
	if bucket == nil || bucket.ResetAt == nil {
		return time.Time{}
	}
	return bucket.ResetAt.UTC()
}

func (o *Orchestrator) advanceGitHubLookupBackoff(
	state *State,
	existing BackendOutage,
	signal githubLookupSignal,
	now time.Time,
	probeDeadline time.Time,
) BackendOutage {
	if state.BackendOutages == nil {
		state.BackendOutages = map[string]BackendOutage{}
	}
	attempt := existing.ProbeAttempts + 1
	delay := githubLookupBackoffDelay(attempt, o.cfg.Project.ID+"\x00"+signal.trigger)
	nextProbeAt := now.Add(delay).UTC()
	if probeDeadline.After(now) && probeDeadline.Before(nextProbeAt) {
		nextProbeAt = probeDeadline.UTC()
		delay = nextProbeAt.Sub(now)
	}
	detectedAt := existing.DetectedAt
	if detectedAt.IsZero() {
		detectedAt = now.UTC()
	}
	resetAt := signal.resetAt.UTC()
	if resetAt.IsZero() {
		resetAt = existing.ResetAt
	}
	outage := BackendOutage{
		Scope:           githubLookupBackoffScope,
		Kind:            githubLookupBackoffKind,
		Reason:          strings.TrimSpace(signal.reason),
		Trigger:         strings.TrimSpace(signal.trigger),
		DetectedAt:      detectedAt,
		LastObservedAt:  now.UTC(),
		ResetAt:         resetAt,
		ResumeAt:        nextProbeAt,
		NextProbeAt:     nextProbeAt,
		LastProbeAt:     existing.LastProbeAt,
		LastProbeResult: githubLookupProbeResultBackingOff,
		LastProbeDetail: strings.TrimSpace(signal.reason),
		ProbeAttempts:   attempt,
	}
	state.BackendOutages[githubLookupBackoffScope.Key()] = outage
	o.markDispatchRecoveryWait(state, dispatchRecoveryGitHubLookup, outage.Reason, nextProbeAt, now)
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      now,
		Event:   "github_lookup_backoff_scheduled",
		Message: fmt.Sprintf("GitHub lookups backed off after %s for %s; next probe at %s", outage.Trigger, delay, nextProbeAt.Format(time.RFC3339)),
	})
	if o.logger != nil {
		o.logger.Warn(
			"github lookup backoff scheduled",
			"project_id", o.cfg.Project.ID,
			"trigger", outage.Trigger,
			"delay", delay,
			"next_probe_at", nextProbeAt,
			"backoff_step", attempt,
			"reason", outage.Reason,
		)
	}
	return outage
}

func (o *Orchestrator) recoverGitHubLookupBackoff(state *State, key string, outage BackendOutage, now time.Time, detail string) {
	delete(state.BackendOutages, key)
	o.activateDispatchRecovery(state, dispatchRecoveryGitHubLookup, outage.Reason, now, "")
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      now,
		Event:   "github_lookup_backoff_recovered",
		Message: "GitHub lookup backoff recovered: " + strings.TrimSpace(detail),
	})
	if o.logger != nil {
		o.logger.Info(
			"github lookup backoff recovered",
			"project_id", o.cfg.Project.ID,
			"trigger", outage.Trigger,
			"backoff_steps", outage.ProbeAttempts,
			"recovered_at", now,
			"detail", strings.TrimSpace(detail),
		)
	}
}

func (o *Orchestrator) captureGitHubLookupProbe(state *State, rateLimit connector.GraphQLRateLimit, now time.Time) {
	if state.RateLimits == nil {
		state.RateLimits = &telemetry.RateLimits{}
	}
	state.RateLimits.GitHubGraphQL = gitHubGraphQLBucket(rateLimit, now, "")
}

func (o *Orchestrator) captureGitHubRESTLookupProbe(state *State, rateLimit connector.RESTRateLimit, now time.Time) {
	if state.RateLimits == nil {
		state.RateLimits = &telemetry.RateLimits{}
	}
	if bucket := gitHubRESTBucket(connector.RESTRateLimitUsage{
		RateLimit:    rateLimit,
		HasRateLimit: true,
	}, now); bucket != nil {
		state.RateLimits.GitHubREST = bucket
	}
	state.RateLimits.RESTUsage = nil
}

func githubLookupBackoff(outages map[string]BackendOutage) (string, BackendOutage, bool) {
	for key, outage := range outages {
		if outage.Kind == githubLookupBackoffKind {
			return key, outage, true
		}
	}
	return "", BackendOutage{}, false
}

func activeProviderCapacityOutage(outages map[string]BackendOutage) (BackendOutage, bool) {
	var selected BackendOutage
	found := false
	for _, key := range sortedKeys(outages) {
		outage := outages[key]
		if outage.Kind == githubLookupBackoffKind || strings.EqualFold(outage.Scope.BackendKind, "tracker") {
			continue
		}
		if !found || earlierProviderProbe(outage, selected) {
			selected = outage
			found = true
		}
	}
	return selected, found
}

func earlierProviderProbe(left BackendOutage, right BackendOutage) bool {
	leftAt := providerCapacityProbeAt(left)
	rightAt := providerCapacityProbeAt(right)
	if leftAt.IsZero() {
		return false
	}
	return rightAt.IsZero() || leftAt.Before(rightAt)
}

func providerCapacityProbeAt(outage BackendOutage) time.Time {
	if strings.TrimSpace(outage.ProbeIssueID) != "" {
		return time.Time{}
	}
	if !outage.NextProbeAt.IsZero() {
		return outage.NextProbeAt.UTC()
	}
	return outage.ResumeAt.UTC()
}

func providerGitHubLookupSignal(outage BackendOutage) githubLookupSignal {
	backend := strings.TrimSpace(outage.Scope.BackendID)
	if backend == "" {
		backend = strings.TrimSpace(outage.Scope.BackendKind)
	}
	return githubLookupSignal{
		trigger: githubLookupTriggerProvider,
		reason:  "provider capacity outage is active for " + backend,
		resetAt: outage.ResetAt,
	}
}

func githubLookupBackoffDelay(attempt int, seed string) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := githubLookupBackoffInitial
	for range attempt - 1 {
		if delay >= githubLookupBackoffMaximum/2 {
			delay = githubLookupBackoffMaximum
			break
		}
		delay *= 2
	}
	window := delay / githubLookupBackoffJitterDivisor
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(seed))
	_, _ = hash.Write([]byte("\x00" + strconv.Itoa(attempt)))
	span := uint64(window*2 + 1)
	offset := time.Duration(int64(hash.Sum64()%span) - int64(window))
	delay += offset
	if delay > githubLookupBackoffMaximum {
		return githubLookupBackoffMaximum
	}
	return delay
}

func githubLookupBackoffPause(state *State, now time.Time) time.Duration {
	if state == nil {
		return 0
	}
	_, outage, ok := githubLookupBackoff(state.BackendOutages)
	if !ok || !outage.NextProbeAt.After(now) {
		return 0
	}
	return outage.NextProbeAt.Sub(now)
}

func githubLookupBackoffAllowsDispatch(state *State, capacityProbeKey string) bool {
	if state == nil {
		return true
	}
	_, outage, ok := githubLookupBackoff(state.BackendOutages)
	if !ok {
		return true
	}
	return outage.Trigger == githubLookupTriggerProvider &&
		outage.LastProbeResult == githubLookupProbeResultProviderDue &&
		strings.TrimSpace(capacityProbeKey) != ""
}

func lookupBucketExhausted(bucket *telemetry.RateLimitBucket, now time.Time) bool {
	return bucket != nil && bucket.Limit > 0 && bucket.Remaining <= 0 &&
		(bucket.ResetAt == nil || !now.After(bucket.ResetAt.Add(githubRateLimitResetSkew)))
}
