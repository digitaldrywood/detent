package github

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/digitaldrywood/symphony/internal/connector"
)

const (
	projectItemsPageSize = 50
	projectItemsPerIssue = 100
)

const projectItemsQuery = `
query SymphonyGitHubProjectItems($projectId: ID!, $first: Int!, $after: String) {
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
              assignees(first: 5) { nodes { login } }
              labels(first: 20) { nodes { name } }
              repository { nameWithOwner }
              closedByPullRequestsReferences(first: 5) { nodes { number url } }
            }
          }
          statusValue: fieldValueByName(name: "Status") {
            ... on ProjectV2ItemFieldSingleSelectValue { name }
          }
          priorityValue: fieldValueByName(name: "Priority") {
            ... on ProjectV2ItemFieldSingleSelectValue { name }
          }
        }
      }
    }
  }
}`

const issuesByIDQuery = `
query SymphonyGitHubIssuesByID($issueIds: [ID!]!, $projectItemsFirst: Int!) {
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
      assignees(first: 5) { nodes { login } }
      labels(first: 20) { nodes { name } }
      repository { nameWithOwner }
      closedByPullRequestsReferences(first: 5) { nodes { number url } }
      projectItems(first: $projectItemsFirst) {
        pageInfo { hasNextPage endCursor }
        nodes {
          id
          project { id }
          statusValue: fieldValueByName(name: "Status") {
            ... on ProjectV2ItemFieldSingleSelectValue { name }
          }
          priorityValue: fieldValueByName(name: "Priority") {
            ... on ProjectV2ItemFieldSingleSelectValue { name }
          }
        }
      }
    }
  }
}`

const issueCommentsQuery = `
query SymphonyGitHubIssueComments($issueIds: [ID!]!) {
  nodes(ids: $issueIds) {
    __typename
    ... on Issue {
      id
      body
      comments(first: 100) { nodes { body } }
    }
  }
}`

const addCommentMutation = `
mutation SymphonyGitHubAddComment($subjectId: ID!, $body: String!) {
  addComment(input: {subjectId: $subjectId, body: $body}) {
    commentEdge { node { id } }
  }
}`

const statusFieldQuery = `
query SymphonyGitHubStatusField($projectId: ID!) {
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
}`

const projectItemForIssueQuery = `
query SymphonyGitHubProjectItemForIssue($issueId: ID!, $projectItemsFirst: Int!, $after: String) {
  node(id: $issueId) {
    ... on Issue {
      projectItems(first: $projectItemsFirst, after: $after) {
        pageInfo { hasNextPage endCursor }
        nodes {
          id
          project { id }
          statusValue: fieldValueByName(name: "Status") {
            ... on ProjectV2ItemFieldSingleSelectValue { name }
          }
          priorityValue: fieldValueByName(name: "Priority") {
            ... on ProjectV2ItemFieldSingleSelectValue { name }
          }
        }
      }
    }
  }
}`

