package github

import (
	"context"
	"fmt"
)

// This is an advisory threshold, not a dispatch limit. Refresh frequency and
// other projects sharing the token also determine the GraphQL budget pressure.
const projectSizeWarningThreshold = 300

// Count connections without requesting item nodes or paging the board. GitHub
// excludes archived items by default, just as the polling queries do.
const projectSizeQuery = `
query DetentGitHubProjectSize($projectId: ID!, $doneQuery: String!) {
  node(id: $projectId) {
    ... on ProjectV2 {
      items(first: 1) { totalCount }
      doneItems: items(first: 1, query: $doneQuery) { totalCount }
    }
  }
  rateLimit { limit used remaining cost resetAt }
}`

func (c githubReadinessChecker) projectSizeCheck(ctx context.Context) ReadinessCheck {
	check := ReadinessCheck{
		Name:   "GitHub ProjectV2 polling cost",
		Status: ReadinessWarn,
		Hint:   "Consider github_status_source: label, especially when several projects share one token, or archive Done items on the board.",
	}
	var response struct {
		Node *struct {
			Items *struct {
				TotalCount *int `json:"totalCount"`
			} `json:"items"`
			DoneItems *struct {
				TotalCount *int `json:"totalCount"`
			} `json:"doneItems"`
		} `json:"node"`
	}
	if err := c.connector.client.GraphQLWithType(ctx, graphQLQueryProjectMetadata, projectSizeQuery, map[string]any{
		"projectId": c.connector.projectID,
		"doneQuery": fmt.Sprintf("status:%q", c.connector.detentToGitHubState("Done")),
	}, &response); err != nil {
		check.Detail = "cannot read ProjectV2 board item counts: " + err.Error()
		return check
	}
	if response.Node == nil || response.Node.Items == nil || response.Node.DoneItems == nil ||
		response.Node.Items.TotalCount == nil || response.Node.DoneItems.TotalCount == nil {
		check.Detail = "cannot read ProjectV2 board item counts: counts unavailable"
		return check
	}
	total, done := *response.Node.Items.TotalCount, *response.Node.DoneItems.TotalCount
	check.Detail = fmt.Sprintf("board has %d items (%d Done); ProjectV2 polling re-reads all %d every cycle, including Done items", total, done, total)
	if total > projectSizeWarningThreshold {
		check.Detail += "; this may exhaust the shared GraphQL budget"
	} else {
		check.Status = ReadinessOK
	}
	return check
}
