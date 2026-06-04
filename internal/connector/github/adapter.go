package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

const (
	projectItemsPageSize                     = 50
	projectItemsPerIssue                     = 100
	pullRequestsPageSize                     = 100
	pullRequestsPageLimit                    = 3
	defaultProjectItemStatusState            = "Backlog"
	defaultProjectItemStatusWriteParallelism = 4
	defaultProjectItemStatusWriteTimeout     = 2 * time.Minute
)

const projectItemsQuery = `
query DetentGitHubProjectItems(
  $projectId: ID!
  $first: Int!
  $after: String
  $query: String
) {
  node(id: $projectId) {
    ... on ProjectV2 {
      items(first: $first, after: $after, query: $query) {
        pageInfo { hasNextPage endCursor }
        nodes {
          id
          content {
            __typename
            ... on Issue {
              id
              number
              title
              state
              url
              repository { nameWithOwner }
            }
          }
          statusValue: fieldValueByName(name: "Status") {
            ... on ProjectV2ItemFieldSingleSelectValue { name updatedAt }
          }
          priorityValue: fieldValueByName(name: "Priority") {
            ... on ProjectV2ItemFieldSingleSelectValue { name }
          }
        }
      }
    }
  }
  rateLimit { limit used remaining cost resetAt }
}`

const issueIdentitiesByIDQuery = `
query DetentGitHubIssueIdentitiesByID($issueIds: [ID!]!) {
  nodes(ids: $issueIds) {
    __typename
    ... on Issue {
      id
      number
      repository { nameWithOwner }
    }
  }
  rateLimit { limit used remaining cost resetAt }
}`

const issueSubIssuesQuery = `
query DetentGitHubIssueSubIssues($issueId: ID!, $after: String) {
  node(id: $issueId) {
    ... on Issue {
      subIssues(first: 100, after: $after) {
        pageInfo { hasNextPage endCursor }
        nodes {
          id
          number
          title
          state
          url
          repository { nameWithOwner }
          projectItems(first: 100) {
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
    }
  }
  rateLimit { limit used remaining cost resetAt }
}`

