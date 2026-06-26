package github

import (
	"context"
	"errors"
	"fmt"
	"maps"
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
	projectItemsPageSize                      = 50
	projectItemsPerIssue                      = 100
	projectItemFieldValuesPageSize            = 100
	linkedIssuePageSize                       = 20
	linkedIssueProjectItemsPageSize           = 10
	linkedIssueProjectItemFieldValuesPageSize = 20
	bodyParentSearchPageSize                  = 100
	pullRequestsPageSize                      = 100
	pullRequestsPageLimit                     = 3
	pullRequestSlowCheckLimit                 = 3
	pullRequestRunningCheckLimit              = 5
	defaultProjectItemStatusState             = "Backlog"
	defaultProjectItemStatusWriteParallelism  = 4
	defaultProjectItemStatusWriteTimeout      = 2 * time.Minute
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
              stateReason
              url
              repository { nameWithOwner }
              labels(first: 20) { nodes { name } }
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

const observedStatusProjectItemsQuery = `
query DetentGitHubObservedStatusProjectItems(
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
              stateReason
              url
              repository { nameWithOwner }
              closedByPullRequestsReferences(first: 5) { nodes { number url state repository { nameWithOwner } } }
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
query DetentGitHubIssueSubIssues(
  $issueId: ID!
  $after: String
  $linkedIssuesFirst: Int!
  $linkedProjectItemsFirst: Int!
  $linkedProjectItemFieldValuesFirst: Int!
) {
  node(id: $issueId) {
    ... on Issue {
      subIssues(first: $linkedIssuesFirst, after: $after) {
        pageInfo { hasNextPage endCursor }
        nodes {
          id
          number
          title
          state
          url
          repository { nameWithOwner }
          projectItems(first: $linkedProjectItemsFirst) {
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
              fieldValues(first: $linkedProjectItemFieldValuesFirst) {
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
query DetentGitHubIssueTrackedIssues(
  $issueId: ID!
  $after: String
  $linkedIssuesFirst: Int!
  $linkedProjectItemsFirst: Int!
  $linkedProjectItemFieldValuesFirst: Int!
) {
  node(id: $issueId) {
    ... on Issue {
      trackedIssues(first: $linkedIssuesFirst, after: $after) {
        pageInfo { hasNextPage endCursor }
        nodes {
          id
          number
          title
          state
          url
          repository { nameWithOwner }
          projectItems(first: $linkedProjectItemsFirst) {
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
              fieldValues(first: $linkedProjectItemFieldValuesFirst) {
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

const issueSubIssuesLabelQuery = `
query DetentGitHubIssueSubIssuesLabel($issueId: ID!, $after: String) {
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
          labels(first: 20) { nodes { name } }
          repository { nameWithOwner }
        }
      }
    }
  }
  rateLimit { limit used remaining cost resetAt }
}`

const issueTrackedIssuesLabelQuery = `
query DetentGitHubIssueTrackedIssuesLabel($issueId: ID!, $after: String) {
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
          labels(first: 20) { nodes { name } }
          repository { nameWithOwner }
        }
      }
    }
  }
  rateLimit { limit used remaining cost resetAt }
}`

const issueParentsQuery = `
query DetentGitHubIssueParents(
  $issueId: ID!
  $trackedInAfter: String
  $projectItemsFirst: Int!
  $projectItemFieldValuesFirst: Int!
  $linkedIssuesFirst: Int!
  $linkedProjectItemsFirst: Int!
  $linkedProjectItemFieldValuesFirst: Int!
) {
  node(id: $issueId) {
    ... on Issue {
      id
      number
      repository { nameWithOwner }
      parent {
        ...DetentGitHubIssueParent
      }
      trackedInIssues(first: $linkedIssuesFirst, after: $trackedInAfter) {
        pageInfo { hasNextPage endCursor }
        nodes {
          ...DetentGitHubIssueParent
        }
      }
    }
  }
  rateLimit { limit used remaining cost resetAt }
}

