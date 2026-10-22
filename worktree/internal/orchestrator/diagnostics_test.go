package orchestrator

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestPullRequestDiagnosticAttrsIncludeReviewEvidenceSource(t *testing.T) {
	t.Parallel()

	attrs := pullRequestDiagnosticAttrs(connector.Issue{PullRequest: &connector.PullRequest{
		CodexReviewState:  "COMMENTED",
		CodexReviewSource: connector.PullRequestReviewSourceSummaryComment,
	}}, time.Time{})
	values := make(map[string]any, len(attrs)/2)
	for index := 0; index+1 < len(attrs); index += 2 {
		key, ok := attrs[index].(string)
		if ok {
			values[key] = attrs[index+1]
		}
	}
	if got := values["pr_codex_review_source"]; got != connector.PullRequestReviewSourceSummaryComment {
		t.Fatalf("pr_codex_review_source = %v, want %q", got, connector.PullRequestReviewSourceSummaryComment)
	}
}