const updateStatusMutation = `
mutation SymphonyGitHubUpdateStatus($projectId: ID!, $itemId: ID!, $fieldId: ID!, $optionId: String!) {
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
	ID            string             `json:"id"`
	Content       *githubIssueNode   `json:"content"`
	Project       *projectRef        `json:"project"`
	StatusValue   *singleSelectValue `json:"statusValue"`
	PriorityValue *singleSelectValue `json:"priorityValue"`
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

type repository struct {
	NameWithOwner string `json:"nameWithOwner"`
}

type projectRef struct {
	ID string `json:"id"`
}

type singleSelectValue struct {
	Name string `json:"name"`
}

func (c *Connector) FetchCandidateIssues(ctx context.Context) ([]connector.Issue, error) {
	if c.projectID == "" {
		return nil, ErrMissingProject
	}

	return c.fetchProjectItems(ctx, func(issue connector.Issue) bool {
		return stateInList(issue.State, c.activeStates)
	})
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

	githubState := c.symphonyToGitHubState(stateName)
	fieldID, optionID, err := c.resolveStatusOption(ctx, githubState)
	if err != nil {
		if errors.Is(err, ErrStatusOptionNotFound) {
			c.statusCache.Clear(c.projectID)
		}
		return err
	}

	itemID, err := c.resolveProjectItemID(ctx, strings.TrimSpace(issueID))
	if err != nil {
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
	return c.buildIssue(*item.Content, singleSelectName(item.StatusValue), singleSelectName(item.PriorityValue)), true
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
	stateName, priorityName, ok, err := c.resolveIssueProjectFields(ctx, issue.ID, issue.ProjectItems)
	if err != nil {
		return connector.Issue{}, false, err
	}
	if ok {
		return c.buildIssue(issue, stateName, priorityName), true, nil
	}
	return c.buildIssue(issue, c.githubIssueStateToSymphonyState(issue.State), ""), true, nil
}

func (c *Connector) resolveIssueProjectFields(ctx context.Context, issueID string, items *projectItemsConnection) (string, string, bool, error) {
	if stateName, priorityName, ok := c.projectFields(issueID, items); ok {
		return stateName, priorityName, true, nil
	}
	if items == nil || !items.PageInfo.HasNextPage {
		return "", "", false, nil
	}
	cursor := strings.TrimSpace(items.PageInfo.EndCursor)
	if cursor == "" {
		return "", "", false, ErrInvalidResponse
	}
	return c.fetchProjectFieldsPage(ctx, issueID, &cursor)
}

func (c *Connector) projectFields(issueID string, items *projectItemsConnection) (string, string, bool) {
	if items == nil {
		return "", "", false
	}
	for _, item := range items.Nodes {
		if item.Project != nil && item.Project.ID == c.projectID {
			c.projectCache.SetItemID(c.projectID, issueID, item.ID)
			return singleSelectName(item.StatusValue), singleSelectName(item.PriorityValue), true
		}
	}
	return "", "", false
}

func (c *Connector) fetchProjectFieldsPage(ctx context.Context, issueID string, after *string) (string, string, bool, error) {
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
		return "", "", false, fmt.Errorf("fetch github project item fields: %w", err)
	}
	if response.Node == nil {
		return "", "", false, ErrProjectItemNotFound
	}
	if stateName, priorityName, ok := c.projectFields(issueID, &response.Node.ProjectItems); ok {
		return stateName, priorityName, true, nil
	}
	if !response.Node.ProjectItems.PageInfo.HasNextPage {
		return "", "", false, nil
	}
	cursor := strings.TrimSpace(response.Node.ProjectItems.PageInfo.EndCursor)
	if cursor == "" {
		return "", "", false, ErrInvalidResponse
	}
	return c.fetchProjectFieldsPage(ctx, issueID, &cursor)
}

func (c *Connector) buildIssue(issue githubIssueNode, statusName string, priorityName string) connector.Issue {
	repo := strings.TrimSpace(issue.Repository.NameWithOwner)
	return connector.Issue{
		ID:               issue.ID,
		Identifier:       buildIdentifier(repo, issue.Number),
		Title:            issue.Title,
		Description:      issue.Body,
		Priority:         c.priorityRank(priorityName),
		State:            c.githubToSymphonyState(statusName),
		URL:              issue.URL,
		PRNumber:         firstPullRequestNumber(issue.ClosedByPullRequestsReferences),
		AssigneeID:       firstAssigneeLogin(issue.Assignees),
		BlockedBy:        parseBlockedBy(issue.Body, repo),
		BlockerReason:    parseBlockerReason(issue),
		Labels:           labelNames(issue.Labels),
		AssignedToWorker: true,
		CreatedAt:        parseGitHubTime(issue.CreatedAt),
		UpdatedAt:        parseGitHubTime(issue.UpdatedAt),
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

func (c *Connector) resolveProjectItemID(ctx context.Context, issueID string) (string, error) {
	if itemID, ok := c.projectCache.GetItemID(c.projectID, issueID); ok {
		return itemID, nil
	}
	return c.fetchProjectItemIDPage(ctx, issueID, nil)
}

func (c *Connector) fetchProjectItemIDPage(ctx context.Context, issueID string, after *string) (string, error) {
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
		return "", fmt.Errorf("fetch github project item: %w", err)
	}
	if response.Node == nil {
		return "", ErrProjectItemNotFound
	}

	for _, item := range response.Node.ProjectItems.Nodes {
		if item.Project != nil && item.Project.ID == c.projectID && strings.TrimSpace(item.ID) != "" {
			c.projectCache.SetItemID(c.projectID, issueID, item.ID)
			return item.ID, nil
		}
	}
	if !response.Node.ProjectItems.PageInfo.HasNextPage {
		return "", ErrProjectItemNotFound
	}
	cursor := strings.TrimSpace(response.Node.ProjectItems.PageInfo.EndCursor)
	if cursor == "" {
		return "", ErrProjectItemNotFound
	}
	return c.fetchProjectItemIDPage(ctx, issueID, &cursor)
}

func (c *Connector) updateStatusFieldValue(ctx context.Context, itemID string, fieldID string, optionID string) error {
	var response struct {
		UpdateProjectV2ItemFieldValue *struct {
			ProjectV2Item *struct {
				ID string `json:"id"`
			} `json:"projectV2Item"`
		} `json:"updateProjectV2ItemFieldValue"`
	}
	if err := c.client.GraphQL(ctx, updateStatusMutation, map[string]any{
		"projectId": c.projectID,
		"itemId":    itemID,
		"fieldId":   fieldID,
		"optionId":  optionID,
	}, &response); err != nil {
		return fmt.Errorf("update github status: %w", err)
	}
	if response.UpdateProjectV2ItemFieldValue == nil ||
		response.UpdateProjectV2ItemFieldValue.ProjectV2Item == nil ||
		strings.TrimSpace(response.UpdateProjectV2ItemFieldValue.ProjectV2Item.ID) == "" {
		return ErrStatusUpdateFailed
	}
	return nil
}

func (c *Connector) symphonyToGitHubState(stateName string) string {
	stateName = strings.TrimSpace(stateName)
	if mapped, ok := c.stateMap[stateName]; ok {
		return strings.TrimSpace(mapped)
	}
	return stateName
}

func (c *Connector) githubToSymphonyState(githubState string) string {
	githubState = strings.TrimSpace(githubState)
	if githubState == "" {
		return ""
	}
	for symphonyState, mapped := range c.stateMap {
		if mapped == githubState {
			return symphonyState
		}
	}
	return githubState
}

func (c *Connector) githubIssueStateToSymphonyState(state string) string {
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

func buildIdentifier(repo string, number int) string {
	if number == 0 {
		return ""
	}
	if repo == "" {
		return fmt.Sprintf("#%d", number)
	}
	return fmt.Sprintf("%s#%d", repo, number)
}

func firstAssigneeLogin(assignees nodeConnection[assignee]) string {
	for _, assignee := range assignees.Nodes {
		if strings.TrimSpace(assignee.Login) != "" {
			return assignee.Login
		}
	}
	return ""
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
