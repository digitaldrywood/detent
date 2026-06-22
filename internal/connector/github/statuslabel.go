package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
)

const (
	repositoryIssuesPageSize = 100
	labelStatusConflictState = "Blocked"
)

type labelStatusResolution struct {
	Status         string
	ConflictLabels []string
}

func (r labelStatusResolution) conflicted() bool {
	return len(r.ConflictLabels) > 1
}

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
				issues = append(issues, c.buildLabelIssue(issue, externalState))
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
	return c.buildLabelIssue(issue, c.githubIssueStateToDetentState(issue.State)), true, nil
}

func (c *Connector) updateIssueStatusLabel(ctx context.Context, ref issueRef, issue githubIssueNode, targetState string) error {
	targetLabel := c.statusLabelForState(targetState)
	if targetLabel == "" {
		return ErrStatusUpdateFailed
	}

	nextLabels := make([]string, 0, len(issue.Labels.Nodes)+1)
	seen := map[string]struct{}{}
	for _, label := range issue.Labels.Nodes {
		labelName := strings.TrimSpace(label.Name)
		if labelName == "" || c.isStatusLabel(labelName) {
			continue
		}
		key := normalizeLabelName(labelName)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		nextLabels = append(nextLabels, labelName)
	}
	nextLabels = append(nextLabels, targetLabel)

	var response []label
	if err := c.client.REST(ctx, http.MethodPut, restIssueLabelsPath(ref), map[string]any{
		"labels": nextLabels,
	}, &response); err != nil {
		return fmt.Errorf("replace github status labels: %w", err)
	}
	resolution := c.labelStatusResolutionFromLabels(nodeConnection[label]{Nodes: response})
	if len(response) == 0 || resolution.conflicted() || !stringSliceContainsFold(labelNames(nodeConnection[label]{Nodes: response}), targetLabel) {
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
	return c.labelStatusResolutionFromLabels(labels).Status
}

func (c *Connector) labelStatusResolutionFromLabels(labels nodeConnection[label]) labelStatusResolution {
	return c.labelStatusResolutionFromNames(labelNames(labels))
}

func (c *Connector) labelStatusResolutionFromNames(names []string) labelStatusResolution {
	statesByLabel := c.statusLabelStates(c.configuredStatusStates())
	seen := map[string]struct{}{}
	matches := []string{}
	statusName := ""
	for _, labelName := range names {
		key := normalizeLabelName(labelName)
		if key == "" {
			continue
		}
		stateName, ok := statesByLabel[key]
		if !ok {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		matches = append(matches, key)
		if statusName == "" {
			statusName = stateName
		}
	}
	if len(matches) <= 1 {
		return labelStatusResolution{Status: statusName}
	}
	sort.Strings(matches)
	return labelStatusResolution{
		Status:         statusName,
		ConflictLabels: matches,
	}
}

func (c *Connector) buildLabelIssue(issue githubIssueNode, fallbackStatus string) connector.Issue {
	resolution := c.labelStatusResolutionFromLabels(issue.Labels)
	if resolution.conflicted() {
		return c.buildStatusLabelConflictIssue(issue, resolution.ConflictLabels)
	}
	statusName := strings.TrimSpace(resolution.Status)
	if statusName == "" {
		statusName = strings.TrimSpace(fallbackStatus)
	}
	return c.buildIssue(issue, statusName, "", nil, map[string]string{c.statusField: statusName})
}

func (c *Connector) buildStatusLabelConflictIssue(issue githubIssueNode, labels []string) connector.Issue {
	out := c.buildIssue(issue, labelStatusConflictState, "", nil, map[string]string{c.statusField: labelStatusConflictState})
	out.BlockerReason = labelStatusConflictReason(labels)
	return out
}

func labelStatusConflictReason(labels []string) string {
	return "multiple configured Detent status labels: " + strings.Join(labels, ", ") + "; remove all but one status label"
}

func statusLabelConflictIssue(issue connector.Issue) bool {
	return strings.Contains(issue.BlockerReason, "multiple configured Detent status labels")
}

func (c *Connector) labelStatusConflictSummaries(issues []connector.Issue) []string {
	summaries := []string{}
	for _, issue := range issues {
		resolution := c.labelStatusResolutionFromNames(issue.Labels)
		if !resolution.conflicted() {
			continue
		}
		summaries = append(summaries, labelStatusConflictReference(issue)+" ("+strings.Join(resolution.ConflictLabels, ", ")+")")
	}
	return summaries
}

func labelStatusConflictReference(issue connector.Issue) string {
	if _, number, ok := strings.Cut(strings.TrimSpace(issue.Identifier), "#"); ok && strings.TrimSpace(number) != "" {
		return "#" + strings.TrimSpace(number)
	}
	if identifier := strings.TrimSpace(issue.Identifier); identifier != "" {
		return identifier
	}
	return strings.TrimSpace(issue.ID)
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
