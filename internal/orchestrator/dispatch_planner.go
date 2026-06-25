package orchestrator

import (
	"math"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/selector"
)

type dispatchPlanner struct {
	cfg Config
}

type dispatchPlanHooks struct {
	hydrate             func(connector.Issue) (connector.Issue, bool)
	beforeDispatch      func(connector.Issue, int) bool
	dispatch            func(connector.Issue, int, string) bool
	dispatchFailed      func(connector.Issue) bool
	retryDispatchFailed func(connector.Issue, Retry)
}

type dispatchAction struct {
	issue      connector.Issue
	attempt    int
	workerHost string
	retry      bool
}

func newDispatchPlanner(cfg Config) dispatchPlanner {
	return dispatchPlanner{cfg: normalizeConfig(cfg)}
}

func (p dispatchPlanner) plan(
	state *State,
	candidates []connector.Issue,
	now time.Time,
	hooks dispatchPlanHooks,
) DispatchPlan {
	state.ensureInitialized(p.cfg)

	plannedCandidates := cloneIssues(candidates)
	sortIssuesForDispatch(plannedCandidates, p.cfg.DispatchPriorityByState, p.cfg.DispatchPriorityByLabel)
	dueRetries := dueRetriesByIssue(state, now)
	p.releaseMissingDueRetries(state, plannedCandidates, dueRetries)

	plan := DispatchPlan{}
	continuations := 0
	for _, issue := range plannedCandidates {
		if retry, ok := dueRetries[issue.ID]; ok {
			action, ok := p.retryAction(state, issue, retry, now)
			if !ok {
				continue
			}
			if p.applyDispatchAction(state, action, now, hooks) {
				plan.Dispatches = append(plan.Dispatches, action.decision())
			} else if hooks.retryDispatchFailed != nil {
				hooks.retryDispatchFailed(action.issue, retry)
			}
			continue
		}
		if availableSlots(state) == 0 {
			break
		}
		if hooks.hydrate != nil {
			var ok bool
			issue, ok = hooks.hydrate(issue)
			if !ok {
				continue
			}
		}
		action, ok := p.dispatchAction(state, issue, now)
		if !ok {
			continue
		}
		continuationIndex := -1
		if continuationDispatch(action.issue) {
			continuationIndex = continuations
			continuations++
		}
		if hooks.beforeDispatch != nil && !hooks.beforeDispatch(action.issue, continuationIndex) {
			break
		}
		if p.applyDispatchAction(state, action, now, hooks) {
			plan.Dispatches = append(plan.Dispatches, action.decision())
		} else if hooks.dispatchFailed != nil && !hooks.dispatchFailed(action.issue) {
			break
		}
	}

	plan.Claimed = claimedIDs(state.Claimed)
	plan.Blocked = blockedIDs(state.Blocked)
	plan.BudgetRefusals = budgetRefusalIDs(state.BudgetRefusals)
	plan.Retry = retryIDs(state.Retry)
	return plan
}

func (p dispatchPlanner) applyDispatchAction(
	state *State,
	action dispatchAction,
	now time.Time,
	hooks dispatchPlanHooks,
) bool {
	if hooks.dispatch != nil {
		return hooks.dispatch(action.issue, action.attempt, action.workerHost)
	}
	p.markDispatched(state, action.issue, action.attempt, now, action.workerHost)
	return true
}

func (p dispatchPlanner) retryAction(
	state *State,
	issue connector.Issue,
	retry Retry,
	now time.Time,
) (dispatchAction, bool) {
	delete(state.Retry, retry.Issue.ID)

	if !p.dispatchableForRetry(issue, state, now, retry.WorkerHost) {
		if p.budgetCooldownActive(state, issue.ID, now) {
			p.scheduleRetry(state, issue, retry.Attempt, now, "budget cooldown active", false, retry.WorkerHost)
			return dispatchAction{}, false
		}
		if !p.slotsAvailable(issue, state, retry.WorkerHost) {
			p.scheduleRetry(state, issue, retry.Attempt, now, "no available orchestrator slots", false, retry.WorkerHost)
			return dispatchAction{}, false
		}
		if _, blocked := state.Blocked[issue.ID]; blocked {
			p.releaseClaim(state, issue.ID)
			return dispatchAction{}, false
		}

		p.releaseIssue(state, issue.ID)
		return dispatchAction{}, false
	}

	return p.newDispatchAction(state, issue, retry.Attempt, retry.WorkerHost, true)
}

