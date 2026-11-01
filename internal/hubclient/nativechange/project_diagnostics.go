package nativechange

import (
	"time"
)

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
		e.diagnostic.LatestError = diagnosticError(err)
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
			e.diagnostic.LatestError = diagnosticError(err)
			e.diagnostic.LatestErrorCode = diagnosticErrorCode(err)
		}
		if e.diagnostic.HostOperation != operation && e.diagnostic.OperationPending {
			return
		}
		if err == nil || previous.OperationPending {
			e.diagnostic.HostOperation = previous.HostOperation
			e.diagnostic.OperationStartedAt = previous.OperationStartedAt
		}
		e.diagnostic.OperationPending = previous.OperationPending
	}
}
func (e *nativeExecution) ProviderCompleted(at time.Time) {
	e.diagnosticMu.Lock()
	defer e.diagnosticMu.Unlock()
	if e.diagnostic.ProviderCompletedAt.IsZero() {
		e.diagnostic.ProviderCompletedAt = at.UTC()
	}
}
