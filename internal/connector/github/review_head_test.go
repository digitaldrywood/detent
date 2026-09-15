package github

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestReviewSummaryRetainsPendingHead(t *testing.T) {
	t.Parallel()
	const head = "ced1be0123456789012345678901234567890123"
	for _, tt := range []struct {
		name, commit, status, author string
		pending, current             bool
	}{
		{"stale completed", "156b300", "✅ **Completed** <relative-time datetime=\"2026-09-15T13:47:45Z\">2026-09-15T13:47:45Z</relative-time>", "chatgpt-codex-connector[bot]", true, false},
		{"current completed", head[:7], "✅ **Completed** <relative-time datetime=\"2026-09-15T13:47:45Z\">2026-09-15T13:47:45Z</relative-time>", "chatgpt-codex-connector[bot]", false, true},
		{"in progress", head[:7], "🔄 **In progress**", "chatgpt-codex-connector[bot]", true, false},
		{"untrusted summary", "156b300", "🔄 **In progress**", "untrusted[bot]", false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body := testCodexReviewSummaryBody(tt.status, tt.commit)
			server := newGraphQLTestServer(t, []graphqlTestResponse{
				{method: http.MethodGet, path: "/repos/owner/repo/pulls/42/reviews?per_page=100", body: `[]`},
				{method: http.MethodGet, path: "/repos/owner/repo/issues/42/comments?per_page=100", body: fmt.Sprintf(`[{"id":1,"body":%q,"user":{"login":%q,"type":"Bot"},"created_at":"2026-09-15T13:00:00Z","updated_at":"2026-09-15T14:00:00Z"}]`, body, tt.author)},
			})
			c := newGitHubTestConnector(t, server, Config{})
			reviews, err := c.fetchPullRequestReviews(t.Context(), pullRequestRepo{Owner: "owner", Name: "repo"}, 42, head)
			if err != nil {
				t.Fatal(err)
			}
			pr := &connector.PullRequest{HeadSHA: head, CodexReviewState: pullRequestCodexReviewStateFromReviews(reviews.CurrentHead), LatestCodexReviewState: pullRequestCodexReviewStateFromReviews(reviews.Latest)}
			if got := pr.AutomatedReviewPending(); got != tt.pending {
				t.Fatalf("pending = %v, want %v", got, tt.pending)
			}
			if got := strings.TrimSpace(pr.CodexReviewState) != ""; got != tt.current {
				t.Fatalf("current review = %v, want %v", got, tt.current)
			}
		})
	}
}
