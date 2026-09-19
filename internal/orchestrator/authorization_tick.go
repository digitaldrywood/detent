package orchestrator

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

// filterAuthorizedTickIssues is the authorization boundary for fetched work.
// Fetch hints are advisory: neither a candidate nor an observed lane grants
// this instance permission to reconcile an issue. Prefer fresh candidates over
// retained snapshots and decide once for issues present in multiple lists.
func (o *Orchestrator) filterAuthorizedTickIssues(ctx context.Context, state *State, fetched tickFetchedIssues, previous *tickPreviousState, now time.Time) tickFetchedIssues {
	if !o.cfg.Authorization.Configured() {
		return fetched
	}
	decisions := make(map[string]bool)
	skipped := 0
	planner := o.dispatchPlanner()
	filter := func(issues []connector.Issue) []connector.Issue {
		out := make([]connector.Issue, 0, len(issues))
		for _, issue := range issues {
			key := issueIdentityKey(issue)
			matched, seen := decisions[key]
			if !seen {
				authorization := planner.authorizationDecision(issue)
				matched = authorization.Matched
				decisions[key] = matched
				if !matched {
					skipped++
					decision := dispatchPlanDecision{Issue: issue, SkipReason: dispatchSkipAuthorizationSelector, SkipDetail: authorization.Detail, AuthorizationDecision: &authorization}
					if retry, ok := state.Retry[issue.ID]; ok {
						decision.Retry, decision.Attempt, decision.WorkerHost = true, retry.Attempt, retry.WorkerHost
						if _, blocked := state.Blocked[issue.ID]; blocked {
							planner.releaseClaim(state, issue.ID)
						} else {
							planner.releaseIssue(state, issue.ID)
						}
					}
					// Only dispatch lanes need per-card evidence; all exclusions still
					// contribute to the aggregate and release retry ownership.
					if !issue.Closed && stateIn(issue.State, o.cfg.ActiveStates) && !stateIn(issue.State, o.cfg.TerminalStates) {
						o.recordSchedulerDecision(ctx, state, now, decision, "skipped", dispatchSkipAuthorizationSelector)
					}
				}
			}
			if matched {
				out = append(out, issue)
			}
		}
		return out
	}
	fetched.candidates = filter(fetched.candidates)
	fetched.status = filter(fetched.status)
	// Retained transition inputs must not reintroduce excluded cards after fetch.
	previous.pipeline = filter(previous.pipeline)
	previous.epicTransitionWatch = filter(previous.epicTransitionWatch)
	previous.blockedStatusIssues = filter(previous.blockedStatusIssues)
	for key, issue := range previous.pendingEpicParentLookups {
		if len(filter([]connector.Issue{issue})) == 0 {
			delete(previous.pendingEpicParentLookups, key)
		}
	}
	state.Pipeline = filter(state.Pipeline)
	state.LaneSignalCandidates = filter(state.LaneSignalCandidates)
	if skipped > 0 && o.logger != nil {
		o.logger.Info("authorization_selector_declined", "skipped_count", skipped)
	}
	return fetched
}
