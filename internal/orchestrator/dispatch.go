package orchestrator

import (
	"context"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/activity"
	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/dispatchpriority"
	"github.com/digitaldrywood/detent/internal/gate"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/selector"
	"github.com/digitaldrywood/detent/internal/store"
)

func (o *Orchestrator) dispatchPlanner() dispatchPlanner {
	return newDispatchPlanner(o.cfg)
}

func (o *Orchestrator) pruneBudgetRefusals(ctx context.Context, state *State, now time.Time) {
	o.dispatchPlanner().pruneBudgetRefusals(
		state,
		now,
		o.currentDailyBudgetStatus(ctx, state, now),
		o.currentIssueBudgetStatuses(ctx, state),
	)
}

func (o *Orchestrator) currentIssueBudgetStatuses(ctx context.Context, state *State) map[string]IssueBudgetStatus {
	if o.issueBudgetStatus == nil {
		return nil
	}

	statuses := make(map[string]IssueBudgetStatus)
	for issueID, refusal := range state.BudgetRefusals {
		if refusal.Code != string(budget.ReasonPerIssueMaxUSD) {
			continue
		}
		status, known, err := o.issueBudgetStatus.IssueBudgetStatus(ctx, refusal.Issue)
		if err != nil {
			if o.logger != nil {
				o.logger.Warn("per-issue budget hold re-evaluation failed", "issue_id", issueID, "error", err)
			}
			continue
		}
		if known {
			statuses[issueID] = status
		}
	}
	return statuses
}

func (o *Orchestrator) currentDailyBudgetStatus(ctx context.Context, state *State, now time.Time) *DailyBudgetStatus {
	if o.dailyBudgetStatus == nil || !hasActiveDailyBudgetRefusal(state, now) {
		return nil
	}

	status, known, err := o.dailyBudgetStatus.DailyBudgetStatus(ctx, now)
	if err != nil {
		if o.logger != nil {
			o.logger.Warn("daily budget refusal re-evaluation failed", "error", err)
		}
		return nil
	}
	if !known {
		return nil
	}
	return &status
}

func hasActiveDailyBudgetRefusal(state *State, now time.Time) bool {
	for _, refusal := range state.BudgetRefusals {
		if refusal.Code == "per_day_max_usd" && refusal.ResetAt != nil && now.Before(*refusal.ResetAt) {
			return true
		}
	}
	return false
}

func (o *Orchestrator) selectWorkerHost(state *State, preferredWorkerHost string) (string, bool) {
	return o.dispatchPlanner().selectWorkerHost(state, preferredWorkerHost)
}

func leastLoadedWorkerHost(state *State, hosts []string) string {
	selected := hosts[0]
	selectedCount := runningWorkerHostCount(state, selected)
	for _, host := range hosts[1:] {
		count := runningWorkerHostCount(state, host)
		if count < selectedCount {
			selected = host
			selectedCount = count
		}
	}
	return selected
}

func runningWorkerHostCount(state *State, workerHost string) int {
	count := 0
	for _, running := range state.Running {
		if running.WorkerHost == workerHost {
			count++
		}
	}
	return count
}

func normalizeWorkerHosts(hosts []string) []string {
	normalized := make([]string, 0, len(hosts))
	seen := make(map[string]struct{}, len(hosts))
	for _, host := range hosts {
		host = strings.TrimSpace(host)
		if host == "" {
			continue
		}
		if _, ok := seen[host]; ok {
			continue
		}
		seen[host] = struct{}{}
		normalized = append(normalized, host)
	}
	return normalized
}

