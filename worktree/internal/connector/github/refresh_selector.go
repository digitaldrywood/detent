package github

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/selector"
)

func refreshSelector(hint connector.IssueFilterHint) func(connector.Issue) bool {
	rule := selector.Selector{AuthorIn: hint.Authors, AssigneeIn: hint.Assignees, Labels: selector.Labels{Include: hint.LabelInclude, Exclude: hint.LabelExclude}}
	rule.Normalize()
	return func(issue connector.Issue) bool { return selector.Match(issue, rule, selector.Context{}) }
}

// Complete only selector connections. Scheduler evidence is still fetched by the
// existing batched hydrator, after selection, including its legacy fallbacks.
func (c *Connector) completeRefreshSelectors(ctx context.Context, items []projectItemNode, hint connector.IssueFilterHint) error {
	labels := len(hint.LabelInclude) > 0 || len(hint.LabelExclude) > 0
	assignees := len(hint.Assignees) > 0
	for _, item := range items {
		node := item.Content
		if node == nil || node.TypeName != "Issue" {
			continue
		}
		if labels {
			if err := completeRefreshConnection(ctx, c, node.ID, "labels", "name", &node.Labels); err != nil {
				return err
			}
		}
		if assignees {
			if err := completeRefreshConnection(ctx, c, node.ID, "assignees", "login", &node.Assignees); err != nil {
				return err
			}
		}
	}
	return nil
}

func completeRefreshConnection[T any](ctx context.Context, c *Connector, id, field, selection string, connection *nodeConnection[T]) error {
	if connection.metadataPresent && !connection.PageInfo.HasNextPage && connection.TotalCount == len(connection.Nodes) {
		return nil
	}
	// An incomplete initial response is reread; it cannot establish eligibility.
	if !connection.metadataPresent {
		*connection = nodeConnection[T]{}
	}
	seen := map[string]bool{}
	for {
		cursor := ""
		if connection.PageInfo.HasNextPage {
			cursor = strings.TrimSpace(connection.PageInfo.EndCursor)
			if cursor == "" || seen[cursor] {
				return fmt.Errorf("complete github selector %s for %s: %w", field, id, ErrInvalidResponse)
			}
			seen[cursor] = true
		} else if connection.metadataPresent {
			return fmt.Errorf("incomplete github selector %s for %s: %w", field, id, ErrInvalidResponse)
		}
		var after *string
		if cursor != "" {
			after = &cursor
		}
		query := fmt.Sprintf(`query DetentGitHubRefreshSelectorMetadata($id:ID!,$after:String) { node(id:$id) { ... on Issue { id values:%s(first:100,after:$after) { totalCount pageInfo { hasNextPage endCursor } nodes { %s } } } } rateLimit { limit used remaining cost resetAt } }`, field, selection)
		var response struct {
			Node *struct {
				ID     string
				Values nodeConnection[T]
			}
		}
		if err := c.client.GraphQLWithType(ctx, graphQLQueryObservedStatus, query, map[string]any{"id": id, "after": after}, &response); err != nil {
			return fmt.Errorf("complete github selector %s: %w", field, err)
		}
		if response.Node == nil || response.Node.ID != id || !response.Node.Values.metadataPresent {
			return fmt.Errorf("complete github selector %s for %s: %w", field, id, ErrInvalidResponse)
		}
		fresh := response.Node.Values
		fresh.Nodes = append(connection.Nodes, fresh.Nodes...)
		*connection = fresh
		if !fresh.PageInfo.HasNextPage && len(fresh.Nodes) == fresh.TotalCount {
			return nil
		}
	}
}

// Project filters narrow enumeration; the local selector remains authoritative.
// Projects documents label and assignee filters, but not author filters. Values
// whose search syntax could change exact matching stay local as well.
func refreshProjectFilter(hint connector.IssueFilterHint) string {
	var filters []string
	quote := func(value string) (string, bool) {
		value = strings.TrimSpace(value)
		if value == "" || strings.ContainsAny(value, "\"'\\,*@\r\n\t") {
			return "", false
		}
		return `"` + value + `"`, true
	}
	var assignees []string
	for _, value := range hint.Assignees {
		if strings.TrimSpace(value) == "" {
			continue
		}
		quoted, ok := quote(value)
		if !ok {
			assignees = nil
			break
		}
		assignees = append(assignees, quoted)
	}
	if len(assignees) > 0 {
		filters = append(filters, "assignee:"+strings.Join(assignees, ","))
	}
	for _, group := range []struct {
		prefix string
		values []string
	}{{"label:", hint.LabelInclude}, {"-label:", hint.LabelExclude}} {
		for _, value := range group.values {
			if quoted, ok := quote(value); ok {
				filters = append(filters, group.prefix+quoted)
			}
		}
	}
	return strings.Join(filters, " ")
}

// The filtered board no longer supplies lanes for unowned native blockers.
// Read only those referenced cards through the existing project-item reader;
// preserve native closed state and human-prerequisite evidence.
func (c *Connector) resolveFilteredRefreshBlockers(ctx context.Context, issues []connector.Issue, board map[string]connector.Issue) error {
	states := map[string]string{}
	for i := range issues {
		for j := range issues[i].BlockedBy {
			ref := &issues[i].BlockedBy[j]
			if _, ok := board[normalizedIssueIdentifier(ref.Identifier)]; ok || ref.ID == "" || stateInList(ref.State, c.terminalStates) {
				continue
			}
			state, known := states[ref.ID]
			if !known {
				lane, _, _, _, found, err := c.fetchProjectFieldsPage(ctx, ref.ID, nil)
				if err != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					if !connector.IsRetryable(err) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
						return fmt.Errorf("resolve filtered refresh blocker %s: %w", ref.Identifier, err)
					}
					// Match the existing dependency reader: an unavailable lane
					// stays unresolved without withholding unrelated candidates.
					states[ref.ID] = ""
					ref.State = ""
					continue
				}
				state = ref.State
				if found {
					state = c.githubToDetentState(lane)
				}
				states[ref.ID] = state
			}
			ref.State = state
		}
	}
	return nil
}
