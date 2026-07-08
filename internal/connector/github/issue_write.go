package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
)

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

func (c *Connector) CreateIssue(ctx context.Context, draft connector.IssueDraft) (connector.Issue, error) {
	if !validPullRequestRepo(c.repository) {
		return connector.Issue{}, ErrMissingRepository
	}
	title := strings.TrimSpace(draft.Title)
	if title == "" {
		return connector.Issue{}, ErrStatusUpdateFailed
	}

	body := strings.TrimSpace(draft.Body)
	labels := normalizedIssueDraftLabels(draft.Labels)
	payload := map[string]any{
		"title": title,
		"body":  body,
	}
	if len(labels) > 0 {
		payload["labels"] = labels
	}

	var response restIssue
	if err := c.client.REST(ctx, http.MethodPost, restRepositoryIssueCreatePath(c.repository), payload, &response); err != nil {
		return connector.Issue{}, fmt.Errorf("create github issue: %w", err)
	}
	if strings.TrimSpace(response.NodeID) == "" || response.Number <= 0 {
		return connector.Issue{}, ErrStatusUpdateFailed
	}

	ref := issueRef{Owner: c.repository.Owner, Name: c.repository.Name, Number: response.Number}
	node := githubIssueNodeFromREST(ref, response)
	c.cacheIssueRef(node)
	status := c.labelStatusFromLabels(node.Labels)
	if status == "" {
		status = c.githubIssueStateToDetentState(node.State)
	}
	return c.buildLabelIssue(node, status), nil
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

func normalizedIssueDraftLabels(labels []string) []string {
	out := make([]string, 0, len(labels))
	seen := map[string]struct{}{}
	for _, label := range labels {
		label = strings.TrimSpace(label)
		if label == "" {
			continue
		}
		key := strings.ToLower(label)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, label)
	}
	return out
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
			return c.stateUpdateBlockedError(issueID, currentStatus, stateName)
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
			return c.stateUpdateBlockedError(issueID, currentStatus, stateName)
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
		return c.stateUpdateBlockedError(issueID, item.StatusName, stateName)
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

func (c *Connector) stateUpdateBlockedError(issueID string, currentStatus string, targetState string) error {
	currentState := c.githubToDetentState(currentStatus)
	if strings.TrimSpace(currentState) == "" {
		currentState = strings.TrimSpace(currentStatus)
	}
	targetDetentState := c.githubToDetentState(targetState)
	if strings.TrimSpace(targetDetentState) == "" {
		targetDetentState = strings.TrimSpace(targetState)
	}
	return &connector.StateUpdateBlockedError{
		IssueID:      strings.TrimSpace(issueID),
		CurrentState: strings.TrimSpace(currentState),
		TargetState:  strings.TrimSpace(targetDetentState),
	}
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
