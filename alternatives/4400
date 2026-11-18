package dispatchplan

import (
	"slices"
	"sort"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/dispatchpriority"
	"github.com/digitaldrywood/detent/internal/scheduler"
)

type CandidateOrder struct {
	StatePriority        []string
	LabelPriority        []string
	PrioritizeUnblockers bool
	NativeMergeOrder     bool
	MergeState           string
}

type WaitState struct {
	DueRetry     bool
	Running      bool
	RetryPending bool
	Claimed      bool
	Blocked      bool
}

func OrderCandidates(issues []connector.Issue, cfg CandidateOrder, now time.Time) {
	ranker := dispatchpriority.New(cfg.StatePriority, cfg.LabelPriority)
	sort.SliceStable(issues, func(i, j int) bool {
		return ranker.Compare(dispatchpriority.Candidate{Issue: issues[i]}, dispatchpriority.Candidate{Issue: issues[j]}, cfg.PrioritizeUnblockers) < 0
	})
	if cfg.NativeMergeOrder {
		slices.SortStableFunc(issues, func(a, b connector.Issue) int {
			am, bm := MergeWorkerIssue(a, cfg.MergeState), MergeWorkerIssue(b, cfg.MergeState)
			if am && bm {
				return MergeQueueEnteredAt(a, now).Compare(MergeQueueEnteredAt(b, now))
			}
			if am {
				return -1
			}
			if bm {
				return 1
			}
			return 0
		})
	}
}

func DeferKnownWaits(issues []connector.Issue, knownWaits map[string]bool) {
	slices.SortStableFunc(issues, func(a, b connector.Issue) int {
		waitingA, waitingB := knownWaits[a.ID], knownWaits[b.ID]
		if waitingA == waitingB {
			return 0
		}
		if waitingA {
			return 1
		}
		return -1
	})
}

func KnownWaitReason(issue connector.Issue, state WaitState, terminalStates []string, dependencyBlocks func(connector.Issue, []string) bool) string {
	if state.DueRetry {
		return ""
	}
	if state.Running {
		return scheduler.DecisionReasonAlreadyRunning
	}
	if state.RetryPending {
		return scheduler.DecisionReasonRetryPending
	}
	if state.Claimed {
		return scheduler.DecisionReasonAlreadyClaimed
	}
	if state.Blocked {
		return scheduler.DecisionReasonBlocked
	}
	if issue.DependencySource == connector.BlockedRefSourceNative && dependencyBlocks(issue, terminalStates) {
		return scheduler.DecisionReasonBlockedByDependency
	}
	return ""
}

func ClearBlockedUnblockerCounts[T any](issues []connector.Issue, blocked map[string]T) {
	for index := range issues {
		if _, ok := blocked[issues[index].ID]; ok {
			issues[index].UnblockerCount = 0
		}
	}
}

func MergeQueueEnteredAt(issue connector.Issue, fallback time.Time) time.Time {
	for _, candidate := range []*time.Time{issue.StageUpdatedAt, issue.UpdatedAt, issue.CreatedAt} {
		if candidate != nil && !candidate.IsZero() {
			return candidate.UTC()
		}
	}
	return fallback.UTC()
}

func MergeWorkerIssue(issue connector.Issue, mergeState string) bool {
	return normalizeState(issue.State) == normalizeState(mergeState) || issue.NativeLandingHeadSHA != ""
}
