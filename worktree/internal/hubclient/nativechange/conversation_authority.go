package nativechange

import (
	"context"
)

type conversationAuthority struct{ execution *nativeExecution }

func (a conversationAuthority) Ready() <-chan struct{} {
	e := a.execution
	e.mu.Lock()
	defer e.mu.Unlock()
	var ready chan struct{}
	if e.data.Identity == nil {
		ready = make(chan struct{})
		e.conversationReady = ready
	}
	return ready
}
func (a conversationAuthority) Bound(continuation bool) {
	e := a.execution
	e.mu.Lock()
	defer e.mu.Unlock()
	e.conversationContinuation = continuation
}
func (a conversationAuthority) LeaseCheck(ctx context.Context) error {
	return a.execution.leaseCheck(ctx)
}
