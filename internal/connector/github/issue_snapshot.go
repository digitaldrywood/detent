package github

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

const issueSnapshotQuery = `query($owner: String!, $repo: String!, $number: Int!, $cursor: String) {
  repository(owner: $owner, name: $repo) {
    nameWithOwner
    issue(number: $number) {
      id url title body createdAt updatedAt author { login }
      comments(first: 100, after: $cursor) {
        totalCount nodes { id body createdAt updatedAt author { login } }
        pageInfo { hasNextPage endCursor }
      }
    }
  }
}`

type snapshotNode struct {
	ID        string    `json:"id"`
	URL       string    `json:"url"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	Author    *struct {
		Login string `json:"login"`
	} `json:"author"`
}

func (n snapshotNode) provenance(observed time.Time) (tracker.Provenance, error) {
	if n.ID == "" || n.CreatedAt.IsZero() || n.UpdatedAt.Before(n.CreatedAt) || observed.Before(n.UpdatedAt) {
		return tracker.Provenance{}, fmt.Errorf("github issue intake contains incomplete provenance: %w", ErrInvalidResponse)
	}
	author := "unavailable"
	if n.Author != nil && n.Author.Login != "" {
		author = n.Author.Login
	}
	return tracker.Provenance{Provider: "github", ExternalID: n.ID, AuthorID: author, CreatedAt: n.CreatedAt, UpdatedAt: n.UpdatedAt, ObservedAt: observed}, nil
}

// FetchIssueSnapshot returns all current issue discussion or an error. No
// partial page is suitable for agent execution or publication to Hub.
func (c *Client) FetchIssueSnapshot(ctx context.Context, rawURL string) (tracker.GitHubIssueSnapshot, error) {
	var snapshot tracker.GitHubIssueSnapshot
	canonical, repository, number, err := tracker.ParseGitHubIssueURL(rawURL)
	if err != nil {
		return snapshot, err
	}
	parts := strings.SplitN(repository, "/", 2)
	var cursor any
	seenCursors := map[string]bool{}
	seenComments := map[string]bool{}
	var total int
	for {
		var response struct {
			Repository *struct {
				Name  string `json:"nameWithOwner"`
				Issue *struct {
					snapshotNode
					Comments struct {
						Total    int            `json:"totalCount"`
						Nodes    []snapshotNode `json:"nodes"`
						PageInfo struct {
							HasNext bool   `json:"hasNextPage"`
							Cursor  string `json:"endCursor"`
						} `json:"pageInfo"`
					} `json:"comments"`
				} `json:"issue"`
			} `json:"repository"`
		}
		if err := c.GraphQLWithType(ctx, "linked_issue_intake", issueSnapshotQuery, map[string]any{"owner": parts[0], "repo": parts[1], "number": number, "cursor": cursor}, &response); err != nil {
			return tracker.GitHubIssueSnapshot{}, fmt.Errorf("read complete github source context: %w", err)
		}
		if response.Repository == nil || response.Repository.Issue == nil {
			return tracker.GitHubIssueSnapshot{}, fmt.Errorf("github issue is inaccessible; check runner repository read access: %w", ErrNotFound)
		}
		issue := response.Repository.Issue
		returnedURL, _, _, urlErr := tracker.ParseGitHubIssueURL(issue.URL)
		if urlErr != nil || returnedURL != canonical || !strings.EqualFold(response.Repository.Name, repository) || strings.TrimSpace(issue.Title) == "" || len(issue.Title) > 500 || len(issue.Body) > 256<<10 {
			return tracker.GitHubIssueSnapshot{}, fmt.Errorf("github returned mismatched or incomplete issue context: %w", ErrInvalidResponse)
		}
		observed := time.Now().UTC()
		provenance, err := issue.provenance(observed)
		if err != nil {
			return tracker.GitHubIssueSnapshot{}, err
		}
		if cursor == nil {
			total = issue.Comments.Total
			snapshot = tracker.GitHubIssueSnapshot{URL: canonical, Title: issue.Title, Body: issue.Body, Provenance: provenance, Comments: []tracker.GitHubIssueComment{}}
		} else if issue.ID != snapshot.Provenance.ExternalID || issue.Title != snapshot.Title || issue.Body != snapshot.Body || !issue.UpdatedAt.Equal(snapshot.Provenance.UpdatedAt) || issue.Comments.Total != total {
			return tracker.GitHubIssueSnapshot{}, fmt.Errorf("github issue changed during pagination; retry complete intake: %w", ErrInvalidResponse)
		}
		for _, node := range issue.Comments.Nodes {
			if seenComments[node.ID] || len(node.Body) > 64<<10 {
				return tracker.GitHubIssueSnapshot{}, fmt.Errorf("github returned duplicate or oversized discussion: %w", ErrInvalidResponse)
			}
			provenance, err := node.provenance(observed)
			if err != nil {
				return tracker.GitHubIssueSnapshot{}, err
			}
			seenComments[node.ID] = true
			snapshot.Comments = append(snapshot.Comments, tracker.GitHubIssueComment{Body: node.Body, Provenance: provenance})
		}
		if !issue.Comments.PageInfo.HasNext {
			if len(snapshot.Comments) != total {
				return tracker.GitHubIssueSnapshot{}, fmt.Errorf("github discussion is incomplete; retry complete intake: %w", ErrInvalidResponse)
			}
			snapshot.Provenance.ObservedAt = observed
			return snapshot, nil
		}
		next := issue.Comments.PageInfo.Cursor
		if next == "" || seenCursors[next] || len(issue.Comments.Nodes) == 0 || len(snapshot.Comments) >= total {
			return tracker.GitHubIssueSnapshot{}, fmt.Errorf("github discussion pagination is incomplete: %w", ErrInvalidResponse)
		}
		seenCursors[next] = true
		cursor = next
	}
}
