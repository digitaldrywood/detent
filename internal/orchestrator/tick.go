package orchestrator

import (
	"context"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/digitaldrywood/detent/internal/connector"
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

func (o *Orchestrator) tick(ctx context.Context, state *State, now time.Time) {
	previous := captureTickPreviousState(state)
	o.markRefresh(state, now)
	defer o.finishRefresh(state, now)

	if pause := o.gitHubGraphQLPause(state, now); pause > 0 {
		o.logger.Warn("github graphql polling paused", "remaining", gitHubGraphQLRemaining(state), "pause", pause)
		return
	}

	o.refreshActiveRuns(ctx, state, now)
	if state.Draining {
		return
	}
	fetched, ok := o.fetchTickIssues(ctx)
	if !ok {
		return
	}

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

func (o *Orchestrator) refreshActiveRuns(ctx context.Context, state *State, now time.Time) {
	o.reapWorkspacesIfDue(ctx, state, now)
	o.reconcileRunningIssues(ctx, state, now)
	o.heartbeatRunningClaims(ctx, state, now)
}

func (o *Orchestrator) fetchTickIssues(ctx context.Context) (tickFetchedIssues, bool) {
	var candidateIssues []connector.Issue
	var candidateErr error
	var statusIssues []connector.Issue
	var statusErr error
	observedStates := o.observedStatusFetchStates()

	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(2)
	group.Go(func() error {
		candidateIssues, candidateErr = o.connector.FetchCandidateIssues(groupCtx)
		return candidateErr
	})
	group.Go(func() error {
		statusIssues, statusErr = o.connector.FetchIssuesByStates(groupCtx, observedStates)
		return nil
	})
	if err := group.Wait(); err != nil && candidateErr == nil {
		candidateErr = err
	}
	if candidateErr != nil {
		o.logger.Warn("fetch candidate issues failed", "error", candidateErr)
		return tickFetchedIssues{}, false
	}

	fetched := tickFetchedIssues{
		candidates: cloneIssues(candidateIssues),
	}
	if statusErr != nil {
		o.logger.Warn("fetch observed status issues failed", "error", statusErr)
		return fetched, true
	}
	fetched.status = cloneIssues(statusIssues)
	fetched.statusOK = true
	if !o.hydratePlanIssueComments(ctx, &fetched) {
		return tickFetchedIssues{}, false
	}
	return fetched, true
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
