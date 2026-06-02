package github

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

const (
	projectItemsPageSize  = 50
	projectItemsPerIssue  = 100
	pullRequestsPageSize  = 100
	pullRequestsPageLimit = 3
)

const projectItemsQuery = `
query DetentGitHubProjectItems($projectId: ID!, $first: Int!, $after: String) {
  node(id: $projectId) {
    ... on ProjectV2 {
      items(first: $first, after: $after) {
        pageInfo { hasNextPage endCursor }
        nodes {
          id
          content {
            __typename
            ... on Issue {
              id
              number
              title
              body
              state
              url
              createdAt
              updatedAt
              author { login }
              assignees(first: 100) { nodes { id login } }
              labels(first: 20) { nodes { name } }
              repository { nameWithOwner }
              closedByPullRequestsReferences(first: 5) { nodes { number url } }
            }
          }
          statusValue: fieldValueByName(name: "Status") {
            ... on ProjectV2ItemFieldSingleSelectValue { name updatedAt }
          }
          priorityValue: fieldValueByName(name: "Priority") {
            ... on ProjectV2ItemFieldSingleSelectValue { name }
          }
          fieldValues(first: 100) {
            nodes {
              __typename
              ... on ProjectV2ItemFieldSingleSelectValue {
                name
                field { ... on ProjectV2FieldCommon { name } }
              }
              ... on ProjectV2ItemFieldTextValue {
                text
                field { ... on ProjectV2FieldCommon { name } }
              }
              ... on ProjectV2ItemFieldNumberValue {
                number
                field { ... on ProjectV2FieldCommon { name } }
              }
            }
          }
        }
      }
    }
  }
  rateLimit { limit used remaining cost resetAt }
}`

const issuesByIDQuery = `
query DetentGitHubIssuesByID($issueIds: [ID!]!, $projectItemsFirst: Int!) {
  nodes(ids: $issueIds) {
    __typename
    ... on Issue {
      id
      number
      title
      body
      state
      url
      createdAt
      updatedAt
      author { login }
      assignees(first: 100) { nodes { id login } }
      labels(first: 20) { nodes { name } }
      repository { nameWithOwner }
      closedByPullRequestsReferences(first: 5) { nodes { number url } }
      projectItems(first: $projectItemsFirst) {
        pageInfo { hasNextPage endCursor }
        nodes {
          id
          project { id }
          statusValue: fieldValueByName(name: "Status") {
            ... on ProjectV2ItemFieldSingleSelectValue { name updatedAt }
          }
          priorityValue: fieldValueByName(name: "Priority") {
            ... on ProjectV2ItemFieldSingleSelectValue { name }
          }
          fieldValues(first: 100) {
            nodes {
              __typename
              ... on ProjectV2ItemFieldSingleSelectValue {
                name
                field { ... on ProjectV2FieldCommon { name } }
              }
              ... on ProjectV2ItemFieldTextValue {
                text
                field { ... on ProjectV2FieldCommon { name } }
              }
              ... on ProjectV2ItemFieldNumberValue {
                number
                field { ... on ProjectV2FieldCommon { name } }
              }
            }
          }
        }
      }
    }
  }
  rateLimit { limit used remaining cost resetAt }
}`

const issueCommentsQuery = `
query DetentGitHubIssueComments($issueIds: [ID!]!) {
  nodes(ids: $issueIds) {
    __typename
    ... on Issue {
      id
      body
      comments(first: 100) { nodes { body } }
    }
  }
  rateLimit { limit used remaining cost resetAt }
}`

const pullRequestsQuery = `
query DetentGitHubPullRequests($owner: String!, $name: String!, $states: [PullRequestState!]!, $first: Int!, $after: String) {
  repository(owner: $owner, name: $name) {
    pullRequests(first: $first, after: $after, states: $states, orderBy: {field: UPDATED_AT, direction: DESC}) {
      pageInfo { hasNextPage endCursor }
      nodes {
        number
        url
        state
        headRefName
        commits(last: 1) {
          nodes {
            commit {
              statusCheckRollup { state }
            }
          }
        }
        latestReviews(first: 20) {
          nodes {
            body
            state
            author { login }
          }
        }
      }
    }
  }
  rateLimit { limit used remaining cost resetAt }
}`

const addCommentMutation = `
mutation DetentGitHubAddComment($subjectId: ID!, $body: String!) {
  addComment(input: {subjectId: $subjectId, body: $body}) {
    commentEdge { node { id } }
  }
}`

const userByLoginQuery = `
query DetentGitHubUserByLogin($login: String!) {
  user(login: $login) { id }
}`

const addAssigneesMutation = `
mutation DetentGitHubAddAssignee($assignableId: ID!, $assigneeIds: [ID!]!) {
  addAssigneesToAssignable(input: {assignableId: $assignableId, assigneeIds: $assigneeIds}) {
    assignable { ... on Issue { id } }
  }
}`

const issueAssigneesQuery = `
query DetentGitHubIssueAssignees($issueId: ID!) {
  node(id: $issueId) {
    ... on Issue {
      assignees(first: 100) { nodes { id login } }
    }
  }
}`

const removeAssigneesMutation = `
mutation DetentGitHubRemoveAssignees($assignableId: ID!, $assigneeIds: [ID!]!) {
  removeAssigneesFromAssignable(input: {assignableId: $assignableId, assigneeIds: $assigneeIds}) {
    assignable { ... on Issue { id } }
  }
}`

const statusFieldQuery = `
query DetentGitHubStatusField($projectId: ID!) {
  node(id: $projectId) {
    ... on ProjectV2 {
      field(name: "Status") {
        ... on ProjectV2SingleSelectField {
          id
          options { id name }
        }
      }
    }
  }
  rateLimit { limit used remaining cost resetAt }
}`

const singleSelectFieldQuery = `
query DetentGitHubSingleSelectField($projectId: ID!, $fieldName: String!) {
  node(id: $projectId) {
    __typename
    ... on ProjectV2 {
      field(name: $fieldName) {
        __typename
        ... on ProjectV2SingleSelectField {
          id
          options { id name color description }
        }
      }
    }
  }
}`

const projectItemForIssueQuery = `
query DetentGitHubProjectItemForIssue($issueId: ID!, $projectItemsFirst: Int!, $after: String) {
  node(id: $issueId) {
    ... on Issue {
      projectItems(first: $projectItemsFirst, after: $after) {
        pageInfo { hasNextPage endCursor }
        nodes {
          id
          project { id }
          statusValue: fieldValueByName(name: "Status") {
            ... on ProjectV2ItemFieldSingleSelectValue { name updatedAt }
          }
          priorityValue: fieldValueByName(name: "Priority") {
            ... on ProjectV2ItemFieldSingleSelectValue { name }
          }
          fieldValues(first: 100) {
            nodes {
              __typename
              ... on ProjectV2ItemFieldSingleSelectValue {
                name
                field { ... on ProjectV2FieldCommon { name } }
              }
              ... on ProjectV2ItemFieldTextValue {
                text
                field { ... on ProjectV2FieldCommon { name } }
              }
              ... on ProjectV2ItemFieldNumberValue {
                number
                field { ... on ProjectV2FieldCommon { name } }
              }
            }
          }
        }
      }
    }
  }
  rateLimit { limit used remaining cost resetAt }
}`

