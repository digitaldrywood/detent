package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/workpad"
)

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
  closedByPullRequestsReferences(first: 5) { nodes { number url state updatedAt repository { nameWithOwner } } }
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
  closedByPullRequestsReferences(first: 5) { nodes { number url state updatedAt repository { nameWithOwner } } }
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

func (c *Connector) FetchCandidateIssues(ctx context.Context) ([]connector.Issue, error) {
	return c.FetchCandidateIssuesByStates(ctx, c.activeStates)
}

func (c *Connector) FetchCandidateIssuesByStates(ctx context.Context, stateNames []string) ([]connector.Issue, error) {
	return c.FetchCandidateIssuesByStatesWithFilter(ctx, stateNames, connector.IssueFilterHint{})
}

func (c *Connector) FetchCandidateIssuesByStatesWithFilter(
	ctx context.Context,
	stateNames []string,
	hint connector.IssueFilterHint,
) ([]connector.Issue, error) {
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
		issues, err := c.fetchIssueFieldIssuesByStates(ctx, stateNames, 0, hint)
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
			c.hydrateBlockedByRefs(ctx, issues)
			if err := c.resolveBlockedByProjectState(ctx, issues); err != nil {
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
		issues, err := c.fetchIssueFieldIssuesByStates(ctx, stateNames, 0, connector.IssueFilterHint{})
		if err != nil {
			return nil, err
		}
		if _, ok := wantedStates[normalizeStateName("Blocked")]; ok {
			if err := c.populateBlockerReasons(ctx, issues); err != nil {
				return nil, err
			}
			c.hydrateBlockedByRefs(ctx, issues)
			if err := c.resolveBlockedByProjectState(ctx, issues); err != nil {
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

	issues := []connector.Issue{}
	if stateSetContains(wantedStates, defaultProjectItemStatusState) {
		backlogIssues, err := c.fetchProjectBacklogIssues(ctx, 0)
		if err != nil {
			return nil, err
		}
		issues = appendUniqueIssues(issues, backlogIssues, 0)
		stateNames = stateListWithout(stateNames, defaultProjectItemStatusState)
		wantedStates = normalizedStateSet(stateNames)
		if len(wantedStates) == 0 {
			return issues, nil
		}
	}

	statusIssues, err := c.fetchProjectItems(ctx, graphQLQueryObservedStatus, c.projectStatusQuery(stateNames), func(issue connector.Issue) bool {
		_, ok := wantedStates[normalizeStateName(issue.State)]
		return ok
	})
	if err != nil {
		return nil, err
	}
	if _, ok := wantedStates[normalizeStateName("Blocked")]; ok {
		if err := c.populateBlockerReasons(ctx, statusIssues); err != nil {
			return nil, err
		}
		c.hydrateBlockedByRefs(ctx, statusIssues)
		if err := c.resolveBlockedByProjectState(ctx, statusIssues); err != nil {
			return nil, err
		}
	}
	if attachPullRequestsForStates(wantedStates) {
		if err := c.attachFreshPullRequests(ctx, statusIssues); err != nil {
			return nil, err
		}
	}
	return appendUniqueIssues(issues, statusIssues, 0), nil
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
			c.hydrateBlockedByRefs(ctx, issues)
			if err := c.resolveBlockedByProjectState(ctx, issues); err != nil {
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
		issues, err := c.fetchIssueFieldIssuesByStates(ctx, stateNames, limit, connector.IssueFilterHint{})
		if err != nil {
			return nil, err
		}
		if _, ok := wantedStates[normalizeStateName("Blocked")]; ok {
			if err := c.populateBlockerReasons(ctx, issues); err != nil {
				return nil, err
			}
			c.hydrateBlockedByRefs(ctx, issues)
			if err := c.resolveBlockedByProjectState(ctx, issues); err != nil {
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

	issues := []connector.Issue{}
	if stateSetContains(wantedStates, defaultProjectItemStatusState) {
		backlogIssues, err := c.fetchProjectBacklogIssues(ctx, limit)
		if err != nil {
			return nil, err
		}
		issues = appendUniqueIssues(issues, backlogIssues, limit)
		if len(issues) >= limit {
			return issues, nil
		}
		stateNames = stateListWithout(stateNames, defaultProjectItemStatusState)
		wantedStates = normalizedStateSet(stateNames)
		if len(wantedStates) == 0 {
			return issues, nil
		}
	}

	statusIssues, err := c.fetchProjectItemsWithPullRequestRefsLimit(ctx, graphQLQueryObservedStatus, c.projectStatusQuery(stateNames), func(issue connector.Issue) bool {
		_, ok := wantedStates[normalizeStateName(issue.State)]
		return ok
	}, limit-len(issues))
	if err != nil {
		return nil, err
	}
	if _, ok := wantedStates[normalizeStateName("Blocked")]; ok {
		if err := c.populateBlockerReasons(ctx, statusIssues); err != nil {
			return nil, err
		}
		c.hydrateBlockedByRefs(ctx, statusIssues)
		if err := c.resolveBlockedByProjectState(ctx, statusIssues); err != nil {
			return nil, err
		}
	}
	if attachPullRequestsForStates(wantedStates) {
		if err := c.attachFreshPullRequests(ctx, statusIssues); err != nil {
			return nil, err
		}
	}
	return appendUniqueIssues(issues, statusIssues, limit), nil
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
		return c.fetchIssueFieldIssuesByStates(ctx, stateNames, limit, connector.IssueFilterHint{})
	}
	if c.projectID == "" {
		return nil, ErrMissingProject
	}

	issues := []connector.Issue{}
	if stateSetContains(wantedStates, defaultProjectItemStatusState) {
		backlogIssues, err := c.fetchExplicitProjectBacklogIssues(ctx, limit)
		if err != nil {
			return nil, err
		}
		issues = appendUniqueIssues(issues, backlogIssues, limit)
		if len(issues) >= limit {
			return issues, nil
		}
		stateNames = stateListWithout(stateNames, defaultProjectItemStatusState)
		wantedStates = normalizedStateSet(stateNames)
		if len(wantedStates) == 0 {
			blankIssues, err := c.fetchBlankProjectBacklogIssues(ctx, limit-len(issues))
			if err != nil {
				return nil, err
			}
			return appendUniqueIssues(issues, blankIssues, limit), nil
		}

		statusIssues, err := c.fetchProjectItemsWithPullRequestRefsLimit(ctx, graphQLQueryObservedStatus, c.projectStatusQuery(stateNames), func(issue connector.Issue) bool {
			_, ok := wantedStates[normalizeStateName(issue.State)]
			return ok
		}, limit-len(issues))
		if err != nil {
			return nil, err
		}
		issues = appendUniqueIssues(issues, statusIssues, limit)
		if len(issues) >= limit {
			return issues, nil
		}

		blankIssues, err := c.fetchBlankProjectBacklogIssues(ctx, limit-len(issues))
		if err != nil {
			return nil, err
		}
		return appendUniqueIssues(issues, blankIssues, limit), nil
	}

	statusIssues, err := c.fetchProjectItemsWithPullRequestRefsLimit(ctx, graphQLQueryObservedStatus, c.projectStatusQuery(stateNames), func(issue connector.Issue) bool {
		_, ok := wantedStates[normalizeStateName(issue.State)]
		return ok
	}, limit-len(issues))
	if err != nil {
		return nil, err
	}
	return appendUniqueIssues(issues, statusIssues, limit), nil
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
			c.hydrateIssueBlockedByRefs(ctx, &issue)
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
			c.hydrateIssueBlockedByRefs(ctx, &issue)
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
	response, err := c.fetchRESTIssueRaw(ctx, ref)
	if err != nil {
		return githubIssueNode{}, err
	}
	return githubIssueNodeFromREST(ref, response), nil
}

func (c *Connector) fetchRESTIssueRaw(ctx context.Context, ref issueRef) (restIssue, error) {
	var response restIssue
	if err := c.client.REST(ctx, http.MethodGet, restIssuePath(ref), nil, &response); err != nil {
		if errors.Is(err, ErrNotFound) {
			return restIssue{}, nil
		}
		return restIssue{}, fmt.Errorf("fetch github issue: %w", err)
	}
	return response, nil
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
		issues[index].Comments = connectorIssueComments(comments)
		issues[index].WorkpadSignal = parseWorkpadSignal(node)
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
			ID:        restCommentID(comment),
			Body:      comment.Body,
			URL:       comment.HTMLURL,
			Author:    comment.User,
			CreatedAt: restTimeString(comment.CreatedAt),
			UpdatedAt: restTimeString(comment.UpdatedAt),
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
		out = append(out, connectorIssueComment(comment, connector.IssueCommentTargetIssue))
	}
	return out, nil
}

func (c *Connector) FetchPullRequestComments(ctx context.Context, repository string, number int) ([]connector.IssueComment, error) {
	owner, name, ok := splitRepositoryName(repository)
	if !ok || number <= 0 {
		return []connector.IssueComment{}, nil
	}

	comments, err := c.fetchIssueComments(ctx, issueRef{Owner: owner, Name: name, Number: number})
	if err != nil {
		return nil, fmt.Errorf("fetch github pull request comments: %w", err)
	}
	out := make([]connector.IssueComment, 0, len(comments))
	for _, comment := range comments {
		out = append(out, connectorIssueComment(comment, connector.IssueCommentTargetPullRequest))
	}
	return out, nil
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
	workpadSignal := parseWorkpadSignal(issue)
	return connector.Issue{
		ID:               issue.ID,
		Identifier:       buildIdentifier(repo, issue.Number),
		Title:            issue.Title,
		Description:      issue.Body,
		Priority:         c.priorityRank(priorityName),
		PriorityName:     strings.TrimSpace(priorityName),
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
		BlockerReason:    workpad.Reason(workpadSignal),
		WorkpadSignal:    workpadSignal,
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
		out = append(out, connectorIssueComment(comment, connector.IssueCommentTargetIssue))
	}
	return out
}

func connectorIssueComment(comment issueComment, targetType string) connector.IssueComment {
	return connector.IssueComment{
		ID:          strings.TrimSpace(comment.ID),
		Backend:     connector.BackendGitHub.String(),
		Body:        comment.Body,
		URL:         comment.URL,
		AuthorLogin: actorLogin(comment.Author),
		CreatedAt:   parseGitHubTime(comment.CreatedAt),
		UpdatedAt:   parseGitHubTime(comment.UpdatedAt),
		TargetType:  targetType,
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

func appendUniqueIssues(issues []connector.Issue, additions []connector.Issue, limit int) []connector.Issue {
	seen := make(map[string]struct{}, len(issues)+len(additions))
	for _, issue := range issues {
		key := issueKey(issue)
		if key == "" {
			continue
		}
		seen[key] = struct{}{}
	}
	for _, issue := range additions {
		if limit > 0 && len(issues) >= limit {
			return issues
		}
		key := issueKey(issue)
		if key != "" {
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
		}
		issues = append(issues, issue)
	}
	return issues
}

func issueKey(issue connector.Issue) string {
	if key := strings.TrimSpace(issue.ID); key != "" {
		return key
	}
	return strings.TrimSpace(issue.Identifier)
}

func attachPullRequestsForStates(states map[string]struct{}) bool {
	for _, state := range []string{"Human Review", "Merging", "Blocked"} {
		if _, ok := states[normalizeStateName(state)]; ok {
			return true
		}
	}
	return false
}
