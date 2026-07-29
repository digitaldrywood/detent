package github

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
)

func (c *Connector) CandidateCapabilities() connector.CandidateCapabilities {
	if c == nil {
		return connector.CandidateCapabilities{}
	}
	return connector.CandidateCapabilitiesFor(connector.BackendGitHub, c.statusSource)
}

func (c *Connector) ReadCandidates(ctx context.Context, request connector.CandidateRequest) (connector.CandidateResult, error) {
	if err := request.Validate(c.CandidateCapabilities()); err != nil {
		return connector.CandidateResult{}, err
	}

	var (
		issues     []connector.Issue
		pagesRead  int
		incomplete bool
		err        error
	)
	switch {
	case c.usesLabelStatus():
		issues, pagesRead, incomplete, err = c.readLabelCandidates(ctx, request)
	case c.usesIssueFieldStatus():
		issues, pagesRead, incomplete, err = c.readIssueFieldCandidates(ctx, request)
	default:
		issues, pagesRead, incomplete, err = c.readProjectCandidates(ctx, request)
	}
	if err != nil {
		return connector.CandidateResult{}, err
	}

	result := connector.NewCandidateResult(issues, request, pagesRead, incomplete)
	if err := c.hydrateCandidateIssues(ctx, result.Issues, request.States); err != nil {
		return connector.CandidateResult{}, err
	}
	return result, nil
}

func (c *Connector) readLabelCandidates(
	ctx context.Context,
	request connector.CandidateRequest,
) ([]connector.Issue, int, bool, error) {
	if !validPullRequestRepo(c.repository) {
		return nil, 0, false, ErrMissingRepository
	}
	stateNames := normalizeStateList(request.States, nil)
	stateLabels := c.statusLabelStates(stateNames)
	if len(stateLabels) == 0 {
		return []connector.Issue{}, 0, false, nil
	}
	wantedStates := normalizedStateSet(stateNames)
	scanLimit := request.ProbeLimit()
	issues := []connector.Issue{}
	seen := map[string]struct{}{}
	itemsRead := 0
	pagesRead := 0
	pageSize := min(request.EffectivePageSize(), repositoryIssuesPageSize, scanLimit)

	for stateIndex, stateName := range stateNames {
		labelName := c.statusLabelForState(stateName)
		externalState, ok := stateLabels[normalizeLabelName(labelName)]
		if !ok {
			continue
		}
		for page := 1; ; page++ {
			if itemsRead >= scanLimit {
				return issues, pagesRead, true, nil
			}
			var response []restIssue
			path := restRepositoryIssuesByLabelPagePath(c.repository, labelName, page, pageSize, true)
			if err := c.client.REST(ctx, http.MethodGet, path, nil, &response); err != nil {
				return nil, 0, false, fmt.Errorf("fetch github label candidates: %w", err)
			}
			pagesRead++
			pageItems := response
			if remaining := scanLimit - itemsRead; len(pageItems) > remaining {
				pageItems = pageItems[:remaining]
			}
			itemsRead += len(pageItems)
			for _, item := range pageItems {
				if item.PullRequest != nil {
					continue
				}
				ref := issueRef{Owner: c.repository.Owner, Name: c.repository.Name, Number: item.Number}
				node := githubIssueNodeFromREST(ref, item)
				if strings.TrimSpace(node.ID) == "" {
					continue
				}
				if githubIssueClosed(node.State) && !stateInList(c.githubToDetentState(externalState), c.terminalStates) {
					continue
				}
				if _, ok := seen[node.ID]; ok {
					continue
				}
				built := c.buildLabelIssue(node, externalState)
				if _, ok := wantedStates[normalizeStateName(built.State)]; !ok {
					continue
				}
				seen[node.ID] = struct{}{}
				c.cacheIssueRef(node)
				issues = append(issues, built)
			}
			if len(pageItems) < len(response) {
				return issues, pagesRead, true, nil
			}
			if len(response) < pageSize {
				break
			}
			if itemsRead >= scanLimit {
				return issues, pagesRead, true, nil
			}
		}
		if stateIndex < len(stateNames)-1 && itemsRead >= scanLimit {
			return issues, pagesRead, true, nil
		}
	}
	return issues, pagesRead, false, nil
}

