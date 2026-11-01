package nativechange

import (
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func (e *nativeExecution) DiagnosticAttempt(r runnerauth.DiagnosticAttempt) runnerauth.DiagnosticAttempt {
	r.RecoveryOwner = "hubclient.native_execution"
	e.diagnosticMu.Lock()
	observation := e.diagnostic
	e.diagnosticMu.Unlock()
	r.NativeAttemptID = executionID("attempt", string(e.claim.lease.ID))
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
	return r
}