func (o *Orchestrator) dispatchReadyIssues(ctx context.Context, state *State, issues []connector.Issue, now time.Time) {
	o.beginGlobalProjectCycle()
	defer o.endGlobalProjectCycle()
	if state.Draining {
		return
	}
	planner := o.dispatchPlanner()
	var lastDispatchFailure string
	planner.plan(state, issues, now, dispatchPlanHooks{
		hydrate: func(issue connector.Issue) (connector.Issue, bool) {
			return o.hydrateDispatchIssue(ctx, issue)
		},
		beforeDispatch: func(_ connector.Issue, continuationIndex int) bool {
			if continuationIndex < 0 {
				return true
			}
			return waitForDispatchBackoff(ctx, continuationDelay(continuationIndex))
		},
		dispatch: func(issue connector.Issue, attempt int, workerHost string) bool {
			outcome := o.dispatchIssueWithOutcome(ctx, state, issue, attempt, now, workerHost)
			if !outcome.dispatched {
				lastDispatchFailure = outcome.reason
			} else {
				lastDispatchFailure = ""
			}
			return outcome.dispatched
		},
		dispatchFailed: func(issue connector.Issue) bool {
			return !mergeWorkerIssue(issue) || lastDispatchFailure != dispatchIssueFailureGlobalSlotUnavailable
		},
		retryDispatchFailed: func(issue connector.Issue, retry Retry) {
			planner.scheduleRetry(state, issue, retry.Attempt, now, "claim verification failed", false, retry.WorkerHost)
			rescheduled := state.Retry[issue.ID]
			rescheduled.RetryMode = retry.RetryMode
			rescheduled.ResumeState = retry.ResumeState
			state.Retry[issue.ID] = rescheduled
		},
		preserveMissingDueRetry: func(retry Retry) bool {
			return o.preserveMissingDueRetry(state, retry)
		},
		decision: func(decision dispatchPlanDecision) {
			o.logDispatchPlanDecision(ctx, state, now, decision)
		},
	})
}

func (o *Orchestrator) preserveMissingDueRetry(state *State, retry Retry) bool {
	if normalizeState(retry.Issue.State) != normalizeState(autoPromoteMergingState) {
		return false
	}
	return !o.mergeWorkerLocalSlotsAvailable(state)
}

func (o *Orchestrator) hydrateDispatchIssue(ctx context.Context, issue connector.Issue) (connector.Issue, bool) {
	if strings.TrimSpace(issue.ID) == "" || len(issue.Fields) > 0 || o.connector == nil {
		return issue, true
	}
	issues, err := o.connector.FetchIssueStatesByIDs(ctx, []string{issue.ID})
	if err != nil {
		if o.logger != nil {
			o.logger.Warn("hydrate dispatch issue failed", "issue_id", issue.ID, "error", err)
		}
		return connector.Issue{}, false
	}
	for _, hydrated := range issues {
		if hydrated.ID == issue.ID {
			return mergeIssueTrackerFields(issue, hydrated), true
		}
	}
	return connector.Issue{}, false
}

func (o *Orchestrator) dispatchCandidates(ctx context.Context, state *State, issues []connector.Issue, now time.Time) {
	o.beginGlobalProjectCycle()
	defer o.endGlobalProjectCycle()
	if state.Draining {
		return
	}
	for _, issue := range issues {
		if o.dispatchPlanner().availableSlots(state) == 0 {
			return
		}
		issue, ok := o.hydrateDispatchIssue(ctx, issue)
		if !ok {
			continue
		}
		if !o.dispatchable(issue, state, now) {
			continue
		}

		o.dispatchIssue(ctx, state, issue, 0, now, "")
	}
}

func dueRetriesByIssue(state *State, now time.Time) map[string]Retry {
	retries := make(map[string]Retry, len(state.Retry))
	for _, retry := range state.Retry {
		if !retry.DueAt.After(now) {
			retries[retry.Issue.ID] = retry
		}
	}
	return retries
}

func (o *Orchestrator) dispatchable(issue connector.Issue, state *State, now time.Time) bool {
	return o.dispatchPlanner().dispatchable(issue, state, now)
}

const (
	dispatchIssueFailureLocalSlotUnavailable  = "local_slot_unavailable"
	dispatchIssueFailureWorkerHostUnavailable = "worker_host_unavailable"
	dispatchIssueFailureGlobalSlotUnavailable = "global_slot_unavailable"
	dispatchIssueFailureClaimFailed           = "claim_failed"
	dispatchIssueFailureWorkAttemptStart      = "work_attempt_start_failed"
	dispatchIssueFailureStartStateTransition  = "start_state_transition_failed"
	dispatchIssueFailureBackendCapacityPaused = "backend_capacity_paused"
	dispatchIssueFailureGitHubRESTPaused      = "github_rest_capacity_paused"
)

type dispatchIssueOutcome struct {
	dispatched bool
	reason     string
}

func (o *Orchestrator) dispatchIssue(
	ctx context.Context,
	state *State,
	issue connector.Issue,
	attempt int,
	now time.Time,
	preferredWorkerHost string,
) bool {
	return o.dispatchIssueWithOutcome(ctx, state, issue, attempt, now, preferredWorkerHost).dispatched
}

