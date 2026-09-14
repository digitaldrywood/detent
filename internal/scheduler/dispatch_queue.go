package scheduler

import (
	"cmp"
	"context"
	"slices"
	"time"
)

// Host selection and acquisition share the gate lock, so each request sees
// earlier real grants even before their owners have started the workers.
func (g *GlobalDispatchGate) acquireRequestHostLocked(call *dispatchRequest) (Slot, bool, DispatchGateDecision, error) {
	if call.ctx != nil && call.ctx.Err() != nil {
		return Slot{}, false, DispatchGateDecision{}, call.ctx.Err()
	}
	if len(call.request.HostCandidates) == 0 {
		return g.acquireLocked(call.ctx, call.project, call.request, call.now)
	}
	hosts := slices.Clone(call.request.HostCandidates)
	used := make(map[string]int, len(hosts))
	for _, host := range hosts {
		used[host.Host] = host.Used
	}
	for _, running := range g.running {
		if running.ProjectID == call.project.ID {
			used[running.slot.Host]++
		}
	}
	slices.SortStableFunc(hosts, func(a, b HostCandidate) int {
		if a.Host == call.request.Host && b.Host != call.request.Host {
			return -1
		}
		if b.Host == call.request.Host && a.Host != call.request.Host {
			return 1
		}
		return cmp.Compare(used[a.Host], used[b.Host])
	})
	decision := g.decisionLocked(call.project.ID, call.request, DecisionReasonWorkerHostUnavailable)
	for _, host := range hosts {
		req := call.request
		req.Host = host.Host
		if req.ProjectHostCapacity > 0 {
			if used[host.Host] >= req.ProjectHostCapacity {
				continue
			}
			req.ProjectHostCapacity -= host.Used
		}
		slot, granted, attemptDecision, err := g.acquireLocked(call.ctx, call.project, req, call.now)
		if granted || err != nil {
			return slot, granted, attemptDecision, err
		}
		decision = attemptDecision
	}
	return Slot{}, false, decision, nil
}

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
