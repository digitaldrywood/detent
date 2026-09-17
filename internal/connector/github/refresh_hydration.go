package github

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// hydrateRefreshPage shares the board scan's lifetime. Successful batches survive
// an interrupted page or later hydration request; unavailable schema fields still
// use the existing legacy readers after enumeration.
func (c *Connector) hydrateRefreshPage(ctx context.Context, progress *projectItemsScanProgress, states map[string]struct{}) error {
	if progress.evidence == nil {
		progress.evidence = make(map[string]githubIssueNode)
	}
	if progress.hydrated == nil {
		progress.hydrated = make(map[string]bool)
	}
	var nodes []githubIssueNode
	for _, issue := range progress.scan.Issues {
		if _, wanted := states[normalizeStateName(issue.State)]; !wanted || progress.hydrated[issue.ID] {
			continue
		}
		ref, ok := issueRefFromIdentifier(issue.Identifier)
		if !ok {
			continue
		}
		node := githubIssueNode{ID: issue.ID, Number: ref.Number, Title: issue.Title, CandidateState: issue.State, Repository: repository{NameWithOwner: ref.Owner + "/" + ref.Name}}
		for _, name := range issue.Labels {
			node.Labels.Nodes = append(node.Labels.Nodes, label{Name: name})
		}
		nodes = append(nodes, node)
	}
	for start := 0; start < len(nodes); start += candidateHydrationBatchSize {
		batch := nodes[start:min(start+candidateHydrationBatchSize, len(nodes))]
		evidence, err := c.candidateEvidenceBatch(ctx, batch, true)
		for id, node := range evidence {
			progress.evidence[id] = node
			progress.hydrated[id] = true
		}
		if err != nil && !projectSchedulerFieldsUnavailable(err) {
			return fmt.Errorf("hydrate github refresh candidates: %w", err)
		}
		for _, node := range batch {
			progress.hydrated[node.ID] = true
		}
	}
	return nil
}

// Project revisions and comment counts cannot detect edits to old comments.
// Read all comment identities and updatedAt values (without bodies) explicitly
// before reusing an interrupted scan's complete scheduler observations.
func (c *Connector) validateRefreshEvidence(ctx context.Context, progress *projectItemsScanProgress) error {
	if err := c.validateRefreshBlockers(ctx, progress); err != nil {
		return err
	}
	ids := make([]string, 0, len(progress.evidence))
	for id := range progress.evidence {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for start := 0; start < len(ids); start += candidateHydrationBatchSize {
		pending := ids[start:min(start+candidateHydrationBatchSize, len(ids))]
		cursors := make(map[string]string)
		observed := make(map[string]githubIssueNode)
		for len(pending) > 0 {
			var query strings.Builder
			variables := make(map[string]any)
			query.WriteString("query DetentGitHubRefreshEvidenceRevision(")
			for i, id := range pending {
				fmt.Fprintf(&query, "$id%d:ID!,$after%d:String,", i, i)
				variables[fmt.Sprintf("id%d", i)] = id
				if cursor := cursors[id]; cursor != "" {
					variables[fmt.Sprintf("after%d", i)] = cursor
				}
			}
			query.WriteString(") { rateLimit { limit used cost remaining resetAt }")
			for i := range pending {
				fmt.Fprintf(&query, "issue%d:node(id:$id%d) { ... on Issue { id updatedAt comments(first:100,after:$after%d) { totalCount pageInfo { hasNextPage endCursor } nodes { id updatedAt } } } }", i, i, i)
			}
			query.WriteString("}")
			var response map[string]*githubIssueNode
			if err := c.client.GraphQLWithType(ctx, graphQLQueryCandidateIssues, query.String(), variables, &response); err != nil {
				return err
			}
			var next []string
			for i, id := range pending {
				node := response[fmt.Sprintf("issue%d", i)]
				if node == nil || node.ID != id {
					delete(progress.evidence, id)
					delete(progress.hydrated, id)
					continue
				}
				previous := observed[id]
				node.Comments.Nodes = append(previous.Comments.Nodes, node.Comments.Nodes...)
				observed[id] = *node
				if node.Comments.PageInfo.HasNextPage {
					cursor := strings.TrimSpace(node.Comments.PageInfo.EndCursor)
					if cursor == "" || cursor == cursors[id] {
						return ErrInvalidResponse
					}
					cursors[id] = cursor
					next = append(next, id)
					continue
				}
				if !sameRefreshCommentRevision(progress.evidence[id], *node) {
					delete(progress.evidence, id)
					delete(progress.hydrated, id)
				}
			}
			pending = next
		}
	}
	return nil
}

func sameRefreshCommentRevision(retained, current githubIssueNode) bool {
	if retained.UpdatedAt == nil || current.UpdatedAt == nil || *retained.UpdatedAt == "" || *retained.UpdatedAt != *current.UpdatedAt || retained.Comments.TotalCount != current.Comments.TotalCount || len(current.Comments.Nodes) != current.Comments.TotalCount || len(retained.Comments.Nodes) != len(current.Comments.Nodes) {
		return false
	}
	for i, comment := range retained.Comments.Nodes {
		fresh := current.Comments.Nodes[i]
		if comment.ID == "" || comment.ID != fresh.ID || comment.UpdatedAt == nil || fresh.UpdatedAt == nil || *comment.UpdatedAt == "" || *comment.UpdatedAt != *fresh.UpdatedAt {
			return false
		}
	}
	return true
}

// A dependency can change without changing its dependent or the project.
// Check each retained blocker's own revision before reusing dependency evidence.
func (c *Connector) validateRefreshBlockers(ctx context.Context, progress *projectItemsScanProgress) error {
	blockers := make(map[string]struct{})
	for _, node := range progress.evidence {
		if node.BlockedBy != nil {
			for _, blocker := range node.BlockedBy.Nodes {
				if blocker.ID != "" {
					blockers[blocker.ID] = struct{}{}
				}
			}
		}
	}
	ids := make([]string, 0, len(blockers))
	for id := range blockers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	revisions := make(map[string]string, len(ids))
	for start := 0; start < len(ids); start += candidateHydrationBatchSize {
		var response struct{ Nodes []githubIssueNode }
		const query = "query DetentGitHubRefreshBlockerRevision($ids:[ID!]!) { nodes(ids:$ids) { ... on Issue { id updatedAt } } rateLimit { limit used cost remaining resetAt } }"
		if err := c.client.GraphQLWithType(ctx, graphQLQueryCandidateIssues, query, map[string]any{"ids": ids[start:min(start+candidateHydrationBatchSize, len(ids))]}, &response); err != nil {
			return err
		}
		for _, node := range response.Nodes {
			if node.UpdatedAt != nil {
				revisions[node.ID] = *node.UpdatedAt
			}
		}
	}
	for id, node := range progress.evidence {
		if node.BlockedBy == nil {
			continue
		}
		for _, blocker := range node.BlockedBy.Nodes {
			if blocker.UpdatedAt == nil || revisions[blocker.ID] == "" || revisions[blocker.ID] != *blocker.UpdatedAt {
				delete(progress.evidence, id)
				delete(progress.hydrated, id)
				break
			}
		}
	}
	return nil
}