func (o *Orchestrator) dispatchIssueWithOutcome(
	ctx context.Context,
	state *State,
	issue connector.Issue,
	attempt int,
	now time.Time,
	preferredWorkerHost string,
) dispatchIssueOutcome {
	if _, paused := activeGitHubRESTCapacityOutage(state, now); paused {
		return dispatchIssueOutcome{reason: dispatchIssueFailureGitHubRESTPaused}
	}
	if !projectFailureBreakerAllowsDispatch(state, now) {
		return dispatchIssueOutcome{reason: projectFailureBreakerDispatchPaused}
	}
	queuedRetry, retryQueued := state.Retry[issue.ID]
	runMode := o.dispatchMode(ctx, state, issue)
	capacityRequest := runpkg.RunRequest{Issue: issue, Mode: runMode, SelectorContext: o.selectorContext()}
	capacityScope, capacityProbeKey, capacityPaused := o.backendCapacityDispatch(state, capacityRequest, now)
	if capacityPaused {
		return dispatchIssueOutcome{reason: dispatchIssueFailureBackendCapacityPaused}
	}
	targetState := dispatchStartTransitionState(issue, runMode, o.cfg.ActiveStates)
	slotIssue := issue
	if targetState != "" {
		slotIssue = cloneIssue(issue)
		slotIssue.State = targetState
		if !o.dispatchPlanner().slotsAvailable(slotIssue, state, preferredWorkerHost) {
			return dispatchIssueOutcome{reason: dispatchIssueFailureLocalSlotUnavailable}
		}
	}
	projectStats := o.projectStateSlotStats(slotIssue, state)

	workerHost, ok := o.selectWorkerHost(state, preferredWorkerHost)
	if !ok {
		o.logMergeWorkerFailure(issue, "worker_host_unavailable", nil)
		o.recordMergeFailed(state, issue, now, "worker_host_unavailable", nil)
		return dispatchIssueOutcome{reason: dispatchIssueFailureWorkerHostUnavailable}
	}

	globalSlot, ok, decision := o.acquireGlobalDispatchSlot(ctx, slotIssue, workerHost, now)
	if !ok {
		o.logSchedulerSlotDecision(issue, "waiting", decision, projectStats)
		if mergeWorkerIssue(issue) {
			o.logMergeWorkerSlotWait(issue, decision, projectStats)
		} else {
			o.logDispatchSlotWait(issue, decision, projectStats)
			o.recordDispatchSlotWait(state, issue, decision, projectStats, now)
		}
		return dispatchIssueOutcome{reason: dispatchIssueFailureGlobalSlotUnavailable}
	}
	mergeTiming := o.markMergeWorkerSlotAcquired(state, issue, now)
	o.logSchedulerSlotDecision(issue, "acquired", decision, projectStats)
	o.logMergeWorkerSlotAcquired(issue, decision, projectStats, mergeTiming)
	o.logWorkerLifecycle(issue, "worker_slot_acquired",
		"attempt", attempt,
		"worker_host", strings.TrimSpace(workerHost),
	)
	canary, allowed := tryReserveProjectFailureBreakerCanary(state, issue.ID, now)
	if !allowed {
		o.releaseGlobalDispatchSlot(globalSlot)
		return dispatchIssueOutcome{reason: projectFailureBreakerDispatchPaused}
	}

	claimedIssue, claim, ok := o.claimIssue(ctx, issue, now)
	if !ok {
		if canary {
			releaseProjectFailureBreakerCanary(state, issue.ID)
		}
		o.releaseGlobalDispatchSlot(globalSlot)
		o.logWorkerLifecycle(issue, "worker_capacity_released",
			"attempt", attempt,
			"worker_host", strings.TrimSpace(workerHost),
			"reason", dispatchIssueFailureClaimFailed,
		)
		o.logMergeWorkerFailure(issue, "claim_failed", nil)
		o.recordMergeFailed(state, issue, now, "claim_failed", nil)
		return dispatchIssueOutcome{reason: dispatchIssueFailureClaimFailed}
	}

	issue = cloneIssue(claimedIssue)
	priorAttempt := state.PriorAttempts[issue.ID]
	if !priorAttemptPresent(priorAttempt) {
		if breakerAttempt, ok := o.spendProgressPriorAttempt(ctx, issue); ok {
			priorAttempt = breakerAttempt
		}
	}
	workAttemptID, ok := o.startDurableWorkAttempt(ctx, state, issue, attempt, now, workerHost, runMode)
	if !ok {
		if canary {
			releaseProjectFailureBreakerCanary(state, issue.ID)
		}
		o.releaseGlobalDispatchSlot(globalSlot)
		o.logWorkerLifecycle(issue, "worker_capacity_released",
			"attempt", attempt,
			"worker_host", strings.TrimSpace(workerHost),
			"reason", dispatchIssueFailureWorkAttemptStart,
		)
		if abandonErr := o.abandonClaim(ctx, issue.ID); abandonErr != nil && o.logger != nil {
			o.logger.Warn("abandon claim after work attempt start failed", "issue_id", issue.ID, "error", abandonErr)
		}
		o.logMergeWorkerFailure(issue, "work_attempt_start_failed", nil)
		o.recordMergeFailed(state, issue, now, "work_attempt_start_failed", nil)
		return dispatchIssueOutcome{reason: dispatchIssueFailureWorkAttemptStart}
	}
	dispatchSourceState := ""
	dispatchTargetState := ""
	dispatchStartSourceState := ""
	dispatchStartTargetState := ""
	if targetState != "" {
		sourceState := issue.State
		if err := o.updateIssueState(ctx, state, issue, targetState, now, "dispatch_start"); err != nil {
			if canary {
				releaseProjectFailureBreakerCanary(state, issue.ID)
			}
			o.releaseGlobalDispatchSlot(globalSlot)
			o.completeDurableWorkAttempt(ctx, state, Running{
				Issue:         issue,
				Attempt:       attempt,
				WorkAttemptID: workAttemptID,
				Mode:          runMode,
				StartedAt:     now,
				WorkerHost:    workerHost,
			}, now, store.WorkAttemptTerminalFailure, workAttemptErrorStartTransition, err.Error(), "starting", "start state transition failed")
			o.logWorkerLifecycle(issue, "worker_capacity_released",
				"attempt", attempt,
				"worker_host", strings.TrimSpace(workerHost),
				"reason", dispatchIssueFailureStartStateTransition,
			)
			if abandonErr := o.abandonClaim(ctx, issue.ID); abandonErr != nil && o.logger != nil {
				o.logger.Warn("abandon claim after start state transition failed", "issue_id", issue.ID, "error", abandonErr)
			}
			if o.logger != nil {
				o.logger.Warn("start state transition failed", "issue_id", issue.ID, "identifier", issue.Identifier, "from_state", issue.State, "target_state", targetState, "error", err)
			}
			o.logMergeWorkerFailure(issue, "start_state_transition_failed", err)
			o.recordMergeFailed(state, issue, now, "start_state_transition_failed", err)
			return dispatchIssueOutcome{reason: dispatchIssueFailureStartStateTransition}
		}
		issue.State = targetState
		dispatchSourceState = sourceState
		dispatchTargetState = targetState
		dispatchStartSourceState = sourceState
		dispatchStartTargetState = targetState
	}
	if dispatchSourceState == "" || dispatchTargetState == "" {
		dispatchSourceState, dispatchTargetState = o.dispatchTimelineTransitionContext(ctx, issue)
	}
	o.markMergeStarted(state, issue, now)
	claim.Issue = issue
	runCtx, stop := context.WithCancelCause(ctx)
	cancel := func() { stop(nil) }
	o.markBackendCapacityProbe(state, capacityProbeKey, issue.ID, now)
	state.Running[issue.ID] = Running{
		Issue:               issue,
		Attempt:             attempt,
		WorkAttemptID:       workAttemptID,
		Mode:                runMode,
		DispatchSourceState: dispatchStartSourceState,
		DispatchTargetState: dispatchStartTargetState,
		StartedAt:           now,
		WorkerHost:          workerHost,
		CapacityScope:       capacityScope,
		CapacityProbe:       capacityProbeKey != "",
		StopDestination:     o.cfg.StopRunTargetState,
		globalSlot:          globalSlot,
		cancel:              cancel,
		stop:                stop,
	}
	o.setGlobalDispatchPreempt(globalSlot, cancel)
	state.Claimed[issue.ID] = claim
	delete(state.Retry, issue.ID)
	delete(state.Blocked, issue.ID)
	delete(state.BudgetRefusals, issue.ID)
	delete(state.ReapedWorkspaces, issue.ID)
	delete(state.Completed, issue.ID)

	request := RunRequest{
		Issue:               issue,
		Attempt:             attempt,
		WorkAttemptID:       workAttemptID,
		Mode:                runMode,
		DispatchSourceState: dispatchSourceState,
		DispatchTargetState: dispatchTargetState,
		PriorAttempt:        priorAttempt,
		StartedAt:           now,
		WorkerHost:          workerHost,
		SelectorContext:     o.selectorContext(),
		OnUsageUpdate:       o.usageUpdateHandler(runCtx, issue.ID),
		OnActivityUpdate:    o.activityUpdateHandler(runCtx, issue),
		OnOverrideRejected:  o.agentOverrideRejectionHandler(runCtx, issue),
	}
	if retryQueued {
		request.RetryMode = queuedRetry.RetryMode
		request.ResumeState = queuedRetry.ResumeState
	}
	if priorAttempt.ExplainBeforeRetry {
		delete(state.PriorAttempts, issue.ID)
	}
	o.logMergeWorkerAttempt(issue, attempt, workerHost)
	o.logWorkerLifecycle(issue, "worker_attempt_started",
		"attempt", attempt,
		"worker_host", strings.TrimSpace(workerHost),
		"mode", strings.TrimSpace(runMode),
	)
	running := state.Running[issue.ID]
	running.done = o.supervisor.Dispatch(runCtx, request, o.runResults)
	state.Running[issue.ID] = running
	return dispatchIssueOutcome{dispatched: true}
}

