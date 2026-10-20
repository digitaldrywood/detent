package github

import (
	"testing"
	"time"
)

func TestIssueClosedAtSurvivesActivity(t *testing.T) {
	t.Parallel()
	closed := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	for _, age := range []time.Duration{0, time.Hour, 14 * 24 * time.Hour} {
		t.Run(age.String(), func(t *testing.T) {
			updated := closed.Add(age)
			node := githubIssueNodeFromREST(issueRef{}, restIssue{ClosedAt: &closed, UpdatedAt: &updated})
			c := &Connector{}
			issue := c.buildIssue(node, "Done", "", nil, nil)
			if issue.ClosedAt == nil || !issue.ClosedAt.Equal(closed) {
				t.Fatalf("closed_at=%v", issue.ClosedAt)
			}
		})
	}
}
