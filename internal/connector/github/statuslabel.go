package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
)

const repositoryIssuesPageSize = 100

func (c *Connector) fetchLabelIssuesByStates(ctx context.Context, stateNames []string, limit int) ([]connector.Issue, error) {
	if !validPullRequestRepo(c.repository) {
		return nil, ErrMissingRepository
	}
	stateLabels := c.statusLabelStates(stateNames)
	if len(stateLabels) == 0 {
		return []connector.Issue{}, nil
	}

	issues := []connector.Issue{}
	seen := map[string]struct{}{}
	for _, stateName := range stateNames {
		labelName := c.statusLabelForState(stateName)
		externalState, ok := stateLabels[normalizeLabelName(labelName)]
		if !ok {
			continue
		}
		for page := 1; ; page++ {
			var response []restIssue
			if err := c.client.REST(ctx, http.MethodGet, restRepositoryIssuesByLabelPath(c.repository, labelName, page), nil, &response); err != nil {
				return nil, fmt.Errorf("fetch github label issues: %w", err)
			}
			if len(response) == 0 {
				break
			}
			for _, item := range response {
				if item.PullRequest != nil {
					continue
				}
				ref := issueRef{Owner: c.repository.Owner, Name: c.repository.Name, Number: item.Number}
				issue := githubIssueNodeFromREST(ref, item)
				if strings.TrimSpace(issue.ID) == "" {
					continue
				}
				if githubIssueClosed(issue.State) && !stateInList(c.githubToDetentState(externalState), c.terminalStates) {
					continue
				}
				if _, ok := seen[issue.ID]; ok {
					continue
				}
				seen[issue.ID] = struct{}{}
				c.cacheIssueRef(issue)
				issues = append(issues, c.buildIssue(issue, externalState, "", nil, map[string]string{
					c.statusField: externalState,
				}))
				if limit > 0 && len(issues) >= limit {
					resolveBlockedByProjectState(issues)
					return issues, nil
				}
			}
			if len(response) < repositoryIssuesPageSize {
				break
			}
		}
	}
	resolveBlockedByProjectState(issues)
	return issues, nil
}

func (c *Connector) fetchLabelIssueByRef(ctx context.Context, ref issueRef) (connector.Issue, bool, error) {
	issue, err := c.fetchRESTIssue(ctx, ref)
	if err != nil {
		return connector.Issue{}, false, err
	}
	if strings.TrimSpace(issue.ID) == "" {
		return connector.Issue{}, false, nil
	}
	c.cacheIssueRef(issue)
	statusName := c.labelStatusFromLabels(issue.Labels)
	if statusName == "" {
		statusName = c.githubIssueStateToDetentState(issue.State)
	}
	return c.buildIssue(issue, statusName, "", nil, map[string]string{c.statusField: statusName}), true, nil
}

func (c *Connector) updateIssueStatusLabel(ctx context.Context, ref issueRef, issue githubIssueNode, targetState string) error {
	targetLabel := c.statusLabelForState(targetState)
	if targetLabel == "" {
		return ErrStatusUpdateFailed
	}

	currentLabels := labelNames(issue.Labels)
	for _, labelName := range currentLabels {
		if !c.isStatusLabel(labelName) || strings.EqualFold(labelName, targetLabel) {
			continue
		}
		var response []label
		if err := c.client.REST(ctx, http.MethodDelete, restIssueLabelPath(ref, labelName), nil, &response); err != nil {
			return fmt.Errorf("remove github status label: %w", err)
		}
	}
	if stringSliceContainsFold(currentLabels, targetLabel) {
		return nil
	}

	var response []label
	if err := c.client.REST(ctx, http.MethodPost, restIssueLabelsPath(ref), map[string]any{
		"labels": []string{targetLabel},
	}, &response); err != nil {
		return fmt.Errorf("add github status label: %w", err)
	}
	if len(response) == 0 {
		return ErrStatusUpdateFailed
	}
	return nil
}

func (c *Connector) EnsureLabelStateOptions(ctx context.Context) error {
	if !validPullRequestRepo(c.repository) {
		return ErrMissingRepository
	}
	existing, err := fetchRESTList[label](ctx, c.client, restRepositoryLabelsListPath(c.repository))
	if err != nil {
		return fmt.Errorf("fetch github labels: %w", err)
	}
	seen := map[string]struct{}{}
	for _, label := range existing {
		seen[normalizeLabelName(label.Name)] = struct{}{}
	}
	for _, stateName := range c.configuredStatusStates() {
		labelName := c.statusLabelForState(stateName)
		if labelName == "" {
			continue
		}
		if _, ok := seen[normalizeLabelName(labelName)]; ok {
			continue
		}
		option := statusOptionDefaults(stateName)
		var response label
		if err := c.client.REST(ctx, http.MethodPost, restRepositoryLabelsPath(c.repository), map[string]any{
			"name":        labelName,
			"color":       statusLabelColor(option.Color),
			"description": strings.TrimSpace(option.Description),
		}, &response); err != nil {
			return fmt.Errorf("create github status label: %w", err)
		}
		if strings.TrimSpace(response.Name) == "" {
			return ErrStatusOptionNotFound
		}
		seen[normalizeLabelName(labelName)] = struct{}{}
	}
	return nil
}

