package github

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestHeadCommitReferenceEvidence(t *testing.T) {
	t.Parallel()
	var node pullRequest
	if err := json.Unmarshal([]byte(`{"number":42,"headRefOid":"head","updatedAt":"2026-09-14T13:00:00Z","commits":{"nodes":[{"commit":{"oid":"head","committedDate":"2026-09-14T12:00:00Z"}}]},"repository":{"nameWithOwner":"o/r"}}`), &node); err != nil {
		t.Fatal(err)
	}
	ref := pullRequestReferenceFromNode(node)
	for _, tt := range []struct {
		name, head, repo string
		number           int
		want             bool
	}{{"same head", "head", "o/r", 42, true}, {"new head", "new", "o/r", 42, false}, {"other PR", "head", "o/r", 43, false}, {"other repo", "head", "other/r", 42, false}} {
		t.Run(tt.name, func(t *testing.T) {
			issue := connector.Issue{PRNumber: &ref.Number, PRRepository: ref.Repository, PRHeadSHA: ref.HeadSHA, PRHeadCommittedAt: ref.HeadCommittedAt}
			attachPullRequestToIssue(&issue, pullRequestRepo{Owner: tt.repo[:len(tt.repo)-2], Name: "r"}, pullRequestNode{Number: tt.number, HeadSHA: tt.head})
			got := issue.PullRequest.HeadCommittedAt
			if (got != nil) != tt.want {
				t.Fatalf("HeadCommittedAt = %v", got)
			}
			if got != nil && !got.Equal(time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)) {
				t.Fatalf("commit date = %v; must not be PR activity time", got)
			}
		})
	}
}

func TestBranchPullRequestHeadEvidence(t *testing.T) {
	for _, tt := range []struct {
		name, oid string
		want      bool
	}{
		{"matching", "head", true}, {"changed head", "old", false}, {"missing", "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := newGraphQLTestServer(t, []graphqlTestResponse{{method: "POST", path: "/", body: `{"data":{"repository":{"pullRequest":{"number":42,"state":"OPEN","headSHA":"head","mergeableState":"CLEAN","labels":{"nodes":[{"name":"ready"}]},"commits":{"nodes":[{"commit":{"oid":"` + tt.oid + `","committedDate":"2026-09-14T12:00:00Z"}}]}}}}}`}})
			c := newGitHubTestConnector(t, server, Config{})
			pr, err := c.fetchBranchPullRequest(t.Context(), pullRequestRepo{Owner: "o", Name: "r"}, 42)
			if err != nil {
				t.Fatal(err)
			}
			applyPullRequestStatus(&pr, pullRequestStatus{})
			issue := connector.Issue{}
			attachPullRequestToIssue(&issue, pullRequestRepo{Owner: "o", Name: "r"}, pr)
			// A subsequent ordinary REST refresh of the same head retains the date.
			attachPullRequestToIssue(&issue, pullRequestRepo{Owner: "o", Name: "r"}, pullRequestNode{Number: 42, HeadSHA: "head", MergeableState: "clean"})
			if (issue.PullRequest.HeadCommittedAt != nil) != tt.want || issue.PullRequest.MergeableState != "clean" || len(pr.Labels) != 1 {
				t.Fatalf("PR = %+v", issue.PullRequest)
			}
		})
	}
}
