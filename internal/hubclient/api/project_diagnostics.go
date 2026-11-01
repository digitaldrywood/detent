package api

import (
	"context"
	"errors"
	"slices"

	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func DiagnosticError(err error) string {
	var failure *APIError
	if errors.As(err, &failure) && failure.Code == "invalid_request" && failure.Rejection != nil && failure.Rejection.Valid() {
		return failure.Rejection.DiagnosticError()
	}
	return runnerauth.DiagnosticError(err.Error())
}

func DiagnosticErrorCode(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "context_deadline_exceeded"
	}
	if errors.Is(err, context.Canceled) {
		return "context_canceled"
	}
	if errors.Is(err, ErrNoClaimableWork) {
		return "no_claimable_work"
	}
	code := HubErrorCode(err)
	if slices.Contains([]string{"lease_conflict", "lease_lost", "provider_capacity", "provider_incompatible", "provider_candidate_changed", "runner_capacity", "host_capacity", "plan_capacity", "policy_mismatch", "invalid_request", "unauthorized", "forbidden", "not_found", "collaboration_bytes", "budget_exhausted", "no_claimable_work"}, code) {
		return code
	}
	return "unavailable"
}
