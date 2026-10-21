package templates

import "github.com/digitaldrywood/detent/internal/telemetry"

func projectKanbanHumanDependencyWait(refs []telemetry.BlockedRef) string {
	for _, ref := range refs {
		if ref.HumanOwned && !ref.HumanCompletionReady {
			return "Needs you · human prerequisite " + ref.Identifier + " · closure and completion evidence required"
		}
	}
	return ""
}
