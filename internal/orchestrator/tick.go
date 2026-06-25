package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

type tickPreviousState struct {
	lastRefreshAt            time.Time
	pipeline                 []connector.Issue
	epicTransitionWatch      []connector.Issue
	blockedStatusIssues      []connector.Issue
	pendingEpicParentLookups map[string]connector.Issue
}

type tickFetchedIssues struct {
	candidates []connector.Issue
	status     []connector.Issue
	statusOK   bool
}

type tickTransitionRefresh struct {
	issues               []connector.Issue
	pendingTransitions   []connector.Issue
	pendingParentLookups map[string]connector.Issue
	blockedRefreshOK     bool
}

type githubBudgetReserveDecision struct {
	degraded       bool
	restRemaining  int64
	restReserve    int64
	graphRemaining int64
	graphReserve   int64
}

func (o *Orchestrator) tick(ctx context.Context, state *State, now time.Time) {
	o.tickWithManual(ctx, state, now, nil)
}

func (o *Orchestrator) tickManual(ctx context.Context, state *State, request manualRefreshRequest) {
	o.tickWithManual(ctx, state, request.requestedAt, &request)
}

func (o *Orchestrator) tickWithManual(ctx context.Context, state *State, now time.Time, manual *manualRefreshRequest) {
	if manual != nil {
		startManualRefresh(state, *manual, now)
	}
	completed := false
	previous := captureTickPreviousState(state)
	o.markRefresh(state, now)
	defer func() {
		o.finishRefresh(state, now)
		if manual != nil {
			finishManualRefresh(state, *manual, completed)
		}
	}()

	if pause := o.gitHubGraphQLPause(state, now); pause > 0 {
		o.logger.Warn("github graphql polling paused", "remaining", gitHubGraphQLRemaining(state), "pause", pause)
		return
	}
	if pause := o.gitHubRESTPause(state, now); pause > 0 {
		o.logger.Warn("github rest polling paused", "remaining", gitHubRESTRemaining(state), "pause", pause)
		return
	}

	reserve := o.githubBudgetReserveDecision(state)
	if reserve.degraded {
		o.logGitHubBudgetReserveDecision(reserve)
		recordStateEvent(state, telemetry.ActivityEvent{
			At:      now,
			Event:   "github_budget_reserved",
			Message: githubBudgetReserveMessage(reserve),
		})
	}

	o.refreshActiveRuns(ctx, state, now, reserve)
	if state.Draining {
		return
	}
	fetched, ok := o.fetchTickIssues(ctx, state, now, reserve)
	if !ok {
		return
	}
	fetched = retainUnavailablePullRequestsFromPrevious(fetched, previous)

	transitions := o.refreshTransitionSets(ctx, state, fetched, previous)
	completedEpics := o.resolveCompletedEpics(ctx, state, transitions, previous)
	fetched = filterReconciledTickIssues(
		state,
		fetched,
		o.reconcileClosedCompletedIssueStatuses(ctx, state, transitions.issues, now),
	)
	if fetched.statusOK {
		fetched = filterReconciledTickIssues(
			state,
			fetched,
			o.recoverBlockedIssues(ctx, state, fetched.status, now),
		)
		fetched = filterReconciledTickIssues(
			state,
			fetched,
			o.autoUnblockDependencyIssues(ctx, state, fetched.status, now),
		)
		fetched = filterReconciledTickIssues(
			state,
			fetched,
			o.reviewPlanIssues(ctx, state, fetched.status, now),
		)
		autoPromoted := o.autoPromoteHumanReviewIssues(ctx, state, fetched.status, now)
		fetched = filterReconciledTickIssues(
			state,
			fetched,
			autoPromoted.transitioned,
		)
		fetched = filterReconciledTickIssues(
			state,
			fetched,
			o.reconcileStaleMergingPullRequestIssues(ctx, state, fetched.status, now),
		)
		fetched.candidates = mergeIssueSlices(
			fetched.candidates,
			autoPromoted.dispatchCandidates,
		)
		fetched.candidates = mergeIssueSlices(
			fetched.candidates,
			o.mergeWorkerDispatchCandidates(state, fetched.status),
		)
	}
	fetched = filterReconciledTickIssues(
		state,
		fetched,
		o.reconcileStaleTodoPullRequestIssues(ctx, state, fetched.candidates, now),
	)
	fetched = filterReconciledTickIssues(
		state,
		fetched,
		o.transitionCompletedActiveIssuesToReview(ctx, state, fetched.candidates, now),
	)
	state.BoardIssues = boardIssuesFromFetched(fetched)
	o.dispatchTickIssues(ctx, state, fetched, transitions, previous, completedEpics, now)
	completed = true
}

