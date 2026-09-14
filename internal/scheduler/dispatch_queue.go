package scheduler

import (
	"context"
	"slices"
	"time"
)

// DispatchResult transfers a real slot to the request's consuming event loop.
type DispatchResult struct {
	Order    uint64
	Slot     Slot
	Decision DispatchGateDecision
	Err      error
}

// QueuedProjectDispatchGate keeps eligible requests, never selected capacity.
// Cancel removes a pending request and prevents further delivery. The caller
// must drain any already delivered result and release its slot after Cancel.
// Wake must be buffered; it coalesces notifications to inspect result channels.
type QueuedProjectDispatchGate interface {
	Submit(context.Context, ProjectCandidate, SlotRequest, time.Time, chan<- struct{}) (<-chan DispatchResult, func(), DispatchGateDecision)
}

func (g *GlobalDispatchGate) Submit(ctx context.Context, project ProjectCandidate, req SlotRequest, now time.Time, wake chan<- struct{}) (<-chan DispatchResult, func(), DispatchGateDecision) {
	call := &dispatchRequest{ctx: ctx, project: project, request: req, now: now, result: make(chan DispatchResult, 1), wake: wake}
	if g == nil || g.global == nil {
		slot, _, decision, err := g.TryAcquireWithDecision(ctx, project, req, now)
		call.result <- DispatchResult{Slot: slot, Decision: decision, Err: err}
		return call.result, func() {}, decision
	}
	_, _, decision, _ := g.acquireRequest(call) //nolint:errcheck // Acquisition errors are delivered to the owner through call.result.
	return call.result, func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.removeRequestLocked(call)
	}, decision
}

func (g *GlobalDispatchGate) removeRequestLocked(call *dispatchRequest) {
	g.waiting = slices.DeleteFunc(g.waiting, func(pending *dispatchRequest) bool { return pending == call })
}

func (g *GlobalDispatchGate) finishRequestLocked(call *dispatchRequest) {
	if call.result == nil {
		return
	}
	if !call.granted && call.err == nil {
		if call.decision.Reason == DispatchGateReasonOutsideActiveWindow {
			call.err = ErrNoCandidates
		} else {
			g.waiting = append(g.waiting, call)
			return
		}
	}
	slot := call.slot
	if call.granted && call.decorate != nil {
		slot = call.decorate(slot)
	}
	call.result <- DispatchResult{Order: slot.token, Slot: slot, Decision: call.decision, Err: call.err}
	select {
	case call.wake <- struct{}{}:
	default:
	}
}

func (g *GlobalDispatchGate) discardProjectRequestsLocked(projectID string) {
	waiting := g.waiting
	g.waiting = nil
	for _, call := range waiting {
		if call.project.ID == projectID {
			call.err = ErrNoCandidates
			g.finishRequestLocked(call)
		} else {
			g.waiting = append(g.waiting, call)
		}
	}
}

func (r *PoolRegistry) Submit(ctx context.Context, project ProjectCandidate, req SlotRequest, now time.Time, wake chan<- struct{}) (<-chan DispatchResult, func(), DispatchGateDecision) {
	call := &dispatchRequest{ctx: ctx, project: project, request: req, now: now, result: make(chan DispatchResult, 1), wake: wake}
	_, _, decision, _ := r.acquireRequest(call) //nolint:errcheck // Acquisition errors are delivered to the owner through call.result.
	return call.result, func() {
		r.reconfigureMu.Lock()
		defer r.reconfigureMu.Unlock()
		if call.gate == nil {
			return
		}
		call.gate.mu.Lock()
		defer call.gate.mu.Unlock()
		call.gate.removeRequestLocked(call)
	}, decision
}

func (g *projectPoolGate) Submit(ctx context.Context, project ProjectCandidate, req SlotRequest, now time.Time, wake chan<- struct{}) (<-chan DispatchResult, func(), DispatchGateDecision) {
	project.ID = g.projectID
	return g.registry.Submit(ctx, project, req, now, wake)
}
