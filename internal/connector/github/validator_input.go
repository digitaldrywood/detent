package github

import (
	"context"
	"fmt"

	"github.com/digitaldrywood/detent/internal/connector"
)

// FetchValidationIssue reads the task directly rather than a cached board item.
func (c *Connector) FetchValidationIssue(ctx context.Context, issue connector.Issue) (connector.Issue, error) {
	var response struct {
		Node struct {
			ID       string `json:"id"`
			Title    string `json:"title"`
			Body     string `json:"body"`
			Comments struct {
				Nodes    []issueComment `json:"nodes"`
				PageInfo pageInfo       `json:"pageInfo"`
			} `json:"comments"`
		} `json:"node"`
	}
	const query = `query DetentValidatorTask($id: ID!, $after: String) { node(id: $id) { ... on Issue { id title body comments(first: 100, after: $after) { nodes { id body url author { login __typename } authorAssociation createdAt updatedAt } pageInfo { hasNextPage endCursor } } } } rateLimit { limit used remaining cost resetAt } }`
	var after any
	issue.Comments = nil
	for {
		if err := c.client.GraphQL(ctx, query, map[string]any{"id": issue.ID, "after": after}, &response); err != nil {
			return connector.Issue{}, err
		}
		if response.Node.ID != issue.ID {
			return connector.Issue{}, fmt.Errorf("validator task %s not returned", issue.ID)
		}
		issue.Title, issue.Description = response.Node.Title, response.Node.Body
		issue.Comments = append(issue.Comments, connectorIssueComments(response.Node.Comments.Nodes)...)
		page := response.Node.Comments.PageInfo
		if !page.HasNextPage {
			break
		}
		if page.EndCursor == "" || page.EndCursor == after {
			return connector.Issue{}, fmt.Errorf("validator task %s comment cursor did not advance", issue.ID)
		}
		after = page.EndCursor
	}
	issue.WorkpadSignal = nil
	return issue, nil
}
