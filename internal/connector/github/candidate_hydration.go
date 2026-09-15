package github

import (
	"context"
	"fmt"
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
)

const candidateCommentFields = `totalCount pageInfo { hasNextPage endCursor }
 nodes { id body url author { login } authorAssociation createdAt updatedAt }`
const candidateDependencyFields = `pageInfo { hasNextPage endCursor }
 nodes { id number body state repository { nameWithOwner } labels(first: 100) { pageInfo { hasNextPage endCursor } nodes { name } } }`
const candidateSchedulerFields = `comments(first: 100) { ` + candidateCommentFields + ` }
 blockedBy(first: 20) { ` + candidateDependencyFields + ` }`

var candidateProjectItemsQuery = strings.Replace(observedStatusProjectItemsQuery, "comments { totalCount }", candidateSchedulerFields, 1)

// candidateEvidence is scoped to one source page. Only complete snapshots can
// replace REST hydration; nil native connections are not authoritative empties.
func (c *Connector) candidateEvidence(ctx context.Context, nodes []githubIssueNode, fetch bool) map[string]githubIssueNode {
	complete := make(map[string]githubIssueNode)
	pending := make([]githubIssueNode, 0, len(nodes))
	for _, node := range nodes {
		if strings.TrimSpace(node.ID) == "" {
			continue
		}
		if !fetch && node.BlockedBy == nil {
			continue
		}
		pending = append(pending, node)
	}
	for len(pending) > 0 {
		next := make([]githubIssueNode, 0, len(pending))
		if !fetch {
			for _, node := range pending {
				if candidateEvidenceComplete(node) {
					complete[node.ID] = node
				} else if candidateEvidenceCanPage(node) {
					next = append(next, node)
				}
			}
			pending = next
		}
		if len(pending) == 0 {
			break
		}
		var query strings.Builder
		query.WriteString("query DetentGitHubCandidateHydration(")
		variables := make(map[string]any)
		for i, node := range pending {
			fmt.Fprintf(&query, "$id%d: ID!, $comments%d: String, $dependencies%d: String,", i, i, i)
			variables[fmt.Sprintf("id%d", i)] = node.ID
			if !fetch {
				variables[fmt.Sprintf("comments%d", i)] = node.Comments.PageInfo.EndCursor
				variables[fmt.Sprintf("dependencies%d", i)] = node.BlockedBy.PageInfo.EndCursor
			}
		}
		query.WriteString(") {")
		for i, node := range pending {
			fmt.Fprintf(&query, "issue%d: node(id: $id%d) { ... on Issue { id body updatedAt ", i, i)
			// Fetch no nodes from a finished connection while its sibling advances.
			if fetch || node.Comments.PageInfo.HasNextPage {
				fmt.Fprintf(&query, "comments(first:100, after:$comments%d) { %s }", i, candidateCommentFields)
			} else {
				fmt.Fprintf(&query, "comments(first:0, after:$comments%d) { totalCount }", i)
			}
			if fetch || node.BlockedBy.PageInfo.HasNextPage {
				fmt.Fprintf(&query, "blockedBy(first:20, after:$dependencies%d) { %s }", i, candidateDependencyFields)
			} else {
				fmt.Fprintf(&query, "blockedBy(first:0, after:$dependencies%d) { pageInfo { hasNextPage endCursor } }", i)
			}
			query.WriteString("} }")
		}
		query.WriteString("}")
		var response map[string]*githubIssueNode
		if err := c.client.GraphQLWithType(ctx, graphQLQueryCandidateIssues, query.String(), variables, &response); err != nil {
			return complete
		}
		next = nil
		for i, previous := range pending {
			node := response[fmt.Sprintf("issue%d", i)]
			if node == nil || node.ID != previous.ID || node.BlockedBy == nil {
				continue
			}
			if !fetch {
				if previous.Comments.PageInfo.HasNextPage {
					if node.Comments.PageInfo.HasNextPage && node.Comments.PageInfo.EndCursor == previous.Comments.PageInfo.EndCursor {
						continue
					}
					node.Comments.Nodes = append(previous.Comments.Nodes, node.Comments.Nodes...)
				} else {
					node.Comments = previous.Comments
				}
				if previous.BlockedBy.PageInfo.HasNextPage {
					if node.BlockedBy.PageInfo.HasNextPage && node.BlockedBy.PageInfo.EndCursor == previous.BlockedBy.PageInfo.EndCursor {
						continue
					}
					node.BlockedBy.Nodes = append(previous.BlockedBy.Nodes, node.BlockedBy.Nodes...)
				} else {
					node.BlockedBy = previous.BlockedBy
				}
			}
			next = append(next, *node)
		}
		pending = next
		fetch = false
	}
	return complete
}

func candidateEvidenceComplete(node githubIssueNode) bool {
	if node.BlockedBy == nil || node.BlockedBy.PageInfo.HasNextPage || node.Comments.PageInfo.HasNextPage || len(node.Comments.Nodes) < node.Comments.TotalCount {
		return false
	}
	for _, blocker := range node.BlockedBy.Nodes {
		if blocker.Labels.PageInfo.HasNextPage {
			return false
		}
	}
	return true
}

func candidateEvidenceCanPage(node githubIssueNode) bool {
	if node.BlockedBy == nil {
		return false
	}
	for _, info := range []pageInfo{node.Comments.PageInfo, node.BlockedBy.PageInfo} {
		if info.HasNextPage && strings.TrimSpace(info.EndCursor) == "" {
			return false
		}
	}
	return node.Comments.PageInfo.HasNextPage || node.BlockedBy.PageInfo.HasNextPage
}

func (c *Connector) hydrateCandidateWithEvidence(ctx context.Context, issue connector.Issue, states []string, evidence map[string]githubIssueNode) (connector.Issue, error) {
	node, ok := evidence[issue.ID]
	issues := []connector.Issue{issue}
	if !ok {
		err := c.hydrateCandidateIssues(ctx, issues, states)
		return issues[0], err
	}
	ref, ok := issueRefFromIdentifier(issue.Identifier)
	if !ok {
		return issue, ErrInvalidResponse
	}
	node.Repository = repository{NameWithOwner: ref.Owner + "/" + ref.Name}
	issue.Description = node.Body
	issue.UpdatedAt = parseGitHubTime(node.UpdatedAt)
	issue.ModelOverride = parseModelOverride(node.Body)
	issue.Comments = connectorIssueComments(node.Comments.Nodes)
	issue.CommentCount = node.Comments.TotalCount
	issue.WorkpadSignal = parseWorkpadSignal(node)
	issue.BlockerReason = parseBlockerReason(node)
	native := make([]restIssueDependency, 0, len(node.BlockedBy.Nodes))
	for _, blocker := range node.BlockedBy.Nodes {
		native = append(native, restIssueDependency{NodeID: blocker.ID, Number: blocker.Number, Body: blocker.Body, State: blocker.State, HTMLURL: "https://github.com/" + blocker.Repository.NameWithOwner + fmt.Sprintf("/issues/%d", blocker.Number), Labels: blocker.Labels.Nodes})
	}
	refs := c.nativeBlockedRefsFromREST(ref, native)
	c.recordNativeDependencyCapability(node.Repository.NameWithOwner, nativeDependencyStatusAvailable, "")
	applyNativeDependencyEvidence(&issue, node.Repository.NameWithOwner, refs)
	issues[0] = issue
	if err := c.resolveBlockedByProjectState(ctx, issues); err != nil {
		return issue, err
	}
	err := c.attachStatePullRequests(ctx, issues, true)
	return issues[0], err
}