func (c *Connector) verifyLabelStatusOptions(ctx context.Context, stateNames []string) error {
	if !validPullRequestRepo(c.repository) {
		return ErrMissingRepository
	}
	existing, err := fetchRESTList[label](ctx, c.client, restRepositoryLabelsListPath(c.repository))
	if err != nil {
		return fmt.Errorf("fetch github labels: %w", err)
	}
	seen := map[string]struct{}{}
	for _, label := range existing {
		seen[normalizeLabelName(label.Name)] = struct{}{}
	}
	for _, stateName := range stateNames {
		labelName := c.statusLabelForState(stateName)
		if labelName == "" {
			continue
		}
		if _, ok := seen[normalizeLabelName(labelName)]; !ok {
			return fmt.Errorf("%w: %s maps to %s", ErrStatusOptionNotFound, stateName, labelName)
		}
	}
	return nil
}

func (c *Connector) statusLabelStates(stateNames []string) map[string]string {
	out := map[string]string{}
	for _, stateName := range stateNames {
		stateName = strings.TrimSpace(stateName)
		if stateName == "" {
			continue
		}
		externalState := c.detentToGitHubState(stateName)
		labelName := c.statusLabelForExternalState(externalState)
		if labelName == "" {
			continue
		}
		out[normalizeLabelName(labelName)] = externalState
	}
	return out
}

func (c *Connector) labelStatusFromLabels(labels nodeConnection[label]) string {
	statesByLabel := c.statusLabelStates(c.configuredStatusStates())
	for _, labelName := range labelNames(labels) {
		if stateName, ok := statesByLabel[normalizeLabelName(labelName)]; ok {
			return stateName
		}
	}
	return ""
}

func (c *Connector) configuredStatusStates() []string {
	return appendStatusStates(nil, c.activeStates, c.observedStates, c.terminalStates, []string{defaultProjectItemStatusState})
}

func (c *Connector) statusLabelForState(stateName string) string {
	return c.statusLabelForExternalState(c.detentToGitHubState(stateName))
}

func (c *Connector) statusLabelForExternalState(stateName string) string {
	slug := statusLabelSlug(stateName)
	if slug == "" {
		return ""
	}
	return c.statusLabelPrefix + slug
}

func (c *Connector) isStatusLabel(labelName string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(labelName)), strings.ToLower(c.statusLabelPrefix))
}

func statusLabelSlug(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	lastSeparator := false
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastSeparator = false
		default:
			if b.Len() == 0 || lastSeparator {
				continue
			}
			b.WriteByte('-')
			lastSeparator = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func statusLabelColor(color string) string {
	switch strings.ToUpper(strings.TrimSpace(color)) {
	case "RED":
		return "d73a4a"
	case "ORANGE":
		return "d93f0b"
	case "YELLOW":
		return "fbca04"
	case "PURPLE":
		return "6f42c1"
	case "GREEN":
		return "0e8a16"
	case "BLUE":
		return "5319e7"
	default:
		return "cfd3d7"
	}
}

func normalizeLabelName(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func stringSliceContainsFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), strings.TrimSpace(want)) {
			return true
		}
	}
	return false
}

func restRepositoryLabelsPath(repo pullRequestRepo) string {
	return "/repos/" + url.PathEscape(repo.Owner) + "/" + url.PathEscape(repo.Name) + "/labels"
}

func restRepositoryLabelsListPath(repo pullRequestRepo) string {
	values := url.Values{}
	values.Set("per_page", "100")
	return restRepositoryLabelsPath(repo) + "?" + values.Encode()
}

func restRepositoryIssuesByLabelPath(repo pullRequestRepo, labelName string, page int) string {
	values := url.Values{}
	values.Set("state", "all")
	values.Set("labels", labelName)
	values.Set("per_page", strconv.Itoa(repositoryIssuesPageSize))
	values.Set("page", strconv.Itoa(page))
	return "/repos/" + url.PathEscape(repo.Owner) + "/" + url.PathEscape(repo.Name) + "/issues?" + values.Encode()
}

func restIssueLabelsPath(ref issueRef) string {
	return restIssuePath(ref) + "/labels"
}

func restIssueLabelPath(ref issueRef, labelName string) string {
	return restIssueLabelsPath(ref) + "/" + url.PathEscape(labelName)
}
