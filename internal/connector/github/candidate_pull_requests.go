package github

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// These observations live only for the source page. Cached status is reusable
// only after a new observation of each independently changing collection.
type candidatePullRequestEvidence struct {
	complete    bool
	number      int
	repo        pullRequestRepo
	source      string
	pullRequest *pullRequestNode
}

type candidatePullRequestReference struct {
	pullRequest
	HeadRefName string `json:"headRefName"`
}

const candidatePRReferenceFields = `number url state updatedAt headRefOid headRefName repository { nameWithOwner }`

// The observation includes independent collections, not just updatedAt or
// headRefOid. Check contents validate cached timing; reviews and comments are
// interpreted afresh, so their edits never depend on the check revision.
const candidatePRFields = `id number url state mergeStateStatus isDraft updatedAt mergedAt
 headRefName baseRefName headRefOid baseRefOid
 labels(first:100) { totalCount pageInfo { hasNextPage } nodes { name } }
 reviews(first:100) { totalCount pageInfo { hasNextPage } nodes { body url state submittedAt author { login __typename } commit { oid } } }
 comments(first:100) { totalCount pageInfo { hasNextPage } nodes { id databaseId body url createdAt updatedAt author { login __typename } authorAssociation } }
 commits(last:1) { nodes { commit { oid committedDate statusCheckRollup { contexts(first:100) { totalCount pageInfo { hasNextPage } nodes {
 __typename
 ... on CheckRun { databaseId name status conclusion startedAt completedAt detailsUrl title summary text annotations(first:100) { totalCount pageInfo { hasNextPage } nodes { path annotationLevel message rawDetails } } checkSuite { updatedAt workflowRun { databaseId updatedAt runAttempt } } }
 ... on StatusContext { context state createdAt targetUrl }
 } } } } } }`

type candidatePRSnapshot struct {
	ID               string                          `json:"id"`
	Number           int                             `json:"number"`
	URL              string                          `json:"url"`
	State            string                          `json:"state"`
	MergeStateStatus string                          `json:"mergeStateStatus"`
	IsDraft          bool                            `json:"isDraft"`
	UpdatedAt        *string                         `json:"updatedAt"`
	MergedAt         *string                         `json:"mergedAt"`
	HeadRefName      string                          `json:"headRefName"`
	BaseRefName      string                          `json:"baseRefName"`
	HeadRefOID       string                          `json:"headRefOid"`
	BaseRefOID       string                          `json:"baseRefOid"`
	Labels           nodeConnection[label]           `json:"labels"`
	Reviews          nodeConnection[json.RawMessage] `json:"reviews"`
	Comments         nodeConnection[json.RawMessage] `json:"comments"`
	Commits          nodeConnection[struct {
		Commit struct {
			OID               string  `json:"oid"`
			CommittedDate     *string `json:"committedDate"`
			StatusCheckRollup *struct {
				Contexts nodeConnection[json.RawMessage] `json:"contexts"`
			} `json:"statusCheckRollup"`
		} `json:"commit"`
	}] `json:"commits"`
}

func (s candidatePRSnapshot) complete() bool {
	if s.ID == "" || s.Number <= 0 || s.HeadRefOID == "" || len(s.Commits.Nodes) != 1 || s.Commits.Nodes[0].Commit.OID != s.HeadRefOID {
		return false
	}
	if s.Labels.PageInfo.HasNextPage || len(s.Labels.Nodes) != s.Labels.TotalCount || s.Reviews.PageInfo.HasNextPage || len(s.Reviews.Nodes) != s.Reviews.TotalCount || s.Comments.PageInfo.HasNextPage || len(s.Comments.Nodes) != s.Comments.TotalCount {
		return false
	}
	rollup := s.Commits.Nodes[0].Commit.StatusCheckRollup
	if rollup == nil {
		return true
	}
	if rollup.Contexts.PageInfo.HasNextPage || len(rollup.Contexts.Nodes) != rollup.Contexts.TotalCount {
		return false
	}
	for _, raw := range rollup.Contexts.Nodes {
		var context struct {
			Annotations nodeConnection[json.RawMessage]
		}
		if err := json.Unmarshal(raw, &context); err != nil {
			return false
		}
		if context.Annotations.PageInfo.HasNextPage || len(context.Annotations.Nodes) != context.Annotations.TotalCount {
			return false
		}
	}
	return true
}