func (p dispatchPlanner) dispatchAction(state *State, issue connector.Issue, now time.Time) (dispatchAction, bool) {
	if !p.dispatchable(issue, state, now) {
		if todoBlockedByNonTerminal(issue, p.cfg.TerminalStates) {
			state.Blocked[issue.ID] = Blocked{
				Issue:     cloneIssue(issue),
				Reason:    blockedReasonDependency,
				BlockedAt: now,
				Source:    BlockedSourceDependency,
			}
		}
		return dispatchAction{}, false
	}

	return p.newDispatchAction(state, issue, 0, "", false)
}

func (p dispatchPlanner) newDispatchAction(
	state *State,
	issue connector.Issue,
	attempt int,
	preferredWorkerHost string,
	retry bool,
) (dispatchAction, bool) {
	workerHost, ok := p.selectWorkerHost(state, preferredWorkerHost)
	if !ok {
		return dispatchAction{}, false
	}

	return dispatchAction{
		issue:      cloneIssue(issue),
		attempt:    attempt,
		workerHost: workerHost,
		retry:      retry,
	}, true
}

func (p dispatchPlanner) markDispatched(
	state *State,
	issue connector.Issue,
	attempt int,
	now time.Time,
	workerHost string,
) {
	issue = cloneIssue(issue)
	state.Running[issue.ID] = Running{
		Issue:      issue,
		Attempt:    attempt,
		StartedAt:  now,
		WorkerHost: workerHost,
	}
	state.Claimed[issue.ID] = Claimed{
		Issue:     issue,
		ClaimedAt: now,
	}
	delete(state.Retry, issue.ID)
	delete(state.Blocked, issue.ID)
	delete(state.BudgetRefusals, issue.ID)
	delete(state.Completed, issue.ID)
}

func (a dispatchAction) decision() DispatchDecision {
	return DispatchDecision{
		IssueID:    a.issue.ID,
		Identifier: a.issue.Identifier,
		State:      a.issue.State,
		Attempt:    a.attempt,
		WorkerHost: a.workerHost,
		Retry:      a.retry,
	}
}

func (p dispatchPlanner) pruneBudgetRefusals(state *State, now time.Time) {
	for issueID, refusal := range state.BudgetRefusals {
		if !p.budgetRefusalActive(refusal, now) {
			delete(state.BudgetRefusals, issueID)
		}
	}
}

func (p dispatchPlanner) budgetCooldownActive(state *State, issueID string, now time.Time) bool {
	refusal, ok := state.BudgetRefusals[issueID]
	if !ok {
		return false
	}

	return p.budgetRefusalActive(refusal, now)
}

func (p dispatchPlanner) budgetRefusalActive(refusal BudgetRefusal, now time.Time) bool {
	if refusal.ResetAt != nil && now.Before(*refusal.ResetAt) {
		return true
	}
	if p.cfg.BudgetRefusalCooldown <= 0 || refusal.RefusedAt.IsZero() {
		return false
	}

	return now.Before(refusal.RefusedAt.Add(p.cfg.BudgetRefusalCooldown))
}

