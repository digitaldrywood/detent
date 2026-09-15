package github

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
)

const markPullRequestReadyMutation = `mutation DetentMarkPullRequestReady($pullRequestId: ID!) {
 markPullRequestReadyForReview(input: {pullRequestId: $pullRequestId}) {
  pullRequest { id isDraft }
 }
}`

func (c *Connector) MarkPullRequestReady(ctx context.Context, issue connector.Issue) error {
	if issue.PullRequest == nil || strings.TrimSpace(issue.PullRequest.NodeID) == "" {
		return errors.New("mark github pull request ready: missing pull request node id")
	}
	var response struct {
		MarkPullRequestReadyForReview *struct {
			PullRequest *struct {
				ID      string `json:"id"`
				IsDraft bool   `json:"isDraft"`
			} `json:"pullRequest"`
		} `json:"markPullRequestReadyForReview"`
	}
	if err := c.client.GraphQL(ctx, markPullRequestReadyMutation, map[string]any{"pullRequestId": issue.PullRequest.NodeID}, &response); err != nil {
		return fmt.Errorf("mark github pull request ready: %w", err)
	}
	result := response.MarkPullRequestReadyForReview
	if result == nil || result.PullRequest == nil || result.PullRequest.ID != issue.PullRequest.NodeID || result.PullRequest.IsDraft {
		return fmt.Errorf("mark github pull request ready: %w", ErrInvalidResponse)
	}
	return nil
}