func (s candidatePRSnapshot) node() pullRequestNode {
	node := pullRequestNode{NodeID: s.ID, Number: s.Number, URL: s.URL, State: s.State, MergeableState: s.MergeStateStatus, Draft: s.IsDraft, ActivityAt: parseGitHubTime(s.UpdatedAt), MergedAt: parseGitHubTime(s.MergedAt), HeadRefName: s.HeadRefName, BaseRefName: s.BaseRefName, HeadSHA: s.HeadRefOID, BaseSHA: s.BaseRefOID}
	for _, l := range s.Labels.Nodes {
		node.Labels = append(node.Labels, l.Name)
	}
	if len(s.Commits.Nodes) == 1 {
		node.HeadCommittedAt = parseGitHubTime(s.Commits.Nodes[0].Commit.CommittedDate)
	}
	return node
}

func (c *Connector) observeCandidatePullRequests(ctx context.Context, sources []githubIssueNode, evidence map[string]githubIssueNode) {
	var selected []githubIssueNode
	repos := map[string]pullRequestRepo{}
	for _, source := range sources {
		if pullRequestStatusPolicy(source.CandidateState, true) == pullRequestStatusSkip {
			continue
		}
		if _, ok := evidence[source.ID]; !ok {
			continue
		}
		repo, ok := pullRequestRepoFromName(source.Repository.NameWithOwner)
		if !ok {
			c.logCandidatePRFallback(ctx, source.ID, "missing repository")
			continue
		}
		node := evidence[source.ID]
		node.CandidatePR = &candidatePullRequestEvidence{}
		evidence[source.ID] = node
		selected = append(selected, source)
		repos[pullRequestRepoName(repo)] = repo
	}
	if len(selected) == 0 {
		return
	}
	repoNames := make([]string, 0, len(repos))
	for name := range repos {
		repoNames = append(repoNames, name)
	}
	sort.Strings(repoNames)
	var query strings.Builder
	query.WriteString("query DetentGitHubCandidatePullRequestReferences($ids:[ID!]!) { nodes(ids:$ids) { ... on Issue { id timelineItems(last:100,itemTypes:[LABELED_EVENT,UNLABELED_EVENT]) { nodes { __typename ... on LabeledEvent { createdAt label { name } actor { login __typename } } ... on UnlabeledEvent { createdAt label { name } actor { login __typename } } } } closedByPullRequestsReferences(first:100) { totalCount pageInfo { hasNextPage } nodes { " + candidatePRReferenceFields + " } } } }")
	for i, name := range repoNames {
		repo := repos[name]
		fmt.Fprintf(&query, "repo%d: repository(owner:%q,name:%q) { pullRequests(first:100,orderBy:{field:UPDATED_AT,direction:DESC}) { pageInfo { hasNextPage } nodes { %s } } }", i, repo.Owner, repo.Name, candidatePRReferenceFields)
	}
	query.WriteString("rateLimit { limit used cost remaining resetAt } }")
	ids := make([]string, 0, len(selected))
	for _, source := range selected {
		ids = append(ids, source.ID)
	}
	var response map[string]json.RawMessage
	if err := c.client.GraphQLWithType(ctx, graphQLQueryCandidateIssues, query.String(), map[string]any{"ids": ids}, &response); err != nil {
		c.logCandidatePRFallback(ctx, "", "association observation unavailable")
		return
	}
	var nodes []struct {
		ID            string                                         `json:"id"`
		TimelineItems nodeConnection[timelineItem]                   `json:"timelineItems"`
		References    *nodeConnection[candidatePullRequestReference] `json:"closedByPullRequestsReferences"`
	}
	if err := json.Unmarshal(response["nodes"], &nodes); err != nil {
		c.logCandidatePRFallback(ctx, "", "missing association observation")
		return
	}
	refs := map[string]*nodeConnection[candidatePullRequestReference]{}
	for _, node := range nodes {
		refs[node.ID] = node.References
		if prior, ok := evidence[node.ID]; ok {
			prior.TimelineItems = node.TimelineItems
			evidence[node.ID] = prior
		}
	}
	discovery := map[string]nodeConnection[candidatePullRequestReference]{}
	for i, name := range repoNames {
		var repository struct {
			PullRequests *nodeConnection[candidatePullRequestReference] `json:"pullRequests"`
		}
		if err := json.Unmarshal(response[fmt.Sprintf("repo%d", i)], &repository); err == nil && repository.PullRequests != nil {
			discovery[name] = *repository.PullRequests
		}
	}
	byKey := map[pullRequestKey][]string{}
	for _, source := range selected {
		connection := refs[source.ID]
		if connection == nil || connection.PageInfo.HasNextPage || len(connection.Nodes) != connection.TotalCount {
			c.logCandidatePRFallback(ctx, source.ID, "incomplete associations")
			continue
		}
		simple := nodeConnection[pullRequest]{}
		for _, ref := range connection.Nodes {
			simple.Nodes = append(simple.Nodes, ref.pullRequest)
		}
		ref, found := firstPullRequestReference(simple)
		association := "github_closing_reference"
		if !found && (normalizeStateName(source.CandidateState) != normalizeStateName("Blocked") || statusLabelConflictIssue(c.buildLabelIssue(source, source.CandidateState))) {
			collection, ok := discovery[source.Repository.NameWithOwner]
			if !ok {
				c.logCandidatePRFallback(ctx, source.ID, "missing branch discovery")
				continue
			}
			for _, pr := range collection.Nodes {
				if branchMatchesIssuePrefix(pr.HeadRefName, detentIssueBranchPrefix(buildIdentifier(source.Repository.NameWithOwner, source.Number))) {
					ref = pullRequestReferenceFromNode(pr.pullRequest)
					found = true
					break
				}
			}
			if !found && collection.PageInfo.HasNextPage {
				c.logCandidatePRFallback(ctx, source.ID, "incomplete branch discovery")
				continue
			}
			association = "detent_branch"
		}
		node := evidence[source.ID]
		node.CandidatePR = &candidatePullRequestEvidence{complete: !found, source: association}
		evidence[source.ID] = node
		if !found {
			continue
		}
		repo, ok := pullRequestRepoFromName(ref.Repository)
		if !ok {
			c.logCandidatePRFallback(ctx, source.ID, "missing linked repository")
			continue
		}
		node.CandidatePR.repo = repo
		node.CandidatePR.number = ref.Number
		key := pullRequestKey{Repo: repo, Number: ref.Number}
		byKey[key] = append(byKey[key], source.ID)
	}
	c.observeCandidatePullRequestStatus(ctx, byKey, evidence)
}

