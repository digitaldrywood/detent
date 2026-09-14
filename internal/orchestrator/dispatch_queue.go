package orchestrator

import (
	"cmp"
	"context"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/scheduler"
)

type pendingGlobalDispatch struct {
	action dispatchAction
	result <-chan scheduler.DispatchResult
	cancel func()
}

func (o *Orchestrator) acquireOrQueueGlobalDispatchSlot(ctx context.Context, state *State, action dispatchAction, slotIssue connector.Issue, workerHost string, now time.Time, pressureCapacity int, mergeControlEligible bool) (scheduler.Slot, bool, scheduler.DispatchGateDecision) {
	gate, queued := o.globalDispatchGate.(scheduler.QueuedProjectDispatchGate)
	// Direct dispatch tests and non-event-loop callers consume synchronous
	// acquisitions. Merge control already has its own executable capacity path.
	if !queued || o.globalDispatchReady == nil || mergeControlEligible {
		return o.acquireGlobalDispatchSlot(ctx, slotIssue, workerHost, now, pressureCapacity)
	}
	if _, pending := o.globalDispatchPending[action.issue.ID]; pending {
		return scheduler.Slot{}, false, scheduler.DispatchGateDecision{Reason: scheduler.DispatchGateReasonGlobalCapacityFull}
	}
	projectCapacity := o.cfg.MaxConcurrentAgents
	if action.modelPermitRequired {
		projectCapacity = len(state.Running) + o.dispatchPlanner().availableSlots(state)
	}
	stateCapacity := o.projectStateSlotStats(slotIssue, state).capacity
	hostCapacity := o.cfg.MaxConcurrentAgentsPerHost
	hosts := make([]scheduler.HostCandidate, 0, len(o.cfg.WorkerHosts))
	for _, host := range o.cfg.WorkerHosts {
		hosts = append(hosts, scheduler.HostCandidate{Host: host})
	}
	for _, running := range state.Running {
		if running.globalSlot != (scheduler.Slot{}) {
			continue
		}
		projectCapacity--
		if normalizeState(running.Issue.State) == normalizeState(slotIssue.State) {
			stateCapacity--
		}
		for i := range hosts {
			if running.WorkerHost == hosts[i].Host {
				hosts[i].Used++
			}
		}
		if len(hosts) == 0 && running.WorkerHost == workerHost && hostCapacity > 0 {
			hostCapacity--
		}
	}
	if projectCapacity <= 0 || stateCapacity <= 0 || (o.cfg.MaxConcurrentAgentsPerHost > 0 && hostCapacity <= 0) {
		return scheduler.Slot{}, false, scheduler.DispatchGateDecision{Reason: scheduler.DecisionReasonProjectCapacityFull}
	}
	if len(hosts) > 0 {
		// A poll-time choice is not affinity. Only a retry has a preferred host.
		workerHost = ""
		if action.retryState != nil {
			workerHost = action.retryState.WorkerHost
		}
	}
	result, cancel, decision := gate.Submit(ctx, o.cfg.Project, scheduler.SlotRequest{
		ProjectCapacity: projectCapacity, ProjectStateCapacity: stateCapacity, ProjectHostCapacity: hostCapacity,
		State: slotIssue.State, Host: workerHost, HostCandidates: hosts,
		Priority: o.dispatchStatePriority(slotIssue.State), PressureCapacity: pressureCapacity,
	}, now, o.globalDispatchReady)
	select {
	case grant := <-result:
		cancel()
		return grant.Slot, grant.Err == nil, grant.Decision
	default:
		o.globalDispatchPending[action.issue.ID] = pendingGlobalDispatch{action: action, result: result, cancel: cancel}
		return scheduler.Slot{}, false, decision
	}
}

func (o *Orchestrator) cancelPendingGlobalDispatches() {
	// Remove every old request before releasing delivered slots, so release
	// cannot grant another candidate from the snapshot being discarded.
	for _, pending := range o.globalDispatchPending {
		pending.cancel()
	}
	for id, pending := range o.globalDispatchPending {
		select {
		case grant := <-pending.result:
			o.releaseGlobalDispatchSlot(grant.Slot)
		default:
		}
		delete(o.globalDispatchPending, id)
	}
}

func (o *Orchestrator) dispatchGrantedRequests(ctx context.Context, state *State, now time.Time) {
	type readyDispatch struct {
		pending pendingGlobalDispatch
		grant   scheduler.DispatchResult
	}
	var ready []readyDispatch
	for id, pending := range o.globalDispatchPending {
		select {
		case grant := <-pending.result:
			delete(o.globalDispatchPending, id)
			pending.cancel()
			ready = append(ready, readyDispatch{pending: pending, grant: grant})
		default:
		}
	}
	slices.SortFunc(ready, func(a, b readyDispatch) int { return cmp.Compare(a.grant.Order, b.grant.Order) })
	for _, request := range ready {
		o.dispatchGrantedRequest(ctx, state, request.pending.action, request.grant, now)
	}
}

func (o *Orchestrator) dispatchGrantedRequest(ctx context.Context, state *State, action dispatchAction, grant scheduler.DispatchResult, now time.Time) {
	defer func() { o.releaseGlobalDispatchSlot(grant.Slot) }()
	if grant.Err != nil || ctx.Err() != nil {
		return
	}
	issues, err := o.connector.FetchIssueStatesByIDs(ctx, []string{action.issue.ID})
	if err != nil {
		return
	}
	var fresh connector.Issue
	found := false
	for _, issue := range issues {
		if issue.ID == action.issue.ID {
			fresh = cloneIssue(issue)
			found = true
			break
		}
	}
	if !found {
		return
	}
	// State reads need not include PR enrichment. Keep fresh tracker fields
	// authoritative (including an empty lane or assignee), while preserving the
	// PR identity needed by the existing hydrator to refresh its head and checks.
	if fresh.PullRequest == nil {
		fresh.PullRequest = cloneIssue(action.issue).PullRequest
	}
	if fresh.PRNumber == nil {
		fresh.PRNumber = action.issue.PRNumber
	}
	if fresh.BranchName == "" {
		fresh.BranchName = action.issue.BranchName
	}
	if hydrator, ok := o.connector.(connector.PullRequestHydrator); ok && (fresh.PullRequest != nil || fresh.PRNumber != nil) {
		fresh, err = hydrator.HydratePullRequest(ctx, fresh)
		if err != nil {
			return
		}
	}
	action.issue = o.hydrateDispatchDependencies(ctx, fresh, make(map[string]dependencyBlocker))
	action.workerHost = grant.Slot.Host
	retry, retryQueued := state.Retry[action.issue.ID]
	if retryQueued {
		if action.retryState == nil || retry.DueAt.After(now) {
			return
		}
		delete(state.Retry, action.issue.ID)
		defer func() {
			if _, running := state.Running[action.issue.ID]; !running {
				state.Retry[action.issue.ID] = retry
			}
		}()
	}
	if !o.dispatchPlanner().dispatchableIssueDecisionForModelRequirement(action.issue, state, action.retryState != nil, now, action.workerHost, action.modelPermitRequired).dispatchable {
		return
	}
	consumed := grant
	grant.Slot = scheduler.Slot{}
	o.dispatchIssueWithGlobalGrant(ctx, state, action.issue, action.attempt, now, action.workerHost, action.modelPermitRequired, action.allowMergeControl, action.retryState, &consumed)
}
