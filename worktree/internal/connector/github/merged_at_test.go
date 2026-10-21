package github

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestPullRequestMergeTimeSurvivesLaterActivity(t *testing.T) {
	t.Parallel()
	merged := "2026-09-14T10:00:00Z"
	mergedAt, err := time.Parse(time.RFC3339, merged)
	if err != nil {
		t.Fatal(err)
	}
	for _, elapsed := range []time.Duration{0, time.Hour, 24 * time.Hour} {
		t.Run(elapsed.String(), func(t *testing.T) {
			updated := mergedAt.Add(elapsed)
			node := pullRequestNodeFromREST(restPullRequest{Number: 1, MergedAt: &merged, UpdatedAt: &updated})
			var issue connector.Issue
			attachPullRequestToIssue(&issue, pullRequestRepo{}, node)
			if issue.PullRequest.MergedAt == nil || !issue.PullRequest.MergedAt.Equal(mergedAt) {
				t.Fatalf("merge time = %v", issue.PullRequest.MergedAt)
			}
			if issue.PullRequest.ActivityAt == nil || !issue.PullRequest.ActivityAt.Equal(updated) {
				t.Fatalf("activity time = %v", issue.PullRequest.ActivityAt)
			}
		})
	}
}
