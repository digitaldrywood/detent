package orchestrator

import (
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
)

func humanDependencyWaitReason(refs []connector.BlockedRef) string {
	var waiting []string
	for _, ref := range refs {
		if ref.HumanOwned && !ref.HumanCompletionReady {
			waiting = append(waiting, ref.Identifier)
		}
	}
	if len(waiting) == 0 {
		return ""
	}
	return "waiting on human prerequisite " + strings.Join(waiting, ", ") + "; closure and completion evidence required"
}

func dependencyWaitTarget(issue connector.Issue, sourceState string) string {
	for _, state := range []string{sourceState, issue.State} {
		if normalizeState(state) == "merging" || normalizeState(state) == "rework" {
			return state
		}
	}
	if dependencyAutoUnblockStartedSignal(issue) {
		return autoPromoteReworkState
	}
	return "Todo"
}
