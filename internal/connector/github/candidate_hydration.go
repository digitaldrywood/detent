package github

import (
	"context"
	"fmt"
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
)

// Each scheduler alias has at most 100 + 20 + 20*20 connection nodes.
const candidateHydrationBatchSize = 25

const candidateCommentFields = `totalCount pageInfo { hasNextPage endCursor }
 nodes { id body url author { login } authorAssociation createdAt updatedAt }`
const candidateDependencyFields = `pageInfo { hasNextPage endCursor }
 nodes { id number body state updatedAt repository { nameWithOwner } labels(first: 20) { pageInfo { hasNextPage endCursor } nodes { name } } }`
const candidateSchedulerFields = `comments(first: 100) { ` + candidateCommentFields + ` }
 blockedBy(first: 20) { ` + candidateDependencyFields + ` }`

var candidateProjectItemsQuery = strings.Replace(schedulerProjectItemsQuery, "comments { totalCount }", candidateSchedulerFields, 1)

// candidateEvidence is scoped to one source page. Only complete snapshots can
// replace REST hydration; nil native connections are not authoritative empties.
func (c *Connector) candidateEvidence(ctx context.Context, nodes []githubIssueNode, fetch bool) map[string]githubIssueNode {
	// Admission preserves completed observations and hydrates missing entries
	// through its legacy reader when batching fails.
	complete, err := c.candidateEvidenceBatched(ctx, nodes, fetch)
	if err != nil && c.logger != nil {
		c.logger.DebugContext(ctx, "github candidate hydration incomplete; using legacy readers for missing evidence", "error", err)
	}
	return complete
}

func (c *Connector) candidateEvidenceBatched(ctx context.Context, nodes []githubIssueNode, fetch bool) (map[string]githubIssueNode, error) {
	complete := make(map[string]githubIssueNode)
	defer func() { c.observeCandidatePullRequests(ctx, nodes, complete) }()
	for start := 0; start < len(nodes); start += candidateHydrationBatchSize {
		batch, err := c.candidateEvidenceBatch(ctx, nodes[start:min(start+candidateHydrationBatchSize, len(nodes))], fetch)
		for id, node := range batch {
			complete[id] = node
		}
		if err != nil {
			return complete, err
		}
	}
	return complete, nil
}

func (c *Connector) candidateEvidenceBatch(ctx context.Context, nodes []githubIssueNode, fetch bool) (map[string]githubIssueNode, error) {
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
		query.WriteString(") { rateLimit { limit used cost remaining resetAt }")
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
			return complete, err
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
	return complete, nil
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
		if pullRequestStatusPolicy(issue.State, true) != pullRequestStatusSkip {
			c.logCandidatePRFallback(ctx, issue.Identifier, "incomplete scheduler observation")
		}
		err := c.hydrateCandidateIssues(ctx, issues, states)
		return issues[0], err
	}
	issue, err := c.applySchedulerEvidence(issue, node)
	if err != nil {
		return issue, err
	}
	return c.hydratePullRequestWithEvidence(ctx, issue, node, true)
}

// applySchedulerEvidence is shared by admission and normal refresh. Callers
// only supply complete observations, including an authoritative native relation.
func (c *Connector) applySchedulerEvidence(issue connector.Issue, node githubIssueNode) (connector.Issue, error) {
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
	if reason := parseBlockerReason(node); reason != "" {
		issue.BlockerReason = reason
	}
	native := make([]restIssueDependency, 0, len(node.BlockedBy.Nodes))
	for _, blocker := range node.BlockedBy.Nodes {
		native = append(native, restIssueDependency{NodeID: blocker.ID, Number: blocker.Number, Body: blocker.Body, State: blocker.State, HTMLURL: "https://github.com/" + blocker.Repository.NameWithOwner + fmt.Sprintf("/issues/%d", blocker.Number), Labels: blocker.Labels.Nodes})
	}
	refs := c.nativeBlockedRefsFromREST(ref, native)
	c.recordNativeDependencyCapability(node.Repository.NameWithOwner, nativeDependencyStatusAvailable, "")
	applyNativeDependencyEvidence(&issue, node.Repository.NameWithOwner, refs)
	return issue, nil
}

func (c *Connector) hydratePullRequestWithEvidence(ctx context.Context, issue connector.Issue, node githubIssueNode, useStatusCache bool) (connector.Issue, error) {
	issues := []connector.Issue{issue}
	if node.CandidatePR != nil && node.CandidatePR.complete {
		issues[0] = withoutPullRequestAssociation(issues[0])
		issues[0].PRHeadSHA = ""
		issues[0].PRHeadCommittedAt = nil
		if c.usesLabelStatus() {
			if transition, ok := currentLabelTransition(node.TimelineItems.Nodes, c.statusLabelForState(issue.State)); ok {
				issues[0].StageUpdatedAt = &transition.EnteredAt
				issues[0].StageUpdatedActor = transition.Actor
			}
		}
		if node.CandidatePR.pullRequest != nil {
			issues[0].PRSource = node.CandidatePR.source
			attachPullRequestToIssue(&issues[0], node.CandidatePR.repo, *node.CandidatePR.pullRequest)
		}
		return issues[0], nil
	}
	if node.CandidatePR != nil {
		// The board list carries only a short association preview. If the
		// observation could not resolve it, use the existing paginated reader.
		issues[0] = withoutPullRequestAssociation(issues[0])
		issues[0].PRHeadSHA, issues[0].PRHeadCommittedAt = "", nil
		if node.CandidatePR.number > 0 {
			number := node.CandidatePR.number
			issues[0].PRNumber = &number
			issues[0].PRRepository = pullRequestRepoName(node.CandidatePR.repo)
			issues[0].PRSource = node.CandidatePR.source
		} else if err := c.attachIssuePullRequestReferences(ctx, issues, c.usesLabelStatus(), false); err != nil {
			return issues[0], err
		}
	}
	err := c.attachStatePullRequests(ctx, issues, useStatusCache && node.CandidatePR == nil)
	return issues[0], err
}