const updateSingleSelectFieldValueMutation = `
mutation DetentGitHubUpdateSingleSelectField($projectId: ID!, $itemId: ID!, $fieldId: ID!, $optionId: String!) {
  updateProjectV2ItemFieldValue(input: {
    projectId: $projectId,
    itemId: $itemId,
    fieldId: $fieldId,
    value: { singleSelectOptionId: $optionId }
  }) {
    projectV2Item { id }
  }
}`

var (
	modelOverridePattern  = regexp.MustCompile(`(?i)<!--\s*model:\s*(\S+?)\s*-->`)
	dependencyLinePattern = regexp.MustCompile("(?i)^\\s*(?:>\\s*)?(?:[-*+]\\s+)?(?:[*_`~]+)?\\s*(?:blocked\\s+by|depends[\\s-]+on)(?:[*_`~]+)?\\s*:\\s*(?:[*_`~]+)?\\s*(.+)\\s*$")
	issueRefPattern       = regexp.MustCompile(`(?:([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+))?#(\d+)`)
	numberedListPattern   = regexp.MustCompile(`^\d+[.)]\s+`)
	branchKeyPattern      = regexp.MustCompile(`[^A-Za-z0-9._-]`)
)

type pageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

type projectItemsConnection struct {
	PageInfo pageInfo          `json:"pageInfo"`
	Nodes    []projectItemNode `json:"nodes"`
}

type projectItemNode struct {
	ID            string                            `json:"id"`
	Content       *githubIssueNode                  `json:"content"`
	Project       *projectRef                       `json:"project"`
	StatusValue   *singleSelectValue                `json:"statusValue"`
	PriorityValue *singleSelectValue                `json:"priorityValue"`
	FieldValues   nodeConnection[projectFieldValue] `json:"fieldValues"`
}

type githubIssueNode struct {
	TypeName                       string                       `json:"__typename"`
	ID                             string                       `json:"id"`
	Number                         int                          `json:"number"`
	Title                          string                       `json:"title"`
	Body                           string                       `json:"body"`
	State                          string                       `json:"state"`
	URL                            string                       `json:"url"`
	CreatedAt                      *string                      `json:"createdAt"`
	UpdatedAt                      *string                      `json:"updatedAt"`
	Author                         *actor                       `json:"author"`
	Assignees                      nodeConnection[assignee]     `json:"assignees"`
	Labels                         nodeConnection[label]        `json:"labels"`
	Comments                       nodeConnection[issueComment] `json:"comments"`
	Repository                     repository                   `json:"repository"`
	ClosedByPullRequestsReferences nodeConnection[pullRequest]  `json:"closedByPullRequestsReferences"`
	ProjectItems                   *projectItemsConnection      `json:"projectItems"`
}

type nodeConnection[T any] struct {
	Nodes []T `json:"nodes"`
}

type assignee struct {
	ID    string `json:"id"`
	Login string `json:"login"`
}

type label struct {
	Name string `json:"name"`
}

type issueComment struct {
	Body string `json:"body"`
}

type pullRequest struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
}

type pullRequestNode struct {
	Number        int                               `json:"number"`
	URL           string                            `json:"url"`
	State         string                            `json:"state"`
	HeadRefName   string                            `json:"headRefName"`
	Commits       nodeConnection[pullRequestCommit] `json:"commits"`
	LatestReviews nodeConnection[pullRequestReview] `json:"latestReviews"`
}

type pullRequestCommit struct {
	Commit commitNode `json:"commit"`
}

type commitNode struct {
	StatusCheckRollup *statusCheckRollup `json:"statusCheckRollup"`
}

type statusCheckRollup struct {
	State string `json:"state"`
}

type pullRequestReview struct {
	Body   string `json:"body"`
	State  string `json:"state"`
	Author *actor `json:"author"`
}

type actor struct {
	Login string `json:"login"`
}

type pullRequestsConnection struct {
	PageInfo pageInfo          `json:"pageInfo"`
	Nodes    []pullRequestNode `json:"nodes"`
}

type repository struct {
	NameWithOwner string `json:"nameWithOwner"`
}

type projectRef struct {
	ID string `json:"id"`
}

type singleSelectValue struct {
	Name      string  `json:"name"`
	UpdatedAt *string `json:"updatedAt"`
}

type projectFieldValue struct {
	TypeName string       `json:"__typename"`
	Field    projectField `json:"field"`
	Name     string       `json:"name"`
	Text     string       `json:"text"`
	Number   *float64     `json:"number"`
}

type projectField struct {
	Name string `json:"name"`
}

func (c *Connector) FetchCandidateIssues(ctx context.Context) ([]connector.Issue, error) {
	if c.projectID == "" {
		return nil, ErrMissingProject
	}

	issues, err := c.fetchProjectItems(ctx, func(issue connector.Issue) bool {
		return stateInList(issue.State, c.activeStates)
	})
	if err != nil {
		return nil, err
	}
	if err := c.attachPullRequests(ctx, issues); err != nil {
		return nil, err
	}
	return issues, nil
}

func (c *Connector) FetchIssuesByStates(ctx context.Context, stateNames []string) ([]connector.Issue, error) {
	wantedStates := normalizedStateSet(stateNames)
	if len(wantedStates) == 0 {
		return []connector.Issue{}, nil
	}
	if c.projectID == "" {
		return nil, ErrMissingProject
	}

	issues, err := c.fetchProjectItems(ctx, func(issue connector.Issue) bool {
		_, ok := wantedStates[normalizeStateName(issue.State)]
		return ok
	})
	if err != nil {
		return nil, err
	}
	if _, ok := wantedStates[normalizeStateName("Blocked")]; ok {
		if err := c.populateBlockerReasons(ctx, issues); err != nil {
			return nil, err
		}
	}
	if attachPullRequestsForStates(wantedStates) {
		if err := c.attachPullRequests(ctx, issues); err != nil {
			return nil, err
		}
	}
	return issues, nil
}

func (c *Connector) FetchIssueStatesByIDs(ctx context.Context, issueIDs []string) ([]connector.Issue, error) {
	ids := uniqueNonBlank(issueIDs)
	if len(ids) == 0 {
		return []connector.Issue{}, nil
	}
	if c.projectID == "" {
		return nil, ErrMissingProject
	}

	var response struct {
		Nodes []githubIssueNode `json:"nodes"`
	}
	if err := c.client.GraphQL(ctx, issuesByIDQuery, map[string]any{
		"issueIds":          ids,
		"projectItemsFirst": projectItemsPerIssue,
	}, &response); err != nil {
		return nil, fmt.Errorf("fetch github issue states by ids: %w", err)
	}

	issues := make([]connector.Issue, 0, len(response.Nodes))
	for _, node := range response.Nodes {
		issue, ok, err := c.normalizeIssueNode(ctx, node)
		if err != nil {
			return nil, err
		}
		if ok {
			issues = append(issues, issue)
		}
	}
	sortIssuesByRequestedIDs(issues, ids)
	return issues, nil
}

