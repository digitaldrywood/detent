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
		issues         []connector.Issue
		pagesRead      int
		itemsRead      int
		incomplete     bool
		authorRejected int
		err            error
	)
	switch request.Selector {
	case connector.CandidateSelectorLabels:
		issues, pagesRead, incomplete, err = c.readRepositoryLabelCandidates(ctx, request)
	case connector.CandidateSelectorUntracked:
		var drift connector.StatusDrift
		drift, pagesRead, incomplete, err = c.readLabelStatusDrift(ctx, labelStatusDriftReadOptions{
			PageSize:      request.EffectivePageSize(),
			Limit:         request.ProbeLimit(),
			Deterministic: true,
		})
		issues = drift.UntrackedOpen
	case connector.CandidateSelectorStates:
		switch {
		case c.usesLabelStatus():
			issues, pagesRead, incomplete, err = c.readLabelCandidates(ctx, request)
		case c.usesIssueFieldStatus():
			issues, pagesRead, incomplete, authorRejected, err = c.readIssueFieldCandidates(ctx, request)
		default:
			issues, pagesRead, itemsRead, incomplete, err = c.readProjectCandidates(ctx, request)
		}
	}
	if err != nil {
		return connector.CandidateResult{}, err
	}

	result := connector.NewCandidateResult(issues, request, pagesRead, incomplete)
	if itemsRead > 0 {
		result.ItemsRead = itemsRead
	}
	if authorRejected > 0 {
		result.Filtered["author"] = authorRejected
	}
	if err := c.hydrateCandidateIssues(ctx, result.Issues, request.States); err != nil {
		return connector.CandidateResult{}, err
	}
	return result, nil
}

func (c *Connector) readRepositoryLabelCandidates(
	ctx context.Context,
	request connector.CandidateRequest,
) ([]connector.Issue, int, bool, error) {
	if !validPullRequestRepo(c.repository) {
		return nil, 0, false, ErrMissingRepository
	}
	labelNames := normalizeStateList(request.Labels, nil)
	scanLimit := request.ProbeLimit()
	issues := []connector.Issue{}
	seenIssues := map[string]struct{}{}
	queriedLabels := map[string]struct{}{}
	pagesRead := 0
	pageSize := min(request.EffectivePageSize(), repositoryIssuesPageSize, scanLimit)
	incomplete := false

	for _, labelName := range labelNames {
		labelKey := normalizeLabelName(labelName)
		if _, ok := queriedLabels[labelKey]; ok {
			continue
		}
		queriedLabels[labelKey] = struct{}{}
		itemsRead := 0
		for page := 1; ; page++ {
			if itemsRead >= scanLimit {
				incomplete = true
				break
			}
			var response []restIssue
			path := restRepositoryIssuesByLabelPagePath(c.repository, labelName, page, pageSize, true)
			if err := c.client.REST(ctx, http.MethodGet, path, nil, &response); err != nil {
				return nil, 0, false, fmt.Errorf("fetch github label selector candidates: %w", err)
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
				if _, ok := seenIssues[node.ID]; ok {
					continue
				}
				var (
					issue connector.Issue
					ok    bool
					err   error
				)
				if c.usesIssueFieldStatus() {
					issue, ok, err = c.fetchIssueFieldIssueFromREST(ctx, ref, item)
				} else {
					issue = c.buildLabelIssue(node, c.githubIssueStateToDetentState(node.State))
					ok = true
					c.cacheIssueRef(node)
				}
				if err != nil {
					return nil, 0, false, err
				}
				if !ok {
					continue
				}
				seenIssues[node.ID] = struct{}{}
				issues = append(issues, issue)
			}
			if len(pageItems) < len(response) {
				incomplete = true
				break
			}
			if len(response) < pageSize {
				break
			}
			if itemsRead >= scanLimit {
				incomplete = true
				break
			}
		}
	}
	return issues, pagesRead, incomplete, nil
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
	queriedLabels := map[string]struct{}{}
	pagesRead := 0
	pageSize := min(request.EffectivePageSize(), repositoryIssuesPageSize, scanLimit)
	incomplete := false

	for _, stateName := range stateNames {
		labelName := c.statusLabelForState(stateName)
		labelKey := normalizeLabelName(labelName)
		externalState, ok := stateLabels[labelKey]
		if !ok {
			continue
		}
		if _, ok := queriedLabels[labelKey]; ok {
			continue
		}
		queriedLabels[labelKey] = struct{}{}
		itemsRead := 0
		for page := 1; ; page++ {
			if itemsRead >= scanLimit {
				incomplete = true
				break
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
				incomplete = true
				break
			}
			if len(response) < pageSize {
				break
			}
			if itemsRead >= scanLimit {
				incomplete = true
				break
			}
		}
	}
	return issues, pagesRead, incomplete, nil
}

