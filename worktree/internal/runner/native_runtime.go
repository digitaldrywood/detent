package runner

import (
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func nativeRuntimePhase(req RunRequest) string {
	switch req.Mode {
	case RunModePlan:
		return "planning"
	case RunModeMerge:
		return "merging"
	}
	if runRole(req.Mode, req.Issue) == RoleRework {
		return "rework"
	}
	return "implementation"
}

func nativeRESTEvidence(usage connector.RESTRateLimitUsage, at time.Time) *tracker.NativeRESTEvidence {
	e := &tracker.NativeRESTEvidence{Source: "ordinary_response_headers", Coverage: "selected_client_only; subprocesses_other_clients_hosts_and_account_consumer_attribution_unavailable", ObservedAt: at, Requests: usage.TotalRequests, Windows: []tracker.NativeRESTWindow{}}
	e.WindowsDropped = max(0, len(usage.Requests)-64)
	e.DivergencesDropped = max(0, len(usage.Divergences)-64)
	for _, operation := range usage.Requests {
		if len(e.Windows) == 64 {
			break
		}
		e.Windows = append(e.Windows, tracker.NativeRESTWindow{CredentialIdentity: operation.CredentialIdentity, UsedObserved: operation.UsedObserved, Resource: operation.ResourceHeader, EndpointFamily: operation.EndpointFamily, BudgetScope: operation.BudgetScope, Requests: operation.Count, Limit: operation.Limit, Used: operation.Used, Remaining: operation.Remaining, ResetAt: operation.ResetAt, ObservedAt: operation.LastObservedAt, Status: operation.LastStatus, RateLimited: operation.RateLimited, RetryAfterSeconds: operation.RetryAfter.Seconds()})
	}
	for _, d := range usage.Divergences {
		if len(e.Divergences) == 64 {
			break
		}
		e.Divergences = append(e.Divergences, tracker.NativeRESTDivergence{CredentialIdentity: d.CredentialIdentity, Resource: d.Resource, Attribution: d.Attribution, ObservedRequests: d.ObservedRequests, DetentRequests: d.DetentRequests, AttributedRequests: d.AttributedRequests, UnattributedRequests: d.UnattributedRequests, WindowStartedAt: d.WindowStartedAt, LastObservedAt: d.LastObservedAt, ResetAt: d.ResetAt})
	}
	return e
}

func nativeGitHubScope(scope *connector.RESTScope, started time.Time) *tracker.NativeGitHubScope {
	counts, timings := scope.Counts(), scope.Timings()
	wall := int64(time.Since(started))
	e := &tracker.NativeGitHubScope{Scope: "native_landing", StartedAt: started.UTC(), ObservedAt: time.Now().UTC(), WallElapsedNS: &wall,
		RESTCounts: []tracker.NativeGitHubCount{}, Timings: []tracker.NativeGitHubTiming{},
		CountsDropped: max(0, len(counts)-tracker.NativeGitHubAggregateLimit), TimingsDropped: max(0, len(timings)-tracker.NativeGitHubAggregateLimit),
		Coverage: "selected_client_completed_attempts; sub_steps_subprocesses_other_clients_unavailable", ElapsedSemantics: "http_sums_can_exceed_wall; token_resolution_inclusive_can_contain_installation_http", BuildCoverage: "unavailable", HTTPBoundary: "do_body_close", TimingUnit: "nanoseconds"}
	key := func(k connector.RESTScopeKey) tracker.NativeGitHubKey {
		return tracker.NativeGitHubKey{Stage: k.Stage, Step: k.Step, EndpointFamily: k.EndpointFamily, Outcome: k.Outcome}
	}
	for _, c := range counts[:min(len(counts), tracker.NativeGitHubAggregateLimit)] {
		e.RESTCounts = append(e.RESTCounts, tracker.NativeGitHubCount{NativeGitHubKey: key(c.RESTScopeKey), Count: c.Count})
	}
	for _, timing := range timings[:min(len(timings), tracker.NativeGitHubAggregateLimit)] {
		protocol := "rest"
		if timing.EndpointFamily == "graphql" {
			protocol = "graphql"
		}
		item := tracker.NativeGitHubTiming{NativeGitHubKey: key(timing.RESTScopeKey), Protocol: protocol, QueryPurpose: timing.QueryPurpose, Boundary: timing.Boundary,
			AttemptCount: timing.AttemptCount, TimedCount: timing.TimedCount, FirstObservedAt: timing.FirstObservedAt, LastObservedAt: timing.LastObservedAt}
		if timing.TimedCount > 0 {
			sum, maximum := timing.ElapsedSumNS, timing.ElapsedMaxNS
			item.ElapsedSumNS, item.ElapsedMaxNS = &sum, &maximum
		}
		e.Timings = append(e.Timings, item)
	}
	return e
}
