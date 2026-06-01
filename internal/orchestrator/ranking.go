package orchestrator

import (
	"sort"

	"github.com/digitaldrywood/detent/internal/connector"
)

func sortIssuesForDispatch(issues []connector.Issue, dispatchPriority []string) {
	stateRanks := stateDispatchRanks(dispatchPriority)

	sort.SliceStable(issues, func(i, j int) bool {
		left := issues[i]
		right := issues[j]

		if leftRank, rightRank := stateDispatchRank(stateRanks, left.State), stateDispatchRank(stateRanks, right.State); leftRank != rightRank {
			return leftRank < rightRank
		}
		if leftRank, rightRank := priorityRank(left.Priority), priorityRank(right.Priority); leftRank != rightRank {
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

func stateDispatchRanks(states []string) map[string]int {
	ranks := make(map[string]int, len(states))
	for _, state := range states {
		state = normalizeState(state)
		if state == "" {
			continue
		}
		if _, ok := ranks[state]; ok {
			continue
		}
		ranks[state] = len(ranks)
	}
	return ranks
}

func stateDispatchRank(ranks map[string]int, state string) int {
	if rank, ok := ranks[normalizeState(state)]; ok {
		return rank
	}
	return len(ranks)
}

func priorityRank(priority *int) int {
	if priority == nil || *priority < 1 || *priority > 4 {
		return 5
	}
	return *priority
}