func (c *Connector) populateBlockerReasons(ctx context.Context, issues []connector.Issue) error {
	ids := make([]string, 0, len(issues))
	for _, issue := range issues {
		if normalizeStateName(issue.State) == normalizeStateName("Blocked") {
			ids = append(ids, issue.ID)
		}
	}
	ids = uniqueNonBlank(ids)
	if len(ids) == 0 {
		return nil
	}

	var response struct {
		Nodes []githubIssueNode `json:"nodes"`
	}
	if err := c.client.GraphQL(ctx, issueCommentsQuery, map[string]any{
		"issueIds": ids,
	}, &response); err != nil {
		return fmt.Errorf("fetch github issue comments: %w", err)
	}

	reasonsByID := make(map[string]string, len(response.Nodes))
	for _, node := range response.Nodes {
		if node.TypeName != "Issue" {
			continue
		}
		if reason := parseBlockerReason(node); reason != "" {
			reasonsByID[node.ID] = reason
		}
	}
	for index := range issues {
		if reason, ok := reasonsByID[issues[index].ID]; ok {
			issues[index].BlockerReason = reason
		}
	}
	return nil
}

func (c *Connector) CreateComment(ctx context.Context, issueID string, body string) error {
	var response struct {
		AddComment *struct {
			CommentEdge *struct {
				Node *struct {
					ID string `json:"id"`
				} `json:"node"`
			} `json:"commentEdge"`
		} `json:"addComment"`
	}
	if err := c.client.GraphQL(ctx, addCommentMutation, map[string]any{
		"subjectId": strings.TrimSpace(issueID),
		"body":      body,
	}, &response); err != nil {
		return fmt.Errorf("create github comment: %w", err)
	}
	if response.AddComment == nil ||
		response.AddComment.CommentEdge == nil ||
		response.AddComment.CommentEdge.Node == nil ||
		strings.TrimSpace(response.AddComment.CommentEdge.Node.ID) == "" {
		return ErrCommentCreateFailed
	}

	return nil
}

func (c *Connector) UpdateIssueState(ctx context.Context, issueID string, stateName string) error {
	if c.projectID == "" {
		return ErrMissingProject
	}

	item, err := c.resolveProjectItem(ctx, strings.TrimSpace(issueID))
	if err != nil {
		return err
	}
	if c.terminalStatusUpdateBlocked(item.StatusName, stateName) {
		return nil
	}

	githubState := c.detentToGitHubState(stateName)
	fieldID, optionID, err := c.resolveStatusOption(ctx, githubState)
	if err != nil {
		if errors.Is(err, ErrStatusOptionNotFound) {
			c.statusCache.Clear(c.projectID)
		}
		return err
	}

	if err := c.updateStatusFieldValue(ctx, item.ID, fieldID, optionID); err == nil {
		return nil
	}

	c.statusCache.Clear(c.projectID)
	fieldID, optionID, err = c.resolveStatusOption(ctx, githubState)
	if err != nil {
		return err
	}
	return c.updateStatusFieldValue(ctx, item.ID, fieldID, optionID)
}

func (c *Connector) SetAssignee(ctx context.Context, issueID string, login string) error {
	issueID = strings.TrimSpace(issueID)
	userID, err := c.resolveUserID(ctx, login)
	if err != nil {
		return err
	}
	currentAssignees, err := c.fetchIssueAssignees(ctx, issueID)
	if err != nil {
		return err
	}
	removeIDs, alreadyAssigned := assigneeReplacement(currentAssignees, userID)
	if len(removeIDs) > 0 {
		if err := c.removeAssignees(ctx, issueID, removeIDs); err != nil {
			return err
		}
	}
	if alreadyAssigned {
		return nil
	}
	return c.addAssignee(ctx, issueID, userID)
}

func (c *Connector) SetField(ctx context.Context, issueID string, fieldName string, value string) error {
	if c.projectID == "" {
		return ErrMissingProject
	}

	item, err := c.resolveProjectItem(ctx, strings.TrimSpace(issueID))
	if err != nil {
		return err
	}
	fieldID, optionID, err := c.resolveSingleSelectFieldOption(ctx, fieldName, value)
	if err != nil {
		return err
	}
	return c.updateProjectV2SingleSelectFieldValue(ctx, item.ID, fieldID, optionID, ErrProjectFieldUpdateFailed)
}

type issuePullRequestCandidate struct {
	Index        int
	BranchPrefix string
}

type pullRequestRepo struct {
	Owner string
	Name  string
}

func (c *Connector) attachPullRequests(ctx context.Context, issues []connector.Issue) error {
	byRepo := make(map[pullRequestRepo][]issuePullRequestCandidate)
	for index, issue := range issues {
		repo, ok := pullRequestRepoFromIdentifier(issue.Identifier)
		if !ok {
			continue
		}
		branchPrefix := detentIssueBranchPrefix(issue.Identifier)
		if branchPrefix == "" {
			continue
		}
		byRepo[repo] = append(byRepo[repo], issuePullRequestCandidate{
			Index:        index,
			BranchPrefix: branchPrefix,
		})
	}
	if len(byRepo) == 0 {
		return nil
	}

	repos := make([]pullRequestRepo, 0, len(byRepo))
	for repo := range byRepo {
		repos = append(repos, repo)
	}
	sort.Slice(repos, func(i, j int) bool {
		left := repos[i].Owner + "/" + repos[i].Name
		right := repos[j].Owner + "/" + repos[j].Name
		return left < right
	})

	for _, repo := range repos {
		pullRequests, err := c.fetchRepositoryPullRequests(ctx, repo)
		if err != nil {
			return err
		}
		attachMatchingPullRequests(issues, byRepo[repo], pullRequests)
	}
	return nil
}

func (c *Connector) fetchRepositoryPullRequests(ctx context.Context, repo pullRequestRepo) ([]pullRequestNode, error) {
	return c.fetchRepositoryPullRequestsPage(ctx, repo, nil, 1)
}

func (c *Connector) fetchRepositoryPullRequestsPage(
	ctx context.Context,
	repo pullRequestRepo,
	after *string,
	page int,
) ([]pullRequestNode, error) {
	var response struct {
		Repository *struct {
			PullRequests pullRequestsConnection `json:"pullRequests"`
		} `json:"repository"`
	}
	if err := c.client.GraphQL(ctx, pullRequestsQuery, map[string]any{
		"owner":  repo.Owner,
		"name":   repo.Name,
		"states": []string{"OPEN", "MERGED"},
		"first":  pullRequestsPageSize,
		"after":  after,
	}, &response); err != nil {
		return nil, fmt.Errorf("fetch github pull requests: %w", err)
	}
	if response.Repository == nil {
		return nil, ErrInvalidResponse
	}

	pullRequests := append([]pullRequestNode(nil), response.Repository.PullRequests.Nodes...)
	if !response.Repository.PullRequests.PageInfo.HasNextPage || page >= pullRequestsPageLimit {
		return pullRequests, nil
	}
	cursor := strings.TrimSpace(response.Repository.PullRequests.PageInfo.EndCursor)
	if cursor == "" {
		return nil, ErrInvalidResponse
	}
	next, err := c.fetchRepositoryPullRequestsPage(ctx, repo, &cursor, page+1)
	if err != nil {
		return nil, err
	}
	return append(pullRequests, next...), nil
}