func startManualRefresh(state *State, request manualRefreshRequest, now time.Time) {
	if state == nil {
		return
	}
	requestedAt := request.requestedAt.UTC()
	startedAt := now.UTC()
	state.ManualRefresh = telemetry.RefreshAttempt{
		ID:          request.id,
		Status:      telemetry.RefreshAttemptStatusInProgress,
		RequestedAt: &requestedAt,
		StartedAt:   &startedAt,
		Operations:  append([]string(nil), request.operations...),
	}
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      startedAt,
		Event:   "manual_refresh_started",
		Message: "manual refresh " + request.id + " started",
	})
}

func finishManualRefresh(state *State, request manualRefreshRequest, completed bool) {
	if state == nil {
		return
	}
	completedAt := time.Now().UTC()
	manual := state.ManualRefresh
	if strings.TrimSpace(manual.ID) != request.id {
		manual = telemetry.RefreshAttempt{
			ID:          request.id,
			RequestedAt: timePointer(request.requestedAt),
			StartedAt:   timePointer(request.requestedAt),
			Operations:  append([]string(nil), request.operations...),
		}
	}
	manual.CompletedAt = &completedAt
	if completed && strings.TrimSpace(state.LastRefreshError) == "" && state.LastRefreshErrorAt.IsZero() {
		manual.Status = telemetry.RefreshAttemptStatusSucceeded
		manual.LastError = ""
		manual.LastErrorAt = nil
		recordStateEvent(state, telemetry.ActivityEvent{
			At:      completedAt,
			Event:   "manual_refresh_succeeded",
			Message: "manual refresh " + request.id + " succeeded",
		})
		state.ManualRefresh = manual
		return
	}

	manual.Status = telemetry.RefreshAttemptStatusFailed
	manual.LastError = strings.TrimSpace(state.LastRefreshError)
	if manual.LastError == "" {
		manual.LastError = "manual refresh did not complete"
	}
	errorAt := state.LastRefreshErrorAt
	if errorAt.IsZero() {
		errorAt = completedAt
	}
	errorAt = errorAt.UTC()
	manual.LastErrorAt = &errorAt
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      completedAt,
		Event:   "manual_refresh_failed",
		Message: "manual refresh " + request.id + " failed: " + manual.LastError,
	})
	state.ManualRefresh = manual
}

func captureTickPreviousState(state *State) tickPreviousState {
	return tickPreviousState{
		lastRefreshAt:            state.LastRefreshAt,
		pipeline:                 cloneIssues(state.Pipeline),
		epicTransitionWatch:      cloneIssues(state.epicTransitionWatch),
		blockedStatusIssues:      blockedStatusTransitionIssues(state.Blocked),
		pendingEpicParentLookups: cloneIssueMap(state.pendingEpicParentLookups),
	}
}

func (o *Orchestrator) refreshActiveRuns(ctx context.Context, state *State, now time.Time, reserve githubBudgetReserveDecision) {
	if reserve.degraded {
		o.logger.Warn(
			"workspace cleanup skipped to preserve shared github budget",
			"rest_remaining", reserve.restRemaining,
			"rest_reserve", reserve.restReserve,
			"graphql_remaining", reserve.graphRemaining,
			"graphql_reserve", reserve.graphReserve,
		)
	} else {
		o.reapWorkspacesIfDue(ctx, state, now)
	}
	o.reconcileRunningIssues(ctx, state, now)
	o.failStalledMergeWorkerStarts(state, now)
	o.heartbeatRunningClaims(ctx, state, now)
}