func (c *Connector) observeCandidatePullRequestStatus(ctx context.Context, byKey map[pullRequestKey][]string, evidence map[string]githubIssueNode) {
	keys := make([]pullRequestKey, 0, len(byKey))
	for key := range byKey {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Repo != keys[j].Repo {
			return pullRequestRepoName(keys[i].Repo) < pullRequestRepoName(keys[j].Repo)
		}
		return keys[i].Number < keys[j].Number
	})
	if len(keys) == 0 {
		return
	}
	// Preserve the authoritative 100-item connections while bounding each
	// request to one PR: 100 + 100 + 100 + 1 + 100 + 100*100 = 10,401 nodes.
	if len(keys) > 1 {
		for _, key := range keys {
			c.observeCandidatePullRequestStatus(ctx, map[pullRequestKey][]string{key: byKey[key]}, evidence)
		}
		return
	}
	var query strings.Builder
	query.WriteString("query DetentGitHubCandidatePullRequestStatus {")
	for i, key := range keys {
		fmt.Fprintf(&query, "pr%d: repository(owner:%q,name:%q) { pullRequest(number:%d) { %s } }", i, key.Repo.Owner, key.Repo.Name, key.Number, candidatePRFields)
	}
	query.WriteString("rateLimit { limit used cost remaining resetAt } }")
	var response map[string]*struct {
		PullRequest *candidatePRSnapshot `json:"pullRequest"`
	}
	if err := c.client.GraphQLWithType(ctx, graphQLQueryCandidateIssues, query.String(), nil, &response); err != nil {
		c.logCandidatePRFallback(ctx, "", "status observation unavailable")
		return
	}
	for i, key := range keys {
		result := response[fmt.Sprintf("pr%d", i)]
		if result == nil || result.PullRequest == nil || !result.PullRequest.complete() || result.PullRequest.Number != key.Number {
			c.logCandidatePRFallback(ctx, fmt.Sprintf("%s#%d", pullRequestRepoName(key.Repo), key.Number), "incomplete status observation")
			continue
		}
		snapshot := result.PullRequest
		raw, err := json.Marshal(snapshot.Commits)
		if err != nil {
			c.logCandidatePRFallback(ctx, fmt.Sprintf("%s#%d", pullRequestRepoName(key.Repo), key.Number), "invalid status observation")
			continue
		}
		revision := sha256.Sum256(raw)
		pr := snapshot.node()
		status, cached := pullRequestStatus{}, false
		if c.pullRequests != nil {
			status, cached = c.pullRequests.GetObserved(key.Repo, key.Number, pr.HeadSHA, revision)
		}
		if !cached {
			var err error
			rollup := snapshot.Commits.Nodes[0].Commit.StatusCheckRollup
			if rollup != nil && rollup.Contexts.TotalCount > 0 {
				// GraphQL omits check creation and workflow start times used
				// by queue telemetry; REST fills these on a changed revision.
				c.logCandidatePRFallback(ctx, fmt.Sprintf("%s#%d", pullRequestRepoName(key.Repo), key.Number), "check timing detail missing or revision changed")
				c.deletePullRequestCIConditionalEntries(key.Repo, pr.HeadSHA)
				status.ci, err = c.fetchPullRequestCI(ctx, key.Repo, pr.HeadSHA)
				if err != nil {
					continue
				}
			} else {
				failures := requiredStatusCheckFailures(nil, nil, c.requiredChecks)
				status.ci = pullRequestCI{State: combinedCIState(requiredStatusCheckState(failures), combinedCIState(checkRunsState(nil), commitStatusesState(nil))), RequiredFailures: failures}
			}
		}
		status.reviews, err = snapshot.reviews()
		if err != nil {
			c.logCandidatePRFallback(ctx, fmt.Sprintf("%s#%d", pullRequestRepoName(key.Repo), key.Number), "invalid review observation")
			continue
		}
		// Time can advance without a GitHub revision. Recompute queue ages
		// from the retained REST timing inputs on every applicable refresh.
		now := time.Now()
		if c.now != nil {
			now = c.now()
		}
		telemetry := checkRunTelemetry(status.ci.checkRuns, status.ci.workflowRuns, now, c.unstartedThreshold)
		status.ci.CIQueueSeconds, status.ci.CIDurationSeconds = telemetry.QueueSeconds, telemetry.DurationSeconds
		status.ci.SlowChecks, status.ci.RunningChecks = telemetry.SlowChecks, telemetry.RunningChecks
		status.ci.UnstartedCheckCount, status.ci.UnstartedChecks = telemetry.UnstartedCount, telemetry.UnstartedChecks
		if c.pullRequests != nil {
			c.pullRequests.SetObserved(key.Repo, key.Number, pr.HeadSHA, revision, status)
		}
		applyPullRequestStatus(&pr, status)

		for _, id := range byKey[key] {
			node := evidence[id]
			node.CandidatePR.complete = true
			copyPR := pr
			node.CandidatePR.pullRequest = &copyPR
		}
	}
}