func (o *Orchestrator) dispatchTimelineTransitionContext(ctx context.Context, issue connector.Issue) (string, string) {
	match, ok := o.latestWorkflowLaneEntry(ctx, issue)
	if !ok {
		return "", ""
	}
	sourceState := strings.TrimSpace(match.Event.PreviousPhaseName)
	targetState := strings.TrimSpace(match.Event.PhaseName)
	if sourceState == "" || targetState == "" {
		return "", ""
	}
	if normalizeState(targetState) != normalizeState(issue.State) {
		return "", ""
	}
	return sourceState, targetState
}

// dispatchWorkingStates lists the active-state names recognized as the
// "agent is working" lane, in preference order: "In Progress" is the GitHub
// Projects convention, "Production" the non-code artifact template's. Without
// this resolution, projects using the template vocabulary never leave Todo
// while an agent works them and the board shows nothing running.
var dispatchWorkingStates = []string{planImplementationState, "Production"}

func dispatchStartTransitionState(issue connector.Issue, mode string, activeStates []string) string {
	if mode != runpkg.RunModeImplement {
		return ""
	}
	if normalizeState(issue.State) != "todo" {
		return ""
	}
	for _, working := range dispatchWorkingStates {
		if stateIn(working, activeStates) {
			return working
		}
	}
	return ""
}

