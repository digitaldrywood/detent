package orchestrator

import (
	"sort"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/dispatchpriority"
)

func sortIssuesForDispatch(issues []connector.Issue, dispatchStatePriority []string, dispatchLabelPriority []string) {
	ranker := dispatchpriority.New(dispatchStatePriority, dispatchLabelPriority)

	sort.SliceStable(issues, func(i, j int) bool {
		left := issues[i]
		right := issues[j]

		if leftRank, rightRank := ranker.State(left.State), ranker.State(right.State); leftRank != rightRank {
			return leftRank < rightRank
		}
		if leftRank, rightRank := dispatchpriority.Priority(left.Priority), dispatchpriority.Priority(right.Priority); leftRank != rightRank {
			return leftRank < rightRank
		}
		if leftRank, rightRank := ranker.Label(left.Labels), ranker.Label(right.Labels); leftRank != rightRank {
			return leftRank < rightRank
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