const issueTrackedIssuesQuery = `
query DetentGitHubIssueTrackedIssues($issueId: ID!, $after: String) {
  node(id: $issueId) {
    ... on Issue {
      trackedIssues(first: 100, after: $after) {
        pageInfo { hasNextPage endCursor }
        nodes {
          id
          number
          title
          state
          url
          repository { nameWithOwner }
          projectItems(first: 100) {
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
    }
  }
  rateLimit { limit used remaining cost resetAt }
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
query DetentGitHubProjectField($projectId: ID!, $fieldName: String!) {
  node(id: $projectId) {
    __typename
    ... on ProjectV2 {
      field(name: $fieldName) {
        __typename
        ... on ProjectV2Field {
          id
          dataType
        }
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

const updateTextFieldValueMutation = `
mutation DetentGitHubUpdateTextField($projectId: ID!, $itemId: ID!, $fieldId: ID!, $text: String!) {
  updateProjectV2ItemFieldValue(input: {
    projectId: $projectId,
    itemId: $itemId,
    fieldId: $fieldId,
    value: { text: $text }
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
	SubIssues                      linkedIssuesConnection       `json:"subIssues"`
	TrackedIssues                  linkedIssuesConnection       `json:"trackedIssues"`
}

type linkedIssuesConnection struct {
	PageInfo pageInfo      `json:"pageInfo"`
	Nodes    []linkedIssue `json:"nodes"`
}

type linkedIssue struct {
	ID           string                  `json:"id"`
	Number       int                     `json:"number"`
	Title        string                  `json:"title"`
	State        string                  `json:"state"`
	URL          string                  `json:"url"`
	Repository   repository              `json:"repository"`
	ProjectItems *projectItemsConnection `json:"projectItems"`
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
	HeadSHA       string                            `json:"headSHA"`
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

type restIssue struct {
	NodeID    string         `json:"node_id"`
	Number    int            `json:"number"`
	Title     string         `json:"title"`
	Body      *string        `json:"body"`
	State     string         `json:"state"`
	HTMLURL   string         `json:"html_url"`
	CreatedAt *time.Time     `json:"created_at"`
	UpdatedAt *time.Time     `json:"updated_at"`
	User      *actor         `json:"user"`
	Assignees []restAssignee `json:"assignees"`
	Labels    []label        `json:"labels"`
}

type restAssignee struct {
	NodeID string `json:"node_id"`
	Login  string `json:"login"`
}

type restPullRequest struct {
	Number   int      `json:"number"`
	HTMLURL  string   `json:"html_url"`
	State    string   `json:"state"`
	Head     restHead `json:"head"`
	MergedAt *string  `json:"merged_at"`
}

type restHead struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

type restReview struct {
	Body  string `json:"body"`
	State string `json:"state"`
	User  *actor `json:"user"`
}

type restCheckRuns struct {
	CheckRuns []restCheckRun `json:"check_runs"`
}

type restCheckRun struct {
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

type restComment struct {
	Body string `json:"body"`
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

	issues, err := c.fetchProjectItems(ctx, c.projectStatusQuery(c.activeStates), func(issue connector.Issue) bool {
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

	issues, err := c.fetchProjectItems(ctx, c.projectStatusQuery(stateNames), func(issue connector.Issue) bool {
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

	refs, err := c.issueRefsForIDs(ctx, ids)
	if err != nil {
		return nil, err
	}

	issues := make([]connector.Issue, 0, len(ids))
	for _, id := range ids {
		ref, ok := refs[id]
		if !ok {
			continue
		}
		issue, ok, err := c.fetchIssueByRef(ctx, ref)
		if err != nil {
			return nil, fmt.Errorf("fetch github issue states by ids: %w", err)
		}
		if ok {
			issues = append(issues, issue)
		}
	}
	sortIssuesByRequestedIDs(issues, ids)
	return issues, nil
}

func (c *Connector) FetchIssueStatesByIdentifiers(ctx context.Context, identifiers []string) ([]connector.Issue, error) {
	identifiers = uniqueNonBlank(identifiers)
	if len(identifiers) == 0 {
		return []connector.Issue{}, nil
	}

	issues := make([]connector.Issue, 0, len(identifiers))
	for _, identifier := range identifiers {
		issue, ok, err := c.fetchIssueByIdentifier(ctx, identifier)
		if err != nil {
			return nil, err
		}
		if ok {
			issues = append(issues, issue)
		}
	}
	sortIssuesByRequestedIdentifiers(issues, identifiers)
	return issues, nil
}

func (c *Connector) FetchIssueChildren(ctx context.Context, issueID string) ([]connector.BlockedRef, error) {
	issueID = strings.TrimSpace(issueID)
	if issueID == "" {
		return []connector.BlockedRef{}, nil
	}

	seen := map[string]struct{}{}
	children := []connector.BlockedRef{}
	subIssues, err := c.fetchLinkedIssueRefs(ctx, issueID, issueSubIssuesQuery, "subIssues")
	if err != nil {
		return nil, err
	}
	children = appendUniqueLinkedIssueRefs(children, seen, subIssues)
	trackedIssues, err := c.fetchLinkedIssueRefs(ctx, issueID, issueTrackedIssuesQuery, "trackedIssues")
	if err != nil {
		return nil, err
	}
	children = appendUniqueLinkedIssueRefs(children, seen, trackedIssues)
	return children, nil
}

func (c *Connector) fetchLinkedIssueRefs(ctx context.Context, issueID string, query string, connectionName string) ([]connector.BlockedRef, error) {
	var after *string
	seen := map[string]struct{}{}
	refs := []connector.BlockedRef{}
	for {
		connection, err := c.fetchLinkedIssuePage(ctx, issueID, query, connectionName, after)
		if err != nil {
			return nil, err
		}
		pageRefs := c.appendLinkedChildIssues(nil, map[string]struct{}{}, connection.Nodes, "")
		refs = appendUniqueLinkedIssueRefs(refs, seen, pageRefs)
		if !connection.PageInfo.HasNextPage {
			return refs, nil
		}
		cursor := strings.TrimSpace(connection.PageInfo.EndCursor)
		if cursor == "" {
			return nil, ErrInvalidResponse
		}
		after = &cursor
	}
}

func appendUniqueLinkedIssueRefs(
	refs []connector.BlockedRef,
	seen map[string]struct{},
	incoming []connector.BlockedRef,
) []connector.BlockedRef {
	for _, ref := range incoming {
		key := normalizedIssueIdentifier(ref.Identifier)
		if key == "" {
			key = "id:" + strings.TrimSpace(ref.ID)
		}
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		refs = append(refs, ref)
	}
	return refs
}

func (c *Connector) fetchLinkedIssuePage(
	ctx context.Context,
	issueID string,
	query string,
	connectionName string,
	after *string,
) (linkedIssuesConnection, error) {
	var response struct {
		Node *struct {
			SubIssues     linkedIssuesConnection `json:"subIssues"`
			TrackedIssues linkedIssuesConnection `json:"trackedIssues"`
		} `json:"node"`
	}
	if err := c.client.GraphQL(ctx, query, map[string]any{
		"issueId": issueID,
		"after":   after,
	}, &response); err != nil {
		return linkedIssuesConnection{}, fmt.Errorf("fetch github issue children: %w", err)
	}
	if response.Node == nil {
		return linkedIssuesConnection{}, ErrInvalidResponse
	}
	switch connectionName {
	case "subIssues":
		return response.Node.SubIssues, nil
	case "trackedIssues":
		return response.Node.TrackedIssues, nil
	default:
		return linkedIssuesConnection{}, ErrInvalidResponse
	}
}

func (c *Connector) fetchIssueByIdentifier(ctx context.Context, identifier string) (connector.Issue, bool, error) {
	ref, ok := issueRefFromIdentifier(identifier)
	if !ok {
		return connector.Issue{}, false, nil
	}
	return c.fetchIssueByRef(ctx, ref)
}

func (c *Connector) issueRefsForIDs(ctx context.Context, ids []string) (map[string]issueRef, error) {
	refs := make(map[string]issueRef, len(ids))
	missing := make([]string, 0, len(ids))
	for _, id := range ids {
		if ref, ok := c.projectCache.GetIssueRef(id); ok {
			refs[id] = ref
			continue
		}
		missing = append(missing, id)
	}
	if len(missing) == 0 {
		return refs, nil
	}

	fetched, err := c.fetchIssueRefsByID(ctx, missing)
	if err != nil {
		return nil, err
	}
	for id, ref := range fetched {
		refs[id] = ref
	}
	return refs, nil
}

func (c *Connector) issueRefForID(ctx context.Context, issueID string) (issueRef, bool, error) {
	issueID = strings.TrimSpace(issueID)
	if issueID == "" {
		return issueRef{}, false, nil
	}
	if ref, ok := c.projectCache.GetIssueRef(issueID); ok {
		return ref, true, nil
	}
	refs, err := c.fetchIssueRefsByID(ctx, []string{issueID})
	if err != nil {
		return issueRef{}, false, err
	}
	ref, ok := refs[issueID]
	return ref, ok, nil
}

func (c *Connector) fetchIssueRefsByID(ctx context.Context, ids []string) (map[string]issueRef, error) {
	var response struct {
		Nodes []githubIssueNode `json:"nodes"`
	}
	if err := c.client.GraphQL(ctx, issueIdentitiesByIDQuery, map[string]any{"issueIds": ids}, &response); err != nil {
		return nil, fmt.Errorf("fetch github issue identities by ids: %w", err)
	}

	refs := make(map[string]issueRef, len(response.Nodes))
	for _, node := range response.Nodes {
		if node.TypeName != "Issue" {
			continue
		}
		ref, ok := issueRefFromNode(node)
		if !ok {
			continue
		}
		refs[node.ID] = ref
		c.projectCache.SetIssueRef(node.ID, ref)
	}
	return refs, nil
}

func (c *Connector) fetchIssueByRef(ctx context.Context, ref issueRef) (connector.Issue, bool, error) {
	issue, err := c.fetchRESTIssue(ctx, ref)
	if err != nil {
		return connector.Issue{}, false, err
	}
	if strings.TrimSpace(issue.ID) == "" {
		return connector.Issue{}, false, nil
	}
	c.cacheIssueRef(issue)

	stateName, priorityName, statusUpdatedAt, fields, ok, err := c.fetchProjectFieldsPage(ctx, issue.ID, nil)
	if err != nil {
		return connector.Issue{}, false, err
	}
	if ok {
		return c.buildIssue(issue, stateName, priorityName, statusUpdatedAt, fields), true, nil
	}
	return c.buildIssue(issue, c.githubIssueStateToDetentState(issue.State), "", nil, nil), true, nil
}

func (c *Connector) fetchRESTIssue(ctx context.Context, ref issueRef) (githubIssueNode, error) {
	var response restIssue
	if err := c.client.REST(ctx, http.MethodGet, restIssuePath(ref), nil, &response); err != nil {
		if errors.Is(err, ErrNotFound) {
			return githubIssueNode{}, nil
		}
		return githubIssueNode{}, fmt.Errorf("fetch github issue: %w", err)
	}
	return githubIssueNodeFromREST(ref, response), nil
}

func (c *Connector) populateBlockerReasons(ctx context.Context, issues []connector.Issue) error {
	for index := range issues {
		if normalizeStateName(issues[index].State) != normalizeStateName("Blocked") {
			continue
		}
		ref, ok := issueRefFromIdentifier(issues[index].Identifier)
		if !ok {
			continue
		}
		comments, err := c.fetchIssueComments(ctx, ref)
		if err != nil {
			return fmt.Errorf("fetch github issue comments: %w", err)
		}
		node := githubIssueNode{
			ID:       issues[index].ID,
			Body:     issues[index].Description,
			Comments: nodeConnection[issueComment]{Nodes: comments},
		}
		if reason := parseBlockerReason(node); reason != "" {
			issues[index].BlockerReason = reason
		}
	}
	return nil
}

func (c *Connector) fetchIssueComments(ctx context.Context, ref issueRef) ([]issueComment, error) {
	var response []restComment
	if err := c.client.REST(ctx, http.MethodGet, restIssueCommentsListPath(ref), nil, &response); err != nil {
		return nil, err
	}
	comments := make([]issueComment, 0, len(response))
	for _, comment := range response {
		comments = append(comments, issueComment(comment))
	}
	return comments, nil
}

func (c *Connector) CreateComment(ctx context.Context, issueID string, body string) error {
	ref, ok, err := c.issueRefForID(ctx, issueID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrCommentCreateFailed
	}

	var response struct {
		NodeID string `json:"node_id"`
	}
	if err := c.client.REST(ctx, http.MethodPost, restIssueCommentsPath(ref), map[string]any{"body": body}, &response); err != nil {
		return fmt.Errorf("create github comment: %w", err)
	}
	if strings.TrimSpace(response.NodeID) == "" {
		return ErrCommentCreateFailed
	}

	return nil
}

func (c *Connector) CloseIssue(ctx context.Context, issueID string) error {
	ref, ok, err := c.issueRefForID(ctx, issueID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrIssueCloseFailed
	}

	var response restIssue
	if err := c.client.REST(ctx, http.MethodPatch, restIssuePath(ref), map[string]any{
		"state":        "closed",
		"state_reason": "completed",
	}, &response); err != nil {
		return fmt.Errorf("close github issue: %w", err)
	}
	if strings.TrimSpace(response.NodeID) == "" || !strings.EqualFold(response.State, "closed") {
		return ErrIssueCloseFailed
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
	return c.setProjectItemStatus(ctx, item.ID, githubState)
}

func (c *Connector) setProjectItemStatus(ctx context.Context, itemID string, githubState string) error {
	fieldID, optionID, err := c.resolveStatusOption(ctx, githubState)
	if err != nil {
		if errors.Is(err, ErrStatusOptionNotFound) {
			c.statusCache.Clear(c.projectID)
		}
		return err
	}

	if err := c.updateStatusFieldValue(ctx, itemID, fieldID, optionID); err == nil {
		return nil
	}

	c.statusCache.Clear(c.projectID)
	fieldID, optionID, err = c.resolveStatusOption(ctx, githubState)
	if err != nil {
		return err
	}
	return c.updateStatusFieldValue(ctx, itemID, fieldID, optionID)
}

func (c *Connector) SetAssignee(ctx context.Context, issueID string, login string) error {
	issueID = strings.TrimSpace(issueID)
	login = strings.TrimSpace(login)
	if issueID == "" || login == "" {
		return ErrAssigneeNotFound
	}
	ref, ok, err := c.issueRefForID(ctx, issueID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrAssigneeNotFound
	}
	currentAssignees, err := c.fetchIssueAssignees(ctx, ref)
	if err != nil {
		return err
	}
	removeLogins, alreadyAssigned := assigneeLoginReplacement(currentAssignees, login)
	if !alreadyAssigned {
		if err := c.addAssignee(ctx, ref, login); err != nil {
			return err
		}
	}
	if len(removeLogins) > 0 {
		if err := c.removeAssignees(ctx, ref, removeLogins); err != nil {
			return err
		}
	}
	return nil
}

func (c *Connector) SetField(ctx context.Context, issueID string, fieldName string, value string) error {
	if c.projectID == "" {
		return ErrMissingProject
	}

	item, err := c.resolveProjectItem(ctx, strings.TrimSpace(issueID))
	if err != nil {
		return err
	}
	field, err := c.fetchProjectField(ctx, fieldName)
	if err != nil {
		return err
	}
	if projectTextField(field) {
		return c.updateProjectV2TextFieldValue(ctx, item.ID, field.ID, strings.TrimSpace(value), ErrProjectFieldUpdateFailed)
	}
	fieldID, optionID, err := c.resolveSingleSelectFieldOptionFromField(ctx, fieldName, value, field)
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
		if err := c.attachMatchingPullRequests(ctx, repo, issues, byRepo[repo], pullRequests); err != nil {
			return err
		}
	}
	return nil
}

func (c *Connector) fetchRepositoryPullRequests(ctx context.Context, repo pullRequestRepo) ([]pullRequestNode, error) {
	pullRequests := []pullRequestNode{}
	for page := 1; page <= pullRequestsPageLimit; page++ {
		pagePullRequests, err := c.fetchRepositoryPullRequestsPage(ctx, repo, page)
		if err != nil {
			return nil, err
		}
		pullRequests = append(pullRequests, pagePullRequests...)
		if len(pagePullRequests) < pullRequestsPageSize {
			break
		}
	}
	return pullRequests, nil
}

func (c *Connector) fetchRepositoryPullRequestsPage(
	ctx context.Context,
	repo pullRequestRepo,
	page int,
) ([]pullRequestNode, error) {
	var response []restPullRequest
	if err := c.client.REST(ctx, http.MethodGet, restPullRequestsPath(repo, page), nil, &response); err != nil {
		return nil, fmt.Errorf("fetch github pull requests: %w", err)
	}
	pullRequests := make([]pullRequestNode, 0, len(response))
	for _, pullRequest := range response {
		pullRequests = append(pullRequests, pullRequestNode{
			Number:      pullRequest.Number,
			URL:         pullRequest.HTMLURL,
			State:       restPullRequestState(pullRequest),
			HeadRefName: pullRequest.Head.Ref,
			HeadSHA:     pullRequest.Head.SHA,
		})
	}
	return pullRequests, nil
}

func (c *Connector) attachMatchingPullRequests(
	ctx context.Context,
	repo pullRequestRepo,
	issues []connector.Issue,
	candidates []issuePullRequestCandidate,
	pullRequests []pullRequestNode,
) error {
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

			if err := c.populatePullRequestStatus(ctx, repo, &pullRequest); err != nil {
				return err
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
	return nil
}

func (c *Connector) populatePullRequestStatus(ctx context.Context, repo pullRequestRepo, pullRequest *pullRequestNode) error {
	if strings.TrimSpace(pullRequest.HeadSHA) != "" {
		ciState, err := c.fetchPullRequestCIState(ctx, repo, pullRequest.HeadSHA)
		if err != nil {
			return err
		}
		pullRequest.Commits = nodeConnection[pullRequestCommit]{Nodes: []pullRequestCommit{{
			Commit: commitNode{StatusCheckRollup: &statusCheckRollup{State: ciState}},
		}}}
	}
	reviews, err := c.fetchPullRequestReviews(ctx, repo, pullRequest.Number)
	if err != nil {
		return err
	}
	pullRequest.LatestReviews = nodeConnection[pullRequestReview]{Nodes: reviews}
	return nil
}

func (c *Connector) fetchPullRequestCIState(ctx context.Context, repo pullRequestRepo, sha string) (string, error) {
	var response restCheckRuns
	if err := c.client.REST(ctx, http.MethodGet, restCommitCheckRunsPath(repo, sha), nil, &response); err != nil {
		return "", fmt.Errorf("fetch github check runs: %w", err)
	}
	return checkRunsState(response.CheckRuns), nil
}

func (c *Connector) fetchPullRequestReviews(ctx context.Context, repo pullRequestRepo, number int) ([]pullRequestReview, error) {
	var response []restReview
	if err := c.client.REST(ctx, http.MethodGet, restPullRequestReviewsPath(repo, number), nil, &response); err != nil {
		return nil, fmt.Errorf("fetch github pull request reviews: %w", err)
	}
	reviews := make([]pullRequestReview, 0, len(response))
	for _, review := range response {
		reviews = append(reviews, pullRequestReview{
			Body:   review.Body,
			State:  review.State,
			Author: review.User,
		})
	}
	return reviews, nil
}

func (c *Connector) fetchProjectItems(ctx context.Context, query string, keepIssue func(connector.Issue) bool) ([]connector.Issue, error) {
	var after *string
	allIssues := []connector.Issue{}
	blankStatusItemIDs := []string{}
	var projectQuery *string
	if query != "" {
		projectQuery = &query
	}

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
			"query":     projectQuery,
		}, &response); err != nil {
			return nil, fmt.Errorf("fetch github project items: %w", err)
		}
		if response.Node == nil {
			return nil, ErrProjectNotFound
		}

		for _, item := range response.Node.Items.Nodes {
			issue, ok, blankStatusItemID, err := c.normalizeProjectItem(item)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			if blankStatusItemID != "" {
				blankStatusItemIDs = append(blankStatusItemIDs, blankStatusItemID)
			}
			allIssues = append(allIssues, issue)
		}

		if !response.Node.Items.PageInfo.HasNextPage {
			c.defaultBlankProjectItemStatuses(ctx, blankStatusItemIDs)
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

func (c *Connector) normalizeProjectItem(item projectItemNode) (connector.Issue, bool, string, error) {
	if item.Content == nil || item.Content.TypeName != "Issue" {
		return connector.Issue{}, false, "", nil
	}
	c.cacheIssueRef(*item.Content)
	statusName, statusUpdatedAt, blankStatusItemID, err := c.projectItemStatusOrDefault(item)
	if err != nil {
		return connector.Issue{}, false, "", err
	}
	return c.buildIssue(
		*item.Content,
		statusName,
		singleSelectName(item.PriorityValue),
		statusUpdatedAt,
		projectFieldValues(item.FieldValues),
	), true, blankStatusItemID, nil
}

func (c *Connector) projectItemStatusOrDefault(item projectItemNode) (string, *time.Time, string, error) {
	statusName := singleSelectName(item.StatusValue)
	if statusName != "" {
		return statusName, singleSelectUpdatedAt(item.StatusValue), "", nil
	}

	itemID := strings.TrimSpace(item.ID)
	if itemID == "" {
		return "", nil, "", ErrInvalidResponse
	}

	statusName = c.detentToGitHubState(defaultProjectItemStatusState)
	if statusName == "" {
		return "", nil, "", nil
	}
	return statusName, nil, itemID, nil
}

func (c *Connector) defaultBlankProjectItemStatuses(ctx context.Context, itemIDs []string) {
	itemIDs = uniqueNonBlank(itemIDs)
	if len(itemIDs) == 0 {
		return
	}

	statusName := c.detentToGitHubState(defaultProjectItemStatusState)
	if statusName == "" {
		return
	}

	go c.defaultBlankProjectItemStatusesAsync(ctx, itemIDs, statusName)
}

func (c *Connector) defaultBlankProjectItemStatusesAsync(parentCtx context.Context, itemIDs []string, statusName string) {
	baseCtx := context.Background()
	if parentCtx != nil {
		baseCtx = context.WithoutCancel(parentCtx)
	}
	ctx, cancel := context.WithTimeout(baseCtx, defaultProjectItemStatusWriteTimeout)
	defer cancel()

	if err := c.writeDefaultProjectItemStatuses(ctx, itemIDs, statusName); err != nil {
		c.client.logger.WarnContext(ctx, "default github project item statuses failed", "count", len(itemIDs), "error", err)
	}
}

func (c *Connector) writeDefaultProjectItemStatuses(ctx context.Context, itemIDs []string, statusName string) error {
	itemIDs = uniqueNonBlank(itemIDs)
	if len(itemIDs) == 0 {
		return nil
	}

	workerCount := min(defaultProjectItemStatusWriteParallelism, len(itemIDs))
	jobs := make(chan string)
	errs := make(chan error, len(itemIDs))
	var wg sync.WaitGroup
	wg.Add(workerCount)
	for range workerCount {
		go func() {
			defer wg.Done()
			for itemID := range jobs {
				if err := c.setProjectItemStatus(ctx, itemID, statusName); err != nil {
					errs <- fmt.Errorf("%s: %w", itemID, err)
				}
			}
		}()
	}

	for _, itemID := range itemIDs {
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			close(errs)
			return errors.Join(ctx.Err(), joinErrors(errs))
		case jobs <- itemID:
		}
	}

	close(jobs)
	wg.Wait()
	close(errs)
	return joinErrors(errs)
}

func joinErrors(errs <-chan error) error {
	var joined error
	for err := range errs {
		joined = errors.Join(joined, err)
	}
	return joined
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
		Closed:           githubIssueClosed(issue.State),
		PRNumber:         firstPullRequestNumber(issue.ClosedByPullRequestsReferences),
		AuthorID:         actorLogin(issue.Author),
		AssigneeID:       firstAssigneeLogin(issue.Assignees),
		Assignees:        allAssigneeLogins(issue.Assignees),
		BlockedBy:        parseBlockedBy(issue.Body, repo),
		ChildIssues:      c.linkedChildIssues(issue, repo),
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

func (c *Connector) linkedChildIssues(issue githubIssueNode, fallbackRepo string) []connector.BlockedRef {
	seen := map[string]struct{}{}
	children := []connector.BlockedRef{}
	children = c.appendLinkedChildIssues(children, seen, issue.SubIssues.Nodes, fallbackRepo)
	children = c.appendLinkedChildIssues(children, seen, issue.TrackedIssues.Nodes, fallbackRepo)
	if len(children) == 0 {
		return nil
	}
	return children
}

func (c *Connector) appendLinkedChildIssues(
	children []connector.BlockedRef,
	seen map[string]struct{},
	linked []linkedIssue,
	fallbackRepo string,
) []connector.BlockedRef {
	for _, child := range linked {
		identifier := buildIdentifier(strings.TrimSpace(child.Repository.NameWithOwner), child.Number)
		if identifier == "" {
			identifier = buildIdentifier(strings.TrimSpace(fallbackRepo), child.Number)
		}
		if identifier == "" {
			continue
		}
		key := normalizedIssueIdentifier(identifier)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		children = append(children, connector.BlockedRef{
			ID:         strings.TrimSpace(child.ID),
			Identifier: identifier,
			State:      c.linkedChildIssueState(child),
		})
	}
	return children
}

func (c *Connector) linkedChildIssueState(child linkedIssue) string {
	state := c.githubIssueStateToDetentState(child.State)
	if stateName, _, _, _, ok := c.projectFields(child.ID, child.ProjectItems); ok {
		if stateName = strings.TrimSpace(stateName); stateName != "" {
			state = c.githubToDetentState(stateName)
		}
	}
	return state
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

func (c *Connector) addAssignee(ctx context.Context, ref issueRef, login string) error {
	login = strings.TrimSpace(login)
	if login == "" {
		return ErrAssigneeNotFound
	}

	var response restIssue
	if err := c.client.REST(ctx, http.MethodPost, restIssueAssigneesPath(ref), map[string]any{
		"assignees": []string{login},
	}, &response); err != nil {
		return fmt.Errorf("set github assignee: %w", err)
	}
	if strings.TrimSpace(response.NodeID) == "" {
		return ErrAssigneeUpdateFailed
	}
	return nil
}

func (c *Connector) fetchIssueAssignees(ctx context.Context, ref issueRef) ([]assignee, error) {
	issue, err := c.fetchRESTIssue(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("fetch github issue assignees: %w", err)
	}
	if strings.TrimSpace(issue.ID) == "" {
		return nil, ErrAssigneeNotFound
	}
	return issue.Assignees.Nodes, nil
}

func (c *Connector) removeAssignees(ctx context.Context, ref issueRef, logins []string) error {
	logins = uniqueNonBlank(logins)
	if len(logins) == 0 {
		return ErrAssigneeUpdateFailed
	}

	var response restIssue
	if err := c.client.REST(ctx, http.MethodDelete, restIssueAssigneesPath(ref), map[string]any{
		"assignees": logins,
	}, &response); err != nil {
		return fmt.Errorf("replace github assignee: %w", err)
	}
	if strings.TrimSpace(response.NodeID) == "" {
		return ErrAssigneeUpdateFailed
	}
	return nil
}

func (c *Connector) resolveSingleSelectFieldOptionFromField(
	ctx context.Context,
	fieldName string,
	value string,
	field projectOptionsFieldResponse,
) (string, string, error) {
	fieldName = strings.TrimSpace(fieldName)
	value = strings.TrimSpace(value)
	if fieldName == "" || value == "" {
		return "", "", ErrProjectFieldOptionNotFound
	}

	decoded, err := decodeProjectSingleSelectField(fieldName, &field)
	if err != nil {
		return "", "", err
	}
	if optionID := singleSelectOptionID(decoded.Options, value); optionID != "" {
		return decoded.ID, optionID, nil
	}

	for range 3 {
		refetched, err := c.fetchProjectField(ctx, fieldName)
		if err != nil {
			return "", "", err
		}
		decoded, err = decodeProjectSingleSelectField(fieldName, &refetched)
		if err != nil {
			return "", "", err
		}
		if optionID := singleSelectOptionID(decoded.Options, value); optionID != "" {
			return decoded.ID, optionID, nil
		}
		options := singleSelectOptionsWithRequiredAtEnd(decoded.Options, []projectSingleSelectOption{ownershipOption(value)})
		updatedOptions, err := c.updateProjectFieldOptions(ctx, decoded.ID, options)
		if err != nil {
			return "", "", fmt.Errorf("ensure github project field options: %w", err)
		}
		if optionID := singleSelectOptionID(updatedOptions, value); optionID != "" {
			return decoded.ID, optionID, nil
		}
	}
	return "", "", fmt.Errorf("%w: %s=%s", ErrProjectFieldOptionNotFound, fieldName, value)
}

func (c *Connector) fetchProjectField(ctx context.Context, fieldName string) (projectOptionsFieldResponse, error) {
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
		return projectOptionsFieldResponse{}, fmt.Errorf("fetch github project field: %w", err)
	}
	if response.Node == nil || response.Node.TypeName != "ProjectV2" {
		return projectOptionsFieldResponse{}, ErrProjectNotFound
	}
	if response.Node.Field == nil {
		return projectOptionsFieldResponse{}, fmt.Errorf("%w: %s", ErrProjectFieldNotFound, strings.TrimSpace(fieldName))
	}
	return *response.Node.Field, nil
}

func projectTextField(field projectOptionsFieldResponse) bool {
	return field.TypeName == "ProjectV2Field" &&
		strings.EqualFold(strings.TrimSpace(field.DataType), "TEXT") &&
		strings.TrimSpace(field.ID) != ""
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

func (c *Connector) updateProjectV2TextFieldValue(
	ctx context.Context,
	itemID string,
	fieldID string,
	text string,
	emptyResponseError error,
) error {
	var response struct {
		UpdateProjectV2ItemFieldValue *struct {
			ProjectV2Item *struct {
				ID string `json:"id"`
			} `json:"projectV2Item"`
		} `json:"updateProjectV2ItemFieldValue"`
	}
	if err := c.client.GraphQL(ctx, updateTextFieldValueMutation, map[string]any{
		"projectId": c.projectID,
		"itemId":    itemID,
		"fieldId":   fieldID,
		"text":      text,
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
	normalized := normalizeStateName(stateName)
	for detentState, mapped := range c.stateMap {
		if normalizeStateName(detentState) == normalized {
			return strings.TrimSpace(mapped)
		}
	}
	return stateName
}

func (c *Connector) detentToGitHubStates(stateNames []string) []string {
	states := make([]string, 0, len(stateNames))
	seen := make(map[string]struct{}, len(stateNames))
	for _, stateName := range stateNames {
		state := c.detentToGitHubState(stateName)
		key := normalizeStateName(state)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		states = append(states, state)
	}
	return states
}

func (c *Connector) projectStatusQuery(stateNames []string) string {
	if stateInList(defaultProjectItemStatusState, stateNames) {
		return ""
	}
	states := c.detentToGitHubStates(stateNames)
	if len(states) == 0 {
		return ""
	}

	values := make([]string, 0, len(states))
	for _, state := range states {
		values = append(values, projectFilterValue(state))
	}
	return "status:" + strings.Join(values, ",")
}

func projectFilterValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	for _, r := range value {
		if r >= 'a' && r <= 'z' {
			continue
		}
		if r >= 'A' && r <= 'Z' {
			continue
		}
		if r >= '0' && r <= '9' {
			continue
		}
		if r == '_' || r == '-' || r == '.' {
			continue
		}
		return strconv.Quote(value)
	}
	return value
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

func githubIssueClosed(state string) bool {
	return strings.EqualFold(strings.TrimSpace(state), "CLOSED")
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

func issueRefFromIdentifier(identifier string) (issueRef, bool) {
	repo, number, ok := splitIssueIdentifier(identifier)
	if !ok {
		return issueRef{}, false
	}
	owner, name, ok := splitRepositoryName(repo)
	if !ok {
		return issueRef{}, false
	}
	return issueRef{Owner: owner, Name: name, Number: number}, true
}

func issueRefFromNode(issue githubIssueNode) (issueRef, bool) {
	owner, name, ok := splitRepositoryName(issue.Repository.NameWithOwner)
	if !ok || issue.Number <= 0 {
		return issueRef{}, false
	}
	return issueRef{Owner: owner, Name: name, Number: issue.Number}, true
}

func splitRepositoryName(repo string) (string, string, bool) {
	parts := strings.Split(strings.TrimSpace(repo), "/")
	if len(parts) != 2 {
		return "", "", false
	}
	owner := strings.TrimSpace(parts[0])
	name := strings.TrimSpace(parts[1])
	if owner == "" || name == "" {
		return "", "", false
	}
	return owner, name, true
}

func (c *Connector) cacheIssueRef(issue githubIssueNode) {
	ref, ok := issueRefFromNode(issue)
	if !ok {
		return
	}
	c.projectCache.SetIssueRef(issue.ID, ref)
}

func restIssuePath(ref issueRef) string {
	return "/repos/" + url.PathEscape(ref.Owner) + "/" + url.PathEscape(ref.Name) + "/issues/" + strconv.Itoa(ref.Number)
}

func restIssueCommentsPath(ref issueRef) string {
	return restIssuePath(ref) + "/comments"
}

func restIssueCommentsListPath(ref issueRef) string {
	return restIssueCommentsPath(ref) + "?per_page=100"
}

func restIssueAssigneesPath(ref issueRef) string {
	return restIssuePath(ref) + "/assignees"
}

func restPullRequestsPath(repo pullRequestRepo, page int) string {
	values := url.Values{}
	values.Set("state", "all")
	values.Set("sort", "updated")
	values.Set("direction", "desc")
	values.Set("per_page", strconv.Itoa(pullRequestsPageSize))
	values.Set("page", strconv.Itoa(page))
	return "/repos/" + url.PathEscape(repo.Owner) + "/" + url.PathEscape(repo.Name) + "/pulls?" + values.Encode()
}

func restPullRequestReviewsPath(repo pullRequestRepo, number int) string {
	return "/repos/" + url.PathEscape(repo.Owner) + "/" + url.PathEscape(repo.Name) + "/pulls/" + strconv.Itoa(number) + "/reviews?per_page=100"
}

func restCommitCheckRunsPath(repo pullRequestRepo, sha string) string {
	values := url.Values{}
	values.Set("per_page", "100")
	return "/repos/" + url.PathEscape(repo.Owner) + "/" + url.PathEscape(repo.Name) + "/commits/" + url.PathEscape(sha) + "/check-runs?" + values.Encode()
}

func githubIssueNodeFromREST(ref issueRef, issue restIssue) githubIssueNode {
	repo := ref.Owner + "/" + ref.Name
	return githubIssueNode{
		TypeName:  "Issue",
		ID:        strings.TrimSpace(issue.NodeID),
		Number:    issue.Number,
		Title:     issue.Title,
		Body:      restStringValue(issue.Body),
		State:     issue.State,
		URL:       issue.HTMLURL,
		CreatedAt: restTimeString(issue.CreatedAt),
		UpdatedAt: restTimeString(issue.UpdatedAt),
		Author:    issue.User,
		Assignees: restAssigneesConnection(issue.Assignees),
		Labels:    nodeConnection[label]{Nodes: issue.Labels},
		Repository: repository{
			NameWithOwner: repo,
		},
	}
}

func restStringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func restTimeString(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := value.UTC().Format(time.RFC3339)
	return &formatted
}

func restAssigneesConnection(values []restAssignee) nodeConnection[assignee] {
	out := make([]assignee, 0, len(values))
	for _, value := range values {
		out = append(out, assignee{
			ID:    strings.TrimSpace(value.NodeID),
			Login: strings.TrimSpace(value.Login),
		})
	}
	return nodeConnection[assignee]{Nodes: out}
}

func pullRequestRepoFromIdentifier(identifier string) (pullRequestRepo, bool) {
	repo, _, ok := splitIssueIdentifier(identifier)
	if !ok {
		return pullRequestRepo{}, false
	}
	owner, name, ok := splitRepositoryName(repo)
	if !ok {
		return pullRequestRepo{}, false
	}
	return pullRequestRepo{Owner: owner, Name: name}, true
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

func assigneeLoginReplacement(current []assignee, targetLogin string) ([]string, bool) {
	targetLogin = strings.TrimSpace(targetLogin)
	removeLogins := make([]string, 0, len(current))
	alreadyAssigned := false
	for _, candidate := range current {
		login := strings.TrimSpace(candidate.Login)
		if login == "" {
			continue
		}
		if strings.EqualFold(login, targetLogin) {
			alreadyAssigned = true
			continue
		}
		removeLogins = append(removeLogins, login)
	}
	return removeLogins, alreadyAssigned
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

func restPullRequestState(pullRequest restPullRequest) string {
	if pullRequest.MergedAt != nil && strings.TrimSpace(*pullRequest.MergedAt) != "" {
		return "MERGED"
	}
	return strings.ToUpper(strings.TrimSpace(pullRequest.State))
}

func checkRunsState(checkRuns []restCheckRun) string {
	if len(checkRuns) == 0 {
		return ""
	}
	pending := false
	for _, checkRun := range checkRuns {
		status := strings.ToLower(strings.TrimSpace(checkRun.Status))
		conclusion := strings.ToLower(strings.TrimSpace(checkRun.Conclusion))
		if status != "" && status != "completed" {
			pending = true
			continue
		}
		switch conclusion {
		case "success", "skipped", "neutral":
		case "":
			pending = true
		default:
			return "failure"
		}
	}
	if pending {
		return "pending"
	}
	return "success"
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

func sortIssuesByRequestedIdentifiers(issues []connector.Issue, identifiers []string) {
	order := make(map[string]int, len(identifiers))
	for index, identifier := range identifiers {
		order[normalizedIssueIdentifier(identifier)] = index
	}
	fallback := len(order)
	sort.SliceStable(issues, func(i, j int) bool {
		left := normalizedIssueIdentifier(issues[i].Identifier)
		right := normalizedIssueIdentifier(issues[j].Identifier)
		leftOrder, ok := order[left]
		if !ok {
			leftOrder = fallback
		}
		rightOrder, ok := order[right]
		if !ok {
			rightOrder = fallback
		}
		return leftOrder < rightOrder
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