func (o *Orchestrator) fetchTickIssues(
	ctx context.Context,
	state *State,
	now time.Time,
	reserve githubBudgetReserveDecision,
) (tickFetchedIssues, bool) {
	observedStates := o.observedStatusFetchStates()

	candidateIssues, err := o.connector.FetchCandidateIssues(ctx)
	if err != nil {
		o.logger.Warn("fetch candidate issues failed", "error", err)
		markRefreshError(state, "fetch candidate issues failed: "+err.Error(), now)
		return tickFetchedIssues{}, false
	}

	fetched := tickFetchedIssues{
		candidates: cloneIssues(candidateIssues),
	}
	if len(observedStates) == 0 {
		fetched.statusOK = true
		clearRefreshError(state)
		return fetched, true
	}
	if reserve.degraded {
		o.logger.Warn(
			"observed status polling skipped to preserve shared github budget",
			"state_count", len(observedStates),
			"rest_remaining", reserve.restRemaining,
			"rest_reserve", reserve.restReserve,
			"graphql_remaining", reserve.graphRemaining,
			"graphql_reserve", reserve.graphReserve,
		)
		return fetched, true
	}
	if !tickHasActiveWork(state, candidateIssues) && !o.observedWorkExists(ctx, observedStates) {
		fetched.statusOK = true
		clearRefreshError(state)
		return fetched, true
	}

	statusIssues, statusErr := o.connector.FetchIssuesByStates(ctx, observedStates)
	if statusErr != nil {
		o.logger.Warn("fetch observed status issues failed", "error", statusErr)
		markRefreshError(state, "fetch observed status issues failed: "+statusErr.Error(), now)
		return fetched, true
	}
	fetched.status = cloneIssues(statusIssues)
	fetched.statusOK = true
	if !o.hydratePlanIssueComments(ctx, &fetched) {
		markRefreshError(state, "fetch plan issue comments failed", now)
		return tickFetchedIssues{}, false
	}
	clearRefreshError(state)
	return fetched, true
}

func markRefreshError(state *State, message string, at time.Time) {
	if state == nil {
		return
	}
	message = strings.TrimSpace(message)
	if message == "" {
		message = "tracker refresh failed"
	}
	if at.IsZero() {
		at = time.Now()
	}
	state.LastRefreshError = message
	state.LastRefreshErrorAt = at.UTC()
}

func clearRefreshError(state *State) {
	if state == nil {
		return
	}
	state.LastRefreshError = ""
	state.LastRefreshErrorAt = time.Time{}
}

func (o *Orchestrator) observedWorkExists(ctx context.Context, observedStates []string) bool {
	if prober, ok := o.connector.(connector.IssueStateProber); ok {
		issues, err := prober.FetchIssueStateProbe(ctx, observedStates, 1)
		if err != nil {
			o.logger.Warn("fetch lightweight observed status probe failed", "error", err)
			return false
		}
		return len(issues) > 0
	}
	limiter, ok := o.connector.(connector.IssuesByStatesLimiter)
	if !ok {
		return true
	}
	issues, err := limiter.FetchIssuesByStatesLimit(ctx, observedStates, 1)
	if err != nil {
		o.logger.Warn("fetch bounded observed status probe failed", "error", err)
		return false
	}
	return len(issues) > 0
}

func (o *Orchestrator) githubBudgetReserveDecision(state *State) githubBudgetReserveDecision {
	decision := githubBudgetReserveDecision{
		restRemaining:  gitHubRESTRemaining(state),
		restReserve:    o.cfg.GitHubRESTMinReserve,
		graphRemaining: gitHubGraphQLRemaining(state),
		graphReserve:   o.cfg.GitHubGraphQLMinReserve,
	}
	if bucket := gitHubRESTBucketFromState(state); budgetBelowReserve(bucket, o.cfg.GitHubRESTMinReserve) {
		decision.degraded = true
	}
	if bucket := gitHubGraphQLBucketFromState(state); budgetBelowReserve(bucket, o.cfg.GitHubGraphQLMinReserve) {
		decision.degraded = true
	}
	return decision
}

