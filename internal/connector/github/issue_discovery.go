package github

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

const issueDiscoveryQuery = `query($owner:String!, $repo:String!, $states:[IssueState!], $labels:[String!], $cursor:String, $snapshots:Boolean!, $projectV2:Boolean!, $projectID:ID!) {
 projectSource:node(id:$projectID) @include(if:$projectV2) { ... on ProjectV2 { id } }
 repository(owner:$owner, name:$repo) { nameWithOwner
 issues(first:100, after:$cursor, states:$states, labels:$labels, orderBy:{field:CREATED_AT,direction:ASC}) {
 totalCount nodes { id number url title body closed labels(first:100) { nodes { name } }
 createdAt @include(if:$snapshots) updatedAt @include(if:$snapshots) author @include(if:$snapshots) { login }
 comments(first:100) @include(if:$snapshots) { totalCount nodes { id body createdAt updatedAt author { login } } pageInfo { hasNextPage endCursor } }
 projectItems(first:100, includeArchived:true) @include(if:$projectV2) { ...DiscoveryProjectItems } }
 pageInfo { hasNextPage endCursor }
 } }
}` + discoveryProjectItemsFragment

const discoveryProjectItemsFragment = `
fragment DiscoveryProjectItems on ProjectV2ItemConnection {
 nodes { project { id } fieldValueByName(name:"Status") { ... on ProjectV2ItemFieldSingleSelectValue { name } } }
 pageInfo { hasNextPage endCursor }
}`

const issueDiscoveryProjectQuery = `query($id:ID!, $cursor:String!) {
 node(id:$id) { ... on Issue { projectItems(first:100, after:$cursor, includeArchived:true) { ...DiscoveryProjectItems } } }
}` + discoveryProjectItemsFragment

type discoveryProjectItems struct {
	Nodes []struct {
		Project struct {
			ID string `json:"id"`
		} `json:"project"`
		Status *struct {
			Name string `json:"name"`
		} `json:"fieldValueByName"`
	} `json:"nodes"`
	PageInfo struct {
		HasNext bool   `json:"hasNextPage"`
		Cursor  string `json:"endCursor"`
	} `json:"pageInfo"`
}

// DiscoverIssues performs one bounded page read. The caller explicitly requests
// subsequent pages; the connector never polls or synchronizes the repository.
func (c *Client) DiscoverIssues(ctx context.Context, request tracker.GitHubDiscovery) (tracker.GitHubDiscoveryPage, error) {
	page, err := FetchDiscoveryPage(ctx, c, request)
	if err != nil {
		return page, err
	}
	for index := range page.Issues {
		preview := &page.Issues[index]
		if preview.NeedsDiscussion {
			snapshot, err := c.FetchIssueSnapshot(ctx, preview.URL)
			if err != nil {
				return tracker.GitHubDiscoveryPage{}, err
			}
			preview.Snapshot = &snapshot
			preview.NeedsDiscussion = false
		}
	}
	return page, nil
}

type DiscoveryClient interface {
	GraphQL(context.Context, string, map[string]any, any) error
}

