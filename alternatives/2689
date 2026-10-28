package runner

import "github.com/digitaldrywood/detent/internal/workspace"

func reworkPreparationOutcome(status workspace.MergePrepareStatus, err error) string {
	if err != nil {
		return "rework"
	}
	switch status {
	case workspace.MergePrepareStatusClean, workspace.MergePrepareStatusConflict, workspace.MergePrepareStatusDirty:
		return string(status)
	default:
		return "rework"
	}
}
