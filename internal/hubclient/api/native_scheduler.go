package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func NativeLeaseDeadline(started time.Time, lease tracker.NativeLease) time.Time {
	if lease.ServerTime.IsZero() {
		return started
	}
	return started.Add(lease.ExpiresAt.Sub(lease.ServerTime))
}

func NativeTrackerLease(lease tracker.NativeLease) tracker.Lease {
	return tracker.Lease{LeaseSummary: tracker.LeaseSummary{PolicyID: lease.PolicyID, ID: lease.ID, FencingToken: lease.FencingToken,
		Machine: tracker.MachineSummary{ID: lease.MachineID}, SessionID: lease.SessionID, AcquiredAt: lease.AcquiredAt, RenewedAt: lease.RenewedAt, ExpiresAt: lease.ExpiresAt}}
}

func NativeTransportUnavailable(err error) bool {
	if errors.Is(err, runner.ErrExecutionAuthorityUnavailable) || errors.Is(err, orchestrator.ErrSchedulingClaimLost) {
		return false
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status >= http.StatusInternalServerError || apiErr.Status == http.StatusTooManyRequests && apiErr.Code == requestBudgetCode
	}
	return errors.Is(err, ErrUnavailable) || errors.Is(err, context.DeadlineExceeded)
}

func NativeAuthorityLost(err error) bool {
	var apiErr *APIError
	return errors.Is(err, orchestrator.ErrSchedulingClaimLost) || NativeLeaseLost(err) || errors.As(err, &apiErr) && apiErr != nil &&
		(apiErr.Status == http.StatusUnauthorized || apiErr.Status == http.StatusForbidden || apiErr.Status == http.StatusNotFound)
}
func NativeAdmissionCapacityFull(err error) bool {
	var failure *APIError
	if !errors.As(err, &failure) {
		return false
	}
	switch failure.Code {
	case "provider_capacity", "runner_capacity", "host_capacity", "plan_capacity":
		return true
	default:
		return false
	}
}
