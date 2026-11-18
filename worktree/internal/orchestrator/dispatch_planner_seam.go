package orchestrator

import (
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/orchestrator/dispatchplan"
)

type dispatchCandidateOrder = dispatchplan.CandidateOrder
type dispatchWaitState = dispatchplan.WaitState

func (p dispatchPlanner) eligibility() dispatchplan.Eligibility {
	return dispatchplan.NewEligibility(dispatchplan.EligibilityConfig{
		ActiveStates:     p.cfg.ActiveStates,
		TerminalStates:   p.cfg.TerminalStates,
		Authorization:    p.cfg.Authorization,
		SelectorContext:  p.cfg.SelectorContext,
		OwnershipSet:     p.cfg.Claiming.OwnershipSet,
		OwnershipMode:    p.cfg.Claiming.OwnershipMode,
		AssigneeRequired: p.cfg.Claiming.AssigneeRequired,
	}, cloneIssue)
}

func dispatchWaitStateForIssue(issueID string, state *State, dueRetries map[string]Retry) dispatchWaitState {
	_, due := dueRetries[issueID]
	_, running := state.Running[issueID]
	_, retry := state.Retry[issueID]
	_, claimed := state.Claimed[issueID]
	blocked, parked := state.Blocked[issueID]
	return dispatchWaitState{DueRetry: due, Running: running, RetryPending: retry, Claimed: claimed, Blocked: parked && !blockedFromDependency(blocked)}
}

func dispatchHostConfig(cfg Config) dispatchplan.HostConfig {
	return dispatchplan.HostConfig{
		WorkerHosts:                cfg.WorkerHosts,
		WorkerHostSelection:        cfg.WorkerHostSelection,
		WorkerHostCaps:             cfg.WorkerHostCaps,
		MaxConcurrentAgentsPerHost: cfg.MaxConcurrentAgentsPerHost,
	}
}

type workerHostCounts struct{ state *State }

func (c workerHostCounts) count(host string) int { return runningWorkerHostCount(c.state, host) }

func dispatchMergeOrder(state *State, issues []connector.Issue, now time.Time, cfg Config) dispatchplan.MergeOrder {
	return dispatchplan.MergeOrder{
		StickyIssueID: stickyMergingIssueID(state, issues, now, cfg.MergeFairnessAge),
		MergeState:    autoPromoteMergingState,
		RepositoryKey: mergeWorkerRepositoryKey,
		Aged:          func(issue connector.Issue) bool { return mergeWorkerIssueAged(issue, now, cfg.MergeFairnessAge) },
		HeadReady:     func(issue connector.Issue) bool { return mergeWorkerHeadReady(issue, cfg) },
	}
}

func dispatchMergePriority(priority dispatchplan.MergePriority) mergingIssuePriority {
	return mergingIssuePriority{reasons: priority.Reasons, stickyIssueID: priority.StickyIssueID}
}