func attachMatchingPullRequests(
	issues []connector.Issue,
	candidates []issuePullRequestCandidate,
	pullRequests []pullRequestNode,
) {
	for _, pullRequest := range pullRequests {
		branchName := strings.TrimSpace(pullRequest.HeadRefName)
		if branchName == "" {
			continue
		}
		for _, candidate := range candidates {
			if issues[candidate.Index].PullRequest != nil {
				continue
			}
			if !branchMatchesIssuePrefix(branchName, candidate.BranchPrefix) {
				continue
			}

			issues[candidate.Index].PullRequest = &connector.PullRequest{
				Number:           pullRequest.Number,
				URL:              strings.TrimSpace(pullRequest.URL),
				BranchName:       branchName,
				State:            strings.ToUpper(strings.TrimSpace(pullRequest.State)),
				CIStatus:         normalizePullRequestCIStatus(pullRequestCIState(pullRequest)),
				CodexReviewState: pullRequestCodexReviewState(pullRequest),
			}
			if issues[candidate.Index].PRNumber == nil && pullRequest.Number > 0 {
				number := pullRequest.Number
				issues[candidate.Index].PRNumber = &number
			}
		}
	}
}

func (c *Connector) fetchProjectItems(ctx context.Context, keepIssue func(connector.Issue) bool) ([]connector.Issue, error) {
	var after *string
	allIssues := []connector.Issue{}

	for {
		var response struct {
			Node *struct {
				Items projectItemsConnection `json:"items"`
			} `json:"node"`
		}
		if err := c.client.GraphQL(ctx, projectItemsQuery, map[string]any{
			"projectId": c.projectID,
			"first":     projectItemsPageSize,
			"after":     after,
		}, &response); err != nil {
			return nil, fmt.Errorf("fetch github project items: %w", err)
		}
		if response.Node == nil {
			return nil, ErrProjectNotFound
		}

		for _, item := range response.Node.Items.Nodes {
			issue, ok := c.normalizeProjectItem(item)
			if !ok {
				continue
			}
			allIssues = append(allIssues, issue)
		}

		if !response.Node.Items.PageInfo.HasNextPage {
			resolveBlockedByProjectState(allIssues)
			issues := make([]connector.Issue, 0, len(allIssues))
			for _, issue := range allIssues {
				if keepIssue(issue) {
					issues = append(issues, issue)
				}
			}
			return issues, nil
		}
		cursor := strings.TrimSpace(response.Node.Items.PageInfo.EndCursor)
		if cursor == "" {
			return nil, ErrInvalidResponse
		}
		after = &cursor
	}
}

func (c *Connector) normalizeProjectItem(item projectItemNode) (connector.Issue, bool) {
	if item.Content == nil || item.Content.TypeName != "Issue" {
		return connector.Issue{}, false
	}
	return c.buildIssue(
		*item.Content,
		singleSelectName(item.StatusValue),
		singleSelectName(item.PriorityValue),
		singleSelectUpdatedAt(item.StatusValue),
		projectFieldValues(item.FieldValues),
	), true
}

func resolveBlockedByProjectState(issues []connector.Issue) {
	byIdentifier := make(map[string]connector.Issue, len(issues))
	for _, issue := range issues {
		identifier := normalizedIssueIdentifier(issue.Identifier)
		if identifier != "" {
			byIdentifier[identifier] = issue
		}
	}

	for issueIndex := range issues {
		for blockerIndex := range issues[issueIndex].BlockedBy {
			identifier := normalizedIssueIdentifier(issues[issueIndex].BlockedBy[blockerIndex].Identifier)
			blocker, ok := byIdentifier[identifier]
			if !ok {
				continue
			}
			issues[issueIndex].BlockedBy[blockerIndex].ID = blocker.ID
			issues[issueIndex].BlockedBy[blockerIndex].Identifier = blocker.Identifier
			issues[issueIndex].BlockedBy[blockerIndex].State = blocker.State
		}
	}
}

func normalizedIssueIdentifier(identifier string) string {
	return strings.ToLower(strings.TrimSpace(identifier))
}

func (c *Connector) normalizeIssueNode(ctx context.Context, issue githubIssueNode) (connector.Issue, bool, error) {
	if issue.TypeName != "Issue" {
		return connector.Issue{}, false, nil
	}
	stateName, priorityName, statusUpdatedAt, fields, ok, err := c.resolveIssueProjectFields(ctx, issue.ID, issue.ProjectItems)
	if err != nil {
		return connector.Issue{}, false, err
	}
	if ok {
		return c.buildIssue(issue, stateName, priorityName, statusUpdatedAt, fields), true, nil
	}
	return c.buildIssue(issue, c.githubIssueStateToDetentState(issue.State), "", nil, nil), true, nil
}

func (c *Connector) resolveIssueProjectFields(ctx context.Context, issueID string, items *projectItemsConnection) (string, string, *time.Time, map[string]string, bool, error) {
	if stateName, priorityName, statusUpdatedAt, fields, ok := c.projectFields(issueID, items); ok {
		return stateName, priorityName, statusUpdatedAt, fields, true, nil
	}
	if items == nil || !items.PageInfo.HasNextPage {
		return "", "", nil, nil, false, nil
	}
	cursor := strings.TrimSpace(items.PageInfo.EndCursor)
	if cursor == "" {
		return "", "", nil, nil, false, ErrInvalidResponse
	}
	return c.fetchProjectFieldsPage(ctx, issueID, &cursor)
}

func (c *Connector) projectFields(issueID string, items *projectItemsConnection) (string, string, *time.Time, map[string]string, bool) {
	if items == nil {
		return "", "", nil, nil, false
	}
	for _, item := range items.Nodes {
		if item.Project != nil && item.Project.ID == c.projectID {
			c.projectCache.SetItemID(c.projectID, issueID, item.ID)
			return singleSelectName(item.StatusValue), singleSelectName(item.PriorityValue), singleSelectUpdatedAt(item.StatusValue), projectFieldValues(item.FieldValues), true
		}
	}
	return "", "", nil, nil, false
}

