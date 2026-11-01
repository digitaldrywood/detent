package api

import (
	"errors"
	"strconv"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func ClaimLost(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.Code == "stale_fencing_token" || apiErr.Code == "lease_not_found" || apiErr.Code == "lease_conflict"
}

func CloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}
func QueuePriorityName(priority int) string {
	switch priority {
	case tracker.QueuePriorityUrgent:
		return "urgent"
	case tracker.QueuePriorityHigh:
		return "high"
	case tracker.QueuePriorityNormal:
		return "normal"
	case tracker.QueuePriorityLow:
		return "low"
	default:
		return ""
	}
}
func ClaimedIssue(issue connector.Issue, lease tracker.Lease) orchestrator.Claimed {
	if issue.Metadata == nil {
		issue.Metadata = make(map[string]string)
	}
	issue.Metadata["hub_lease_id"] = string(lease.ID)
	issue.Metadata["hub_fencing_token"] = strconv.FormatInt(int64(lease.FencingToken), 10)
	issue.Metadata["hub_session_id"] = lease.SessionID
	issue.Metadata["hub_policy_id"] = lease.PolicyID
	return orchestrator.Claimed{
		Issue: issue, ClaimedAt: lease.AcquiredAt, Owner: string(lease.Machine.ID),
		LeaseRenewedAt: lease.RenewedAt, LeaseExpiresAt: lease.ExpiresAt,
	}
}
