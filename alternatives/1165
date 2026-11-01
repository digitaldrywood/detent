package claim

import (
	"context"
	"errors"
	"net/http"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Source interface {
	Recovery(context.Context, tracker.NativeWorkItemID) (tracker.NativeRecovery, error)
	Release(context.Context, tracker.NativeLease, string) error
	Forget(tracker.NativeLease)
	Intake(context.Context, tracker.NativeLease, tracker.NativeIssue) error
}

type Candidate struct {
	Issue    connector.Issue
	Lease    tracker.NativeLease
	Recovery tracker.NativeRecovery
}

type Result struct {
	Candidates []Candidate
	Degraded   error
}

func Hydrate(ctx context.Context, source Source, leases []tracker.NativeLease, reconnecting bool, err error) (Result, error) {
	release := func(leases []tracker.NativeLease, cause error) error {
		for _, lease := range leases {
			if reconnecting {
				continue
			}
			releaseErr := source.Release(context.WithoutCancel(ctx), lease, "work_item_hydration_failed")
			if releaseErr == nil || api.ClaimLost(releaseErr) {
				source.Forget(lease)
			}
			cause = errors.Join(cause, releaseErr)
		}
		return SchedulingError(cause)
	}
	if err != nil && (len(leases) == 0 || !independentFailure(ctx, err)) {
		return Result{}, release(leases, err)
	}
	degraded := err
	candidates := make([]Candidate, 0, len(leases))
	for i, lease := range leases {
		recovery, err := source.Recovery(ctx, lease.WorkItemID)
		if err == nil && recovery.Issue.LinkedSource != nil && recovery.Issue.LinkedSource.Status != "complete" {
			err = source.Intake(ctx, lease, recovery.Issue)
			if err == nil {
				recovery, err = source.Recovery(ctx, lease.WorkItemID)
			}
		}
		if err != nil {
			if len(candidates) == 0 || !independentFailure(ctx, err) {
				return Result{}, release(leases, errors.Join(degraded, err))
			}
			if releaseErr := release(leases[i:], nil); releaseErr != nil {
				return Result{}, release(leases[:i], errors.Join(degraded, err, releaseErr))
			}
			leases = leases[:i]
			degraded = errors.Join(degraded, err)
			break
		}
		issue := api.IssueFromNative(recovery.Issue)
		issue.AssignedToWorker = true
		issue.IsolationPolicy = lease.IsolationPolicy
		candidates = append(candidates, Candidate{Issue: issue, Lease: lease, Recovery: recovery})
	}
	if err := ctx.Err(); err != nil {
		return Result{}, release(leases, errors.Join(degraded, err))
	}
	return Result{Candidates: candidates, Degraded: degraded}, nil
}

func independentFailure(ctx context.Context, err error) bool {
	if ctx.Err() != nil || errors.Is(err, orchestrator.ErrSchedulingUnavailable) || Deferred(err) || api.ClaimLost(err) {
		return false
	}
	var failure *api.APIError
	if errors.As(err, &failure) && (failure.Status < http.StatusInternalServerError || failure.Code == "policy_mismatch") {
		return false
	}
	return errors.Is(err, api.ErrUnavailable)
}

func SchedulingError(err error) error {
	if err == nil || !errors.Is(err, api.ErrUnavailable) && !Deferred(err) {
		return err
	}
	return errors.Join(orchestrator.ErrSchedulingUnavailable, err)
}

func Deferred(err error) bool {
	var failure *api.APIError
	if !errors.As(err, &failure) {
		return false
	}
	switch failure.Code {
	case "provider_capacity", "provider_incompatible", "provider_candidate_changed", "selector_no_match", "runner_disabled", "runner_draining", "runner_offline", "runner_capacity", "host_capacity", "plan_capacity", "project_access_denied", "claim_not_permitted":
		return true
	default:
		return false
	}
}