func (c *Connector) fetchProjectFieldsPage(ctx context.Context, issueID string, after *string) (string, string, *time.Time, map[string]string, bool, error) {
	var response struct {
		Node *struct {
			ProjectItems projectItemsConnection `json:"projectItems"`
		} `json:"node"`
	}
	if err := c.client.GraphQL(ctx, projectItemForIssueQuery, map[string]any{
		"issueId":           issueID,
		"projectItemsFirst": projectItemsPerIssue,
		"after":             after,
	}, &response); err != nil {
		return "", "", nil, nil, false, fmt.Errorf("fetch github project item fields: %w", err)
	}
	if response.Node == nil {
		return "", "", nil, nil, false, ErrProjectItemNotFound
	}
	if stateName, priorityName, statusUpdatedAt, fields, ok := c.projectFields(issueID, &response.Node.ProjectItems); ok {
		return stateName, priorityName, statusUpdatedAt, fields, true, nil
	}
	if !response.Node.ProjectItems.PageInfo.HasNextPage {
		return "", "", nil, nil, false, nil
	}
	cursor := strings.TrimSpace(response.Node.ProjectItems.PageInfo.EndCursor)
	if cursor == "" {
		return "", "", nil, nil, false, ErrInvalidResponse
	}
	return c.fetchProjectFieldsPage(ctx, issueID, &cursor)
}

func (c *Connector) buildIssue(issue githubIssueNode, statusName string, priorityName string, statusUpdatedAt *time.Time, fields map[string]string) connector.Issue {
	repo := strings.TrimSpace(issue.Repository.NameWithOwner)
	return connector.Issue{
		ID:               issue.ID,
		Identifier:       buildIdentifier(repo, issue.Number),
		Title:            issue.Title,
		Description:      issue.Body,
		Priority:         c.priorityRank(priorityName),
		State:            c.githubToDetentState(statusName),
		URL:              issue.URL,
		PRNumber:         firstPullRequestNumber(issue.ClosedByPullRequestsReferences),
		AuthorID:         actorLogin(issue.Author),
		AssigneeID:       firstAssigneeLogin(issue.Assignees),
		Assignees:        allAssigneeLogins(issue.Assignees),
		BlockedBy:        parseBlockedBy(issue.Body, repo),
		BlockerReason:    parseBlockerReason(issue),
		Labels:           labelNames(issue.Labels),
		Fields:           cloneStringMap(fields),
		AssignedToWorker: true,
		CreatedAt:        parseGitHubTime(issue.CreatedAt),
		UpdatedAt:        parseGitHubTime(issue.UpdatedAt),
		StageUpdatedAt:   statusUpdatedAt,
		ModelOverride:    parseModelOverride(issue.Body),
	}
}

func (c *Connector) resolveStatusOption(ctx context.Context, githubState string) (string, string, error) {
	metadata, err := c.resolveStatusMetadata(ctx)
	if err != nil {
		return "", "", err
	}

	optionID, ok := metadata.OptionIDsByName[githubState]
	if !ok || strings.TrimSpace(optionID) == "" {
		return "", "", fmt.Errorf("%w: %s", ErrStatusOptionNotFound, githubState)
	}
	return metadata.FieldID, optionID, nil
}

