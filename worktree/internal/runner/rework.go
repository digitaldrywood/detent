package runner

import (
	"errors"

	"github.com/digitaldrywood/detent/internal/workspace"
)

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

func reworkPreparationReason(status workspace.MergePrepareStatus, err error) string {
	if err != nil {
		var preparationErr *workspace.ReworkPreparationError
		if errors.As(err, &preparationErr) {
			return string(preparationErr.Reason)
		}
		return string(workspace.ReworkPreparationFailed)
	}
	if status == workspace.MergePrepareStatusDirty {
		return string(workspace.ReworkPreparationDirtyWorktree)
	}
	return reworkPreparationOutcome(status, nil)
}