func (o *Orchestrator) dispatchMode(ctx context.Context, state *State, issue connector.Issue) string {
	if normalizeState(issue.State) == normalizeState(autoPromoteMergingState) && o.cfg.MergeFastPathEnabled {
		return runpkg.RunModeMerge
	}
	cfg := gate.EffectivePlan(o.cfg.Plan)
	if !cfg.Enabled {
		return runpkg.RunModeImplement
	}
	switch normalizeState(issue.State) {
	case "todo":
		return runpkg.RunModePlan
	case normalizeState(autoPromoteReworkState):
		if match, ok := o.latestWorkflowLaneEntry(ctx, issue); ok {
			if normalizeState(match.Event.PhaseName) == normalizeState(autoPromoteReworkState) &&
				workflowLaneMetadataHasAction(match.Metadata, workflowActionPlanReviewRework) {
				return runpkg.RunModePlan
			}
			return runpkg.RunModeImplement
		}
		issueID := strings.TrimSpace(issue.ID)
		if issueID != "" {
			if _, ok := state.planRework[issueID]; ok {
				return runpkg.RunModePlan
			}
		}
	}
	return runpkg.RunModeImplement
}

func (o *Orchestrator) markGlobalProjectIdle() {
	if o.globalDispatchGate == nil {
		return
	}
	o.globalDispatchGate.MarkIdle(o.cfg.Project.ID)
}

