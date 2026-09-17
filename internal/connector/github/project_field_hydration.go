package github

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Item aliases share candidate hydration's 25-consumer bound. Keeping this
// selection out of the board page avoids multiplying field connections by all
// board members, including cards that have no scheduler consumer.
func appendProjectFieldVariables(query *strings.Builder, variables map[string]any, items map[string]string) []string {
	ids := make([]string, 0, len(items))
	for id := range items {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for i, id := range ids {
		fmt.Fprintf(query, "$item%d:ID!,", i)
		variables[fmt.Sprintf("item%d", i)] = items[id]
	}
	return ids
}

func appendProjectFieldSelections(query *strings.Builder, ids []string) {
	for i := range ids {
		fmt.Fprintf(query, `item%d:node(id:$item%d) { ... on ProjectV2Item { id updatedAt
   statusValue:fieldValueByName(name:"Status") { ... on ProjectV2ItemFieldSingleSelectValue { name updatedAt } }
   priorityValue:fieldValueByName(name:"Priority") { ... on ProjectV2ItemFieldSingleSelectValue { name } }
   %s } }`, i, i, projectItemFieldValuesSelection)
	}
}

func decodeProjectFields(response map[string]json.RawMessage, ids []string, items map[string]string) (map[string]projectItemFields, error) {
	fields := make(map[string]projectItemFields, len(ids))
	for i, id := range ids {
		var item *projectItemNode
		raw := response[fmt.Sprintf("item%d", i)]
		if len(raw) == 0 {
			return nil, ErrInvalidResponse
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, err
		}
		if item == nil {
			continue
		}
		if item.ID != items[id] || item.FieldValues.Nodes == nil {
			return nil, ErrInvalidResponse
		}
		fields[id] = projectItemFields{itemID: item.ID, updatedAt: item.UpdatedAt, statusName: singleSelectName(item.StatusValue), priorityName: singleSelectName(item.PriorityValue), statusUpdatedAt: singleSelectUpdatedAt(item.StatusValue), fields: projectFieldValues(item.FieldValues)}
	}
	return fields, nil
}

func (c *Connector) hydrateProjectFields(ctx context.Context, items map[string]string) (map[string]projectItemFields, error) {
	if len(items) == 0 {
		return map[string]projectItemFields{}, nil
	}
	var query strings.Builder
	query.WriteString("query DetentGitHubProjectFieldHydration(")
	variables := make(map[string]any)
	ids := appendProjectFieldVariables(&query, variables, items)
	query.WriteString(") { rateLimit { limit used cost remaining resetAt }")
	appendProjectFieldSelections(&query, ids)
	query.WriteString("}")
	var response map[string]json.RawMessage
	if err := c.client.GraphQLWithType(ctx, graphQLQueryRunningStates, query.String(), variables, &response); err != nil {
		return nil, err
	}
	return decodeProjectFields(response, ids, items)
}

// A cold active-state cache discovers membership only for the requested issues.
// Membership pages contain IDs, never nested custom-field connections.
func (c *Connector) projectItemsForFieldHydration(ctx context.Context, ids []string) (map[string]string, error) {
	items := make(map[string]string)
	var pending []string
	for _, id := range ids {
		if item, ok := c.projectCache.GetItemID(c.projectID, id); ok {
			items[id] = item
		} else {
			pending = append(pending, id)
		}
	}
	cursors := make(map[string]string)
	for len(pending) > 0 {
		var query strings.Builder
		query.WriteString("query DetentGitHubProjectFieldItems(")
		variables := make(map[string]any)
		for i, id := range pending {
			fmt.Fprintf(&query, "$id%d:ID!,$after%d:String,", i, i)
			variables[fmt.Sprintf("id%d", i)] = id
			if cursors[id] != "" {
				variables[fmt.Sprintf("after%d", i)] = cursors[id]
			}
		}
		query.WriteString(") { rateLimit { limit used cost remaining resetAt }")
		for i := range pending {
			fmt.Fprintf(&query, "issue%d:node(id:$id%d) { ... on Issue { id number repository { nameWithOwner } projectItems(first:20,after:$after%d) { pageInfo { hasNextPage endCursor } nodes { id project { id } } } } }", i, i, i)
		}
		query.WriteString("}")
		var response map[string]*githubIssueNode
		if err := c.client.GraphQLWithType(ctx, graphQLQueryRunningStates, query.String(), variables, &response); err != nil {
			return nil, err
		}
		var next []string
		for i, id := range pending {
			node := response[fmt.Sprintf("issue%d", i)]
			if node == nil || node.ID != id || node.ProjectItems == nil {
				return nil, ErrInvalidResponse
			}
			c.cacheIssueRef(*node)
			for _, item := range node.ProjectItems.Nodes {
				if item.Project != nil && item.Project.ID == c.projectID {
					items[id] = item.ID
					break
				}
			}
			if items[id] != "" || !node.ProjectItems.PageInfo.HasNextPage {
				continue
			}
			cursor := strings.TrimSpace(node.ProjectItems.PageInfo.EndCursor)
			if cursor == "" || cursor == cursors[id] {
				return nil, ErrInvalidResponse
			}
			cursors[id] = cursor
			next = append(next, id)
		}
		pending = next
	}
	return items, nil
}