func (c *Connector) readIssueFieldCandidates(
	ctx context.Context,
	request connector.CandidateRequest,
) ([]connector.Issue, int, bool, error) {
	if !validPullRequestRepo(c.repository) {
		return nil, 0, false, ErrMissingRepository
	}
	wantedStates := normalizedStateSet(request.States)
	githubStates := c.detentToGitHubStates(request.States)
	if len(wantedStates) == 0 || len(githubStates) == 0 {
		return []connector.Issue{}, 0, false, nil
	}
	if err := c.verifyIssueFieldStatusOptions(ctx, request.States); err != nil {
		return nil, 0, false, err
	}

	scanLimit := request.ProbeLimit()
	issues := []connector.Issue{}
	itemsRead := 0
	pagesRead := 0
	pageSize := min(request.EffectivePageSize(), issueSearchPageSize, scanLimit)
	for page := 1; ; page++ {
		if itemsRead >= scanLimit {
			return issues, pagesRead, true, nil
		}
		var response restIssueSearchResponse
		path := restIssueFieldSearchPagePath(
			c.repository,
			c.statusField,
			githubStates,
			connector.IssueFilterHint{},
			page,
			pageSize,
			true,
		)
		if err := c.client.REST(ctx, http.MethodGet, path, nil, &response); err != nil {
			return nil, 0, false, fmt.Errorf("search github issue field candidates: %w", err)
		}
		pagesRead++
		pageItems := response.Items
		if remaining := scanLimit - itemsRead; len(pageItems) > remaining {
			pageItems = pageItems[:remaining]
		}
		itemsRead += len(pageItems)
		for _, item := range pageItems {
			ref, ok := issueRefFromRESTSearchItem(item, issueRef{Owner: c.repository.Owner, Name: c.repository.Name})
			if !ok {
				continue
			}
			issue, ok, err := c.fetchIssueFieldIssueFromREST(ctx, ref, item)
			if err != nil {
				return nil, 0, false, err
			}
			if !ok {
				continue
			}
			if _, ok := wantedStates[normalizeStateName(issue.State)]; ok {
				issues = append(issues, issue)
			}
		}
		if len(pageItems) < len(response.Items) {
			return issues, pagesRead, true, nil
		}
		exhausted := len(response.Items) < pageSize ||
			(response.TotalCount > 0 && itemsRead >= response.TotalCount)
		if exhausted {
			return issues, pagesRead, false, nil
		}
		if itemsRead >= scanLimit {
			return issues, pagesRead, true, nil
		}
	}
}

func (c *Connector) readProjectCandidates(
	ctx context.Context,
	request connector.CandidateRequest,
) ([]connector.Issue, int, bool, error) {
	if c.projectID == "" {
		return nil, 0, false, ErrMissingProject
	}
	wantedStates := normalizedStateSet(request.States)
	if len(wantedStates) == 0 {
		return []connector.Issue{}, 0, false, nil
	}

	scanLimit := request.ProbeLimit()
	var after *string
	issues := []connector.Issue{}
	blankStatusItemIDs := []string{}
	_, repairBlankStatuses := wantedStates[normalizeStateName(defaultProjectItemStatusState)]
	itemsRead := 0
	totalItems := 0
	pagesRead := 0
	for {
		if itemsRead >= scanLimit {
			c.defaultBlankProjectItemStatuses(ctx, blankStatusItemIDs)
			return issues, pagesRead, true, nil
		}
		var response struct {
			Node *struct {
				Items projectItemsConnection `json:"items"`
			} `json:"node"`
		}
		pageSize := min(request.EffectivePageSize(), projectItemsPageSize, scanLimit-itemsRead)
		if err := c.client.GraphQLWithType(ctx, graphQLQueryCandidateIssues, observedStatusProjectItemsQuery, map[string]any{
			"projectId": c.projectID,
			"first":     pageSize,
			"after":     after,
		}, &response); err != nil {
			return nil, 0, false, fmt.Errorf("fetch github project candidates: %w", err)
		}
		pagesRead++
		if response.Node == nil {
			return nil, 0, false, ErrProjectNotFound
		}
		pageItems := response.Node.Items.Nodes
		if remaining := scanLimit - itemsRead; len(pageItems) > remaining {
			pageItems = pageItems[:remaining]
		}
		itemsRead += len(pageItems)
		totalItems = max(totalItems, response.Node.Items.TotalCount)
		for _, item := range pageItems {
			issue, ok, blankStatusItemID, err := c.normalizeProjectItem(item)
			if err != nil {
				return nil, 0, false, err
			}
			if !ok {
				continue
			}
			if blankStatusItemID != "" && repairBlankStatuses {
				blankStatusItemIDs = append(blankStatusItemIDs, blankStatusItemID)
			}
			if _, ok := wantedStates[normalizeStateName(issue.State)]; ok {
				issues = append(issues, issue)
			}
		}
		if len(pageItems) < len(response.Node.Items.Nodes) {
			c.defaultBlankProjectItemStatuses(ctx, blankStatusItemIDs)
			return issues, pagesRead, true, nil
		}

		if !response.Node.Items.PageInfo.HasNextPage {
			c.defaultBlankProjectItemStatuses(ctx, blankStatusItemIDs)
			return issues, pagesRead, totalItems > itemsRead, nil
		}
		if itemsRead >= scanLimit || pagesRead >= scanLimit {
			c.defaultBlankProjectItemStatuses(ctx, blankStatusItemIDs)
			return issues, pagesRead, true, nil
		}
		cursor := strings.TrimSpace(response.Node.Items.PageInfo.EndCursor)
		if cursor == "" {
			return nil, 0, false, ErrInvalidResponse
		}
		after = &cursor
	}
}

func (c *Connector) hydrateCandidateIssues(ctx context.Context, issues []connector.Issue, stateNames []string) error {
	wantedStates := normalizedStateSet(stateNames)
	if _, ok := wantedStates[normalizeStateName("Blocked")]; ok {
		if err := c.populateBlockerReasons(ctx, issues); err != nil {
			return err
		}
		c.hydrateBlockedByRefs(ctx, issues)
		if err := c.resolveBlockedByProjectState(ctx, issues); err != nil {
			return err
		}
	}
	return c.attachStatePullRequests(ctx, issues, true)
}
