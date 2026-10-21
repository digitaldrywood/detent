package runner

import (
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
)

func testValidatorRequest(issue connector.Issue) ValidatorRequest {
	repo := strings.SplitN(issue.Identifier, "#", 2)[0]
	if !strings.Contains(repo, "/") {
		repo = "digitaldrywood/detent"
	}
	if issue.PullRequest == nil {
		issue.PullRequest = &connector.PullRequest{Number: 1, BaseSHA: "base", HeadSHA: "head"}
	}
	issue.PRRepository = repo
	return ValidatorRequest{Issue: issue, Diff: &connector.ValidationDiff{
		Repository: repo, PRNumber: issue.PullRequest.Number,
		BaseSHA: issue.PullRequest.BaseSHA, HeadSHA: issue.PullRequest.HeadSHA,
		Files: []string{"README.md"}, Patch: "diff --git a/README.md b/README.md\n+test\n", Digest: "test-digest",
	}}
}