func (c *Connector) readIssueFieldCandidates(
	ctx context.Context,
	request connector.CandidateRequest,
) ([]connector.Issue, int, bool, int, error) {
	if !validPullRequestRepo(c.repository) {
		return nil, 0, false, 0, ErrMissingRepository
	}
	wantedStates := normalizedStateSet(request.States)
	githubStates := c.detentToGitHubStates(request.States)
	if len(wantedStates) == 0 || len(githubStates) == 0 {
		return []connector.Issue{}, 0, false, 0, nil
	}
	if err := c.verifyIssueFieldStatusOptions(ctx, request.States); err != nil {
		return nil, 0, false, 0, err
	}

	scanLimit := request.ProbeLimit()
	issues := []connector.Issue{}
	itemsRead := 0
	pagesRead := 0
	filteredTotal := 0
	pageSize := min(request.EffectivePageSize(), issueSearchPageSize, scanLimit)
	for page := 1; ; page++ {
		if itemsRead >= scanLimit {
			return issues, pagesRead, true, c.bestEffortIssueFieldAuthorRejections(ctx, request, filteredTotal), nil
		}
		var response restIssueSearchResponse
		path := restIssueFieldSearchPagePath(
			c.repository,
			c.statusField,
			githubStates,
			connector.IssueFilterHint{Authors: connector.NormalizeAuthorHandles(request.Authors)},
			page,
			pageSize,
			true,
		)
		if err := c.client.REST(ctx, http.MethodGet, path, nil, &response); err != nil {
			return nil, 0, false, 0, fmt.Errorf("search github issue field candidates: %w", err)
		}
		pagesRead++
		filteredTotal = max(filteredTotal, response.TotalCount)
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
				return nil, 0, false, 0, err
			}
			if !ok {
				continue
			}
			if _, ok := wantedStates[normalizeStateName(issue.State)]; ok {
				issues = append(issues, issue)
			}
		}
		if len(pageItems) < len(response.Items) {
			return issues, pagesRead, true, c.bestEffortIssueFieldAuthorRejections(ctx, request, filteredTotal), nil
		}
		exhausted := len(response.Items) < pageSize ||
			(response.TotalCount > 0 && itemsRead >= response.TotalCount)
		if exhausted {
			return issues, pagesRead, false, c.bestEffortIssueFieldAuthorRejections(ctx, request, filteredTotal), nil
		}
		if itemsRead >= scanLimit {
			return issues, pagesRead, true, c.bestEffortIssueFieldAuthorRejections(ctx, request, filteredTotal), nil
		}
	}
}

func (c *Connector) bestEffortIssueFieldAuthorRejections(
	ctx context.Context,
	request connector.CandidateRequest,
	filteredTotal int,
) int {
	rejected, err := c.issueFieldAuthorRejections(ctx, request, filteredTotal)
	if err != nil {
		c.logger.WarnContext(ctx, "count github issue field author rejections failed", "error", err)
		return 0
	}
	return rejected
}

func (c *Connector) issueFieldAuthorRejections(
	ctx context.Context,
	request connector.CandidateRequest,
	filteredTotal int,
) (int, error) {
	if len(connector.NormalizeAuthorHandles(request.Authors)) == 0 {
		return 0, nil
	}
	var response restIssueSearchResponse
	path := restIssueFieldSearchPagePath(
		c.repository,
		c.statusField,
		c.detentToGitHubStates(request.States),
		connector.IssueFilterHint{},
		1,
		1,
		true,
	)
	if err := c.client.REST(ctx, http.MethodGet, path, nil, &response); err != nil {
		return 0, fmt.Errorf("count github issue field author rejections: %w", err)
	}
	return max(0, response.TotalCount-filteredTotal), nil
}

func (c *Connector) readProjectCandidates(
	ctx context.Context,
	request connector.CandidateRequest,
) ([]connector.Issue, int, int, bool, error) {
	if c.projectID == "" {
		return nil, 0, 0, false, ErrMissingProject
	}
	wantedStates := normalizedStateSet(request.States)
	if len(wantedStates) == 0 {
		return []connector.Issue{}, 0, 0, false, nil
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
		var response struct {
			Node *struct {
				Items projectItemsConnection `json:"items"`
			} `json:"node"`
		}
		pageSize := min(request.EffectivePageSize(), projectItemsPageSize)
		if err := c.client.GraphQLWithType(ctx, graphQLQueryCandidateIssues, observedStatusProjectItemsQuery, map[string]any{
			"projectId": c.projectID,
			"first":     pageSize,
			"after":     after,
		}, &response); err != nil {
			return nil, 0, 0, false, fmt.Errorf("fetch github project candidates: %w", err)
		}
		pagesRead++
		if response.Node == nil {
			return nil, 0, 0, false, ErrProjectNotFound
		}
		pageItems := response.Node.Items.Nodes
		itemsRead += len(pageItems)
		totalItems = max(totalItems, response.Node.Items.TotalCount)
		for _, item := range pageItems {
			issue, ok, blankStatusItemID, err := c.normalizeProjectItem(item)
			if err != nil {
				return nil, 0, 0, false, err
			}
			if !ok {
				continue
			}
			if blankStatusItemID != "" && repairBlankStatuses {
				blankStatusItemIDs = append(blankStatusItemIDs, blankStatusItemID)
			}
			if _, ok := wantedStates[normalizeStateName(issue.State)]; ok && len(issues) < scanLimit {
				issues = append(issues, issue)
			}
		}
		if len(issues) >= scanLimit {
			c.defaultBlankProjectItemStatuses(ctx, blankStatusItemIDs)
			return issues, pagesRead, itemsRead, true, nil
		}

		if !response.Node.Items.PageInfo.HasNextPage {
			c.defaultBlankProjectItemStatuses(ctx, blankStatusItemIDs)
			return issues, pagesRead, itemsRead, totalItems > itemsRead, nil
		}
		if pagesRead >= projectCandidatePageLimit {
			c.defaultBlankProjectItemStatuses(ctx, blankStatusItemIDs)
			return issues, pagesRead, itemsRead, true, nil
		}
		cursor := strings.TrimSpace(response.Node.Items.PageInfo.EndCursor)
		if cursor == "" {
			return nil, 0, 0, false, ErrInvalidResponse
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