fragment DetentGitHubIssueParent on Issue {
  __typename
  id
  number
  title
  body
  state
  stateReason
  url
  createdAt
  updatedAt
  author { login }
  assignees(first: 100) { nodes { id login } }
  labels(first: 20) { nodes { name } }
  repository { nameWithOwner }
  closedByPullRequestsReferences(first: 5) { nodes { number url state repository { nameWithOwner } } }
  subIssues(first: $linkedIssuesFirst) {
    pageInfo { hasNextPage endCursor }
    nodes {
      id
      number
      title
      state
      url
      repository { nameWithOwner }
      projectItems(first: $linkedProjectItemsFirst) {
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
          fieldValues(first: $linkedProjectItemFieldValuesFirst) {
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
  trackedIssues(first: $linkedIssuesFirst) {
    pageInfo { hasNextPage endCursor }
    nodes {
      id
      number
      title
      state
      url
      repository { nameWithOwner }
      projectItems(first: $linkedProjectItemsFirst) {
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
          fieldValues(first: $linkedProjectItemFieldValuesFirst) {
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
      fieldValues(first: $projectItemFieldValuesFirst) {
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
}`

const issueParentsLabelQuery = `
query DetentGitHubIssueParentsLabel(
  $issueId: ID!
  $trackedInAfter: String
  $linkedIssuesFirst: Int!
) {
  node(id: $issueId) {
    ... on Issue {
      id
      number
      repository { nameWithOwner }
      parent {
        ...DetentGitHubIssueParentLabel
      }
      trackedInIssues(first: 100, after: $trackedInAfter) {
        pageInfo { hasNextPage endCursor }
        nodes {
          ...DetentGitHubIssueParentLabel
        }
      }
    }
  }
  rateLimit { limit used remaining cost resetAt }
}

fragment DetentGitHubIssueParentLabel on Issue {
  __typename
  id
  number
  title
  body
  state
  stateReason
  url
  createdAt
  updatedAt
  author { login }
  assignees(first: 100) { nodes { id login } }
  labels(first: 20) { nodes { name } }
  repository { nameWithOwner }
  closedByPullRequestsReferences(first: 5) { nodes { number url state repository { nameWithOwner } } }
  subIssues(first: $linkedIssuesFirst) {
    pageInfo { hasNextPage endCursor }
    nodes {
      id
      number
      title
      state
      url
      labels(first: 20) { nodes { name } }
      repository { nameWithOwner }
    }
  }
  trackedIssues(first: $linkedIssuesFirst) {
    pageInfo { hasNextPage endCursor }
    nodes {
      id
      number
      title
      state
      url
      labels(first: 20) { nodes { name } }
      repository { nameWithOwner }
    }
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
  rateLimit { limit used remaining cost resetAt }
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

const deleteProjectItemMutation = `
mutation DetentGitHubDeleteProjectItem($projectId: ID!, $itemId: ID!) {
  deleteProjectV2Item(input: {
    projectId: $projectId,
    itemId: $itemId
  }) {
    deletedItemId
  }
}`

var (
	modelOverridePattern  = regexp.MustCompile(`(?i)<!--\s*model:\s*(\S+?)\s*-->`)
	dependencyLinePattern = regexp.MustCompile("(?i)^\\s*(?:>\\s*)?(?:[-*+]\\s+)?(?:[*_`~]+)?\\s*(?:blocked\\s+by|depends[\\s-]+on)(?:[*_`~]+)?\\s*:\\s*(?:[*_`~]+)?\\s*(.+)\\s*$")
	issueRefPattern       = regexp.MustCompile(`(?:([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+))?#(\d+)`)
	issueURLPattern       = regexp.MustCompile(`https?://github\.com/([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)/issues/(\d+)`)
	numberedListPattern   = regexp.MustCompile(`^\d+[.)]\s+`)
	branchKeyPattern      = regexp.MustCompile(`[^A-Za-z0-9._-]`)
	actionRunURLPattern   = regexp.MustCompile(`/actions/runs/([0-9]+)(?:/|$)`)
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
	StateReason                    string                       `json:"stateReason"`
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

type issueNodesConnection struct {
	PageInfo pageInfo          `json:"pageInfo"`
	Nodes    []githubIssueNode `json:"nodes"`
}

type issueParentsNode struct {
	ID              string               `json:"id"`
	Number          int                  `json:"number"`
	Repository      repository           `json:"repository"`
	Parent          *githubIssueNode     `json:"parent"`
	TrackedInIssues issueNodesConnection `json:"trackedInIssues"`
}

type linkedIssue struct {
	ID           string                  `json:"id"`
	Number       int                     `json:"number"`
	Title        string                  `json:"title"`
	State        string                  `json:"state"`
	URL          string                  `json:"url"`
	Labels       nodeConnection[label]   `json:"labels"`
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
	URL  string `json:"url"`
}

type pullRequest struct {
	Number     int        `json:"number"`
	URL        string     `json:"url"`
	State      string     `json:"state"`
	Repository repository `json:"repository"`
}

type pullRequestNode struct {
	Number                     int                               `json:"number"`
	URL                        string                            `json:"url"`
	State                      string                            `json:"state"`
	MergeableState             string                            `json:"mergeableState"`
	Draft                      bool                              `json:"draft"`
	ActivityAt                 *time.Time                        `json:"activityAt"`
	HeadRefName                string                            `json:"headRefName"`
	HeadSHA                    string                            `json:"headSHA"`
	BaseSHA                    string                            `json:"baseRefOid"`
	HydrationUnavailableReason string                            `json:"-"`
	HydrationDegradedReason    string                            `json:"-"`
	HydrationNextRetryAt       *time.Time                        `json:"-"`
	Commits                    nodeConnection[pullRequestCommit] `json:"commits"`
	LatestReviews              nodeConnection[pullRequestReview] `json:"latestReviews"`
	CodexReviews               pullRequestCodexReviews           `json:"-"`
	CI                         pullRequestCI                     `json:"-"`
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
	Body        string     `json:"body"`
	URL         string     `json:"url"`
	State       string     `json:"state"`
	Author      *actor     `json:"author"`
	CommitID    string     `json:"commitId"`
	SubmittedAt *time.Time `json:"submittedAt"`
}

type pullRequestCodexReviews struct {
	CurrentHead []pullRequestReview
	Latest      []pullRequestReview
}

type actor struct {
	Login string `json:"login"`
}

type restIssue struct {
	NodeID      string         `json:"node_id"`
	Number      int            `json:"number"`
	Title       string         `json:"title"`
	Body        *string        `json:"body"`
	State       string         `json:"state"`
	StateReason string         `json:"state_reason"`
	HTMLURL     string         `json:"html_url"`
	CreatedAt   *time.Time     `json:"created_at"`
	UpdatedAt   *time.Time     `json:"updated_at"`
	User        *actor         `json:"user"`
	Assignees   []restAssignee `json:"assignees"`
	Labels      []label        `json:"labels"`
	PullRequest *struct{}      `json:"pull_request"`
}

type restIssueSearchResponse struct {
	TotalCount int         `json:"total_count"`
	Items      []restIssue `json:"items"`
}

type restAssignee struct {
	NodeID string `json:"node_id"`
	Login  string `json:"login"`
}

type restPullRequest struct {
	Number         int        `json:"number"`
	HTMLURL        string     `json:"html_url"`
	State          string     `json:"state"`
	MergeableState string     `json:"mergeable_state"`
	Draft          bool       `json:"draft"`
	Head           restHead   `json:"head"`
	Base           restHead   `json:"base"`
	UpdatedAt      *time.Time `json:"updated_at"`
	MergedAt       *string    `json:"merged_at"`
}

type restPullRequestMergeResponse struct {
	SHA     string `json:"sha"`
	Merged  bool   `json:"merged"`
	Message string `json:"message"`
}

type restHead struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

type restReview struct {
	Body        string     `json:"body"`
	HTMLURL     string     `json:"html_url"`
	State       string     `json:"state"`
	User        *actor     `json:"user"`
	CommitID    string     `json:"commit_id"`
	SubmittedAt *time.Time `json:"submitted_at"`
}

type restCheckRuns struct {
	CheckRuns []restCheckRun `json:"check_runs"`
}

type restCheckRun struct {
	Status      string     `json:"status"`
	Conclusion  string     `json:"conclusion"`
	Name        string     `json:"name"`
	DetailsURL  string     `json:"details_url"`
	CreatedAt   *time.Time `json:"created_at"`
	StartedAt   *time.Time `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at"`
}

type restWorkflowRun struct {
	ID           int64      `json:"id"`
	CreatedAt    *time.Time `json:"created_at"`
	RunStartedAt *time.Time `json:"run_started_at"`
}

type restCommitStatus struct {
	Context   string     `json:"context"`
	State     string     `json:"state"`
	CreatedAt *time.Time `json:"created_at"`
}

type pullRequestCI struct {
	State              string
	CheckRunCount      int
	StatusContextCount int
	CIQueueSeconds     int64
	CIDurationSeconds  int64
	SlowChecks         []connector.PullRequestCheck
	RunningChecks      []string
}

type checkRunTelemetrySummary struct {
	QueueSeconds    int64
	DurationSeconds int64
	SlowChecks      []connector.PullRequestCheck
	RunningChecks   []string
}

type restComment struct {
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
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
	return c.FetchCandidateIssuesByStates(ctx, c.activeStates)
}

func (c *Connector) FetchCandidateIssuesByStates(ctx context.Context, stateNames []string) ([]connector.Issue, error) {
	stateNames = normalizeStateList(stateNames, nil)
	if len(stateNames) == 0 {
		return []connector.Issue{}, nil
	}
	if c.usesLabelStatus() {
		issues, err := c.fetchLabelIssuesByStates(ctx, stateNames, 0)
		if err != nil {
			return nil, err
		}
		if err := c.attachPullRequests(ctx, issues); err != nil {
			return nil, err
		}
		return issues, nil
	}
	if c.usesIssueFieldStatus() {
		issues, err := c.fetchIssueFieldIssuesByStates(ctx, stateNames, 0)
		if err != nil {
			return nil, err
		}
		if err := c.attachPullRequests(ctx, issues); err != nil {
			return nil, err
		}
		return issues, nil
	}
	if c.projectID == "" {
		return nil, ErrMissingProject
	}

	issues, err := c.fetchProjectItems(ctx, graphQLQueryCandidateIssues, c.projectStatusQuery(stateNames), func(issue connector.Issue) bool {
		return stateInList(issue.State, stateNames)
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
	if c.usesLabelStatus() {
		issues, err := c.fetchLabelIssuesByStates(ctx, stateNames, 0)
		if err != nil {
			return nil, err
		}
		if _, ok := wantedStates[normalizeStateName("Blocked")]; ok {
			if err := c.populateBlockerReasons(ctx, issues); err != nil {
				return nil, err
			}
		}
		if attachPullRequestsForStates(wantedStates) {
			if err := c.attachFreshPullRequests(ctx, issues); err != nil {
				return nil, err
			}
		}
		return issues, nil
	}
	if c.usesIssueFieldStatus() {
		issues, err := c.fetchIssueFieldIssuesByStates(ctx, stateNames, 0)
		if err != nil {
			return nil, err
		}
		if _, ok := wantedStates[normalizeStateName("Blocked")]; ok {
			if err := c.populateBlockerReasons(ctx, issues); err != nil {
				return nil, err
			}
		}
		if attachPullRequestsForStates(wantedStates) {
			if err := c.attachFreshPullRequests(ctx, issues); err != nil {
				return nil, err
			}
		}
		return issues, nil
	}
	if c.projectID == "" {
		return nil, ErrMissingProject
	}

	issues, err := c.fetchProjectItems(ctx, graphQLQueryObservedStatus, c.projectStatusQuery(stateNames), func(issue connector.Issue) bool {
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
		if err := c.attachFreshPullRequests(ctx, issues); err != nil {
			return nil, err
		}
	}
	return issues, nil
}

func (c *Connector) FetchIssuesByStatesLimit(ctx context.Context, stateNames []string, limit int) ([]connector.Issue, error) {
	if limit <= 0 {
		return []connector.Issue{}, nil
	}
	wantedStates := normalizedStateSet(stateNames)
	if len(wantedStates) == 0 {
		return []connector.Issue{}, nil
	}
	if c.usesLabelStatus() {
		issues, err := c.fetchLabelIssuesByStates(ctx, stateNames, limit)
		if err != nil {
			return nil, err
		}
		if _, ok := wantedStates[normalizeStateName("Blocked")]; ok {
			if err := c.populateBlockerReasons(ctx, issues); err != nil {
				return nil, err
			}
		}
		if attachPullRequestsForStates(wantedStates) {
			if err := c.attachFreshPullRequests(ctx, issues); err != nil {
				return nil, err
			}
		}
		return issues, nil
	}
	if c.usesIssueFieldStatus() {
		issues, err := c.fetchIssueFieldIssuesByStates(ctx, stateNames, limit)
		if err != nil {
			return nil, err
		}
		if _, ok := wantedStates[normalizeStateName("Blocked")]; ok {
			if err := c.populateBlockerReasons(ctx, issues); err != nil {
				return nil, err
			}
		}
		if attachPullRequestsForStates(wantedStates) {
			if err := c.attachFreshPullRequests(ctx, issues); err != nil {
				return nil, err
			}
		}
		return issues, nil
	}
	if c.projectID == "" {
		return nil, ErrMissingProject
	}

	issues, err := c.fetchProjectItemsWithPullRequestRefsLimit(ctx, graphQLQueryObservedStatus, c.projectStatusQuery(stateNames), func(issue connector.Issue) bool {
		_, ok := wantedStates[normalizeStateName(issue.State)]
		return ok
	}, limit)
	if err != nil {
		return nil, err
	}
	if _, ok := wantedStates[normalizeStateName("Blocked")]; ok {
		if err := c.populateBlockerReasons(ctx, issues); err != nil {
			return nil, err
		}
	}
	if attachPullRequestsForStates(wantedStates) {
		if err := c.attachFreshPullRequests(ctx, issues); err != nil {
			return nil, err
		}
	}
	return issues, nil
}

func (c *Connector) FetchIssueStateProbe(ctx context.Context, stateNames []string, limit int) ([]connector.Issue, error) {
	if limit <= 0 {
		return []connector.Issue{}, nil
	}
	wantedStates := normalizedStateSet(stateNames)
	if len(wantedStates) == 0 {
		return []connector.Issue{}, nil
	}
	if c.usesLabelStatus() {
		return c.fetchLabelIssuesByStates(ctx, stateNames, limit)
	}
	if c.usesIssueFieldStatus() {
		return c.fetchIssueFieldIssuesByStates(ctx, stateNames, limit)
	}
	if c.projectID == "" {
		return nil, ErrMissingProject
	}

	return c.fetchProjectItemsWithPullRequestRefsLimit(ctx, graphQLQueryObservedStatus, c.projectStatusQuery(stateNames), func(issue connector.Issue) bool {
		_, ok := wantedStates[normalizeStateName(issue.State)]
		return ok
	}, limit)
}

func (c *Connector) VerifyStatusOptions(ctx context.Context, stateNames []string) error {
	if c.usesLabelStatus() {
		return c.verifyLabelStatusOptions(ctx, stateNames)
	}
	if c.usesIssueFieldStatus() {
		return c.verifyIssueFieldStatusOptions(ctx, stateNames)
	}
	seen := map[string]struct{}{}
	for _, stateName := range stateNames {
		stateName = strings.TrimSpace(stateName)
		if stateName == "" {
			continue
		}
		githubState := c.detentToGitHubState(stateName)
		key := normalizeStateName(githubState)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		if _, _, err := c.resolveStatusOption(ctx, githubState); err != nil {
			if errors.Is(err, ErrStatusOptionNotFound) {
				return fmt.Errorf("%w: %s maps to %s", ErrStatusOptionNotFound, stateName, githubState)
			}
			return err
		}
	}
	return nil
}

func (c *Connector) FetchIssueStatesByIDs(ctx context.Context, issueIDs []string) ([]connector.Issue, error) {
	ids := uniqueNonBlank(issueIDs)
	if len(ids) == 0 {
		return []connector.Issue{}, nil
	}
	if c.usesLabelStatus() {
		refs, err := c.issueRefsForIDs(ctx, ids, graphQLQueryRunningStates)
		if err != nil {
			return nil, err
		}

		issues := make([]connector.Issue, 0, len(ids))
		for _, id := range ids {
			ref, ok := refs[id]
			if !ok {
				continue
			}
			issue, ok, err := c.fetchLabelIssueByRef(ctx, ref)
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
	if c.usesIssueFieldStatus() {
		refs, err := c.issueRefsForIDs(ctx, ids, graphQLQueryRunningStates)
		if err != nil {
			return nil, err
		}

		issues := make([]connector.Issue, 0, len(ids))
		for _, id := range ids {
			ref, ok := refs[id]
			if !ok {
				continue
			}
			issue, ok, err := c.fetchIssueFieldIssueByRef(ctx, ref)
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
	if c.projectID == "" {
		return nil, ErrMissingProject
	}

	refs, err := c.issueRefsForIDs(ctx, ids, graphQLQueryRunningStates)
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
	if err := c.attachPullRequestMergeStates(ctx, issues); err != nil {
		return nil, err
	}
	sortIssuesByRequestedIdentifiers(issues, identifiers)
	return issues, nil
}

func (c *Connector) FetchIssueParents(ctx context.Context, issueID string) ([]connector.Issue, error) {
	issueID = strings.TrimSpace(issueID)
	if issueID == "" {
		return []connector.Issue{}, nil
	}

	var after *string
	parents := []connector.Issue{}
	seen := map[string]struct{}{}
	var childRef issueRef
	var childRefOK bool
	for {
		var response struct {
			Node *issueParentsNode `json:"node"`
		}
		query := issueParentsQuery
		variables := map[string]any{
			"issueId":                           issueID,
			"trackedInAfter":                    after,
			"projectItemsFirst":                 projectItemsPerIssue,
			"projectItemFieldValuesFirst":       projectItemFieldValuesPageSize,
			"linkedIssuesFirst":                 linkedIssuePageSize,
			"linkedProjectItemsFirst":           linkedIssueProjectItemsPageSize,
			"linkedProjectItemFieldValuesFirst": linkedIssueProjectItemFieldValuesPageSize,
		}
		if c.usesLabelStatus() {
			query = issueParentsLabelQuery
			variables = map[string]any{
				"issueId":           issueID,
				"trackedInAfter":    after,
				"linkedIssuesFirst": linkedIssuePageSize,
			}
		}
		if err := c.client.GraphQLWithType(ctx, graphQLQueryIssueParents, query, variables, &response); err != nil {
			return nil, fmt.Errorf("fetch github issue parents: %w", err)
		}
		if response.Node == nil {
			return nil, ErrInvalidResponse
		}
		if !childRefOK {
			childRef, childRefOK = issueRefFromNode(githubIssueNode{
				ID:         response.Node.ID,
				Number:     response.Node.Number,
				Repository: response.Node.Repository,
			})
			if childRefOK {
				c.projectCache.SetIssueRef(issueID, childRef)
			}
		}

		if response.Node.Parent != nil {
			var err error
			parents, err = c.appendIssueParent(ctx, parents, seen, *response.Node.Parent)
			if err != nil {
				return nil, err
			}
		}
		for _, node := range response.Node.TrackedInIssues.Nodes {
			var err error
			parents, err = c.appendIssueParent(ctx, parents, seen, node)
			if err != nil {
				return nil, err
			}
		}
		if !response.Node.TrackedInIssues.PageInfo.HasNextPage {
			if childRefOK {
				var err error
				parents, err = c.appendBodyReferencedIssueParents(ctx, parents, seen, childRef)
				if err != nil {
					return nil, err
				}
			}
			return parents, nil
		}
		cursor := strings.TrimSpace(response.Node.TrackedInIssues.PageInfo.EndCursor)
		if cursor == "" {
			return nil, ErrInvalidResponse
		}
		after = &cursor
	}
}

func (c *Connector) appendBodyReferencedIssueParents(
	ctx context.Context,
	parents []connector.Issue,
	seen map[string]struct{},
	childRef issueRef,
) ([]connector.Issue, error) {
	childRepo := childRef.Owner + "/" + childRef.Name
	childIdentifier := buildIdentifier(childRepo, childRef.Number)
	for page := 1; ; page++ {
		var response restIssueSearchResponse
		if err := c.client.REST(ctx, http.MethodGet, restIssueSearchPath(childRef, page), nil, &response); err != nil {
			return nil, fmt.Errorf("search github body referenced issue parents: %w", err)
		}
		for _, item := range response.Items {
			ref, ok := issueRefFromRESTSearchItem(item, childRef)
			if !ok || sameIssueRef(ref, childRef) {
				continue
			}
			var issue connector.Issue
			var found bool
			var err error
			if c.usesLabelStatus() || c.usesIssueFieldStatus() {
				issue, found, err = c.fetchIssueByRef(ctx, ref)
			} else {
				issue, found, err = c.fetchProjectIssueByRef(ctx, ref)
			}
			if err != nil {
				return nil, err
			}
			if !found || !githubEpicIssue(issue) {
				continue
			}
			if !bodyReferencesIssue(issue.Description, issueRepo(issue.Identifier), childIdentifier) {
				continue
			}
			key := connectorIssueKey(issue)
			if key == "" {
				continue
			}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			parents = append(parents, issue)
		}
		if len(response.Items) == 0 || page*bodyParentSearchPageSize >= response.TotalCount {
			return parents, nil
		}
	}
}

func (c *Connector) appendIssueParent(
	ctx context.Context,
	parents []connector.Issue,
	seen map[string]struct{},
	node githubIssueNode,
) ([]connector.Issue, error) {
	issue, ok, err := c.normalizeIssueNode(ctx, node)
	if err != nil {
		return nil, err
	}
	if !ok {
		return parents, nil
	}
	key := connectorIssueKey(issue)
	if key == "" {
		return parents, nil
	}
	if _, ok := seen[key]; ok {
		return parents, nil
	}
	seen[key] = struct{}{}
	return append(parents, issue), nil
}

func (c *Connector) FetchIssueChildren(ctx context.Context, issueID string) ([]connector.BlockedRef, error) {
	issueID = strings.TrimSpace(issueID)
	if issueID == "" {
		return []connector.BlockedRef{}, nil
	}

	seen := map[string]struct{}{}
	children := []connector.BlockedRef{}
	subIssuesQuery := issueSubIssuesQuery
	trackedIssuesQuery := issueTrackedIssuesQuery
	if c.usesLabelStatus() {
		subIssuesQuery = issueSubIssuesLabelQuery
		trackedIssuesQuery = issueTrackedIssuesLabelQuery
	}
	subIssues, err := c.fetchLinkedIssueRefs(ctx, issueID, subIssuesQuery, "subIssues")
	if err != nil {
		return nil, err
	}
	children = appendUniqueLinkedIssueRefs(children, seen, subIssues)
	trackedIssues, err := c.fetchLinkedIssueRefs(ctx, issueID, trackedIssuesQuery, "trackedIssues")
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
	if err := c.client.GraphQLWithType(ctx, graphQLQueryEpicChildren, query, map[string]any{
		"issueId":                           issueID,
		"after":                             after,
		"linkedIssuesFirst":                 linkedIssuePageSize,
		"linkedProjectItemsFirst":           linkedIssueProjectItemsPageSize,
		"linkedProjectItemFieldValuesFirst": linkedIssueProjectItemFieldValuesPageSize,
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

func (c *Connector) issueRefsForIDs(ctx context.Context, ids []string, queryType string) (map[string]issueRef, error) {
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

	fetched, err := c.fetchIssueRefsByID(ctx, missing, queryType)
	if err != nil {
		return nil, err
	}
	maps.Copy(refs, fetched)
	return refs, nil
}

func (c *Connector) issueRefForID(ctx context.Context, issueID string, queryType string) (issueRef, bool, error) {
	issueID = strings.TrimSpace(issueID)
	if issueID == "" {
		return issueRef{}, false, nil
	}
	if ref, ok := c.projectCache.GetIssueRef(issueID); ok {
		return ref, true, nil
	}
	refs, err := c.fetchIssueRefsByID(ctx, []string{issueID}, queryType)
	if err != nil {
		return issueRef{}, false, err
	}
	ref, ok := refs[issueID]
	return ref, ok, nil
}

func (c *Connector) fetchIssueRefsByID(ctx context.Context, ids []string, queryType string) (map[string]issueRef, error) {
	var response struct {
		Nodes []githubIssueNode `json:"nodes"`
	}
	if err := c.client.GraphQLWithType(ctx, queryType, issueIdentitiesByIDQuery, map[string]any{"issueIds": ids}, &response); err != nil {
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
	if c.usesLabelStatus() {
		return c.fetchLabelIssueByRef(ctx, ref)
	}
	if c.usesIssueFieldStatus() {
		return c.fetchIssueFieldIssueByRef(ctx, ref)
	}
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

func (c *Connector) fetchProjectIssueByRef(ctx context.Context, ref issueRef) (connector.Issue, bool, error) {
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
	if !ok {
		return connector.Issue{}, false, nil
	}
	return c.buildIssue(issue, stateName, priorityName, statusUpdatedAt, fields), true, nil
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
			ID:         issues[index].ID,
			Number:     ref.Number,
			Body:       issues[index].Description,
			Repository: repository{NameWithOwner: ref.Owner + "/" + ref.Name},
			Comments:   nodeConnection[issueComment]{Nodes: comments},
		}
		if len(issues[index].BlockedBy) == 0 {
			issues[index].BlockedBy = parseBlockedByFromIssueText(node, issueRepo(issues[index].Identifier))
		}
		if reason := parseBlockerReason(node); reason != "" {
			issues[index].BlockerReason = reason
		}
	}
	return nil
}

func (c *Connector) fetchIssueComments(ctx context.Context, ref issueRef) ([]issueComment, error) {
	response, err := fetchRESTList[restComment](ctx, c.client, restIssueCommentsListPath(ref))
	if err != nil {
		return nil, err
	}
	comments := make([]issueComment, 0, len(response))
	for _, comment := range response {
		comments = append(comments, issueComment{
			Body: comment.Body,
			URL:  comment.HTMLURL,
		})
	}
	return comments, nil
}

func (c *Connector) FetchIssueComments(ctx context.Context, issue connector.Issue) ([]connector.IssueComment, error) {
	ref, ok := issueRefFromIdentifier(issue.Identifier)
	if !ok {
		ref, ok = issueRefFromURL(issue.URL)
	}
	if !ok {
		return []connector.IssueComment{}, nil
	}

	comments, err := c.fetchIssueComments(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("fetch github issue comments: %w", err)
	}
	out := make([]connector.IssueComment, 0, len(comments))
	for _, comment := range comments {
		out = append(out, connector.IssueComment{
			Body: comment.Body,
			URL:  comment.URL,
		})
	}
	return out, nil
}

func (c *Connector) CreateComment(ctx context.Context, issueID string, body string) error {
	ref, ok, err := c.issueRefForID(ctx, issueID, graphQLQueryIssueLookup)
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

func (c *Connector) CreatePullRequestComment(ctx context.Context, repository string, number int, body string) error {
	owner, name, ok := splitRepositoryName(repository)
	if !ok || number <= 0 {
		return ErrCommentCreateFailed
	}

	var response struct {
		NodeID string `json:"node_id"`
	}
	ref := issueRef{Owner: owner, Name: name, Number: number}
	if err := c.client.REST(ctx, http.MethodPost, restIssueCommentsPath(ref), map[string]any{"body": body}, &response); err != nil {
		return fmt.Errorf("create github pull request comment: %w", err)
	}
	if strings.TrimSpace(response.NodeID) == "" {
		return ErrCommentCreateFailed
	}

	return nil
}

func (c *Connector) SetIssueField(ctx context.Context, issueID string, fieldID int, value string) error {
	if fieldID <= 0 || strings.TrimSpace(value) == "" {
		return ErrIssueFieldUpdateFailed
	}

	ref, ok, err := c.issueRefForID(ctx, issueID, graphQLQueryIssueLookup)
	if err != nil {
		return err
	}
	if !ok {
		return ErrIssueFieldUpdateFailed
	}

	var response []restIssueFieldValue
	if err := c.client.REST(ctx, http.MethodPost, restIssueFieldValuesPath(ref), map[string]any{
		"issue_field_values": []map[string]any{{
			"field_id": fieldID,
			"value":    strings.TrimSpace(value),
		}},
	}, &response); err != nil {
		return fmt.Errorf("update github issue field: %w", err)
	}
	if len(response) == 0 {
		return ErrIssueFieldUpdateFailed
	}

	return nil
}

func (c *Connector) ClearIssueField(ctx context.Context, issueID string, fieldID int) error {
	if fieldID <= 0 || strings.TrimSpace(issueID) == "" {
		return ErrIssueFieldUpdateFailed
	}

	ref, ok, err := c.issueRefForID(ctx, issueID, graphQLQueryIssueLookup)
	if err != nil {
		return err
	}
	if !ok {
		return ErrIssueFieldUpdateFailed
	}

	return c.deleteIssueFieldValue(ctx, ref, fieldID, ErrIssueFieldUpdateFailed)
}

func (c *Connector) CloseIssue(ctx context.Context, issueID string) error {
	ref, ok, err := c.issueRefForID(ctx, issueID, graphQLQueryIssueLookup)
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
	if c.usesLabelStatus() {
		ref, ok, err := c.issueRefForID(ctx, strings.TrimSpace(issueID), graphQLQueryIssueLookup)
		if err != nil {
			return err
		}
		if !ok {
			return ErrStatusUpdateFailed
		}
		issue, err := c.fetchRESTIssue(ctx, ref)
		if err != nil {
			return err
		}
		if strings.TrimSpace(issue.ID) == "" {
			return ErrStatusUpdateFailed
		}
		currentStatus := c.labelStatusFromLabels(issue.Labels)
		if currentStatus == "" {
			currentStatus = c.githubIssueStateToDetentState(issue.State)
		}
		if c.terminalStatusUpdateBlocked(currentStatus, stateName) {
			return nil
		}
		return c.updateIssueStatusLabel(ctx, ref, issue, stateName)
	}
	if c.usesIssueFieldStatus() {
		ref, ok, err := c.issueRefForID(ctx, strings.TrimSpace(issueID), graphQLQueryIssueLookup)
		if err != nil {
			return err
		}
		if !ok {
			return ErrStatusUpdateFailed
		}
		currentStatus, err := c.fetchIssueFieldStatus(ctx, ref)
		if err != nil {
			return err
		}
		if c.terminalStatusUpdateBlocked(currentStatus, stateName) {
			return nil
		}
		githubState := c.detentToGitHubState(stateName)
		return c.setIssueStatusField(ctx, ref, githubState)
	}
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

func (c *Connector) RemoveIssueFromProject(ctx context.Context, issueID string) error {
	issueID = strings.TrimSpace(issueID)
	if issueID == "" {
		return ErrProjectItemRemoveFailed
	}
	if c.usesLabelStatus() {
		ref, ok, err := c.issueRefForID(ctx, issueID, graphQLQueryIssueLookup)
		if err != nil {
			return err
		}
		if !ok {
			return ErrStatusUpdateFailed
		}
		return c.removeIssueStatusLabels(ctx, ref)
	}
	if c.usesIssueFieldStatus() {
		ref, ok, err := c.issueRefForID(ctx, issueID, graphQLQueryIssueLookup)
		if err != nil {
			return err
		}
		if !ok {
			return ErrStatusUpdateFailed
		}
		return c.clearIssueStatusField(ctx, ref)
	}
	if c.projectID == "" {
		return ErrMissingProject
	}

	item, err := c.resolveProjectItem(ctx, issueID)
	if err != nil {
		return err
	}
	if err := c.deleteProjectItem(ctx, item.ID); err != nil {
		return err
	}
	c.projectCache.ClearItemID(c.projectID, issueID)
	return nil
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
	ref, ok, err := c.issueRefForID(ctx, issueID, graphQLQueryIssueLookup)
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
	if c.usesIssueFieldStatus() {
		return c.setIssueFieldValueByName(ctx, issueID, fieldName, value)
	}
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
	Index             int
	BranchPrefix      string
	PullRequestNumber int
	PullRequestRepo   pullRequestRepo
}

type pullRequestKey struct {
	Repo   pullRequestRepo
	Number int
}

type pullRequestRepo struct {
	Owner string
	Name  string
}

func (c *Connector) attachPullRequests(ctx context.Context, issues []connector.Issue) error {
	return c.attachPullRequestsWithCache(ctx, issues, true)
}

func (c *Connector) attachFreshPullRequests(ctx context.Context, issues []connector.Issue) error {
	return c.attachPullRequestsWithCache(ctx, issues, false)
}

func (c *Connector) attachPullRequestsWithCache(ctx context.Context, issues []connector.Issue, useStatusCache bool) error {
	byRepo := make(map[pullRequestRepo][]issuePullRequestCandidate)
	for index, issue := range issues {
		repo, ok := pullRequestRepoFromIdentifier(issue.Identifier)
		if !ok {
			continue
		}
		branchPrefix := detentIssueBranchPrefix(issue.Identifier)
		pullRequestNumber := 0
		linkedPullRequestRepo := repo
		if issue.PRNumber != nil {
			pullRequestNumber = *issue.PRNumber
		}
		if owner, name, ok := splitRepositoryName(issue.PRRepository); ok {
			linkedPullRequestRepo = pullRequestRepo{Owner: owner, Name: name}
		}
		if normalizeStateName(issue.State) == normalizeStateName("Blocked") && pullRequestNumber <= 0 && !statusLabelConflictIssue(issue) {
			branchPrefix = ""
		}
		if branchPrefix == "" && pullRequestNumber <= 0 {
			continue
		}
		byRepo[repo] = append(byRepo[repo], issuePullRequestCandidate{
			Index:             index,
			BranchPrefix:      branchPrefix,
			PullRequestNumber: pullRequestNumber,
			PullRequestRepo:   linkedPullRequestRepo,
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
		if err := c.attachLinkedPullRequests(ctx, repo, issues, byRepo[repo], useStatusCache); err != nil {
			return err
		}
		if !hasUnattachedBranchPullRequestCandidates(issues, byRepo[repo]) {
			continue
		}
		if state, ok := c.currentPullRequestHydrationState(repo); ok {
			c.logPullRequestHydrationSkip(ctx, repo, state, "shared_backoff")
			markPullRequestHydrationUnavailableForCandidates(issues, byRepo[repo], repo, state)
			continue
		}
		pullRequests, err := c.fetchRepositoryPullRequests(ctx, repo)
		if err != nil {
			if state := c.pullRequestHydrationStateForError(repo, err); state.Reason != "" {
				markPullRequestHydrationUnavailableForCandidates(issues, byRepo[repo], repo, state)
				continue
			}
			return err
		}
		if err := c.attachMatchingPullRequests(ctx, repo, issues, byRepo[repo], pullRequests, useStatusCache); err != nil {
			return err
		}
	}
	return nil
}

func (c *Connector) attachPullRequestMergeStates(ctx context.Context, issues []connector.Issue) error {
	byRepo := make(map[pullRequestRepo][]issuePullRequestCandidate)
	for index, issue := range issues {
		if issue.PullRequest != nil {
			continue
		}
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
			if errors.Is(err, ErrRESTBudgetReserved) {
				continue
			}
			return err
		}
		attachMatchingPullRequestMergeStates(repo, issues, byRepo[repo], pullRequests)
	}
	return nil
}

func attachMatchingPullRequestMergeStates(
	repo pullRequestRepo,
	issues []connector.Issue,
	candidates []issuePullRequestCandidate,
	pullRequests []pullRequestNode,
) {
	for _, pullRequest := range pullRequests {
		if normalizeStateName(pullRequest.State) != "merged" {
			continue
		}
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
				Number:     pullRequest.Number,
				URL:        strings.TrimSpace(pullRequest.URL),
				BranchName: branchName,
				State:      strings.ToUpper(strings.TrimSpace(pullRequest.State)),
				ActivityAt: cloneGitHubTime(pullRequest.ActivityAt),
			}
			if issues[candidate.Index].PRNumber == nil && pullRequest.Number > 0 {
				number := pullRequest.Number
				issues[candidate.Index].PRNumber = &number
			}
			if issues[candidate.Index].PRRepository == "" {
				issues[candidate.Index].PRRepository = pullRequestRepoName(repo)
			}
		}
	}
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

func (c *Connector) fetchRepositoryPullRequest(ctx context.Context, repo pullRequestRepo, number int) (pullRequestNode, error) {
	var response restPullRequest
	if err := c.client.REST(ctx, http.MethodGet, restPullRequestPath(repo, number), nil, &response); err != nil {
		return pullRequestNode{}, fmt.Errorf("fetch github pull request: %w", err)
	}
	return pullRequestNodeFromREST(response), nil
}

func (c *Connector) HydratePullRequest(ctx context.Context, issue connector.Issue) (connector.Issue, error) {
	repo, number, ok := hydratedPullRequestRef(issue)
	if !ok {
		return issue, nil
	}
	pullRequest, err := c.fetchRepositoryPullRequest(ctx, repo, number)
	if err != nil {
		if state := c.pullRequestHydrationStateForError(repo, err); state.Reason != "" {
			attachPullRequestHydrationUnavailableToIssue(&issue, repo, number, state)
			return issue, nil
		}
		return issue, fmt.Errorf("hydrate github pull request: %w", err)
	}
	if err := c.populatePullRequestStatus(ctx, repo, &pullRequest, false); err != nil {
		if state := c.pullRequestHydrationStateForError(repo, err); state.Reason != "" {
			applyPullRequestHydrationUnavailableState(&pullRequest, state)
		} else {
			return issue, fmt.Errorf("hydrate github pull request status: %w", err)
		}
	}
	attachPullRequestToIssue(&issue, repo, pullRequest)
	return issue, nil
}

func (c *Connector) MergePullRequest(ctx context.Context, repository string, number int, headSHA string) error {
	repo, ok := pullRequestRepoFromName(repository)
	if !ok || number <= 0 {
		return fmt.Errorf("merge github pull request: invalid pull request %s#%d", strings.TrimSpace(repository), number)
	}
	body := map[string]string{
		"merge_method": "squash",
	}
	if headSHA = strings.TrimSpace(headSHA); headSHA != "" {
		body["sha"] = headSHA
	}
	var response restPullRequestMergeResponse
	if err := c.client.REST(ctx, http.MethodPut, restPullRequestMergePath(repo, number), body, &response); err != nil {
		return fmt.Errorf("merge github pull request: %w", err)
	}
	if !response.Merged {
		message := strings.TrimSpace(response.Message)
		if message == "" {
			message = "github did not merge pull request"
		}
		return fmt.Errorf("merge github pull request: %s", message)
	}
	return nil
}

func hydratedPullRequestRef(issue connector.Issue) (pullRequestRepo, int, bool) {
	number := 0
	if issue.PullRequest != nil && issue.PullRequest.Number > 0 {
		number = issue.PullRequest.Number
	}
	if number <= 0 && issue.PRNumber != nil {
		number = *issue.PRNumber
	}
	if number <= 0 {
		return pullRequestRepo{}, 0, false
	}
	if repo, ok := pullRequestRepoFromName(issue.PRRepository); ok {
		return repo, number, true
	}
	if repo, ok := pullRequestRepoFromIdentifier(issue.Identifier); ok {
		return repo, number, true
	}
	return pullRequestRepo{}, 0, false
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
		pullRequests = append(pullRequests, pullRequestNodeFromREST(pullRequest))
	}
	return pullRequests, nil
}

func (c *Connector) attachLinkedPullRequests(
	ctx context.Context,
	repo pullRequestRepo,
	issues []connector.Issue,
	candidates []issuePullRequestCandidate,
	useStatusCache bool,
) error {
	pullRequests := map[pullRequestKey]pullRequestNode{}
	for _, candidate := range candidates {
		if issues[candidate.Index].PullRequest != nil || candidate.PullRequestNumber <= 0 {
			continue
		}
		pullRequestRepo := candidate.PullRequestRepo
		if strings.TrimSpace(pullRequestRepo.Owner) == "" || strings.TrimSpace(pullRequestRepo.Name) == "" {
			pullRequestRepo = repo
		}
		if state, ok := c.currentPullRequestHydrationState(pullRequestRepo); ok {
			c.logPullRequestHydrationSkip(ctx, pullRequestRepo, state, "linked_pull_request")
			attachPullRequestHydrationUnavailableToIssue(&issues[candidate.Index], pullRequestRepo, candidate.PullRequestNumber, state)
			continue
		}
		key := pullRequestKey{Repo: pullRequestRepo, Number: candidate.PullRequestNumber}
		pullRequest, ok := pullRequests[key]
		if !ok {
			var err error
			pullRequest, err = c.fetchRepositoryPullRequest(ctx, pullRequestRepo, candidate.PullRequestNumber)
			if err != nil {
				if state := c.pullRequestHydrationStateForError(pullRequestRepo, err); state.Reason != "" {
					attachPullRequestHydrationUnavailableToIssue(&issues[candidate.Index], pullRequestRepo, candidate.PullRequestNumber, state)
					continue
				}
				return err
			}
			if err := c.populatePullRequestStatus(ctx, pullRequestRepo, &pullRequest, useStatusCache); err != nil {
				if state := c.pullRequestHydrationStateForError(pullRequestRepo, err); state.Reason != "" {
					applyPullRequestHydrationUnavailableState(&pullRequest, state)
				} else {
					return err
				}
			}
			pullRequests[key] = pullRequest
		}
		attachPullRequestToIssue(&issues[candidate.Index], pullRequestRepo, pullRequest)
	}
	return nil
}

func (c *Connector) attachMatchingPullRequests(
	ctx context.Context,
	repo pullRequestRepo,
	issues []connector.Issue,
	candidates []issuePullRequestCandidate,
	pullRequests []pullRequestNode,
	useStatusCache bool,
) error {
	hydrated := map[int]pullRequestNode{}
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

			hydratedPullRequest, ok := hydrated[pullRequest.Number]
			if !ok {
				var err error
				hydratedPullRequest, err = c.fetchRepositoryPullRequest(ctx, repo, pullRequest.Number)
				if err != nil {
					if state := c.pullRequestHydrationStateForError(repo, err); state.Reason != "" {
						applyPullRequestHydrationUnavailableState(&pullRequest, state)
						hydrated[pullRequest.Number] = pullRequest
						attachPullRequestToIssue(&issues[candidate.Index], repo, pullRequest)
						markPullRequestHydrationUnavailableForCandidates(issues, candidates, repo, state)
						return nil
					}
					return err
				}
				if err := c.populatePullRequestStatus(ctx, repo, &hydratedPullRequest, useStatusCache); err != nil {
					if state := c.pullRequestHydrationStateForError(repo, err); state.Reason != "" {
						applyPullRequestHydrationUnavailableState(&hydratedPullRequest, state)
						hydrated[pullRequest.Number] = hydratedPullRequest
						attachPullRequestToIssue(&issues[candidate.Index], repo, hydratedPullRequest)
						markPullRequestHydrationUnavailableForCandidates(issues, candidates, repo, state)
						return nil
					} else {
						return err
					}
				}
				hydrated[pullRequest.Number] = hydratedPullRequest
			}
			attachPullRequestToIssue(&issues[candidate.Index], repo, hydratedPullRequest)
		}
	}
	return nil
}

func hasUnattachedBranchPullRequestCandidates(issues []connector.Issue, candidates []issuePullRequestCandidate) bool {
	for _, candidate := range candidates {
		if issues[candidate.Index].PullRequest == nil && strings.TrimSpace(candidate.BranchPrefix) != "" {
			return true
		}
	}
	return false
}

func pullRequestNodeFromREST(pullRequest restPullRequest) pullRequestNode {
	return pullRequestNode{
		Number:         pullRequest.Number,
		URL:            pullRequest.HTMLURL,
		State:          restPullRequestState(pullRequest),
		MergeableState: strings.ToLower(strings.TrimSpace(pullRequest.MergeableState)),
		Draft:          pullRequest.Draft,
		ActivityAt:     cloneGitHubTime(pullRequest.UpdatedAt),
		HeadRefName:    pullRequest.Head.Ref,
		HeadSHA:        pullRequest.Head.SHA,
		BaseSHA:        pullRequest.Base.SHA,
	}
}

func attachPullRequestToIssue(issue *connector.Issue, repo pullRequestRepo, pullRequest pullRequestNode) {
	issue.PullRequest = &connector.PullRequest{
		Number:                       pullRequest.Number,
		URL:                          strings.TrimSpace(pullRequest.URL),
		BranchName:                   strings.TrimSpace(pullRequest.HeadRefName),
		State:                        strings.ToUpper(strings.TrimSpace(pullRequest.State)),
		MergeableState:               strings.ToLower(strings.TrimSpace(pullRequest.MergeableState)),
		Draft:                        pullRequest.Draft,
		ActivityAt:                   cloneGitHubTime(pullRequest.ActivityAt),
		HeadSHA:                      strings.TrimSpace(pullRequest.HeadSHA),
		BaseSHA:                      strings.TrimSpace(pullRequest.BaseSHA),
		HydrationUnavailableReason:   strings.TrimSpace(pullRequest.HydrationUnavailableReason),
		HydrationDegradedReason:      strings.TrimSpace(pullRequest.HydrationDegradedReason),
		HydrationNextRetryAt:         cloneGitHubTime(pullRequest.HydrationNextRetryAt),
		CIStatus:                     normalizePullRequestCIStatus(pullRequestCIState(pullRequest)),
		CheckRunCount:                pullRequest.CI.CheckRunCount,
		StatusContextCount:           pullRequest.CI.StatusContextCount,
		CIQueueSeconds:               pullRequest.CI.CIQueueSeconds,
		CIDurationSeconds:            pullRequest.CI.CIDurationSeconds,
		SlowChecks:                   append([]connector.PullRequestCheck(nil), pullRequest.CI.SlowChecks...),
		RunningChecks:                append([]string(nil), pullRequest.CI.RunningChecks...),
		CodexReviewState:             pullRequestCodexReviewState(pullRequest),
		CodexReviewSubmittedAt:       pullRequestCodexReviewSubmittedAt(pullRequest),
		CodexReviewFindings:          pullRequestCodexReviewFindings(pullRequest),
		LatestCodexReviewState:       pullRequestLatestCodexReviewState(pullRequest),
		LatestCodexReviewCommitSHA:   pullRequestLatestCodexReviewCommitSHA(pullRequest),
		LatestCodexReviewSubmittedAt: pullRequestLatestCodexReviewSubmittedAt(pullRequest),
	}
	if issue.PRNumber == nil && pullRequest.Number > 0 {
		number := pullRequest.Number
		issue.PRNumber = &number
	}
	if issue.PRRepository == "" {
		issue.PRRepository = pullRequestRepoName(repo)
	}
}

func markPullRequestHydrationUnavailableForCandidates(
	issues []connector.Issue,
	candidates []issuePullRequestCandidate,
	defaultRepo pullRequestRepo,
	state pullRequestHydrationState,
) {
	for _, candidate := range candidates {
		if issues[candidate.Index].PullRequest != nil {
			continue
		}
		repo := candidate.PullRequestRepo
		if strings.TrimSpace(repo.Owner) == "" || strings.TrimSpace(repo.Name) == "" {
			repo = defaultRepo
		}
		attachPullRequestHydrationUnavailableToIssue(&issues[candidate.Index], repo, candidate.PullRequestNumber, state)
	}
}

func attachPullRequestHydrationUnavailableToIssue(issue *connector.Issue, repo pullRequestRepo, number int, state pullRequestHydrationState) {
	if strings.TrimSpace(state.Reason) == "" {
		return
	}
	if issue.PullRequest == nil {
		issue.PullRequest = &connector.PullRequest{}
	}
	if number > 0 {
		issue.PullRequest.Number = number
	}
	issue.PullRequest.HydrationUnavailableReason = strings.TrimSpace(state.Reason)
	issue.PullRequest.HydrationNextRetryAt = cloneGitHubTime(state.NextRetryAt)
	if issue.PRNumber == nil && number > 0 {
		issue.PRNumber = &number
	}
	if issue.PRRepository == "" {
		issue.PRRepository = pullRequestRepoName(repo)
	}
}

func applyPullRequestHydrationUnavailableState(pullRequest *pullRequestNode, state pullRequestHydrationState) {
	if pullRequest == nil || strings.TrimSpace(state.Reason) == "" {
		return
	}
	pullRequest.HydrationUnavailableReason = strings.TrimSpace(state.Reason)
	pullRequest.HydrationNextRetryAt = cloneGitHubTime(state.NextRetryAt)
}

func (c *Connector) currentPullRequestHydrationState(repo pullRequestRepo) (pullRequestHydrationState, bool) {
	if c == nil || c.prHydration == nil {
		return pullRequestHydrationState{}, false
	}
	return c.prHydration.Current(repo)
}

func (c *Connector) pullRequestHydrationStateForError(repo pullRequestRepo, err error) pullRequestHydrationState {
	switch {
	case errors.Is(err, ErrRESTBudgetReserved):
		return pullRequestHydrationState{Reason: connector.PullRequestHydrationReasonRESTBudgetReserved}
	case errors.Is(err, ErrRateLimited):
		var statusErr *StatusError
		if errors.As(err, &statusErr) {
			switch statusErr.RateLimitKind {
			case restRateLimitKindSecondaryThrottled:
				return c.tripPullRequestHydrationCircuit(repo, statusErr.RetryAfter)
			case restRateLimitKindPrimaryExhausted:
				return newPullRequestHydrationState(
					connector.PullRequestHydrationReasonPrimaryExhausted,
					c.pullRequestHydrationRetryAt(statusErr),
				)
			}
		}
		return pullRequestHydrationState{Reason: connector.PullRequestHydrationReasonRateLimited}
	default:
		return pullRequestHydrationState{}
	}
}

func (c *Connector) tripPullRequestHydrationCircuit(repo pullRequestRepo, retryAfter time.Duration) pullRequestHydrationState {
	reason := connector.PullRequestHydrationReasonSecondaryThrottled
	if c == nil || c.prHydration == nil {
		return pullRequestHydrationState{Reason: reason}
	}
	state := c.prHydration.Trip(repo, reason, retryAfter)
	if strings.TrimSpace(state.Reason) == "" {
		return pullRequestHydrationState{Reason: reason}
	}
	return state
}

func (c *Connector) pullRequestHydrationRetryAt(statusErr *StatusError) time.Time {
	if statusErr == nil {
		return time.Time{}
	}
	now := time.Now()
	if c != nil && c.prHydration != nil && c.prHydration.now != nil {
		now = c.prHydration.now()
	}
	if statusErr.RetryAfter > 0 {
		return now.Add(statusErr.RetryAfter)
	}
	if statusErr.ResetAt.After(now) {
		return statusErr.ResetAt
	}
	return time.Time{}
}

func pullRequestRepoName(repo pullRequestRepo) string {
	owner := strings.TrimSpace(repo.Owner)
	name := strings.TrimSpace(repo.Name)
	if owner == "" || name == "" {
		return ""
	}
	return owner + "/" + name
}

func (c *Connector) populatePullRequestStatus(ctx context.Context, repo pullRequestRepo, pullRequest *pullRequestNode, useStatusCache bool) error {
	if useStatusCache && c.pullRequests != nil {
		if status, ok := c.pullRequests.Get(repo, pullRequest.Number, pullRequest.HeadSHA); ok {
			c.logPullRequestCache(ctx, repo, pullRequest, true, false, "")
			applyPullRequestStatus(pullRequest, status)
			return nil
		}
		c.logPullRequestCache(ctx, repo, pullRequest, false, false, "")
	}

	status := pullRequestStatus{}
	if strings.TrimSpace(pullRequest.HeadSHA) != "" {
		ci, err := c.fetchPullRequestCI(ctx, repo, pullRequest.HeadSHA)
		if err != nil {
			state := c.pullRequestHydrationStateForError(repo, err)
			if c.applyCachedPullRequestStatusAfterThrottle(ctx, repo, pullRequest, state) {
				return nil
			}
			return err
		}
		status.ci = ci
	}
	reviews, err := c.fetchPullRequestReviews(ctx, repo, pullRequest.Number, pullRequest.HeadSHA)
	if err != nil {
		state := c.pullRequestHydrationStateForError(repo, err)
		if c.applyCachedPullRequestStatusAfterThrottle(ctx, repo, pullRequest, state) {
			return nil
		}
		return err
	}
	status.reviews = reviews
	if c.pullRequests != nil {
		c.pullRequests.Set(repo, pullRequest.Number, pullRequest.HeadSHA, status)
		c.logPullRequestCache(ctx, repo, pullRequest, false, false, "stored")
	}
	applyPullRequestStatus(pullRequest, status)
	return nil
}

func (c *Connector) applyCachedPullRequestStatusAfterThrottle(ctx context.Context, repo pullRequestRepo, pullRequest *pullRequestNode, state pullRequestHydrationState) bool {
	if c.pullRequests == nil || pullRequest == nil {
		return false
	}
	if strings.TrimSpace(state.Reason) == "" {
		return false
	}
	status, ok := c.pullRequests.Get(repo, pullRequest.Number, pullRequest.HeadSHA)
	if !ok {
		return false
	}
	c.logPullRequestCache(ctx, repo, pullRequest, true, true, state.Reason)
	applyPullRequestStatus(pullRequest, status)
	pullRequest.HydrationDegradedReason = connector.PullRequestHydrationReasonStaleCachedPullData
	pullRequest.HydrationNextRetryAt = cloneGitHubTime(state.NextRetryAt)
	return true
}

func (c *Connector) logPullRequestHydrationSkip(ctx context.Context, repo pullRequestRepo, state pullRequestHydrationState, purpose string) {
	if c == nil || c.logger == nil {
		return
	}
	c.logger.DebugContext(ctx, "github pull request hydration skipped",
		"endpoint_family", "pull requests",
		"request_purpose", "hydrate_pull_request",
		"repository", pullRequestRepoName(repo),
		"cache_hit", true,
		"avoidable_request", true,
		"backoff_reason", strings.TrimSpace(state.Reason),
		"purpose", strings.TrimSpace(purpose),
		"retry_at", state.NextRetryAt,
	)
}

func (c *Connector) logPullRequestCache(ctx context.Context, repo pullRequestRepo, pullRequest *pullRequestNode, hit bool, staleFallback bool, reason string) {
	if c == nil || c.logger == nil || pullRequest == nil {
		return
	}
	c.logger.DebugContext(ctx, "github pull request status cache",
		"endpoint_family", "pull_request_status_cache",
		"request_purpose", "hydrate_pull_request_status",
		"repository", pullRequestRepoName(repo),
		"pr_number", pullRequest.Number,
		"head_sha_known", strings.TrimSpace(pullRequest.HeadSHA) != "",
		"cache_hit", hit,
		"avoidable_request", hit,
		"stale_fallback", staleFallback,
		"backoff_reason", strings.TrimSpace(reason),
	)
}

func applyPullRequestStatus(pullRequest *pullRequestNode, status pullRequestStatus) {
	pullRequest.CI = clonePullRequestCI(status.ci)
	pullRequest.Commits = nodeConnection[pullRequestCommit]{Nodes: []pullRequestCommit{{
		Commit: commitNode{StatusCheckRollup: &statusCheckRollup{State: status.ci.State}},
	}}}
	pullRequest.LatestReviews = nodeConnection[pullRequestReview]{Nodes: clonePullRequestReviews(status.reviews.CurrentHead)}
	pullRequest.CodexReviews = clonePullRequestCodexReviews(status.reviews)
}

func (c *Connector) fetchPullRequestCI(ctx context.Context, repo pullRequestRepo, sha string) (pullRequestCI, error) {
	checkRuns, err := fetchRESTCheckRuns(ctx, c.client, restCommitCheckRunsPath(repo, sha))
	if err != nil {
		return pullRequestCI{}, fmt.Errorf("fetch github check runs: %w", err)
	}
	workflowRuns, workflowRunErr := fetchRESTWorkflowRunsForCheckRuns(ctx, c.client, repo, checkRuns)
	if workflowRunErr != nil {
		workflowRuns = nil
	}
	statuses, err := fetchRESTList[restCommitStatus](ctx, c.client, restCommitStatusesPath(repo, sha))
	if err != nil {
		return pullRequestCI{}, fmt.Errorf("fetch github commit statuses: %w", err)
	}
	telemetry := checkRunTelemetry(checkRuns, workflowRuns)
	return pullRequestCI{
		State:              combinedCIState(checkRunsState(checkRuns), commitStatusesState(statuses)),
		CheckRunCount:      len(checkRuns),
		StatusContextCount: len(statuses),
		CIQueueSeconds:     telemetry.QueueSeconds,
		CIDurationSeconds:  telemetry.DurationSeconds,
		SlowChecks:         telemetry.SlowChecks,
		RunningChecks:      telemetry.RunningChecks,
	}, nil
}

func (c *Connector) fetchPullRequestReviews(ctx context.Context, repo pullRequestRepo, number int, headSHA string) (pullRequestCodexReviews, error) {
	response, err := fetchRESTList[restReview](ctx, c.client, restPullRequestReviewsPath(repo, number))
	if err != nil {
		return pullRequestCodexReviews{}, fmt.Errorf("fetch github pull request reviews: %w", err)
	}
	reviews := pullRequestCodexReviews{}
	if review, ok := latestCodexReview(response, headSHA); ok {
		reviews.CurrentHead = []pullRequestReview{review}
	}
	if review, ok := latestCodexReview(response, ""); ok {
		reviews.Latest = []pullRequestReview{review}
	}
	return reviews, nil
}

func (c *Connector) fetchProjectItems(ctx context.Context, queryType string, query string, keepIssue func(connector.Issue) bool) ([]connector.Issue, error) {
	return c.fetchProjectItemsLimit(ctx, queryType, query, keepIssue, 0)
}

func (c *Connector) fetchProjectItemsLimit(
	ctx context.Context,
	queryType string,
	query string,
	keepIssue func(connector.Issue) bool,
	limit int,
) ([]connector.Issue, error) {
	return c.fetchProjectItemsWithLimit(ctx, projectItemsQueryForType(queryType), queryType, query, keepIssue, limit)
}

func (c *Connector) fetchProjectItemsWithPullRequestRefsLimit(
	ctx context.Context,
	queryType string,
	query string,
	keepIssue func(connector.Issue) bool,
	limit int,
) ([]connector.Issue, error) {
	return c.fetchProjectItemsWithLimit(ctx, observedStatusProjectItemsQuery, queryType, query, keepIssue, limit)
}

func (c *Connector) fetchProjectItemsWithLimit(
	ctx context.Context,
	queryDocument string,
	queryType string,
	query string,
	keepIssue func(connector.Issue) bool,
	limit int,
) ([]connector.Issue, error) {
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
		if err := c.client.GraphQLWithType(ctx, queryType, queryDocument, map[string]any{
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
			if limit > 0 && keepIssue(issue) {
				issues := keptProjectIssues(allIssues, keepIssue, limit)
				if len(issues) >= limit {
					c.defaultBlankProjectItemStatuses(ctx, blankStatusItemIDs)
					resolveBlockedByProjectState(issues)
					return issues, nil
				}
			}
		}

		if !response.Node.Items.PageInfo.HasNextPage {
			c.defaultBlankProjectItemStatuses(ctx, blankStatusItemIDs)
			resolveBlockedByProjectState(allIssues)
			return keptProjectIssues(allIssues, keepIssue, limit), nil
		}
		cursor := strings.TrimSpace(response.Node.Items.PageInfo.EndCursor)
		if cursor == "" {
			return nil, ErrInvalidResponse
		}
		after = &cursor
	}
}

func projectItemsQueryForType(queryType string) string {
	if queryType == graphQLQueryObservedStatus {
		return observedStatusProjectItemsQuery
	}
	return projectItemsQuery
}

func keptProjectIssues(allIssues []connector.Issue, keepIssue func(connector.Issue) bool, limit int) []connector.Issue {
	issues := make([]connector.Issue, 0, len(allIssues))
	for _, issue := range allIssues {
		if !keepIssue(issue) {
			continue
		}
		issues = append(issues, issue)
		if limit > 0 && len(issues) >= limit {
			return issues
		}
	}
	return issues
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

func connectorIssueKey(issue connector.Issue) string {
	if id := strings.TrimSpace(issue.ID); id != "" {
		return "id:" + id
	}
	if identifier := normalizedIssueIdentifier(issue.Identifier); identifier != "" {
		return "identifier:" + identifier
	}
	return ""
}

func (c *Connector) normalizeIssueNode(ctx context.Context, issue githubIssueNode) (connector.Issue, bool, error) {
	if issue.TypeName != "Issue" {
		return connector.Issue{}, false, nil
	}
	if c.usesLabelStatus() {
		return c.buildLabelIssue(issue, c.githubIssueStateToDetentState(issue.State)), true, nil
	}
	stateName, priorityName, statusUpdatedAt, fields, ok, err := c.resolveIssueProjectFields(ctx, issue.ID, issue.ProjectItems)
	if err != nil {
		return connector.Issue{}, false, err
	}
	if ok {
		return c.buildIssue(issue, stateName, priorityName, statusUpdatedAt, fields), true, nil
	}
	return connector.Issue{}, false, nil
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
	if err := c.client.GraphQLWithType(ctx, graphQLQueryProjectItem, projectItemForIssueQuery, map[string]any{
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
	pullRequestRef, hasPullRequestRef := firstPullRequestReference(issue.ClosedByPullRequestsReferences)
	var pullRequestNumber *int
	var pullRequestRepository string
	if hasPullRequestRef {
		number := pullRequestRef.Number
		pullRequestNumber = &number
		pullRequestRepository = pullRequestRef.Repository
		if pullRequestRepository == "" {
			pullRequestRepository = repo
		}
	}
	return connector.Issue{
		ID:               issue.ID,
		Identifier:       buildIdentifier(repo, issue.Number),
		Title:            issue.Title,
		Description:      issue.Body,
		Priority:         c.priorityRank(priorityName),
		State:            c.githubToDetentState(statusName),
		URL:              issue.URL,
		Closed:           githubIssueClosed(issue.State),
		ClosedReason:     issue.StateReason,
		PRNumber:         pullRequestNumber,
		PRRepository:     pullRequestRepository,
		AuthorID:         actorLogin(issue.Author),
		AssigneeID:       firstAssigneeLogin(issue.Assignees),
		Assignees:        allAssigneeLogins(issue.Assignees),
		BlockedBy:        parseBlockedBy(issue.Body, repo),
		ChildIssues:      c.linkedChildIssues(issue, repo),
		BlockerReason:    parseBlockerReason(issue),
		Labels:           labelNames(issue.Labels),
		Comments:         connectorIssueComments(issue.Comments.Nodes),
		Fields:           cloneStringMap(fields),
		AssignedToWorker: true,
		CreatedAt:        parseGitHubTime(issue.CreatedAt),
		UpdatedAt:        parseGitHubTime(issue.UpdatedAt),
		StageUpdatedAt:   statusUpdatedAt,
		ModelOverride:    parseModelOverride(issue.Body),
	}
}

func connectorIssueComments(comments []issueComment) []connector.IssueComment {
	if len(comments) == 0 {
		return nil
	}
	out := make([]connector.IssueComment, 0, len(comments))
	for _, comment := range comments {
		out = append(out, connector.IssueComment{
			Body: comment.Body,
			URL:  comment.URL,
		})
	}
	return out
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
	if c.usesLabelStatus() {
		resolution := c.labelStatusResolutionFromLabels(child.Labels)
		if resolution.conflicted() {
			return labelStatusConflictState
		}
		if resolution.Status != "" {
			return c.githubToDetentState(resolution.Status)
		}
		return c.githubIssueStateToDetentState(child.State)
	}
	state := c.githubIssueStateToDetentState(child.State)
	if stateName, _, _, _, ok := c.projectFields(child.ID, child.ProjectItems); ok {
		if stateName = strings.TrimSpace(stateName); stateName != "" {
			state = c.githubToDetentState(stateName)
		}
		return state
	}
	if child.ProjectItems != nil && child.ProjectItems.PageInfo.HasNextPage {
		return ""
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
	if err := c.client.GraphQLWithType(ctx, graphQLQueryProjectMetadata, singleSelectFieldQuery, map[string]any{
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
	if err := c.client.GraphQLWithType(ctx, graphQLQueryProjectMetadata, statusFieldQuery, map[string]any{"projectId": c.projectID}, &response); err != nil {
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
	if err := c.client.GraphQLWithType(ctx, graphQLQueryProjectItem, projectItemForIssueQuery, map[string]any{
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
	if err := c.client.GraphQLWithType(ctx, graphQLQueryUpdateField, updateSingleSelectFieldValueMutation, map[string]any{
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
	if err := c.client.GraphQLWithType(ctx, graphQLQueryUpdateField, updateTextFieldValueMutation, map[string]any{
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

func (c *Connector) deleteProjectItem(ctx context.Context, itemID string) error {
	itemID = strings.TrimSpace(itemID)
	if itemID == "" {
		return ErrProjectItemRemoveFailed
	}

	var response struct {
		DeleteProjectV2Item *struct {
			DeletedItemID string `json:"deletedItemId"`
		} `json:"deleteProjectV2Item"`
	}
	if err := c.client.GraphQLWithType(ctx, graphQLQueryRemoveItem, deleteProjectItemMutation, map[string]any{
		"projectId": c.projectID,
		"itemId":    itemID,
	}, &response); err != nil {
		return fmt.Errorf("remove github project item: %w", err)
	}
	if response.DeleteProjectV2Item == nil || strings.TrimSpace(response.DeleteProjectV2Item.DeletedItemID) == "" {
		return ErrProjectItemRemoveFailed
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
	if state := c.configuredDetentState(githubState); state != "" {
		return state
	}
	for detentState, mapped := range c.stateMap {
		if normalizeStateName(mapped) == normalizeStateName(githubState) {
			return strings.TrimSpace(detentState)
		}
	}
	return githubState
}

func (c *Connector) configuredDetentState(stateName string) string {
	stateName = normalizeStateName(stateName)
	if stateName == "" {
		return ""
	}
	for _, candidate := range c.configuredStatusStates() {
		candidate = strings.TrimSpace(candidate)
		if normalizeStateName(candidate) == stateName {
			return candidate
		}
	}
	return ""
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

func issueRefFromRESTSearchItem(issue restIssue, fallback issueRef) (issueRef, bool) {
	if ref, ok := issueRefFromURL(issue.HTMLURL); ok {
		return ref, true
	}
	if issue.Number <= 0 || fallback.Owner == "" || fallback.Name == "" {
		return issueRef{}, false
	}
	return issueRef{Owner: fallback.Owner, Name: fallback.Name, Number: issue.Number}, true
}

func issueRefFromURL(value string) (issueRef, bool) {
	matches := issueURLPattern.FindStringSubmatch(value)
	if len(matches) != 3 {
		return issueRef{}, false
	}
	owner, name, ok := splitRepositoryName(matches[1])
	if !ok {
		return issueRef{}, false
	}
	number, err := strconv.Atoi(matches[2])
	if err != nil || number <= 0 {
		return issueRef{}, false
	}
	return issueRef{Owner: owner, Name: name, Number: number}, true
}

func sameIssueRef(left issueRef, right issueRef) bool {
	return strings.EqualFold(left.Owner, right.Owner) &&
		strings.EqualFold(left.Name, right.Name) &&
		left.Number == right.Number
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

func restIssueSearchPath(ref issueRef, page int) string {
	values := url.Values{}
	values.Set("q", "user:"+ref.Owner+" is:issue is:open "+strconv.Itoa(ref.Number))
	values.Set("per_page", strconv.Itoa(bodyParentSearchPageSize))
	values.Set("page", strconv.Itoa(page))
	return "/search/issues?" + values.Encode()
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

func restPullRequestPath(repo pullRequestRepo, number int) string {
	return "/repos/" + url.PathEscape(repo.Owner) + "/" + url.PathEscape(repo.Name) + "/pulls/" + strconv.Itoa(number)
}

func restPullRequestMergePath(repo pullRequestRepo, number int) string {
	return restPullRequestPath(repo, number) + "/merge"
}

func restPullRequestReviewsPath(repo pullRequestRepo, number int) string {
	return "/repos/" + url.PathEscape(repo.Owner) + "/" + url.PathEscape(repo.Name) + "/pulls/" + strconv.Itoa(number) + "/reviews?per_page=100"
}

func restCommitCheckRunsPath(repo pullRequestRepo, sha string) string {
	values := url.Values{}
	values.Set("per_page", "100")
	return "/repos/" + url.PathEscape(repo.Owner) + "/" + url.PathEscape(repo.Name) + "/commits/" + url.PathEscape(sha) + "/check-runs?" + values.Encode()
}

func restWorkflowRunPath(repo pullRequestRepo, runID int64) string {
	return "/repos/" + url.PathEscape(repo.Owner) + "/" + url.PathEscape(repo.Name) + "/actions/runs/" + strconv.FormatInt(runID, 10)
}

func restCommitStatusesPath(repo pullRequestRepo, sha string) string {
	values := url.Values{}
	values.Set("per_page", "100")
	return "/repos/" + url.PathEscape(repo.Owner) + "/" + url.PathEscape(repo.Name) + "/commits/" + url.PathEscape(sha) + "/statuses?" + values.Encode()
}

func fetchRESTList[T any](ctx context.Context, client *Client, path string) ([]T, error) {
	values := []T{}
	for path != "" {
		var page []T
		headers, err := client.rest(ctx, http.MethodGet, path, nil, &page)
		if err != nil {
			return nil, err
		}
		values = append(values, page...)
		path, err = client.nextRESTPage(headers)
		if err != nil {
			return nil, err
		}
	}
	return values, nil
}

func fetchRESTCheckRuns(ctx context.Context, client *Client, path string) ([]restCheckRun, error) {
	checkRuns := []restCheckRun{}
	for path != "" {
		var page restCheckRuns
		headers, err := client.rest(ctx, http.MethodGet, path, nil, &page)
		if err != nil {
			return nil, err
		}
		checkRuns = append(checkRuns, page.CheckRuns...)
		path, err = client.nextRESTPage(headers)
		if err != nil {
			return nil, err
		}
	}
	return checkRuns, nil
}

func fetchRESTWorkflowRunsForCheckRuns(ctx context.Context, client *Client, repo pullRequestRepo, checkRuns []restCheckRun) ([]restWorkflowRun, error) {
	runIDs := checkRunWorkflowRunIDs(checkRuns)
	runs := make([]restWorkflowRun, 0, len(runIDs))
	for _, runID := range runIDs {
		var run restWorkflowRun
		_, err := client.rest(ctx, http.MethodGet, restWorkflowRunPath(repo, runID), nil, &run)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, nil
}

func checkRunWorkflowRunIDs(checkRuns []restCheckRun) []int64 {
	seen := map[int64]struct{}{}
	for _, checkRun := range checkRuns {
		match := actionRunURLPattern.FindStringSubmatch(strings.TrimSpace(checkRun.DetailsURL))
		if len(match) != 2 {
			continue
		}
		runID, err := strconv.ParseInt(match[1], 10, 64)
		if err != nil || runID <= 0 {
			continue
		}
		seen[runID] = struct{}{}
	}
	runIDs := make([]int64, 0, len(seen))
	for runID := range seen {
		runIDs = append(runIDs, runID)
	}
	sort.Slice(runIDs, func(i, j int) bool {
		return runIDs[i] < runIDs[j]
	})
	return runIDs
}

func githubIssueNodeFromREST(ref issueRef, issue restIssue) githubIssueNode {
	repo := ref.Owner + "/" + ref.Name
	return githubIssueNode{
		TypeName:    "Issue",
		ID:          strings.TrimSpace(issue.NodeID),
		Number:      issue.Number,
		Title:       issue.Title,
		Body:        restStringValue(issue.Body),
		State:       issue.State,
		StateReason: issue.StateReason,
		URL:         issue.HTMLURL,
		CreatedAt:   restTimeString(issue.CreatedAt),
		UpdatedAt:   restTimeString(issue.UpdatedAt),
		Author:      issue.User,
		Assignees:   restAssigneesConnection(issue.Assignees),
		Labels:      nodeConnection[label]{Nodes: issue.Labels},
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
	if legacyPrefix, ok := strings.CutPrefix(prefix, "detent/"); ok {
		for _, suffix := range []string{"_", "-", "/"} {
			if strings.HasPrefix(branchName, "detent/detent-"+legacyPrefix+suffix) {
				return true
			}
		}
		if branchMatchesCurrentDetentPrefix(branchName, legacyPrefix) {
			return true
		}
		if number := issueNumberFromBranchPrefix(legacyPrefix); number != "" {
			if branchName == "detent/"+number {
				return true
			}
			for _, suffix := range []string{"_", "-", "/"} {
				if strings.HasPrefix(branchName, "detent/"+number+suffix) {
					return true
				}
			}
		}
	}
	return false
}

func branchMatchesCurrentDetentPrefix(branchName string, issueKey string) bool {
	branchStem, ok := strings.CutPrefix(branchName, "detent/")
	if !ok {
		return false
	}
	digestSeparator := strings.LastIndex(branchStem, "-")
	if digestSeparator <= 0 || digestSeparator == len(branchStem)-1 {
		return false
	}
	return strings.HasSuffix(branchStem[:digestSeparator], "-"+issueKey)
}

func issueNumberFromBranchPrefix(prefix string) string {
	prefix = strings.TrimSpace(prefix)
	index := strings.LastIndexAny(prefix, "_-")
	if index < 0 || index == len(prefix)-1 {
		return ""
	}
	number := prefix[index+1:]
	for _, r := range number {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return number
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

type pullRequestReference struct {
	Number     int
	Repository string
}

func firstPullRequestReference(pullRequests nodeConnection[pullRequest]) (pullRequestReference, bool) {
	var fallback pullRequestReference
	fallbackOK := false
	for _, pullRequest := range pullRequests.Nodes {
		if pullRequest.Number <= 0 {
			continue
		}
		ref := pullRequestReference{
			Number:     pullRequest.Number,
			Repository: strings.TrimSpace(pullRequest.Repository.NameWithOwner),
		}
		if !fallbackOK {
			fallback = ref
			fallbackOK = true
		}
		if normalizeStateName(pullRequest.State) == "open" {
			return ref, true
		}
	}
	return fallback, fallbackOK
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

func checkRunTelemetry(checkRuns []restCheckRun, workflowRuns []restWorkflowRun) checkRunTelemetrySummary {
	var queueCreatedAt *time.Time
	var queueStartedAt *time.Time
	var checkStartedAt *time.Time
	var completedAt *time.Time
	hasRunning := false
	slowChecks := make([]connector.PullRequestCheck, 0, len(checkRuns))
	runningChecks := make([]string, 0, len(checkRuns))

	for _, run := range workflowRuns {
		queueCreatedAt = earliestGitHubTime(queueCreatedAt, run.CreatedAt)
		queueStartedAt = earliestGitHubTime(queueStartedAt, run.RunStartedAt)
	}

	for _, checkRun := range checkRuns {
		queueCreatedAt = earliestGitHubTime(queueCreatedAt, checkRun.CreatedAt)
		queueStartedAt = earliestGitHubTime(queueStartedAt, checkRun.StartedAt)
		checkStartedAt = earliestGitHubTime(checkStartedAt, checkRun.StartedAt)
		completedAt = latestGitHubTime(completedAt, checkRun.CompletedAt)

		name := strings.TrimSpace(checkRun.Name)
		status := strings.ToLower(strings.TrimSpace(checkRun.Status))
		conclusion := strings.ToLower(strings.TrimSpace(checkRun.Conclusion))
		if status != "completed" || conclusion == "" {
			hasRunning = true
			runningChecks = append(runningChecks, name)
			continue
		}
		if name == "" || checkRun.StartedAt == nil || checkRun.CompletedAt == nil || checkRun.CompletedAt.Before(*checkRun.StartedAt) {
			continue
		}
		var queueSeconds int64
		if checkRun.CreatedAt != nil && !checkRun.StartedAt.Before(*checkRun.CreatedAt) {
			queueSeconds = int64(checkRun.StartedAt.Sub(*checkRun.CreatedAt) / time.Second)
		}
		slowChecks = append(slowChecks, connector.PullRequestCheck{
			Name:            name,
			Status:          status,
			Conclusion:      conclusion,
			QueueSeconds:    queueSeconds,
			DurationSeconds: int64(checkRun.CompletedAt.Sub(*checkRun.StartedAt) / time.Second),
		})
	}

	sort.SliceStable(slowChecks, func(i, j int) bool {
		if slowChecks[i].DurationSeconds != slowChecks[j].DurationSeconds {
			return slowChecks[i].DurationSeconds > slowChecks[j].DurationSeconds
		}
		return slowChecks[i].Name < slowChecks[j].Name
	})
	if len(slowChecks) > pullRequestSlowCheckLimit {
		slowChecks = slowChecks[:pullRequestSlowCheckLimit]
	}
	runningChecks = uniqueNonBlank(runningChecks)
	sort.Strings(runningChecks)
	if len(runningChecks) > pullRequestRunningCheckLimit {
		runningChecks = runningChecks[:pullRequestRunningCheckLimit]
	}

	var durationSeconds int64
	if !hasRunning && checkStartedAt != nil && completedAt != nil && !completedAt.Before(*checkStartedAt) {
		durationSeconds = int64(completedAt.Sub(*checkStartedAt) / time.Second)
	}
	var queueSeconds int64
	if queueCreatedAt != nil && queueStartedAt != nil && !queueStartedAt.Before(*queueCreatedAt) {
		queueSeconds = int64(queueStartedAt.Sub(*queueCreatedAt) / time.Second)
	}
	return checkRunTelemetrySummary{
		QueueSeconds:    queueSeconds,
		DurationSeconds: durationSeconds,
		SlowChecks:      slowChecks,
		RunningChecks:   runningChecks,
	}
}

func earliestGitHubTime(current *time.Time, candidate *time.Time) *time.Time {
	if candidate == nil {
		return current
	}
	if current == nil || candidate.Before(*current) {
		value := *candidate
		return &value
	}
	return current
}

func latestGitHubTime(current *time.Time, candidate *time.Time) *time.Time {
	if candidate == nil {
		return current
	}
	if current == nil || candidate.After(*current) {
		value := *candidate
		return &value
	}
	return current
}

func commitStatusesState(statuses []restCommitStatus) string {
	if len(statuses) == 0 {
		return ""
	}
	latestByContext := map[string]restCommitStatus{}
	for index, status := range statuses {
		context := strings.TrimSpace(status.Context)
		if context == "" {
			context = strconv.Itoa(index)
		}
		previous, ok := latestByContext[context]
		if !ok || restCommitStatusAfter(status, previous) {
			latestByContext[context] = status
		}
	}
	pending := false
	for _, status := range latestByContext {
		switch strings.ToLower(strings.TrimSpace(status.State)) {
		case "success":
		case "pending":
			pending = true
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

func restCommitStatusAfter(left restCommitStatus, right restCommitStatus) bool {
	if left.CreatedAt == nil {
		return false
	}
	if right.CreatedAt == nil {
		return true
	}
	return left.CreatedAt.After(*right.CreatedAt)
}

func combinedCIState(checkRuns string, statuses string) string {
	states := []string{checkRuns, statuses}
	hasSuccess := false
	hasPending := false
	for _, state := range states {
		switch strings.ToLower(strings.TrimSpace(state)) {
		case "failure", "failed", "error":
			return "failure"
		case "pending", "expected", "queued", "waiting", "in_progress", "in progress":
			hasPending = true
		case "success", "green", "pass", "passed":
			hasSuccess = true
		}
	}
	if hasPending {
		return "pending"
	}
	if hasSuccess {
		return "success"
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

func latestCodexReview(reviews []restReview, headSHA string) (pullRequestReview, bool) {
	headSHA = strings.TrimSpace(headSHA)
	var latest pullRequestReview
	found := false
	for _, review := range reviews {
		if !codexReviewAuthor(review.User) || strings.EqualFold(strings.TrimSpace(review.State), "DISMISSED") {
			continue
		}
		if headSHA != "" && strings.TrimSpace(review.CommitID) != "" && review.CommitID != headSHA {
			continue
		}
		candidate := pullRequestReview{
			Body:        review.Body,
			URL:         review.HTMLURL,
			State:       review.State,
			Author:      review.User,
			CommitID:    review.CommitID,
			SubmittedAt: review.SubmittedAt,
		}
		if !found || pullRequestReviewAfter(candidate, latest) {
			latest = candidate
			found = true
		}
	}
	return latest, found
}

func codexReviewAuthor(author *actor) bool {
	if author == nil {
		return false
	}
	return strings.Contains(strings.ToLower(strings.TrimSpace(author.Login)), "codex")
}

func pullRequestReviewAfter(left pullRequestReview, right pullRequestReview) bool {
	if left.SubmittedAt == nil {
		return right.SubmittedAt == nil
	}
	if right.SubmittedAt == nil {
		return true
	}
	return left.SubmittedAt.After(*right.SubmittedAt)
}

func pullRequestCodexReviewState(pullRequest pullRequestNode) string {
	return pullRequestCodexReviewStateFromReviews(pullRequest.LatestReviews.Nodes)
}

func pullRequestLatestCodexReviewState(pullRequest pullRequestNode) string {
	return pullRequestCodexReviewStateFromReviews(pullRequest.CodexReviews.Latest)
}

func pullRequestCodexReviewStateFromReviews(reviews []pullRequestReview) string {
	hasP2 := false
	reviewState := ""
	for _, review := range reviews {
		if containsReviewSeverity(review.Body, "P1") {
			return "P1"
		}
		if containsReviewSeverity(review.Body, "P2") {
			hasP2 = true
		}
		if state := strings.ToUpper(strings.TrimSpace(review.State)); state != "" {
			reviewState = state
		}
	}
	if hasP2 {
		return "P2"
	}
	return reviewState
}

func pullRequestCodexReviewSubmittedAt(pullRequest pullRequestNode) *time.Time {
	return pullRequestCodexReviewSubmittedAtFromReviews(pullRequest.LatestReviews.Nodes)
}

func pullRequestLatestCodexReviewSubmittedAt(pullRequest pullRequestNode) *time.Time {
	return pullRequestCodexReviewSubmittedAtFromReviews(pullRequest.CodexReviews.Latest)
}

func pullRequestCodexReviewSubmittedAtFromReviews(reviews []pullRequestReview) *time.Time {
	var latest *time.Time
	for _, review := range reviews {
		if review.SubmittedAt == nil {
			continue
		}
		if latest == nil || review.SubmittedAt.After(*latest) {
			value := *review.SubmittedAt
			latest = &value
		}
	}
	return latest
}

func pullRequestLatestCodexReviewCommitSHA(pullRequest pullRequestNode) string {
	for _, review := range pullRequest.CodexReviews.Latest {
		if commitID := strings.TrimSpace(review.CommitID); commitID != "" {
			return commitID
		}
	}
	return ""
}

func pullRequestCodexReviewFindings(pullRequest pullRequestNode) []connector.PullRequestFinding {
	findings := []connector.PullRequestFinding{}
	for _, review := range pullRequest.LatestReviews.Nodes {
		if !containsReviewSeverity(review.Body, "P1") {
			continue
		}
		findings = append(findings, connector.PullRequestFinding{
			Body: strings.TrimSpace(review.Body),
			URL:  strings.TrimSpace(review.URL),
		})
	}
	return findings
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

func cloneGitHubTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
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

func parseBlockedByFromIssueText(issue githubIssueNode, repo string) []connector.BlockedRef {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		repo = strings.TrimSpace(issue.Repository.NameWithOwner)
	}
	self := normalizedIssueIdentifier(buildIdentifier(repo, issue.Number))
	blockers := []connector.BlockedRef{}
	seen := map[string]struct{}{}
	appendBlockers := func(refs []connector.BlockedRef) {
		for _, ref := range refs {
			key := normalizedIssueIdentifier(ref.Identifier)
			if key == "" {
				continue
			}
			if self != "" && key == self {
				continue
			}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			blockers = append(blockers, ref)
		}
	}
	appendBlockers(parseBlockedBy(issue.Body, repo))
	for _, comment := range issue.Comments.Nodes {
		appendBlockers(parseBlockedBy(comment.Body, repo))
		if !strings.Contains(strings.ToLower(comment.Body), "codex workpad") {
			continue
		}
		for _, identifier := range issueReferencesInText(markdownSectionText(comment.Body, "Blockers"), repo) {
			appendBlockers([]connector.BlockedRef{{Identifier: identifier}})
		}
	}
	return blockers
}

func markdownSectionText(body string, title string) string {
	want := normalizeSectionTitle(title)
	inSection := false
	lines := []string{}
	for line := range strings.SplitSeq(body, "\n") {
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
		if after, ok := strings.CutPrefix(line, marker); ok {
			line = strings.TrimSpace(after)
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
		for _, identifier := range issueReferencesInText(lineMatches[1], repo) {
			key := normalizedIssueIdentifier(identifier)
			if key == "" {
				continue
			}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			blockers = append(blockers, connector.BlockedRef{Identifier: identifier})
		}
	}
	return blockers
}

func bodyReferencesIssue(body string, repo string, identifier string) bool {
	want := normalizedIssueIdentifier(identifier)
	if want == "" {
		return false
	}
	for _, candidate := range issueReferencesInText(body, repo) {
		if normalizedIssueIdentifier(candidate) == want {
			return true
		}
	}
	return false
}

func issueReferencesInText(text string, repo string) []string {
	refs := []string{}
	seen := map[string]struct{}{}
	add := func(refRepo string, number string) {
		identifier := blockerIdentifier(refRepo, number, repo)
		if identifier == "" {
			return
		}
		key := normalizedIssueIdentifier(identifier)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		refs = append(refs, identifier)
	}
	for _, matches := range issueURLPattern.FindAllStringSubmatch(text, -1) {
		if len(matches) == 3 {
			add(matches[1], matches[2])
		}
	}
	for _, matches := range issueRefPattern.FindAllStringSubmatch(text, -1) {
		if len(matches) == 3 {
			add(matches[1], matches[2])
		}
	}
	return refs
}

func githubEpicIssue(issue connector.Issue) bool {
	for _, label := range issue.Labels {
		if strings.EqualFold(strings.TrimSpace(label), "epic") {
			return true
		}
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(issue.Title)), "epic:")
}

func issueRepo(identifier string) string {
	identifier = strings.TrimSpace(identifier)
	index := strings.LastIndex(identifier, "#")
	if index <= 0 {
		return ""
	}
	return strings.TrimSpace(identifier[:index])
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
	for _, state := range []string{"Human Review", "Merging", "Blocked"} {
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
		"Urgent":      new(1),
		"High":        new(2),
		"Medium":      new(3),
		"Low":         new(4),
		"No priority": nil,
	}
}
