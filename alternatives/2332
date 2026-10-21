package orchestrator

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestCloneIssueHeadCommitEvidence(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	issue := connector.Issue{PRHeadCommittedAt: &at, PullRequest: &connector.PullRequest{HeadCommittedAt: &at}}
	cloned := cloneIssue(issue)
	*cloned.PRHeadCommittedAt = at.Add(time.Hour)
	*cloned.PullRequest.HeadCommittedAt = at.Add(2 * time.Hour)
	if !issue.PRHeadCommittedAt.Equal(at) || !issue.PullRequest.HeadCommittedAt.Equal(at) {
		t.Fatal("clone shares head-commit evidence")
	}
}