func budgetBelowReserve(bucket *telemetry.RateLimitBucket, reserve int64) bool {
	return bucket != nil && reserve > 0 && bucket.Limit > 0 && bucket.Remaining <= reserve
}

func (o *Orchestrator) logGitHubBudgetReserveDecision(decision githubBudgetReserveDecision) {
	o.logger.Warn(
		"github polling degraded to preserve shared budget",
		"rest_remaining", decision.restRemaining,
		"rest_reserve", decision.restReserve,
		"graphql_remaining", decision.graphRemaining,
		"graphql_reserve", decision.graphReserve,
	)
}

func githubBudgetReserveMessage(decision githubBudgetReserveDecision) string {
	return fmt.Sprintf(
		"GitHub polling degraded to preserve shared budget for user and AI work; REST remaining=%d reserve=%d GraphQL remaining=%d reserve=%d",
		decision.restRemaining,
		decision.restReserve,
		decision.graphRemaining,
		decision.graphReserve,
	)
}

func tickHasActiveWork(state *State, candidates []connector.Issue) bool {
	if len(candidates) > 0 {
		return true
	}
	if state == nil {
		return false
	}
	return len(state.Running) > 0 ||
		len(state.Retry) > 0 ||
		len(state.Blocked) > 0 ||
		len(state.Pipeline) > 0 ||
		len(state.epicTransitionWatch) > 0 ||
		len(state.pendingEpicParentLookups) > 0
}

func (o *Orchestrator) refreshTransitionSets(
	ctx context.Context,
	state *State,
	fetched tickFetchedIssues,
	previous tickPreviousState,
) tickTransitionRefresh {
	transitionIssues := cloneIssues(fetched.candidates)
	pipelineIssues, pipelineRefreshOK := o.fetchEpicTransitionIssueStates(ctx, previous.pipeline)
	transitionIssues = append(transitionIssues, pipelineIssues...)
	watchedIssues, watchRefreshOK := o.fetchEpicTransitionIssueStates(ctx, previous.epicTransitionWatch)
	transitionIssues = append(transitionIssues, watchedIssues...)
	blockedIssues, blockedRefreshOK := o.fetchEpicTransitionIssueStates(ctx, previous.blockedStatusIssues)
	transitionIssues = append(transitionIssues, blockedIssues...)
	pendingTransitions, pendingParentLookups := o.refreshPendingEpicParentLookups(ctx, previous.pendingEpicParentLookups)
	transitionIssues = append(transitionIssues, pendingTransitions...)

	state.epicTransitionWatch = issuesInStates(fetched.candidates, o.cfg.ActiveStates)
	if !watchRefreshOK {
		state.epicTransitionWatch = mergeIssueSlices(state.epicTransitionWatch, previous.epicTransitionWatch)
	}
	if fetched.statusOK {
		transitionIssues = append(transitionIssues, fetched.status...)
		state.Pipeline = issuesInStates(fetched.status, prPipelineFetchStates())
		if !pipelineRefreshOK {
			state.Pipeline = mergeIssueSlices(state.Pipeline, previous.pipeline)
		}
	}

	return tickTransitionRefresh{
		issues:               transitionIssues,
		pendingTransitions:   pendingTransitions,
		pendingParentLookups: pendingParentLookups,
		blockedRefreshOK:     blockedRefreshOK,
	}
}

func (o *Orchestrator) resolveCompletedEpics(
	ctx context.Context,
	state *State,
	transitions tickTransitionRefresh,
	previous tickPreviousState,
) map[string]struct{} {
	previousTransitions := mergeIssueSlices(previous.pipeline, previous.epicTransitionWatch)
	previousTransitions = mergeIssueSlices(previousTransitions, previous.blockedStatusIssues)
	completedEpics, failedParentLookups := o.closeCompletedEpicsForTerminalTransitions(
		ctx,
		transitions.issues,
		previousTransitions,
		previous.lastRefreshAt,
		transitions.pendingTransitions,
	)
	state.pendingEpicParentLookups = mergeIssueMaps(transitions.pendingParentLookups, failedParentLookups)
	return completedEpics
}

