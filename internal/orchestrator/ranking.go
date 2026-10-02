package orchestrator

import (
	"sort"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/dispatchpriority"
)

func sortIssuesForDispatch(issues []connector.Issue, dispatchStatePriority []string, dispatchLabelPriority []string, prioritizeUnblockers bool) {
	ranker := dispatchpriority.New(dispatchStatePriority, dispatchLabelPriority)
	sort.SliceStable(issues, func(i, j int) bool {
		return ranker.Compare(dispatchpriority.Candidate{Issue: issues[i]}, dispatchpriority.Candidate{Issue: issues[j]}, prioritizeUnblockers) < 0
	})
}

func annotateUnblockerCounts(targets []connector.Issue, issues []connector.Issue, activeStates []string, terminalStates []string, enabled bool) {
	dispatchpriority.AnnotateUnblockerCounts(targets, issues, activeStates, terminalStates, enabled)
}