func (p dispatchPlanner) trackBlockedCandidates(state *State, issues []connector.Issue, now time.Time) {
	seenBlocked := make(map[string]struct{})
	for _, issue := range issues {
		if issue.ID == "" {
			continue
		}
		if todoBlockedByNonTerminal(issue, p.cfg.TerminalStates) {
			seenBlocked[issue.ID] = struct{}{}
			state.Blocked[issue.ID] = Blocked{
				Issue:     cloneIssue(issue),
				Reason:    blockedReasonDependency,
				BlockedAt: now,
				Source:    BlockedSourceDependency,
			}
		}
	}

	for issueID, blocked := range state.Blocked {
		if !blockedFromDependency(blocked) {
			continue
		}
		if _, ok := seenBlocked[issueID]; !ok {
			delete(state.Blocked, issueID)
		}
	}
}

func (p dispatchPlanner) releaseMissingDueRetries(
	state *State,
	issues []connector.Issue,
	dueRetries map[string]Retry,
) {
	if len(dueRetries) == 0 {
		return
	}

	byID := make(map[string]struct{}, len(issues))
	for _, issue := range issues {
		byID[issue.ID] = struct{}{}
	}

	for issueID := range dueRetries {
		if _, ok := byID[issueID]; !ok {
			if _, blocked := state.Blocked[issueID]; blocked {
				p.releaseClaim(state, issueID)
				continue
			}
			p.releaseIssue(state, issueID)
		}
	}
}

func (p dispatchPlanner) dispatchable(issue connector.Issue, state *State, now time.Time) bool {
	return p.dispatchableIssue(issue, state, false, now, "")
}

func (p dispatchPlanner) dispatchableForRetry(
	issue connector.Issue,
	state *State,
	now time.Time,
	preferredWorkerHost string,
) bool {
	return p.dispatchableIssue(issue, state, true, now, preferredWorkerHost)
}

func (p dispatchPlanner) dispatchableIssue(
	issue connector.Issue,
	state *State,
	allowClaimed bool,
	now time.Time,
	preferredWorkerHost string,
) bool {
	if !validCandidate(issue) {
		return false
	}
	if !stateIn(issue.State, p.cfg.ActiveStates) || stateIn(issue.State, p.cfg.TerminalStates) {
		return false
	}
	if pullRequestHydrationBlocksDispatch(issue) {
		return false
	}
	if duplicatePullRequestWork(issue) {
		return false
	}
	if !p.authorized(issue) {
		return false
	}
	if todoBlockedByNonTerminal(issue, p.cfg.TerminalStates) {
		return false
	}
	if _, ok := state.Running[issue.ID]; ok {
		return false
	}
	if _, ok := state.Claimed[issue.ID]; ok && !allowClaimed {
		return false
	}
	if _, ok := state.Blocked[issue.ID]; ok {
		return false
	}
	if p.budgetCooldownActive(state, issue.ID, now) {
		return false
	}

	return p.slotsAvailable(issue, state, preferredWorkerHost)
}

func pullRequestHydrationBlocksDispatch(issue connector.Issue) bool {
	pullRequest := issue.PullRequest
	if !pullRequestHydrationBlocksProgress(pullRequest) {
		return false
	}
	if normalizeState(issue.State) != "todo" {
		return true
	}
	return pullRequest.Number > 0 ||
		strings.TrimSpace(pullRequest.URL) != "" ||
		strings.TrimSpace(pullRequest.BranchName) != "" ||
		normalizePullRequestState(pullRequest.State) != ""
}

func (p dispatchPlanner) authorized(issue connector.Issue) bool {
	if !p.cfg.Authorization.Configured() {
		return true
	}
	return selector.Match(issue, p.cfg.Authorization, p.cfg.SelectorContext)
}

func (p dispatchPlanner) slotsAvailable(issue connector.Issue, state *State, preferredWorkerHost string) bool {
	return availableSlots(state) > 0 &&
		p.stateSlotsAvailable(issue, state) &&
		p.workerSlotsAvailable(state, preferredWorkerHost)
}

