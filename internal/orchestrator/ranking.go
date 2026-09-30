package orchestrator

import (
	"sort"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/dispatchpriority"
)

func sortIssuesForDispatch(issues []connector.Issue, dispatchStatePriority []string, dispatchLabelPriority []string, prioritizeUnblockers bool) {
	ranker := dispatchpriority.New(dispatchStatePriority, dispatchLabelPriority)
	mergingFirst := len(dispatchStatePriority) > 0 && normalizeState(dispatchStatePriority[0]) == normalizeState(autoPromoteMergingState)

	sort.SliceStable(issues, func(i, j int) bool {
		left := issues[i]
		right := issues[j]

		leftMerging := mergingFirst && normalizeState(left.State) == normalizeState(autoPromoteMergingState)
		rightMerging := mergingFirst && normalizeState(right.State) == normalizeState(autoPromoteMergingState)
		if leftMerging != rightMerging {
			return leftMerging
		}
		if leftRank, rightRank := dispatchpriority.Priority(left.Priority), dispatchpriority.Priority(right.Priority); leftRank != rightRank {
			return leftRank < rightRank
		}
		leftLabel, leftLabeled := ranker.MatchLabel(left.Labels)
		rightLabel, rightLabeled := ranker.MatchLabel(right.Labels)
		if leftLabeled != rightLabeled {
			return leftLabeled
		}
		if leftLabeled && leftLabel.Rank != rightLabel.Rank {
			return leftLabel.Rank < rightLabel.Rank
		}
		if leftRank, rightRank := ranker.State(left.State), ranker.State(right.State); leftRank != rightRank {
			return leftRank < rightRank
		}
		if prioritizeUnblockers && !leftLabeled && left.UnblockerCount != right.UnblockerCount {
			return left.UnblockerCount > right.UnblockerCount
		}
		if left.CreatedAt != nil && right.CreatedAt != nil && !left.CreatedAt.Equal(*right.CreatedAt) {
			return left.CreatedAt.Before(*right.CreatedAt)
		}
		if left.CreatedAt != nil && right.CreatedAt == nil {
			return true
		}
		if left.CreatedAt == nil && right.CreatedAt != nil {
			return false
		}

		return left.Identifier < right.Identifier
	})
}

func annotateUnblockerCounts(targets []connector.Issue, issues []connector.Issue, activeStates []string, terminalStates []string, enabled bool) {
	dispatchpriority.AnnotateUnblockerCounts(targets, issues, activeStates, terminalStates, enabled)
}
