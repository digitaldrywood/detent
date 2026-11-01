package checkpoint

import "github.com/digitaldrywood/detent/internal/tracker"

func SourceSupersedes(detail tracker.ChangeDetail, selected string, recovery tracker.NativeRecovery) bool {
	var previous, current *tracker.ChangeVersion
	for i := range detail.Versions {
		version := &detail.Versions[i]
		if version.ID == selected {
			previous = version
		}
		if version.ID == detail.Change.CurrentVersion {
			current = version
		}
	}
	if previous == nil || current == nil || previous.ChangeID != detail.Change.ID || current.ChangeID != detail.Change.ID ||
		previous.Number <= 0 || previous.Number >= current.Number || previous.Repository != current.Repository || previous.Source == nil || current.Source == nil {
		return false
	}
	if owner := recovery.SourceAttempt(); owner != nil && owner.Checkpoint.PreservesSource() {
		checkpoint := owner.Checkpoint
		if checkpoint.UncertainForgeEffect() || checkpoint.HeadSHA != previous.HeadSHA && checkpoint.HeadSHA != current.HeadSHA || checkpoint.WorktreeState != "clean" && checkpoint.WorktreeState != "unpushed" {
			return false
		}
	}
	return true
}