func (p dispatchPlanner) stateSlotsAvailable(issue connector.Issue, state *State) bool {
	limit := p.cfg.MaxConcurrentAgents
	if stateLimit, ok := p.cfg.MaxConcurrentAgentsByState[normalizeState(issue.State)]; ok {
		limit = stateLimit
	}

	used := 0
	normalized := normalizeState(issue.State)
	for _, running := range state.Running {
		if normalizeState(running.Issue.State) == normalized {
			used++
		}
	}

	return used < limit
}

func (p dispatchPlanner) workerSlotsAvailable(state *State, preferredWorkerHost string) bool {
	_, ok := p.selectWorkerHost(state, preferredWorkerHost)
	return ok
}

func (p dispatchPlanner) selectWorkerHost(state *State, preferredWorkerHost string) (string, bool) {
	if len(p.cfg.WorkerHosts) == 0 {
		return "", true
	}

	availableHosts := make([]string, 0, len(p.cfg.WorkerHosts))
	for _, host := range p.cfg.WorkerHosts {
		if p.workerHostSlotsAvailable(state, host) {
			availableHosts = append(availableHosts, host)
		}
	}
	if len(availableHosts) == 0 {
		return "", false
	}

	preferredWorkerHost = strings.TrimSpace(preferredWorkerHost)
	if preferredWorkerHost != "" {
		if slices.Contains(availableHosts, preferredWorkerHost) {
			return preferredWorkerHost, true
		}
	}

	return leastLoadedWorkerHost(state, availableHosts), true
}

func (p dispatchPlanner) workerHostSlotsAvailable(state *State, workerHost string) bool {
	if p.cfg.MaxConcurrentAgentsPerHost <= 0 {
		return true
	}

	return runningWorkerHostCount(state, workerHost) < p.cfg.MaxConcurrentAgentsPerHost
}

func (p dispatchPlanner) scheduleRetry(
	state *State,
	issue connector.Issue,
	attempt int,
	now time.Time,
	err string,
	continuation bool,
	workerHost string,
) {
	if attempt < 1 {
		attempt = 1
	}

	p.scheduleRetryAfter(state, issue, attempt, now, p.retryDelay(attempt, continuation), err, workerHost)
}

func (p dispatchPlanner) scheduleRetryAfter(
	state *State,
	issue connector.Issue,
	attempt int,
	now time.Time,
	delay time.Duration,
	err string,
	workerHost string,
) {
	if attempt < 1 {
		attempt = 1
	}
	if delay < 0 {
		delay = 0
	}

	issue = cloneIssue(issue)
	state.Retry[issue.ID] = Retry{
		Issue:      issue,
		Attempt:    attempt,
		DueAt:      now.Add(delay),
		Error:      err,
		WorkerHost: workerHost,
	}
	if _, ok := state.Claimed[issue.ID]; !ok {
		state.Claimed[issue.ID] = Claimed{
			Issue:     issue,
			ClaimedAt: now,
		}
	}
}

func (p dispatchPlanner) retryDelay(attempt int, continuation bool) time.Duration {
	if continuation {
		return p.cfg.ContinuationRetryDelay
	}
	if attempt < 1 {
		attempt = 1
	}
	exponent := min(attempt-1, 30)

	delay := p.cfg.FailureRetryBaseDelay * time.Duration(math.Pow(2, float64(exponent)))
	if delay > p.cfg.MaxRetryBackoff {
		return p.cfg.MaxRetryBackoff
	}
	return delay
}

func (p dispatchPlanner) releaseIssue(state *State, issueID string) {
	cancelRunning(state, issueID)
	delete(state.Running, issueID)
	delete(state.Claimed, issueID)
	delete(state.Blocked, issueID)
	delete(state.Retry, issueID)
	delete(state.BudgetRefusals, issueID)
}

func (p dispatchPlanner) releaseClaim(state *State, issueID string) {
	cancelRunning(state, issueID)
	delete(state.Running, issueID)
	delete(state.Claimed, issueID)
	delete(state.Retry, issueID)
	delete(state.BudgetRefusals, issueID)
}
