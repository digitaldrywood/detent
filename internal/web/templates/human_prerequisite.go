package templates

import "github.com/digitaldrywood/detent/internal/telemetry"

func projectKanbanHumanDependencyWait(refs []telemetry.BlockedRef) string {
	for _, ref := range refs {
		if ref.HumanOwned && !ref.HumanCompletionReady {
			return "waiting on human prerequisite " + ref.Identifier + "; completion evidence required"
		}
	}
	return ""
}
