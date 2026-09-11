package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

// The operator routine is the single home for board remediation. Each kind
// wraps one absorbed recovery loop; a kind outside operator.actions never runs.
const operatorRoutineOrigin = "operator_routine"

type OperatorConfig struct {
	Actions       []string
	MergeWedgeAge time.Duration
}

type laneWriteActionRecorder interface {
	RecordLaneWriteAction(context.Context, uint64, string, string) error
}

func (c OperatorConfig) actionEnabled(kind string) bool {
	actions := c.Actions
	if actions == nil {
		actions = workflowconfig.DefaultOperatorActions()
	}
	for _, action := range actions {
		if strings.EqualFold(strings.TrimSpace(action), kind) {
			return true
		}
	}
	return false
}

func (c OperatorConfig) mergeWedgeAge() time.Duration {
	if c.MergeWedgeAge > 0 {
		return c.MergeWedgeAge
	}
	return time.Duration(workflowconfig.DefaultOperatorMergeWedgeSeconds) * time.Second
}

// runOperatorAction stamps every lane write the action applies with the
// routine origin and action kind so the operations report can show it.
func (o *Orchestrator) runOperatorAction(kind string, run func() map[string]struct{}) map[string]struct{} {
	if !o.cfg.Operator.actionEnabled(kind) {
		return map[string]struct{}{}
	}
	previousOrigin, previousAction := o.laneWriteOrigin, o.laneWriteAction
	o.laneWriteOrigin, o.laneWriteAction = operatorRoutineOrigin, kind
	defer func() { o.laneWriteOrigin, o.laneWriteAction = previousOrigin, previousAction }()
	return run()
}

func (o *Orchestrator) operatorReturnRetiredParks(ctx context.Context, state *State, issues []connector.Issue, now time.Time) map[string]struct{} {
	return o.runOperatorAction(workflowconfig.OperatorActionReturnRetiredParks, func() map[string]struct{} {
		return o.recoverBlockedIssues(ctx, state, issues, now)
	})
}

func (o *Orchestrator) operatorClearClosedDependencies(ctx context.Context, state *State, issues []connector.Issue, now time.Time) map[string]struct{} {
	return o.runOperatorAction(workflowconfig.OperatorActionClearClosedDependencies, func() map[string]struct{} {
		return o.autoUnblockDependencyIssues(ctx, state, issues, now)
	})
}

func (o *Orchestrator) operatorRestoreStuckMerging(ctx context.Context, state *State, issues []connector.Issue, now time.Time) map[string]struct{} {
	return o.runOperatorAction(workflowconfig.OperatorActionRestoreStuckMerging, func() map[string]struct{} {
		return o.reconcileStaleMergingPullRequestIssues(ctx, state, issues, now)
	})
}

func (o *Orchestrator) operatorMergeWedgedPullRequests(ctx context.Context, state *State, issues []connector.Issue, now time.Time) map[string]struct{} {
	return o.runOperatorAction(workflowconfig.OperatorActionMergeWhenWedged, func() map[string]struct{} {
		transitioned := map[string]struct{}{}
		merger, ok := o.connector.(connector.PullRequestMerger)
		if !ok {
			return transitioned
		}
		for _, issue := range issues {
			if !operatorMergeWedgedCandidate(issue, state, o.cfg, now) {
				continue
			}
			if err := merger.MergePullRequest(ctx, pullRequestRepository(issue), pullRequestNumber(issue), strings.TrimSpace(issue.PullRequest.HeadSHA), o.cfg.MergeMethod); err != nil {
				if o.logger != nil {
					o.logger.Warn("operator_merge_when_wedged_failed", mergeWorkerLogAttrs(issue, "error", err)...)
				}
				continue
			}
			merged := cloneIssue(issue)
			merged.PullRequest.State = "MERGED"
			activityAt := now.UTC()
			merged.PullRequest.ActivityAt = &activityAt
			if err := o.updateIssueState(ctx, state, merged, doneStateName(o.cfg.TerminalStates), now, "merge_worker_programmatic_merge"); err != nil {
				if o.logger != nil {
					o.logger.Warn("operator_merge_when_wedged_state_update_failed", mergeWorkerLogAttrs(issue, "error", err)...)
				}
				continue
			}
			transitioned[strings.TrimSpace(issue.ID)] = struct{}{}
			recordStateEvent(state, telemetry.ActivityEvent{
				At:      now,
				Event:   "merge_worker_programmatic_merge",
				Message: fmt.Sprintf("operator routine merged %s after the merge path stayed wedged for %s", issueLabel(issue), o.cfg.Operator.mergeWedgeAge()),
			})
		}
		return transitioned
	})
}

// A Merging issue already passed its promotion gate. It is wedged when its
// green, thread-free head has waited past the threshold with no queue entry
// and no merge worker acting on it.
func operatorMergeWedgedCandidate(issue connector.Issue, state *State, cfg Config, now time.Time) bool {
	pullRequest := issue.PullRequest
	if pullRequest == nil || strings.TrimSpace(issue.ID) == "" || normalizeState(issue.State) != normalizeState(autoPromoteMergingState) {
		return false
	}
	if normalizePullRequestState(pullRequest.State) != "open" || pullRequest.Draft || strings.TrimSpace(pullRequest.HeadSHA) == "" {
		return false
	}
	if !mergeWorkerCIGreen(pullRequest.CIStatus) || len(pullRequest.UnresolvedReviewThreads) > 0 || strings.EqualFold(strings.TrimSpace(pullRequest.MergeableState), "dirty") {
		return false
	}
	if pullRequestRepository(issue) == "" || pullRequestNumber(issue) <= 0 {
		return false
	}
	if issue.StageUpdatedAt == nil || now.Sub(*issue.StageUpdatedAt) < cfg.Operator.mergeWedgeAge() {
		return false
	}
	if state != nil {
		if nativeMergeQueueHasEntry(state, issue) || staleMergingPullRequestDispatchActive(state, strings.TrimSpace(issue.ID)) {
			return false
		}
	}
	return true
}
