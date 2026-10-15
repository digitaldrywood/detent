package github

import (
	"context"
	"fmt"
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
)

const laneSignalStatusBatchSize = 100

const laneSignalIssueFieldStatusesQuery = `
query DetentGitHubLaneSignalIssueFieldStatuses($issueIds: [ID!]!, $after: String) {
  nodes(ids: $issueIds) {
    __typename
    ... on Issue {
      id
      issueFieldValues(first: 100, after: $after) {
        pageInfo { hasNextPage endCursor }
        nodes {
          ... on IssueFieldSingleSelectValue {
            name
            field { ... on IssueFieldCommon { name } }
          }
        }
      }
    }
  }
  rateLimit { limit used remaining cost resetAt }
}`

// Read authoritative diagnostic statuses in batches, like ProjectV2 lane
// signals. Per-issue REST hydration here would spend the shared refresh budget
// before the ordinary scheduling reads can run.
func (c *Connector) hydrateLaneSignalIssueFieldStatuses(ctx context.Context, issues []connector.Issue) error {
	byID := make(map[string]*connector.Issue, len(issues))
	ids := make([]string, 0, len(issues))
	for i := range issues {
		byID[issues[i].ID] = &issues[i]
		ids = append(ids, issues[i].ID)
	}
	for start := 0; start < len(ids); start += laneSignalStatusBatchSize {
		if err := c.readLaneSignalIssueFieldStatuses(ctx, ids[start:min(start+laneSignalStatusBatchSize, len(ids))], nil, byID); err != nil {
			return err
		}
	}
	return nil
}

func (c *Connector) readLaneSignalIssueFieldStatuses(ctx context.Context, ids []string, after *string, byID map[string]*connector.Issue) error {
	var response struct {
		Nodes []struct {
			ID       string `json:"id"`
			TypeName string `json:"__typename"`
			Values   *struct {
				PageInfo pageInfo `json:"pageInfo"`
				Nodes    []struct {
					Name  string `json:"name"`
					Field struct {
						Name string `json:"name"`
					} `json:"field"`
				} `json:"nodes"`
			} `json:"issueFieldValues"`
		} `json:"nodes"`
	}
	if err := c.client.GraphQLWithType(ctx, graphQLQueryLaneSignalStatus, laneSignalIssueFieldStatusesQuery,
		map[string]any{"issueIds": ids, "after": after}, &response); err != nil {
		return fmt.Errorf("fetch github lane signal issue field values: %w", err)
	}
	if len(response.Nodes) != len(ids) {
		return ErrInvalidResponse
	}
	for i, node := range response.Nodes {
		issue := byID[node.ID]
		if node.ID != ids[i] || node.TypeName != "Issue" || node.Values == nil || issue == nil {
			return ErrInvalidResponse
		}
		if issue.Fields == nil {
			issue.Fields = make(map[string]string)
		}
		for _, value := range node.Values.Nodes {
			name, valueName := strings.TrimSpace(value.Field.Name), strings.TrimSpace(value.Name)
			if name == "" || valueName == "" {
				continue
			}
			issue.Fields[name] = valueName
			if name == c.statusField {
				issue.State = c.githubToDetentState(valueName)
			}
			if name == "Priority" {
				issue.PriorityName = valueName
				issue.Priority = c.priorityRank(valueName)
			}
		}
		if node.Values.PageInfo.HasNextPage {
			cursor := strings.TrimSpace(node.Values.PageInfo.EndCursor)
			if cursor == "" || (after != nil && cursor == *after) {
				return ErrInvalidResponse
			}
			if err := c.readLaneSignalIssueFieldStatuses(ctx, []string{node.ID}, &cursor, byID); err != nil {
				return err
			}
		}
	}
	return nil
}

const laneSignalStatusesQuery = `
query DetentGitHubLaneSignalStatuses($issueIds: [ID!]!, $first: Int!, $statusField: String!) {
  nodes(ids: $issueIds) {
    __typename
    ... on Issue {
      id
      projectItems(first: $first) {
        pageInfo { hasNextPage endCursor }
        nodes {
          project { id title url }
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
          project { id title url }
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
	issue.LaneSignalStatuses = nil
	if items == nil {
		return
	}
	wanted := make(map[string]string)
	for _, state := range c.configuredStatusStates() {
		external := strings.TrimSpace(c.detentToGitHubState(state))
		if external != "" {
			wanted[normalizeStateName(external)] = external
		}
	}
	for _, item := range items.Nodes {
		if c.projectID != "" && (item.Project == nil || item.Project.ID != c.projectID) {
			continue
		}
		status := strings.TrimSpace(singleSelectName(item.StatusValue))
		configured, ok := wanted[normalizeStateName(status)]
		if !ok {
			continue
		}
		signal := connector.LaneSignalStatus{Field: c.statusField, Value: configured}
		if item.Project != nil {
			signal.ProjectID = strings.TrimSpace(item.Project.ID)
			signal.ProjectTitle = strings.TrimSpace(item.Project.Title)
			signal.ProjectURL = strings.TrimSpace(item.Project.URL)
		}
		issue.LaneSignalStatuses = append(issue.LaneSignalStatuses, signal)
	}
}