func filterReconciledTickIssues(
	state *State,
	fetched tickFetchedIssues,
	reconciled map[string]struct{},
) tickFetchedIssues {
	fetched.candidates = filterReconciledIssues(fetched.candidates, reconciled)
	fetched.status = filterReconciledIssues(fetched.status, reconciled)
	state.epicTransitionWatch = filterReconciledIssues(state.epicTransitionWatch, reconciled)
	state.Pipeline = filterReconciledIssues(state.Pipeline, reconciled)
	return fetched
}

func boardIssuesFromFetched(fetched tickFetchedIssues) []connector.Issue {
	issues := cloneIssues(fetched.candidates)
	if fetched.statusOK {
		issues = mergeIssueSlices(issues, fetched.status)
	}
	return issues
}

func retainUnavailablePullRequestsFromPrevious(fetched tickFetchedIssues, previous tickPreviousState) tickFetchedIssues {
	previousIssues := mergeIssueSlices(previous.pipeline, previous.epicTransitionWatch)
	fetched.candidates = retainUnavailablePullRequests(fetched.candidates, previousIssues)
	fetched.status = retainUnavailablePullRequests(fetched.status, previousIssues)
	return fetched
}

func retainUnavailablePullRequests(current []connector.Issue, previous []connector.Issue) []connector.Issue {
	if len(current) == 0 || len(previous) == 0 {
		return current
	}
	previousByKey := make(map[string]connector.Issue, len(previous))
	for _, issue := range previous {
		key := issueIdentityKey(issue)
		if key == "" {
			continue
		}
		previousByKey[key] = cloneIssue(issue)
	}
	out := cloneIssues(current)
	for index, issue := range out {
		reason := pullRequestHydrationUnavailableReason(issue.PullRequest)
		if reason == "" {
			continue
		}
		prior, ok := previousByKey[issueIdentityKey(issue)]
		if !ok || prior.PullRequest == nil {
			continue
		}
		retained := cloneIssue(prior).PullRequest
		retained.HydrationUnavailableReason = reason
		retained.HydrationNextRetryAt = cloneTime(issue.PullRequest.HydrationNextRetryAt)
		out[index].PullRequest = retained
		if out[index].PRNumber == nil && prior.PRNumber != nil {
			prNumber := *prior.PRNumber
			out[index].PRNumber = &prNumber
		}
		if out[index].PRRepository == "" {
			out[index].PRRepository = prior.PRRepository
		}
	}
	return out
}

func (o *Orchestrator) dispatchTickIssues(
	ctx context.Context,
	state *State,
	fetched tickFetchedIssues,
	transitions tickTransitionRefresh,
	previous tickPreviousState,
	completedEpics map[string]struct{},
	now time.Time,
) {
	issues := filterCompletedEpicCandidates(fetched.candidates, completedEpics)
	planner := o.dispatchPlanner()
	planner.pruneBudgetRefusals(state, now)
	planner.trackBlockedCandidates(state, issues, now)
	candidateBlockedStatusIssues := issuesInStates(fetched.candidates, []string{blockedStatusState})
	if fetched.statusOK {
		currentBlockedStatusIssues := candidateBlockedStatusIssues
		currentBlockedStatusIssues = mergeIssueSlices(currentBlockedStatusIssues, issuesInStates(fetched.status, []string{blockedStatusState}))
		if !transitions.blockedRefreshOK {
			currentBlockedStatusIssues = mergeIssueSlices(currentBlockedStatusIssues, previous.blockedStatusIssues)
		}
		o.trackBlockedStatusIssues(state, currentBlockedStatusIssues, now)
	} else {
		o.upsertBlockedStatusIssues(state, candidateBlockedStatusIssues, now)
	}
	o.dispatchReadyIssues(ctx, state, issues, now)
}
