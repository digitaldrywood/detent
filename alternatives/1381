package hubclient

import (
	"context"
	"errors"
	"runtime"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (e *nativeExecution) ProviderCompleted(at time.Time) {
	e.diagnosticMu.Lock()
	defer e.diagnosticMu.Unlock()
	if e.diagnostic.ProviderCompletedAt.IsZero() {
		e.diagnostic.ProviderCompletedAt = at.UTC()
	}
}

func (e *nativeExecution) HostOperation(operation string, at time.Time, err error, pending bool) {
	e.diagnosticMu.Lock()
	defer e.diagnosticMu.Unlock()
	e.diagnostic.HostOperation = operation
	e.diagnostic.OperationPending = pending
	e.diagnostic.OperationObservedAt = at.UTC()
	if pending && err == nil {
		e.diagnostic.OperationStartedAt = at.UTC()
	}
	if err != nil {
		e.diagnostic.LatestError = runnerauth.DiagnosticError(err.Error())
		e.diagnostic.LatestErrorCode = diagnosticErrorCode(err)
	}
}

func (e *nativeExecution) diagnosticOperation(operation string) func(error) {
	e.diagnosticMu.Lock()
	previous := e.diagnostic
	e.diagnostic.HostOperation = operation
	e.diagnostic.OperationPending = true
	e.diagnostic.OperationStartedAt = e.scheduler.now().UTC()
	e.diagnosticMu.Unlock()
	return func(err error) {
		e.diagnosticMu.Lock()
		defer e.diagnosticMu.Unlock()
		e.diagnostic.OperationObservedAt = e.scheduler.now().UTC()
		if err != nil {
			e.diagnostic.LatestError = runnerauth.DiagnosticError(err.Error())
			e.diagnostic.LatestErrorCode = diagnosticErrorCode(err)
			return
		}
		if e.diagnostic.HostOperation != operation && e.diagnostic.OperationPending {
			return
		}
		e.diagnostic.HostOperation = previous.HostOperation
		e.diagnostic.OperationPending = previous.OperationPending
		e.diagnostic.OperationStartedAt = previous.OperationStartedAt
	}
}

func (s *Scheduler) projectDiagnosticClaims(source *NativeConnector) []runnerauth.DiagnosticAttempt {
	s.mu.Lock()
	var claims []nativeClaim
	for _, c := range s.nativeClaims {
		if c.source == source {
			claims = append(claims, c)
		}
	}
	s.mu.Unlock()
	result := make([]runnerauth.DiagnosticAttempt, 0, len(claims))
	for _, c := range claims {
		r := runnerauth.DiagnosticAttempt{IssueID: string(c.lease.WorkItemID), Fence: uint64(c.lease.FencingToken), Membership: []string{"native_claim"}, LeaseRenewedAt: c.lease.RenewedAt, LeaseExpiresAt: c.lease.ExpiresAt, Unavailable: map[string]string{}}
		if e := c.execution; e != nil {
			r.RecoveryOwner = "hubclient.native_execution"
			e.diagnosticMu.Lock()
			observation := e.diagnostic
			e.diagnosticMu.Unlock()
			r.NativeAttemptID = executionID("attempt", string(c.lease.ID))
			r.LocalAttemptID, r.Generation, r.Stage = observation.LocalAttemptID, observation.Generation, observation.Stage
			r.ProviderCompletedAt = observation.ProviderCompletedAt
			r.LatestErrorCode = observation.LatestErrorCode
			r.HostOperation, r.OperationPending, r.OperationStartedAt, r.OperationObservedAt, r.LatestError = observation.HostOperation, observation.OperationPending, observation.OperationStartedAt, observation.OperationObservedAt, observation.LatestError
			if e.mu.TryLock() {
				r.NativeAttemptID = e.data.AttemptID
				if e.data.Runtime != nil {
					r.LocalAttemptID, r.Generation, r.Stage = e.data.Runtime.LocalAttemptID, e.data.Runtime.Generation, e.data.Runtime.Phase
				}
				if e.pending != nil {
					r.HostOperation = "append." + e.pending.Type
					r.OperationPending = true
				}
				if e.data.Outcome != "" {
					r.HostOperation = "lease.release"
					r.OperationPending = true
				}
				e.mu.Unlock()
			} else {
				r.Unavailable["execution_detail"] = "owner_operation_in_progress"
			}
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

func (s *Scheduler) enrichProjectDiagnostics(ctx context.Context, source *NativeConnector, view *runnerauth.ProjectConfiguration, update *runnerauth.UpdateObservation) error {
	supported, err := source.client.HubFeature(ctx, tracker.NativeProjectDiagnosticsCapability)
	if err != nil {
		return err
	}
	if !supported {
		view.Diagnostics = nil
		return nil
	}
	d := view.Diagnostics
	if d == nil {
		return nil
	}
	if update != nil {
		build := update.Running
		d.RunningBuild = &build
		delete(d.Unavailable, "running_build")
	}
	if d.RunningBuild == nil {
		s.mu.Lock()
		version := s.machine.Version
		s.mu.Unlock()
		if version != "" {
			d.RunningBuild = &runnerauth.BuildEvidence{Version: version, Commit: "unknown", Source: "unknown", OS: runtime.GOOS, Architecture: runtime.GOARCH, ObservedAt: s.now().UTC()}
			d.Unavailable["running_build"] = "unavailable"
		}
	}
	claims := s.projectDiagnosticClaims(source)
	d.Counts["native_claim"] = len(claims)
	for _, c := range claims {
		matched := false
		for i, r := range d.Records {
			if r.Key != c.Key {
				continue
			}
			c.Membership = append(r.Membership, c.Membership...)
			c.DeferredAt, c.RetryAt, c.RetryAttempt = r.DeferredAt, r.RetryAt, r.RetryAttempt
			if r.RecoveryOwner != "" {
				c.RecoveryOwner = r.RecoveryOwner
			}
			if c.ProviderCompletedAt.IsZero() {
				c.ProviderCompletedAt = r.ProviderCompletedAt
			}
			if c.HostOperation == "" {
				c.HostOperation, c.OperationPending, c.LatestError = r.HostOperation, r.OperationPending, r.LatestError
			}
			if !c.ProviderCompletedAt.IsZero() {
				delete(c.Unavailable, "provider_completion")
			}
			if c.HostOperation != "" {
				delete(c.Unavailable, "host_operation")
			}
			d.Records[i] = c
			matched = true
			break
		}
		if !matched {
			d.Records = append(d.Records, c)
		}
	}
	if d.Truncated {
		delete(d.Counts, "distinct_records")
		d.Unavailable["distinct_records"] = "unavailable"
	} else {
		d.Counts["distinct_records"] = len(d.Records)
	}
	d.Bound()
	return nil
}

func diagnosticErrorCode(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "context_deadline_exceeded"
	}
	if errors.Is(err, context.Canceled) {
		return "context_canceled"
	}
	if errors.Is(err, ErrNoClaimableWork) {
		return "no_claimable_work"
	}
	code := hubErrorCode(err)
	if slices.Contains([]string{"lease_conflict", "lease_lost", "provider_capacity", "provider_incompatible", "provider_candidate_changed", "runner_capacity", "host_capacity", "policy_mismatch", "invalid_request", "unauthorized", "forbidden", "not_found", "collaboration_bytes", "budget_exhausted", "no_claimable_work"}, code) {
		return code
	}
	return "unavailable"
}