func (c *Connector) resolveUserID(ctx context.Context, login string) (string, error) {
	login = strings.TrimSpace(login)
	if login == "" {
		return "", ErrAssigneeNotFound
	}

	var response struct {
		User *struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	if err := c.client.GraphQL(ctx, userByLoginQuery, map[string]any{"login": login}, &response); err != nil {
		return "", fmt.Errorf("fetch github assignee: %w", err)
	}
	if response.User == nil || strings.TrimSpace(response.User.ID) == "" {
		return "", fmt.Errorf("%w: %s", ErrAssigneeNotFound, login)
	}
	return strings.TrimSpace(response.User.ID), nil
}

func (c *Connector) addAssignee(ctx context.Context, issueID string, userID string) error {
	issueID = strings.TrimSpace(issueID)
	userID = strings.TrimSpace(userID)
	if issueID == "" || userID == "" {
		return ErrAssigneeNotFound
	}

	var response struct {
		AddAssigneesToAssignable *struct {
			Assignable *struct {
				ID string `json:"id"`
			} `json:"assignable"`
		} `json:"addAssigneesToAssignable"`
	}
	if err := c.client.GraphQL(ctx, addAssigneesMutation, map[string]any{
		"assignableId": issueID,
		"assigneeIds":  []string{userID},
	}, &response); err != nil {
		return fmt.Errorf("set github assignee: %w", err)
	}
	if response.AddAssigneesToAssignable == nil ||
		response.AddAssigneesToAssignable.Assignable == nil ||
		strings.TrimSpace(response.AddAssigneesToAssignable.Assignable.ID) == "" {
		return ErrAssigneeUpdateFailed
	}
	return nil
}

func (c *Connector) fetchIssueAssignees(ctx context.Context, issueID string) ([]assignee, error) {
	issueID = strings.TrimSpace(issueID)
	if issueID == "" {
		return nil, ErrAssigneeNotFound
	}

	var response struct {
		Node *struct {
			Assignees nodeConnection[assignee] `json:"assignees"`
		} `json:"node"`
	}
	if err := c.client.GraphQL(ctx, issueAssigneesQuery, map[string]any{"issueId": issueID}, &response); err != nil {
		return nil, fmt.Errorf("fetch github issue assignees: %w", err)
	}
	if response.Node == nil {
		return nil, ErrAssigneeNotFound
	}
	return response.Node.Assignees.Nodes, nil
}

func (c *Connector) removeAssignees(ctx context.Context, issueID string, userIDs []string) error {
	issueID = strings.TrimSpace(issueID)
	userIDs = uniqueNonBlank(userIDs)
	if issueID == "" || len(userIDs) == 0 {
		return ErrAssigneeUpdateFailed
	}

	var response struct {
		RemoveAssigneesFromAssignable *struct {
			Assignable *struct {
				ID string `json:"id"`
			} `json:"assignable"`
		} `json:"removeAssigneesFromAssignable"`
	}
	if err := c.client.GraphQL(ctx, removeAssigneesMutation, map[string]any{
		"assignableId": issueID,
		"assigneeIds":  userIDs,
	}, &response); err != nil {
		return fmt.Errorf("replace github assignee: %w", err)
	}
	if response.RemoveAssigneesFromAssignable == nil ||
		response.RemoveAssigneesFromAssignable.Assignable == nil ||
		strings.TrimSpace(response.RemoveAssigneesFromAssignable.Assignable.ID) == "" {
		return ErrAssigneeUpdateFailed
	}
	return nil
}

func (c *Connector) resolveSingleSelectFieldOption(ctx context.Context, fieldName string, value string) (string, string, error) {
	fieldName = strings.TrimSpace(fieldName)
	value = strings.TrimSpace(value)
	if fieldName == "" || value == "" {
		return "", "", ErrProjectFieldOptionNotFound
	}

	field, err := c.fetchSingleSelectField(ctx, fieldName)
	if err != nil {
		return "", "", err
	}
	if optionID := singleSelectOptionID(field.Options, value); optionID != "" {
		return field.ID, optionID, nil
	}

	for range 3 {
		field, err = c.fetchSingleSelectField(ctx, fieldName)
		if err != nil {
			return "", "", err
		}
		if optionID := singleSelectOptionID(field.Options, value); optionID != "" {
			return field.ID, optionID, nil
		}
		options := singleSelectOptionsWithRequiredAtEnd(field.Options, []projectSingleSelectOption{ownershipOption(value)})
		updatedOptions, err := c.updateProjectFieldOptions(ctx, field.ID, options)
		if err != nil {
			return "", "", fmt.Errorf("ensure github project field options: %w", err)
		}
		if optionID := singleSelectOptionID(updatedOptions, value); optionID != "" {
			return field.ID, optionID, nil
		}
	}
	return "", "", fmt.Errorf("%w: %s=%s", ErrProjectFieldOptionNotFound, fieldName, value)
}

func (c *Connector) fetchSingleSelectField(ctx context.Context, fieldName string) (projectSingleSelectField, error) {
	var response struct {
		Node *struct {
			TypeName string                       `json:"__typename"`
			Field    *projectOptionsFieldResponse `json:"field"`
		} `json:"node"`
	}
	if err := c.client.GraphQL(ctx, singleSelectFieldQuery, map[string]any{
		"projectId": c.projectID,
		"fieldName": strings.TrimSpace(fieldName),
	}, &response); err != nil {
		return projectSingleSelectField{}, fmt.Errorf("fetch github project field: %w", err)
	}
	if response.Node == nil || response.Node.TypeName != "ProjectV2" {
		return projectSingleSelectField{}, ErrProjectNotFound
	}
	return decodeProjectSingleSelectField(fieldName, response.Node.Field)
}

func (c *Connector) resolveStatusMetadata(ctx context.Context) (statusMetadata, error) {
	if metadata, ok := c.statusCache.Get(c.projectID); ok {
		return metadata, nil
	}

	var response struct {
		Node *struct {
			Field *struct {
				ID      string `json:"id"`
				Options []struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"options"`
			} `json:"field"`
		} `json:"node"`
	}
	if err := c.client.GraphQL(ctx, statusFieldQuery, map[string]any{"projectId": c.projectID}, &response); err != nil {
		return statusMetadata{}, fmt.Errorf("fetch github status field: %w", err)
	}
	if response.Node == nil || response.Node.Field == nil || strings.TrimSpace(response.Node.Field.ID) == "" {
		return statusMetadata{}, ErrStatusFieldNotFound
	}

	metadata := statusMetadata{
		FieldID:         strings.TrimSpace(response.Node.Field.ID),
		OptionIDsByName: make(map[string]string, len(response.Node.Field.Options)),
	}
	for _, option := range response.Node.Field.Options {
		name := strings.TrimSpace(option.Name)
		id := strings.TrimSpace(option.ID)
		if name == "" || id == "" {
			continue
		}
		metadata.OptionIDsByName[name] = id
	}
	c.statusCache.Set(c.projectID, metadata)
	return metadata, nil
}

type projectItemStatus struct {
	ID         string
	StatusName string
}

func (c *Connector) resolveProjectItem(ctx context.Context, issueID string) (projectItemStatus, error) {
	return c.fetchProjectItemPage(ctx, issueID, nil)
}

func (c *Connector) fetchProjectItemPage(ctx context.Context, issueID string, after *string) (projectItemStatus, error) {
	var response struct {
		Node *struct {
			ProjectItems projectItemsConnection `json:"projectItems"`
		} `json:"node"`
	}
	if err := c.client.GraphQL(ctx, projectItemForIssueQuery, map[string]any{
		"issueId":           issueID,
		"projectItemsFirst": projectItemsPerIssue,
		"after":             after,
	}, &response); err != nil {
		return projectItemStatus{}, fmt.Errorf("fetch github project item: %w", err)
	}
	if response.Node == nil {
		return projectItemStatus{}, ErrProjectItemNotFound
	}

	for _, item := range response.Node.ProjectItems.Nodes {
		if item.Project != nil && item.Project.ID == c.projectID && strings.TrimSpace(item.ID) != "" {
			c.projectCache.SetItemID(c.projectID, issueID, item.ID)
			return projectItemStatus{
				ID:         item.ID,
				StatusName: singleSelectName(item.StatusValue),
			}, nil
		}
	}
	if !response.Node.ProjectItems.PageInfo.HasNextPage {
		return projectItemStatus{}, ErrProjectItemNotFound
	}
	cursor := strings.TrimSpace(response.Node.ProjectItems.PageInfo.EndCursor)
	if cursor == "" {
		return projectItemStatus{}, ErrProjectItemNotFound
	}
	return c.fetchProjectItemPage(ctx, issueID, &cursor)
}

func (c *Connector) terminalStatusUpdateBlocked(currentStatus string, targetState string) bool {
	currentState := c.githubToDetentState(currentStatus)
	if !stateInList(currentState, c.terminalStates) {
		return false
	}
	return !stateInList(targetState, c.terminalStates)
}

func (c *Connector) updateStatusFieldValue(ctx context.Context, itemID string, fieldID string, optionID string) error {
	return c.updateProjectV2SingleSelectFieldValue(ctx, itemID, fieldID, optionID, ErrStatusUpdateFailed)
}

func (c *Connector) updateProjectV2SingleSelectFieldValue(
	ctx context.Context,
	itemID string,
	fieldID string,
	optionID string,
	emptyResponseError error,
) error {
	var response struct {
		UpdateProjectV2ItemFieldValue *struct {
			ProjectV2Item *struct {
				ID string `json:"id"`
			} `json:"projectV2Item"`
		} `json:"updateProjectV2ItemFieldValue"`
	}
	if err := c.client.GraphQL(ctx, updateSingleSelectFieldValueMutation, map[string]any{
		"projectId": c.projectID,
		"itemId":    itemID,
		"fieldId":   fieldID,
		"optionId":  optionID,
	}, &response); err != nil {
		return fmt.Errorf("update github project field: %w", err)
	}
	if response.UpdateProjectV2ItemFieldValue == nil ||
		response.UpdateProjectV2ItemFieldValue.ProjectV2Item == nil ||
		strings.TrimSpace(response.UpdateProjectV2ItemFieldValue.ProjectV2Item.ID) == "" {
		return emptyResponseError
	}
	return nil
}

func (c *Connector) detentToGitHubState(stateName string) string {
	stateName = strings.TrimSpace(stateName)
	if mapped, ok := c.stateMap[stateName]; ok {
		return strings.TrimSpace(mapped)
	}
	return stateName
}

func (c *Connector) githubToDetentState(githubState string) string {
	githubState = strings.TrimSpace(githubState)
	if githubState == "" {
		return ""
	}
	for detentState, mapped := range c.stateMap {
		if mapped == githubState {
			return detentState
		}
	}
	return githubState
}

func (c *Connector) githubIssueStateToDetentState(state string) string {
	switch strings.ToUpper(strings.TrimSpace(state)) {
	case "CLOSED":
		return c.closedIssueState()
	case "OPEN":
		return "Open"
	default:
		return ""
	}
}

func (c *Connector) closedIssueState() string {
	for _, state := range c.terminalStates {
		if normalizeStateName(state) == "done" {
			return state
		}
	}
	for _, state := range c.terminalStates {
		if normalizeStateName(state) == "closed" {
			return state
		}
	}
	if len(c.terminalStates) > 0 {
		return c.terminalStates[0]
	}
	return "Closed"
}

func (c *Connector) priorityRank(name string) *int {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	rank, ok := c.priorityMap[name]
	if !ok || rank == nil {
		return nil
	}
	value := *rank
	return &value
}

func singleSelectName(value *singleSelectValue) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(value.Name)
}

func singleSelectOptionID(options []projectSingleSelectOption, name string) string {
	name = strings.TrimSpace(name)
	for _, option := range options {
		if strings.TrimSpace(option.Name) == name {
			return strings.TrimSpace(option.ID)
		}
	}
	return ""
}

func ownershipOption(name string) projectSingleSelectOption {
	return projectSingleSelectOption{
		Name:        strings.TrimSpace(name),
		Color:       "BLUE",
		Description: "Detent ownership identity.",
	}
}

func singleSelectOptionsWithRequiredAtEnd(current []projectSingleSelectOption, required []projectSingleSelectOption) []projectSingleSelectOption {
	options := normalizedSingleSelectOptions(current)
	seen := make(map[string]struct{}, len(options)+len(required))
	for _, option := range options {
		seen[option.Name] = struct{}{}
	}
	for _, option := range required {
		input := singleSelectOptionInput(option)
		if input.Name == "" {
			continue
		}
		if _, ok := seen[input.Name]; ok {
			continue
		}
		seen[input.Name] = struct{}{}
		options = append(options, input)
	}
	return options
}

func singleSelectUpdatedAt(value *singleSelectValue) *time.Time {
	if value == nil {
		return nil
	}
	return parseGitHubTime(value.UpdatedAt)
}

func buildIdentifier(repo string, number int) string {
	if number == 0 {
		return ""
	}
	if repo == "" {
		return fmt.Sprintf("#%d", number)
	}
	return fmt.Sprintf("%s#%d", repo, number)
}

func pullRequestRepoFromIdentifier(identifier string) (pullRequestRepo, bool) {
	repo, _, ok := splitIssueIdentifier(identifier)
	if !ok {
		return pullRequestRepo{}, false
	}
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return pullRequestRepo{}, false
	}
	return pullRequestRepo{Owner: strings.TrimSpace(parts[0]), Name: strings.TrimSpace(parts[1])}, true
}

func detentIssueBranchPrefix(identifier string) string {
	_, _, ok := splitIssueIdentifier(identifier)
	if !ok {
		return ""
	}

	key := branchKeyPattern.ReplaceAllString(strings.TrimSpace(identifier), "_")
	key = strings.TrimSpace(key)
	if key == "" || key == "." || key == ".." {
		return ""
	}
	return "detent/" + strings.ToLower(key)
}

func splitIssueIdentifier(identifier string) (string, int, bool) {
	identifier = strings.TrimSpace(identifier)
	index := strings.LastIndex(identifier, "#")
	if index <= 0 || index == len(identifier)-1 {
		return "", 0, false
	}
	number, err := strconv.Atoi(identifier[index+1:])
	if err != nil || number <= 0 {
		return "", 0, false
	}
	repo := strings.TrimSpace(identifier[:index])
	if repo == "" {
		return "", 0, false
	}
	return repo, number, true
}

func branchMatchesIssuePrefix(branchName string, prefix string) bool {
	branchName = strings.ToLower(strings.TrimSpace(branchName))
	prefix = strings.ToLower(strings.TrimSpace(prefix))
	if branchName == "" || prefix == "" {
		return false
	}
	if branchName == prefix {
		return true
	}
	for _, suffix := range []string{"_", "-", "/"} {
		if strings.HasPrefix(branchName, prefix+suffix) {
			return true
		}
	}
	return false
}

func actorLogin(actor *actor) string {
	if actor == nil {
		return ""
	}
	return strings.TrimSpace(actor.Login)
}

func firstAssigneeLogin(assignees nodeConnection[assignee]) string {
	logins := allAssigneeLogins(assignees)
	if len(logins) == 0 {
		return ""
	}
	return logins[0]
}

func allAssigneeLogins(assignees nodeConnection[assignee]) []string {
	logins := make([]string, 0, len(assignees.Nodes))
	for _, assignee := range assignees.Nodes {
		login := strings.TrimSpace(assignee.Login)
		if login != "" {
			logins = append(logins, login)
		}
	}
	return logins
}

func assigneeReplacement(current []assignee, targetID string) ([]string, bool) {
	targetID = strings.TrimSpace(targetID)
	removeIDs := make([]string, 0, len(current))
	alreadyAssigned := false
	for _, candidate := range current {
		id := strings.TrimSpace(candidate.ID)
		if id == "" {
			continue
		}
		if id == targetID {
			alreadyAssigned = true
			continue
		}
		removeIDs = append(removeIDs, id)
	}
	return removeIDs, alreadyAssigned
}

func projectFieldValues(values nodeConnection[projectFieldValue]) map[string]string {
	fields := make(map[string]string, len(values.Nodes))
	for _, value := range values.Nodes {
		fieldName := strings.TrimSpace(value.Field.Name)
		if fieldName == "" {
			continue
		}
		fieldValue, ok := projectFieldValueString(value)
		if !ok {
			continue
		}
		fields[fieldName] = fieldValue
	}
	return fields
}

func projectFieldValueString(value projectFieldValue) (string, bool) {
	switch value.TypeName {
	case "ProjectV2ItemFieldSingleSelectValue":
		return strings.TrimSpace(value.Name), true
	case "ProjectV2ItemFieldTextValue":
		return value.Text, true
	case "ProjectV2ItemFieldNumberValue":
		if value.Number == nil {
			return "", false
		}
		return strconv.FormatFloat(*value.Number, 'f', -1, 64), true
	default:
		return "", false
	}
}

func firstPullRequestNumber(pullRequests nodeConnection[pullRequest]) *int {
	for _, pullRequest := range pullRequests.Nodes {
		if pullRequest.Number > 0 {
			number := pullRequest.Number
			return &number
		}
	}
	return nil
}

func pullRequestCIState(pullRequest pullRequestNode) string {
	for _, commit := range pullRequest.Commits.Nodes {
		if commit.Commit.StatusCheckRollup != nil {
			return commit.Commit.StatusCheckRollup.State
		}
	}
	return ""
}

func normalizePullRequestCIStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "success", "green", "pass", "passed":
		return "pass"
	case "failure", "failed", "error", "red":
		return "fail"
	case "pending", "expected", "queued", "waiting", "in_progress", "in progress":
		return "pending"
	default:
		return ""
	}
}

func pullRequestCodexReviewState(pullRequest pullRequestNode) string {
	hasP2 := false
	for _, review := range pullRequest.LatestReviews.Nodes {
		if containsReviewSeverity(review.Body, "P1") {
			return "P1"
		}
		if containsReviewSeverity(review.Body, "P2") {
			hasP2 = true
		}
	}
	if hasP2 {
		return "P2"
	}
	return ""
}

func containsReviewSeverity(body string, severity string) bool {
	body = strings.ToUpper(body)
	severity = strings.ToUpper(strings.TrimSpace(severity))
	if body == "" || severity == "" {
		return false
	}
	return strings.Contains(body, "["+severity+"]") ||
		strings.Contains(body, severity+" BADGE") ||
		strings.Contains(body, severity+":") ||
		strings.Contains(body, severity+" ")
}

func labelNames(labels nodeConnection[label]) []string {
	names := make([]string, 0, len(labels.Nodes))
	for _, label := range labels.Nodes {
		name := strings.ToLower(strings.TrimSpace(label.Name))
		if name != "" {
			names = append(names, name)
		}
	}
	return names
}

func parseGitHubTime(value *string) *time.Time {
	if value == nil || strings.TrimSpace(*value) == "" {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(*value))
	if err != nil {
		return nil
	}
	return &parsed
}

func parseModelOverride(body string) string {
	matches := modelOverridePattern.FindStringSubmatch(body)
	if len(matches) != 2 {
		return ""
	}
	return strings.TrimSpace(matches[1])
}

func parseBlockerReason(issue githubIssueNode) string {
	for index := len(issue.Comments.Nodes) - 1; index >= 0; index-- {
		body := issue.Comments.Nodes[index].Body
		if !strings.Contains(strings.ToLower(body), "codex workpad") {
			continue
		}
		if reason := markdownSectionText(body, "Human Action Needed"); reason != "" {
			return reason
		}
	}
	return markdownSectionText(issue.Body, "Human Action Needed")
}

func markdownSectionText(body string, title string) string {
	want := normalizeSectionTitle(title)
	inSection := false
	lines := []string{}
	for _, line := range strings.Split(body, "\n") {
		heading, ok := markdownHeadingTitle(line)
		if ok {
			if inSection {
				break
			}
			inSection = normalizeSectionTitle(heading) == want
			continue
		}
		if inSection {
			lines = append(lines, line)
		}
	}
	return normalizeSectionLines(lines)
}

func markdownHeadingTitle(line string) (string, bool) {
	line = strings.TrimSpace(line)
	if line == "" || line[0] != '#' {
		return "", false
	}
	index := 0
	for index < len(line) && line[index] == '#' {
		index++
	}
	if index > 6 || index == len(line) {
		return "", false
	}
	if line[index] != ' ' && line[index] != '\t' {
		return "", false
	}
	return strings.Trim(strings.TrimSpace(line[index:]), "# \t"), true
}

func normalizeSectionTitle(title string) string {
	return strings.ToLower(strings.Join(strings.Fields(title), " "))
}

func normalizeSectionLines(lines []string) string {
	parts := make([]string, 0, len(lines))
	for _, line := range lines {
		line = normalizeSectionLine(line)
		if line != "" {
			parts = append(parts, line)
		}
	}
	return strings.Join(parts, "; ")
}

func normalizeSectionLine(line string) string {
	line = strings.TrimSpace(line)
	for _, marker := range []string{"- ", "* ", "+ "} {
		if strings.HasPrefix(line, marker) {
			line = strings.TrimSpace(strings.TrimPrefix(line, marker))
			break
		}
	}
	line = numberedListPattern.ReplaceAllString(line, "")
	return strings.Join(strings.Fields(line), " ")
}

func parseBlockedBy(body string, repo string) []connector.BlockedRef {
	repo = strings.TrimSpace(repo)
	seen := map[string]struct{}{}
	blockers := []connector.BlockedRef{}

	for _, line := range strings.FieldsFunc(body, func(r rune) bool {
		return r == '\n' || r == '\r'
	}) {
		lineMatches := dependencyLinePattern.FindStringSubmatch(line)
		if len(lineMatches) != 2 {
			continue
		}
		for _, refMatches := range issueRefPattern.FindAllStringSubmatch(lineMatches[1], -1) {
			if len(refMatches) != 3 {
				continue
			}
			identifier := blockerIdentifier(refMatches[1], refMatches[2], repo)
			if identifier == "" {
				continue
			}
			if _, ok := seen[identifier]; ok {
				continue
			}
			seen[identifier] = struct{}{}
			blockers = append(blockers, connector.BlockedRef{Identifier: identifier})
		}
	}
	return blockers
}

func blockerIdentifier(refRepo string, number string, repo string) string {
	if strings.TrimSpace(number) == "" {
		return ""
	}
	refRepo = strings.TrimSpace(refRepo)
	if refRepo == "" {
		if repo == "" {
			return "#" + number
		}
		refRepo = repo
	}
	return refRepo + "#" + number
}

func uniqueNonBlank(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func sortIssuesByRequestedIDs(issues []connector.Issue, ids []string) {
	order := make(map[string]int, len(ids))
	for index, id := range ids {
		order[id] = index
	}
	fallback := len(order)
	sort.SliceStable(issues, func(i, j int) bool {
		return orderForIssue(issues[i], order, fallback) < orderForIssue(issues[j], order, fallback)
	})
}

func orderForIssue(issue connector.Issue, order map[string]int, fallback int) int {
	if index, ok := order[issue.ID]; ok {
		return index
	}
	return fallback
}

func normalizedStateSet(states []string) map[string]struct{} {
	out := make(map[string]struct{}, len(states))
	for _, state := range states {
		state = normalizeStateName(state)
		if state != "" {
			out[state] = struct{}{}
		}
	}
	return out
}

func stateInList(state string, states []string) bool {
	normalized := normalizeStateName(state)
	if normalized == "" {
		return false
	}
	for _, candidate := range states {
		if normalized == normalizeStateName(candidate) {
			return true
		}
	}
	return false
}

func attachPullRequestsForStates(states map[string]struct{}) bool {
	for _, state := range []string{"Human Review", "Merging", "Done"} {
		if _, ok := states[normalizeStateName(state)]; ok {
			return true
		}
	}
	return false
}

func normalizeStateList(states []string, defaults []string) []string {
	if len(states) == 0 {
		states = defaults
	}
	out := make([]string, 0, len(states))
	seen := map[string]struct{}{}
	for _, state := range states {
		state = strings.TrimSpace(state)
		if state == "" {
			continue
		}
		key := normalizeStateName(state)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, state)
	}
	return out
}

func normalizeStateName(state string) string {
	return strings.ToLower(strings.TrimSpace(state))
}

func cloneStateMap(values map[string]string) map[string]string {
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key != "" && value != "" {
			cloned[key] = value
		}
	}
	return cloned
}

func clonePriorityMapWithDefault(values map[string]*int) map[string]*int {
	if values == nil {
		values = defaultPriorityMap()
	}
	cloned := make(map[string]*int, len(values))
	for key, value := range values {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if value == nil {
			cloned[key] = nil
			continue
		}
		rank := *value
		cloned[key] = &rank
	}
	return cloned
}

func defaultPriorityMap() map[string]*int {
	return map[string]*int{
		"Urgent":      intValue(1),
		"High":        intValue(2),
		"Medium":      intValue(3),
		"Low":         intValue(4),
		"No priority": nil,
	}
}

func intValue(value int) *int {
	return &value
}
