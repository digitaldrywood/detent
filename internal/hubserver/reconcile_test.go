package hubserver

import (
	"context"
	"errors"
	"sync"
)

type reconcileStep struct {
	snapshot ReconcileSnapshot
	err      error
}

type scriptedReconcileBackend struct {
	mu       sync.Mutex
	steps    []reconcileStep
	requests []ReconcileRequest
}

func (b *scriptedReconcileBackend) Reconcile(_ context.Context, request ReconcileRequest) (ReconcileSnapshot, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.requests = append(b.requests, request)
	if len(b.steps) == 0 {
		return ReconcileSnapshot{}, errors.New("unexpected reconciliation request")
	}
	step := b.steps[0]
	b.steps = b.steps[1:]
	return step.snapshot, step.err
}

func (b *scriptedReconcileBackend) Requests() []ReconcileRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]ReconcileRequest(nil), b.requests...)
}

func int64Pointer(value int64) *int64 {
	return &value
}

func stringPointer(value string) *string {
	return &value
}