func FetchDiscoveryPage(ctx context.Context, client DiscoveryClient, request tracker.GitHubDiscovery) (tracker.GitHubDiscoveryPage, error) {
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
		ProjectSource *struct {
			ID string `json:"id"`
		} `json:"projectSource"`
		Repository *struct {
			Name   string `json:"nameWithOwner"`
			Issues struct {
				Total int `json:"totalCount"`
				Nodes []struct {
					tracker.GitHubIssuePreview
					ProjectItems *discoveryProjectItems `json:"projectItems"`
					BodyValue    *string                `json:"body"`
					CreatedAt    time.Time              `json:"createdAt"`
					UpdatedAt    time.Time              `json:"updatedAt"`
					Author       *struct {
						Login string `json:"login"`
					} `json:"author"`
					Comments struct {
						Total    int            `json:"totalCount"`
						Nodes    []snapshotNode `json:"nodes"`
						PageInfo struct {
							HasNext bool `json:"hasNextPage"`
						} `json:"pageInfo"`
					} `json:"comments"`
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
	projectID := ""
	if request.ProjectV2 != nil {
		projectID = request.ProjectV2.ID
	}
	if err := client.GraphQL(ctx, issueDiscoveryQuery, map[string]any{"owner": parts[0], "repo": parts[1], "states": states, "labels": labels, "cursor": cursor, "snapshots": request.Snapshots, "projectV2": request.ProjectV2 != nil, "projectID": projectID}, &response); err != nil {
		return page, err
	}
	if request.ProjectV2 != nil && (response.ProjectSource == nil || response.ProjectSource.ID != projectID) {
		return page, fmt.Errorf("ProjectV2 source inaccessible; verify existing GitHub read access: %w", ErrNotFound)
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
		preview.ProjectStatus = ""
		if request.ProjectV2 != nil {
			status, err := fetchDiscoveryProjectStatus(ctx, client, node.ID, projectID, node.ProjectItems)
			if err != nil {
				return tracker.GitHubDiscoveryPage{}, err
			}
			preview.ProjectStatus = status
		}
		if node.BodyValue != nil {
			preview.Body = *node.BodyValue
		}
		preview.URL = canonical
		if request.Snapshots {
			if node.BodyValue == nil {
				return tracker.GitHubDiscoveryPage{}, ErrInvalidResponse
			}
			observed := time.Now().UTC()
			provenance, err := (snapshotNode{ID: node.ID, CreatedAt: node.CreatedAt, UpdatedAt: node.UpdatedAt, Author: node.Author}).provenance(observed)
			if err != nil {
				return tracker.GitHubDiscoveryPage{}, err
			}
			snapshot := tracker.GitHubIssueSnapshot{URL: canonical, Title: node.Title, Body: preview.Body, Provenance: provenance}
			for _, comment := range node.Comments.Nodes {
				if strings.Contains(comment.Body, "<!-- detent-summary:") {
					continue
				}
				provenance, err := comment.provenance(observed)
				if err != nil {
					return tracker.GitHubDiscoveryPage{}, err
				}
				snapshot.Comments = append(snapshot.Comments, tracker.GitHubIssueComment{Body: comment.Body, Provenance: provenance})
			}
			if !node.Comments.PageInfo.HasNext && len(node.Comments.Nodes) != node.Comments.Total {
				return tracker.GitHubDiscoveryPage{}, ErrInvalidResponse
			}
			preview.Snapshot = &snapshot
			preview.NeedsDiscussion = node.Comments.PageInfo.HasNext
			preview.DiscussionCount = node.Comments.Total
		}
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

func fetchDiscoveryProjectStatus(ctx context.Context, client DiscoveryClient, issueID, projectID string, items *discoveryProjectItems) (string, error) {
	seen := map[string]bool{}
	for {
		if items == nil {
			return "", ErrInvalidResponse
		}
		for _, item := range items.Nodes {
			if item.Project.ID == projectID {
				if item.Status == nil {
					return "", nil
				}
				return item.Status.Name, nil
			}
		}
		if !items.PageInfo.HasNext {
			return "", nil
		}
		cursor := items.PageInfo.Cursor
		if cursor == "" || seen[cursor] || len(items.Nodes) == 0 {
			return "", ErrInvalidResponse
		}
		seen[cursor] = true
		var response struct {
			Node *struct {
				ProjectItems *discoveryProjectItems `json:"projectItems"`
			} `json:"node"`
		}
		if err := client.GraphQL(ctx, issueDiscoveryProjectQuery, map[string]any{"id": issueID, "cursor": cursor}, &response); err != nil {
			return "", err
		}
		if response.Node == nil {
			return "", ErrInvalidResponse
		}
		items = response.Node.ProjectItems
	}
}
