package github

import (
	"context"
	"fmt"
	"strings"

	"github.com/digitaldrywood/detent/internal/tracker"
)

const issueDiscoveryQuery = `query($owner:String!, $repo:String!, $states:[IssueState!], $labels:[String!], $cursor:String) {
 repository(owner:$owner, name:$repo) { nameWithOwner
 issues(first:100, after:$cursor, states:$states, labels:$labels, orderBy:{field:CREATED_AT,direction:ASC}) {
 totalCount nodes { id number url title body closed labels(first:100) { nodes { name } } }
 pageInfo { hasNextPage endCursor }
 } }
}`

// DiscoverIssues performs one bounded page read. The caller explicitly requests
// subsequent pages; the connector never polls or synchronizes the repository.
func (c *Client) DiscoverIssues(ctx context.Context, request tracker.GitHubDiscovery) (tracker.GitHubDiscoveryPage, error) {
	var page tracker.GitHubDiscoveryPage
	_, repository, _, err := tracker.ParseGitHubIssueURL("https://github.com/" + request.Repository + "/issues/1")
	if err != nil {
		return page, err
	}
	parts := strings.SplitN(repository, "/", 2)
	states := []string{"OPEN"}
	if request.IncludeClosed {
		states = append(states, "CLOSED")
	}
	var cursor any
	if request.Cursor != "" {
		cursor = request.Cursor
	}
	var labels any
	if len(request.Labels) > 0 {
		labels = request.Labels
	}
	var response struct {
		Repository *struct {
			Name   string `json:"nameWithOwner"`
			Issues struct {
				Total int `json:"totalCount"`
				Nodes []struct {
					tracker.GitHubIssuePreview
					Labels struct {
						Nodes []struct {
							Name string `json:"name"`
						} `json:"nodes"`
					} `json:"labels"`
				} `json:"nodes"`
				PageInfo struct {
					HasNext bool   `json:"hasNextPage"`
					Cursor  string `json:"endCursor"`
				} `json:"pageInfo"`
			} `json:"issues"`
		} `json:"repository"`
	}
	if err := c.GraphQLWithType(ctx, "onboarding_issue_discovery", issueDiscoveryQuery, map[string]any{"owner": parts[0], "repo": parts[1], "states": states, "labels": labels, "cursor": cursor}, &response); err != nil {
		return page, err
	}
	if response.Repository == nil {
		return page, fmt.Errorf("repository issues inaccessible; verify runner GitHub read access: %w", ErrNotFound)
	}
	if !strings.EqualFold(repository, response.Repository.Name) {
		return page, ErrInvalidResponse
	}
	page.Total = response.Repository.Issues.Total
	page.Issues = []tracker.GitHubIssuePreview{}
	seen := map[int]bool{}
	for _, node := range response.Repository.Issues.Nodes {
		canonical, repo, number, err := tracker.ParseGitHubIssueURL(node.URL)
		if err != nil || repo != repository || number != node.Number || seen[number] || node.ID == "" || strings.TrimSpace(node.Title) == "" || (!request.IncludeClosed && node.Closed) {
			return tracker.GitHubDiscoveryPage{}, ErrInvalidResponse
		}
		seen[number] = true
		preview := node.GitHubIssuePreview
		preview.URL = canonical
		preview.Body = string([]rune(preview.Body)[:min(len([]rune(preview.Body)), 1000)])
		preview.Labels = []string{}
		for _, label := range node.Labels.Nodes {
			preview.Labels = append(preview.Labels, label.Name)
		}
		page.Issues = append(page.Issues, preview)
	}
	if response.Repository.Issues.PageInfo.HasNext {
		page.NextCursor = response.Repository.Issues.PageInfo.Cursor
		if page.NextCursor == "" || page.NextCursor == request.Cursor || len(page.Issues) == 0 {
			return tracker.GitHubDiscoveryPage{}, ErrInvalidResponse
		}
	}
	return page, nil
}
