package github

import (
	"context"
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
)

const laneSignalStatusBatchSize = 100

const laneSignalStatusesQuery = `
query DetentGitHubLaneSignalStatuses($issueIds: [ID!]!, $first: Int!, $statusField: String!) {
  nodes(ids: $issueIds) {
    __typename
    ... on Issue {
      id
      projectItems(first: $first) {
        pageInfo { hasNextPage endCursor }
        nodes {
          project { id }
          statusValue: fieldValueByName(name: $statusField) {
            ... on ProjectV2ItemFieldSingleSelectValue { name }
          }
        }
      }
    }
  }
  rateLimit { limit used remaining cost resetAt }
}`

const laneSignalStatusesPageQuery = `
query DetentGitHubLaneSignalStatusesPage($issueId: ID!, $first: Int!, $after: String!, $statusField: String!) {
  node(id: $issueId) {
    __typename
    ... on Issue {
      id
      projectItems(first: $first, after: $after) {
        pageInfo { hasNextPage endCursor }
        nodes {
          project { id }
          statusValue: fieldValueByName(name: $statusField) {
            ... on ProjectV2ItemFieldSingleSelectValue { name }
          }
        }
      }
    }
  }
  rateLimit { limit used remaining cost resetAt }
}`

// hydrateIgnoredProjectStatuses is deliberately best effort. A label-backed
// tracker does not require GitHub Projects permission, so this supplemental
// diagnostic must never make its ordinary issue reads unavailable.
func (c *Connector) hydrateIgnoredProjectStatuses(ctx context.Context, issues []connector.Issue) {
	indexesByID := make(map[string][]int, len(issues))
	ids := make([]string, 0, len(issues))
	for index, issue := range issues {
		id := strings.TrimSpace(issue.ID)
		if id == "" {
			continue
		}
		if _, ok := indexesByID[id]; !ok {
			ids = append(ids, id)
		}
		indexesByID[id] = append(indexesByID[id], index)
	}
	for start := 0; start < len(ids); start += laneSignalStatusBatchSize {
		end := min(start+laneSignalStatusBatchSize, len(ids))
		batch := ids[start:end]
		var response struct {
			Nodes []githubIssueNode `json:"nodes"`
		}
		if err := c.client.GraphQLWithType(ctx, graphQLQueryLaneSignalStatus, laneSignalStatusesQuery, map[string]any{
			"issueIds":    batch,
			"first":       projectItemsPerIssue,
			"statusField": c.statusField,
		}, &response); err != nil {
			c.logger.DebugContext(ctx, "read ignored github project statuses unavailable", "issue_count", len(batch), "error", err)
			continue
		}
		for _, node := range response.Nodes {
			id := strings.TrimSpace(node.ID)
			indexes, ok := indexesByID[id]
			if node.TypeName != "Issue" || !ok || node.ProjectItems == nil {
				continue
			}
			items, ok := c.completeLaneSignalProjectItems(ctx, id, *node.ProjectItems)
			if !ok {
				continue
			}
			for _, index := range indexes {
				c.attachIgnoredProjectStatus(&issues[index], &items)
			}
		}
	}
}

func (c *Connector) completeLaneSignalProjectItems(ctx context.Context, issueID string, items projectItemsConnection) (projectItemsConnection, bool) {
	for items.PageInfo.HasNextPage {
		cursor := strings.TrimSpace(items.PageInfo.EndCursor)
		if cursor == "" {
			return projectItemsConnection{}, false
		}
		var response struct {
			Node *githubIssueNode `json:"node"`
		}
		if err := c.client.GraphQLWithType(ctx, graphQLQueryLaneSignalStatus, laneSignalStatusesPageQuery, map[string]any{
			"issueId":     issueID,
			"first":       projectItemsPerIssue,
			"after":       cursor,
			"statusField": c.statusField,
		}, &response); err != nil {
			c.logger.DebugContext(ctx, "read ignored github project status page unavailable", "issue_id", issueID, "error", err)
			return projectItemsConnection{}, false
		}
		if response.Node == nil || response.Node.TypeName != "Issue" || strings.TrimSpace(response.Node.ID) != issueID || response.Node.ProjectItems == nil {
			return projectItemsConnection{}, false
		}
		items.Nodes = append(items.Nodes, response.Node.ProjectItems.Nodes...)
		items.PageInfo = response.Node.ProjectItems.PageInfo
	}
	return items, true
}

func (c *Connector) attachIgnoredProjectStatus(issue *connector.Issue, items *projectItemsConnection) {
	status := c.ignoredProjectStatus(items)
	if status == "" {
		return
	}
	if issue.Fields == nil {
		issue.Fields = map[string]string{}
	}
	issue.Fields[c.statusField] = status
}

func (c *Connector) ignoredProjectStatus(items *projectItemsConnection) string {
	if items == nil {
		return ""
	}
	wanted := make(map[string]string)
	for _, state := range c.configuredStatusStates() {
		external := strings.TrimSpace(c.detentToGitHubState(state))
		if external != "" {
			wanted[normalizeStateName(external)] = external
		}
	}
	matches := map[string]string{}
	for _, item := range items.Nodes {
		if c.projectID != "" && (item.Project == nil || item.Project.ID != c.projectID) {
			continue
		}
		status := strings.TrimSpace(singleSelectName(item.StatusValue))
		configured, ok := wanted[normalizeStateName(status)]
		if !ok {
			continue
		}
		matches[normalizeStateName(configured)] = configured
	}
	if len(matches) != 1 {
		return ""
	}
	for _, status := range matches {
		return status
	}
	return ""
}