type projectCycleDispatchGate interface {
	BeginProjectCycle(scheduler.ProjectCandidate)
	EndProjectCycle(string)
}

func (o *Orchestrator) beginGlobalProjectCycle() {
	if gate, ok := o.globalDispatchGate.(projectCycleDispatchGate); ok {
		gate.BeginProjectCycle(o.cfg.Project)
		return
	}
	o.markGlobalProjectIdle()
}

func (o *Orchestrator) endGlobalProjectCycle() {
	if gate, ok := o.globalDispatchGate.(projectCycleDispatchGate); ok {
		gate.EndProjectCycle(o.cfg.Project.ID)
	}
}

type projectStateSlotStats struct {
	capacity  int
	used      int
	available int
}

type detailedProjectDispatchGate interface {
	TryAcquireWithDecision(context.Context, scheduler.ProjectCandidate, scheduler.SlotRequest, time.Time) (
		scheduler.Slot,
		bool,
		scheduler.DispatchGateDecision,
		error,
	)
}

func (o *Orchestrator) acquireGlobalDispatchSlot(
	ctx context.Context,
	issue connector.Issue,
	workerHost string,
	now time.Time,
) (scheduler.Slot, bool, scheduler.DispatchGateDecision) {
	if o.globalDispatchGate == nil {
		return scheduler.Slot{}, true, scheduler.DispatchGateDecision{Reason: scheduler.DispatchGateReasonGranted}
	}

	req := scheduler.SlotRequest{
		State:    issue.State,
		Host:     workerHost,
		Priority: o.dispatchStatePriority(issue.State),
	}
	var (
		slot     scheduler.Slot
		ok       bool
		decision scheduler.DispatchGateDecision
		err      error
	)
	if detailed, hasDecision := o.globalDispatchGate.(detailedProjectDispatchGate); hasDecision {
		slot, ok, decision, err = detailed.TryAcquireWithDecision(ctx, o.cfg.Project, req, now)
	} else {
		slot, ok, err = o.globalDispatchGate.TryAcquire(ctx, o.cfg.Project, req, now)
		if ok {
			decision.Reason = scheduler.DispatchGateReasonGranted
		}
	}
	if err != nil {
		if o.logger != nil {
			o.logger.Warn("global dispatch slot unavailable", "project_id", o.cfg.Project.ID, "issue_id", issue.ID, "error", err)
		}
		return scheduler.Slot{}, false, decision
	}
	return slot, ok, decision
}

func (o *Orchestrator) dispatchStatePriority(state string) int {
	return dispatchpriority.New(o.cfg.DispatchPriorityByState, nil).State(state)
}

func (o *Orchestrator) projectStateSlotStats(issue connector.Issue, state *State) projectStateSlotStats {
	limit := o.cfg.MaxConcurrentAgents
	normalized := normalizeState(issue.State)
	if stateLimit, ok := o.cfg.MaxConcurrentAgentsByState[normalized]; ok {
		limit = stateLimit
	}

	used := 0
	if state != nil {
		for _, running := range state.Running {
			if normalizeState(running.Issue.State) == normalized {
				used++
			}
		}
	}
	available := limit - used
	if available < 0 {
		available = 0
	}
	return projectStateSlotStats{capacity: limit, used: used, available: available}
}

func (o *Orchestrator) releaseGlobalDispatchSlot(slot scheduler.Slot) {
	if o.globalDispatchGate == nil || slot == (scheduler.Slot{}) {
		return
	}
	if err := o.globalDispatchGate.Release(slot); err != nil && o.logger != nil {
		o.logger.Warn("release global dispatch slot failed", "project_id", o.cfg.Project.ID, "error", err)
	}
}

func (o *Orchestrator) setGlobalDispatchPreempt(slot scheduler.Slot, preempt func()) {
	if o.globalDispatchGate == nil || slot == (scheduler.Slot{}) {
		return
	}
	o.globalDispatchGate.SetPreempt(slot, preempt)
}

func (o *Orchestrator) selectorContext() selector.Context {
	ctx := selector.Context{
		Persona: o.cfg.SelectorPersona,
	}
	if identifier, ok := o.connector.(connector.InstanceIdentifier); ok {
		ctx.InstanceLogin = identifier.InstanceLogin()
	}
	return ctx
}

