package diagnostics

import (
	"time"

	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/hubclient/execution"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func Claims(views []execution.ExecutionClaim, source *api.NativeConnector, now time.Time, ttl time.Duration) []runnerauth.DiagnosticAttempt {
	var claims []execution.ExecutionClaim
	for _, c := range views {
		if c.Source != source || c.Lost || c.Recovery.Issue.Terminal {
			continue
		}
		deadline := c.Deadline
		if deadline.IsZero() {
			deadline = c.Lease.ExpiresAt
		}
		if !deadline.IsZero() && !now.Before(deadline.Add(ttl)) {
			continue
		}
		claims = append(claims, c)
	}
	result := make([]runnerauth.DiagnosticAttempt, 0, len(claims))
	for _, c := range claims {
		r := runnerauth.DiagnosticAttempt{IssueID: string(c.Lease.WorkItemID), Fence: uint64(c.Lease.FencingToken), Membership: []string{"native_claim"}, LeaseRenewedAt: c.Lease.RenewedAt, LeaseExpiresAt: c.Lease.ExpiresAt, Unavailable: map[string]string{}}
		if e := c.Execution; e != nil {
			r = e.DiagnosticAttempt(r)
		}
		r.Key = runnerauth.DiagnosticKey(r.LocalAttemptID, r.NativeAttemptID, r.IssueID)
		if r.NativeAttemptID == "" {
			r.Unavailable["native_attempt_id"] = "unrecorded"
		}
		if r.LocalAttemptID == 0 {
			r.Unavailable["local_attempt_id"] = "unrecorded"
		}
		if r.Generation == 0 {
			r.Unavailable["generation"] = "unrecorded"
		}
		if r.ProviderCompletedAt.IsZero() {
			r.Unavailable["provider_completion"] = "unrecorded"
		}
		if r.HostOperation == "" {
			r.Unavailable["host_operation"] = "unrecorded"
		}
		r.Unavailable["recovery_capability"] = "not_probed_by_read"
		result = append(result, r)
	}
	return result
}
