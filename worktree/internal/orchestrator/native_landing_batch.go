package orchestrator

import (
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type nativeLandingDispatchBatch struct {
	batch   *workspace.LandingBatch
	host    string
	members int
}

func (p dispatchPlanner) joinsNativeLandingBatch(issue connector.Issue) bool {
	return p.nativeLandingBatch != nil && p.nativeLandingBatch.members > 0 && p.nativeWorkflow && !p.cfg.Policy.Gates.GitHubPullRequest && mergeWorkerIssue(issue)
}
