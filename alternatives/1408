package nochange

import (
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workpad"
)

type State struct {
	Message         string
	Worktree        string
	Repository      string
	PolicyID        string
	Validation      *gate.CommandResult
	UncertainEffect bool
}

func Prepare(diff tracker.AttemptDiffRequest, detail tracker.ChangeDetail, state State) *runner.NativeChange {
	signal, _ := runner.NativeCompletionSignalFromMessage(state.Message)
	reported, evidence, ok := workpad.NoChangeCompletion(signal)
	if !ok || len(diff.Files) != 0 || diff.BaseSHA == "" || state.Worktree != "clean" || state.UncertainEffect {
		return nil
	}
	if detail.Change.ID != "" {
		currentEmpty := false
		for _, version := range detail.Versions {
			if version.ID == detail.Change.CurrentVersion && version.Source != nil && version.Source.DiffSHA256 == tracker.ChangeSourceDigest(nil) {
				currentEmpty = true
			}
		}
		if !currentEmpty {
			return nil
		}
	}
	return &runner.NativeChange{BaseSHA: diff.BaseSHA, HeadSHA: diff.BaseSHA, NoChange: &tracker.NativeNoChangeCompletion{ReportedSHA: reported, VerifiedSHA: diff.BaseSHA, Repository: state.Repository, Evidence: evidence, PolicyID: state.PolicyID, Validation: state.Validation}}
}