func (o *Orchestrator) usageUpdateHandler(ctx context.Context, issueID string) runpkg.UsageUpdateHandler {
	return func(update runpkg.UsageUpdate) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		select {
		case o.runUpdates <- runUpdate{issueID: issueID, usage: update}:
			return nil
		default:
			return nil
		}
	}
}

func (o *Orchestrator) activityUpdateHandler(ctx context.Context, issue connector.Issue) runpkg.AgentActivityUpdateHandler {
	return func(update runpkg.AgentActivityUpdate) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if o.activity == nil {
			return nil
		}
		o.activity.Publish(activity.Key{ProjectID: o.cfg.Project.ID, IssueID: issue.ID}, activity.Event{
			At:                update.At,
			DetentSessionID:   update.DetentSessionID,
			ProviderSessionID: update.ProviderSessionID,
			TurnID:            update.TurnID,
			ItemID:            update.ItemID,
			Kind:              string(update.Type),
			Title:             activityUpdateTitle(update),
			Content:           update.Content,
			Status:            update.Status,
			Model:             update.Model,
			TotalTokens:       update.TotalTokens,
		})
		return nil
	}
}

func activityUpdateTitle(update runpkg.AgentActivityUpdate) string {
	switch update.Type {
	case runpkg.AgentUpdateMessageDelta:
		return "Agent"
	case runpkg.AgentUpdateToolStarted:
		return "Tool started · " + strings.TrimSpace(update.Tool)
	case runpkg.AgentUpdateToolOutput:
		return "Tool output · " + strings.TrimSpace(update.Tool)
	case runpkg.AgentUpdateToolCompleted:
		return "Tool finished · " + strings.TrimSpace(update.Tool)
	case runpkg.AgentUpdateTokenUsage:
		return "Usage"
	case runpkg.AgentUpdateTurnStarted:
		return "Turn started"
	case runpkg.AgentUpdateTurnCompleted:
		return "Turn finished"
	case runpkg.AgentUpdateProcessStarted:
		return "Worker started"
	case runpkg.AgentUpdateModelUpdated:
		return "Model updated"
	default:
		return strings.TrimSpace(string(update.Type))
	}
}

func validCandidate(issue connector.Issue) bool {
	return issue.ID != "" &&
		issue.Identifier != "" &&
		issue.Title != "" &&
		issue.State != "" &&
		!issue.Closed &&
		issue.AssignedToWorker
}

func duplicatePullRequestWork(issue connector.Issue) bool {
	if issue.PullRequest == nil {
		return false
	}
	switch normalizePullRequestState(issue.PullRequest.State) {
	case "merged":
		return !staleMergedPullRequestHasFailedCIEvidence(issue.PullRequest, staleMergedPullRequestSummaryFromIssue(issue))
	case "open":
		return normalizeState(issue.State) == "todo"
	default:
		return false
	}
}

func mergedPullRequestReconciliationPending(issue connector.Issue, cfg Config) bool {
	if issue.PullRequest == nil || normalizePullRequestState(issue.PullRequest.State) != "merged" {
		return false
	}
	summary := staleMergedPullRequestSummaryFromIssue(issue)
	decision := staleMergedPullRequestDecision(issue, summary)
	targetState := staleMergedPullRequestTargetState(decision, cfg.AutoPromote, cfg.TerminalStates)
	return targetState != "" && normalizeState(targetState) != normalizeState(issue.State)
}

func continuationDispatch(issue connector.Issue) bool {
	state := normalizeState(issue.State)
	return state != "" && state != "todo"
}

func continuationDelay(index int) time.Duration {
	if index <= 0 {
		return 0
	}
	return continuationDispatchBackoff
}

func waitForDispatchBackoff(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		return true
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func todoBlockedByNonTerminal(issue connector.Issue, terminalStates []string) bool {
	if normalizeState(issue.State) != "todo" {
		return false
	}

	for _, blocker := range issue.BlockedBy {
		if strings.TrimSpace(blocker.State) == "" {
			continue
		}
		if !stateIn(blocker.State, terminalStates) {
			return true
		}
	}
	return false
}

func availableSlots(state *State) int {
	available := state.MaxConcurrentAgents - len(state.Running)
	if available < 0 {
		return 0
	}
	return available
}
