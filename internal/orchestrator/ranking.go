package orchestrator

import (
	"sort"
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/dispatchpriority"
)

func sortIssuesForDispatch(issues []connector.Issue, dispatchStatePriority []string, dispatchLabelPriority []string, prioritizeUnblockers bool) {
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
		leftLabel, leftLabeled := ranker.MatchLabel(left.Labels)
		rightLabel, rightLabeled := ranker.MatchLabel(right.Labels)
		if leftLabeled != rightLabeled {
			return leftLabeled
		}
		if leftLabeled && leftLabel.Rank != rightLabel.Rank {
			return leftLabel.Rank < rightLabel.Rank
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
	for index := range targets {
		targets[index].UnblockerCount = 0
	}
	if !enabled || len(targets) == 0 || len(issues) == 0 {
		return
	}

	targetsByRef := make(map[string]int, len(targets)*2)
	for index, issue := range targets {
		if issue.Closed || normalizeState(issue.State) == "blocked" || !stateIn(issue.State, activeStates) || dependencyWaiting(issue, terminalStates) {
			continue
		}
		for _, ref := range issueReferenceKeys(issue.ID, issue.Identifier) {
			targetsByRef[ref] = index
		}
	}

	counted := make(map[int]map[string]struct{})
	for _, dependent := range issues {
		if normalizeState(dependent.State) != "blocked" && !dependencyWaiting(dependent, terminalStates) {
			continue
		}
		dependentKey := firstNonBlank(strings.TrimSpace(dependent.ID), strings.TrimSpace(dependent.Identifier))
		if dependentKey == "" {
			continue
		}
		for _, blocker := range dependent.BlockedBy {
			index, ok := unblockerTargetIndex(targetsByRef, blocker)
			if !ok || stateIn(blocker.State, terminalStates) {
				continue
			}
			if counted[index] == nil {
				counted[index] = map[string]struct{}{}
			}
			if _, ok := counted[index][dependentKey]; ok {
				continue
			}
			counted[index][dependentKey] = struct{}{}
			targets[index].UnblockerCount++
		}
	}
}

func dependencyWaiting(issue connector.Issue, terminalStates []string) bool {
	for _, blocker := range issue.BlockedBy {
		if strings.TrimSpace(blocker.State) == "" || !stateIn(blocker.State, terminalStates) {
			return true
		}
	}
	return false
}

func unblockerTargetIndex(targets map[string]int, blocker connector.BlockedRef) (int, bool) {
	for _, ref := range issueReferenceKeys(blocker.ID, blocker.Identifier) {
		if index, ok := targets[ref]; ok {
			return index, true
		}
	}
	return 0, false
}

func issueReferenceKeys(values ...string) []string {
	keys := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		key := strings.ToLower(strings.TrimSpace(value))
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	return keys
}