func (c *Connector) logCandidatePRFallback(ctx context.Context, issueID, reason string) {
	if c.logger != nil {
		c.logger.InfoContext(ctx, "github candidate PR detail fallback", "candidate_ref", issueID, "reason", reason)
	}
}

func (s candidatePRSnapshot) reviews() (pullRequestCodexReviews, error) {
	reviews := make([]restReview, 0, len(s.Reviews.Nodes))
	for _, raw := range s.Reviews.Nodes {
		var node struct {
			Body        string
			URL         string
			State       string
			SubmittedAt *time.Time
			Author      *actor
			Commit      struct{ OID string }
		}
		if err := json.Unmarshal(raw, &node); err != nil {
			return pullRequestCodexReviews{}, err
		}
		reviews = append(reviews, restReview{Body: node.Body, HTMLURL: node.URL, State: node.State, SubmittedAt: node.SubmittedAt, User: node.Author, CommitID: node.Commit.OID})
	}
	comments := make([]restComment, 0, len(s.Comments.Nodes))
	for _, raw := range s.Comments.Nodes {
		var node struct {
			issueComment
			DatabaseID int64
		}
		if err := json.Unmarshal(raw, &node); err != nil {
			return pullRequestCodexReviews{}, err
		}
		comments = append(comments, restComment{ID: node.DatabaseID, NodeID: node.ID, Body: node.Body, HTMLURL: node.URL, User: node.Author, AuthorAssociation: node.AuthorAssociation, CreatedAt: parseGitHubTime(node.CreatedAt), UpdatedAt: parseGitHubTime(node.UpdatedAt)})
	}
	return pullRequestReviewsFromEvidence(reviews, comments, s.HeadRefOID), nil
}
