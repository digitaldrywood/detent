package github

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
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

func (c *Connector) fetchProjectItems(ctx context.Context, queryType string, query string, keepIssue func(connector.Issue) bool) ([]connector.Issue, error) {
	return c.fetchProjectItemsLimit(ctx, queryType, query, keepIssue, 0)
}

func (c *Connector) fetchProjectBacklogIssues(ctx context.Context, limit int) ([]connector.Issue, error) {
	issues, err := c.fetchExplicitProjectBacklogIssues(ctx, limit)
	if err != nil {
		return nil, err
	}
	if limit > 0 && len(issues) >= limit {
		return issues, nil
	}

	blankLimit := 0
	if limit > 0 {
		blankLimit = limit - len(issues)
	}
	blankIssues, err := c.fetchBlankProjectBacklogIssues(ctx, blankLimit)
	if err != nil {
		return nil, err
	}
	return appendUniqueIssues(issues, blankIssues, limit), nil
}

func (c *Connector) fetchExplicitProjectBacklogIssues(ctx context.Context, limit int) ([]connector.Issue, error) {
	backlogStates := []string{defaultProjectItemStatusState}
	return c.fetchProjectItemsLimit(ctx, graphQLQueryCandidateIssues, c.projectStatusQuery(backlogStates), func(issue connector.Issue) bool {
		return stateInList(issue.State, backlogStates)
	}, limit)
}

func (c *Connector) fetchBlankProjectBacklogIssues(ctx context.Context, limit int) ([]connector.Issue, error) {
	var after *string
	issues := []connector.Issue{}
	blankStatusItemIDs := []string{}

	for {
		var response struct {
			Node *struct {
				Items projectItemsConnection `json:"items"`
			} `json:"node"`
		}
		if err := c.client.GraphQLWithType(ctx, graphQLQueryCandidateIssues, projectItemsQuery, map[string]any{
			"projectId": c.projectID,
			"first":     projectItemsPageSize,
			"after":     after,
			"query":     nil,
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
			if !ok || blankStatusItemID == "" {
				continue
			}
			issues = append(issues, issue)
			blankStatusItemIDs = append(blankStatusItemIDs, blankStatusItemID)
			if limit > 0 && len(issues) >= limit {
				c.defaultBlankProjectItemStatuses(ctx, blankStatusItemIDs)
				return issues, nil
			}
		}

		if !response.Node.Items.PageInfo.HasNextPage {
			c.defaultBlankProjectItemStatuses(ctx, blankStatusItemIDs)
			return issues, nil
		}
		cursor := strings.TrimSpace(response.Node.Items.PageInfo.EndCursor)
		if cursor == "" {
			return nil, ErrInvalidResponse
		}
		after = &cursor
	}
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
					if err := c.resolveBlockedByProjectState(ctx, issues); err != nil {
						return nil, err
					}
					return issues, nil
				}
			}
		}

		if !response.Node.Items.PageInfo.HasNextPage {
			c.defaultBlankProjectItemStatuses(ctx, blankStatusItemIDs)
			if err := c.resolveBlockedByProjectState(ctx, allIssues); err != nil {
				return nil, err
			}
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
		// Default status repair is a must-finish write: detach cancellation
		// while keeping caller values for logs and traces.
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
