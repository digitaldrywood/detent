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
