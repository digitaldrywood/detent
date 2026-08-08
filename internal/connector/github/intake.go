package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/intake"
)

const intakeIssueSearchPageSize = 100

const addIntakeProjectItemMutation = `
mutation DetentGitHubAddIntakeProjectItem($projectId: ID!, $contentId: ID!) {
  addProjectV2ItemById(input: {projectId: $projectId, contentId: $contentId}) {
    item { id }
  }
}`

func (c *Connector) FindIntakeIssue(ctx context.Context, marker string) (intake.Issue, bool, error) {
	if !validPullRequestRepo(c.repository) {
		return intake.Issue{}, false, ErrMissingRepository
	}
	marker = strings.TrimSpace(marker)
	if marker == "" {
		return intake.Issue{}, false, nil
	}
	for page := 1; ; page++ {
		var response restIssueSearchResponse
		if err := c.client.REST(ctx, http.MethodGet, restIntakeIssueSearchPath(c.repository, marker, page), nil, &response); err != nil {
			return intake.Issue{}, false, fmt.Errorf("find github intake issue: %w", err)
		}
		for _, item := range response.Items {
			if item.PullRequest != nil || item.Body == nil || !strings.Contains(*item.Body, marker) {
				continue
			}
			ref := issueRef{Owner: c.repository.Owner, Name: c.repository.Name, Number: item.Number}
			node := githubIssueNodeFromREST(ref, item)
			if strings.TrimSpace(node.ID) == "" {
				continue
			}
			c.cacheIssueRef(node)
			return intakeIssue(c.buildLabelIssue(node, "")), true, nil
		}
		if len(response.Items) == 0 || page*intakeIssueSearchPageSize >= response.TotalCount {
			return intake.Issue{}, false, nil
		}
	}
}

func (c *Connector) CreateIntakeIssue(ctx context.Context, draft intake.IssueDraft) (intake.Issue, error) {
	issue, err := c.CreateIssue(ctx, connector.IssueDraft{
		Title:  draft.Title,
		Body:   draft.Body,
		Labels: draft.Labels,
	})
	if err != nil {
		return intake.Issue{}, err
	}
	if !c.usesLabelStatus() && !c.usesIssueFieldStatus() {
		if err := c.addIntakeIssueToProject(ctx, issue.ID); err != nil {
			return intakeIssue(issue), err
		}
	}
	return intakeIssue(issue), nil
}

func (c *Connector) UpdateIntakeIssue(ctx context.Context, issueID string, draft intake.IssueDraft) (intake.Issue, error) {
	ref, ok, err := c.issueRefForID(ctx, strings.TrimSpace(issueID), graphQLQueryIssueLookup)
	if err != nil {
		return intake.Issue{}, err
	}
	if !ok {
		return intake.Issue{}, ErrStatusUpdateFailed
	}
	payload := map[string]any{
		"title": strings.TrimSpace(draft.Title),
		"body":  strings.TrimSpace(draft.Body),
	}
	var response restIssue
	if err := c.client.REST(ctx, http.MethodPatch, restIssuePath(ref), payload, &response); err != nil {
		return intake.Issue{}, fmt.Errorf("update github intake issue: %w", err)
	}
	if strings.TrimSpace(response.NodeID) == "" {
		return intake.Issue{}, ErrStatusUpdateFailed
	}
	labels := normalizedIssueDraftLabels(draft.Labels)
	if len(labels) > 0 {
		var labelResponse []label
		if err := c.client.REST(ctx, http.MethodPost, restIssueLabelsPath(ref), map[string]any{"labels": labels}, &labelResponse); err != nil {
			return intake.Issue{}, fmt.Errorf("add github intake issue labels: %w", err)
		}
		response.Labels = labelResponse
	}
	node := githubIssueNodeFromREST(ref, response)
	c.cacheIssueRef(node)
	return intakeIssue(c.buildLabelIssue(node, "")), nil
}

func (c *Connector) SetIntakeIssueState(ctx context.Context, issueID string, state string) error {
	if !c.usesLabelStatus() && !c.usesIssueFieldStatus() {
		itemID, ok := c.projectCache.GetItemID(c.projectID, issueID)
		if !ok {
			item, err := c.resolveProjectItem(ctx, issueID)
			if err != nil {
				return err
			}
			itemID = item.ID
		}
		return c.setProjectItemStatus(ctx, itemID, c.detentToGitHubState(state))
	}
	return c.UpdateIssueState(ctx, issueID, state)
}

func (c *Connector) addIntakeIssueToProject(ctx context.Context, issueID string) error {
	if strings.TrimSpace(c.projectID) == "" {
		return ErrMissingProject
	}
	var response struct {
		AddProjectV2ItemByID *struct {
			Item *struct {
				ID string `json:"id"`
			} `json:"item"`
		} `json:"addProjectV2ItemById"`
	}
	if err := c.client.GraphQLWithType(ctx, graphQLQueryAddProjectItem, addIntakeProjectItemMutation, map[string]any{
		"projectId": c.projectID,
		"contentId": strings.TrimSpace(issueID),
	}, &response); err != nil {
		return fmt.Errorf("add github intake issue to project: %w", err)
	}
	if response.AddProjectV2ItemByID == nil || response.AddProjectV2ItemByID.Item == nil || strings.TrimSpace(response.AddProjectV2ItemByID.Item.ID) == "" {
		return ErrProjectItemNotFound
	}
	c.projectCache.SetItemID(c.projectID, issueID, response.AddProjectV2ItemByID.Item.ID)
	return nil
}

func restIntakeIssueSearchPath(repo pullRequestRepo, marker string, page int) string {
	token := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(marker), "<!--"), "-->"))
	values := url.Values{}
	values.Set("q", "repo:"+repo.Owner+"/"+repo.Name+" is:issue in:body \""+token+"\"")
	values.Set("per_page", strconv.Itoa(intakeIssueSearchPageSize))
	values.Set("page", strconv.Itoa(page))
	return "/search/issues?" + values.Encode()
}

func intakeIssue(issue connector.Issue) intake.Issue {
	number := issue.Number
	if number == 0 {
		if _, value, ok := strings.Cut(strings.TrimSpace(issue.Identifier), "#"); ok {
			parsed, err := strconv.Atoi(strings.TrimSpace(value))
			if err == nil {
				number = parsed
			}
		}
	}
	return intake.Issue{
		ID:         strings.TrimSpace(issue.ID),
		Identifier: strings.TrimSpace(issue.Identifier),
		Number:     number,
		URL:        strings.TrimSpace(issue.URL),
		Body:       issue.Description,
		Closed:     issue.Closed,
	}
}
