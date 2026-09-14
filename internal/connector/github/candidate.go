package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
)

// candidateCursor tracks the next source item, including an unfinished page.
// It contains no issue bodies, and remains useful when hydration consumes the budget.
type candidateCursor struct {
	Source int    `json:"source,omitempty"`
	Page   int    `json:"page,omitempty"`
	Offset int    `json:"offset,omitempty"`
	After  string `json:"after,omitempty"`
}

func (p candidateCursor) encode() (string, error) {
	data, err := json.Marshal(p)
	return string(data), err
}

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
	position := candidateCursor{Page: 1}
	if request.Cursor != "" {
		if err := json.Unmarshal([]byte(request.Cursor), &position); err != nil || position.Page < 1 || position.Source < 0 || position.Offset < 0 {
			return connector.CandidateResult{}, fmt.Errorf("%w: invalid github cursor", connector.ErrInvalidCandidateRequest)
		}
	}
	if request.Selector == connector.CandidateSelectorStates && !c.usesLabelStatus() && !c.usesIssueFieldStatus() {
		return c.readProjectCandidates(ctx, request, position)
	}
	return c.readRESTCandidates(ctx, request, position)
}

func candidateReadResult(result connector.CandidateResult, position candidateCursor, more bool, err error) (connector.CandidateResult, error) {
	connector.SortCandidateIssues(result.Issues)
	result.Truncated = more
	if more {
		var encodeErr error
		result.NextCursor, encodeErr = position.encode()
		err = errors.Join(err, encodeErr)
	}
	return result, err
}

// readRESTCandidates shares pagination and partial-result handling across label,
// repository, and issue-field selectors. Every run bounds source items as well
// as requests; rejected items advance the cursor just like eligible ones.
func (c *Connector) readRESTCandidates(ctx context.Context, request connector.CandidateRequest, position candidateCursor) (connector.CandidateResult, error) {
	result := connector.CandidateResult{Filtered: map[string]int{}}
	if !validPullRequestRepo(c.repository) {
		return result, ErrMissingRepository
	}
	sources := []string{""}
	search := request.Selector == connector.CandidateSelectorStates && c.usesIssueFieldStatus()
	switch {
	case request.Selector == connector.CandidateSelectorLabels:
		sources = normalizeStateList(request.Labels, nil)
	case request.Selector == connector.CandidateSelectorStates && c.usesLabelStatus():
		sources = nil
		labels := c.statusLabelStates(request.States)
		for _, state := range normalizeStateList(request.States, nil) {
			label := c.statusLabelForState(state)
			if _, ok := labels[normalizeLabelName(label)]; ok {
				sources = append(sources, label)
			}
		}
	case search:
		if err := c.verifyIssueFieldStatusOptions(ctx, request.States); err != nil {
			return result, err
		}
	}
	pageSize := min(request.EffectivePageSize(), repositoryIssuesPageSize)
	seen := map[string]bool{}
	filteredTotal := 0
	for position.Source < len(sources) {
		var items []restIssue
		var total int
		var err error
		switch {
		case search:
			var response restIssueSearchResponse
			path := restIssueFieldSearchPagePath(c.repository, c.statusField, c.detentToGitHubStates(request.States), connector.IssueFilterHint{Authors: connector.NormalizeAuthorHandles(request.Authors)}, position.Page, pageSize, true)
			err = c.client.REST(ctx, http.MethodGet, path, nil, &response)
			items, total = response.Items, response.TotalCount
			filteredTotal = max(filteredTotal, total)
		default:
			path := restRepositoryOpenIssuesPagePath(c.repository, position.Page, pageSize, true)
			if request.Selector != connector.CandidateSelectorUntracked {
				path = restRepositoryIssuesByLabelPagePath(c.repository, sources[position.Source], position.Page, pageSize, true)
			}
			err = c.client.REST(ctx, http.MethodGet, path, nil, &items)
		}
		if err != nil {
			return candidateReadResult(result, position, true, fmt.Errorf("fetch github candidates: %w", err))
		}
		result.PagesRead++
		for position.Offset < len(items) {
			item := items[position.Offset]
			issue, ok, err := c.readRESTCandidate(ctx, request, item, sources[position.Source])
			if err != nil {
				return candidateReadResult(result, position, true, err)
			}
			if ok && !seen[issue.ID] {
				hydrated := []connector.Issue{issue}
				if err := c.hydrateCandidateIssues(ctx, hydrated, request.States); err != nil {
					return candidateReadResult(result, position, true, err)
				}
				seen[issue.ID] = true
				result.Issues = append(result.Issues, hydrated[0])
			}
			position.Offset++
			result.ItemsRead++
			if result.ItemsRead >= request.Limit {
				break
			}
		}
		if position.Offset >= len(items) {
			position.Offset = 0
			if len(items) < pageSize || (search && position.Page*pageSize >= total) {
				position.Source++
				position.Page = 1
			} else {
				position.Page++
			}
		}
		if result.ItemsRead >= request.Limit {
			break
		}
	}
	if search {
		result.Filtered["author"] = c.bestEffortIssueFieldAuthorRejections(ctx, request, filteredTotal)
	}
	return candidateReadResult(result, position, position.Source < len(sources), nil)
}

func (c *Connector) readRESTCandidate(ctx context.Context, request connector.CandidateRequest, item restIssue, source string) (connector.Issue, bool, error) {
	if item.PullRequest != nil {
		return connector.Issue{}, false, nil
	}
	ref := issueRef{Owner: c.repository.Owner, Name: c.repository.Name, Number: item.Number}
	if request.Selector == connector.CandidateSelectorStates && c.usesIssueFieldStatus() {
		var ok bool
		ref, ok = issueRefFromRESTSearchItem(item, ref)
		if !ok {
			return connector.Issue{}, false, nil
		}
	}
	node := githubIssueNodeFromREST(ref, item)
	if strings.TrimSpace(node.ID) == "" {
		return connector.Issue{}, false, nil
	}
	var issue connector.Issue
	if c.usesIssueFieldStatus() {
		var ok bool
		var err error
		issue, ok, err = c.fetchIssueFieldIssueFromREST(ctx, ref, item)
		if err != nil || !ok {
			return issue, ok, err
		}
	} else {
		state := c.githubIssueStateToDetentState(node.State)
		switch request.Selector {
		case connector.CandidateSelectorUntracked:
			if c.hasConfiguredStatusLabel(node.Labels) {
				return connector.Issue{}, false, nil
			}
			state = ""
		case connector.CandidateSelectorStates:
			state = c.statusLabelStates(request.States)[normalizeLabelName(source)]
			if githubIssueClosed(node.State) && !stateInList(c.githubToDetentState(state), c.terminalStates) {
				return connector.Issue{}, false, nil
			}
		}
		issue = c.buildLabelIssue(node, state)
		c.cacheIssueRef(node)
	}
	if request.Selector == connector.CandidateSelectorStates {
		if _, ok := normalizedStateSet(request.States)[normalizeStateName(issue.State)]; !ok {
			return connector.Issue{}, false, nil
		}
	}
	return issue, true, nil
}

func (c *Connector) bestEffortIssueFieldAuthorRejections(ctx context.Context, request connector.CandidateRequest, filteredTotal int) int {
	rejected, err := c.issueFieldAuthorRejections(ctx, request, filteredTotal)
	if err != nil {
		c.logger.WarnContext(ctx, "count github issue field author rejections failed", "error", err)
		return 0
	}
	return rejected
}

func (c *Connector) readProjectCandidates(ctx context.Context, request connector.CandidateRequest, position candidateCursor) (connector.CandidateResult, error) {
	result := connector.CandidateResult{Filtered: map[string]int{}}
	wantedStates := normalizedStateSet(request.States)
	_, repairBlankStatuses := wantedStates[normalizeStateName(defaultProjectItemStatusState)]
	for {
		var response struct {
			Node *struct {
				Items projectItemsConnection `json:"items"`
			} `json:"node"`
		}
		var after *string
		if position.After != "" {
			after = &position.After
		}
		if err := c.client.GraphQLWithType(ctx, graphQLQueryCandidateIssues, observedStatusProjectItemsQuery, map[string]any{
			"projectId": c.projectID, "first": min(request.EffectivePageSize(), projectItemsPageSize), "after": after,
		}, &response); err != nil {
			return candidateReadResult(result, position, true, fmt.Errorf("fetch github project candidates: %w", err))
		}
		result.PagesRead++
		if response.Node == nil {
			return result, ErrProjectNotFound
		}
		items := response.Node.Items.Nodes
		for position.Offset < len(items) {
			issue, _, ok, blankStatusItemID, err := c.normalizeProjectItem(items[position.Offset])
			if err != nil {
				return candidateReadResult(result, position, true, err)
			}
			if ok {
				if blankStatusItemID != "" && repairBlankStatuses {
					c.defaultBlankProjectItemStatuses(ctx, []string{blankStatusItemID})
				}
				if _, wanted := wantedStates[normalizeStateName(issue.State)]; wanted {
					hydrated := []connector.Issue{issue}
					if err := c.hydrateCandidateIssues(ctx, hydrated, request.States); err != nil {
						return candidateReadResult(result, position, true, err)
					}
					result.Issues = append(result.Issues, hydrated[0])
				}
			}
			position.Offset++
			result.ItemsRead++
			if len(result.Issues) >= request.Limit {
				break
			}
		}
		if position.Offset >= len(items) {
			if !response.Node.Items.PageInfo.HasNextPage {
				return candidateReadResult(result, position, false, nil)
			}
			cursor := strings.TrimSpace(response.Node.Items.PageInfo.EndCursor)
			if cursor == "" {
				return result, ErrInvalidResponse
			}
			position.After, position.Offset = cursor, 0
		}
		if len(result.Issues) >= request.Limit || result.PagesRead >= projectCandidatePageLimit {
			return candidateReadResult(result, position, true, nil)
		}
	}
}

func (c *Connector) hydrateCandidateIssues(ctx context.Context, issues []connector.Issue, stateNames []string) error {
	wantedStates := normalizedStateSet(stateNames)
	if _, ok := wantedStates[normalizeStateName("Blocked")]; ok {
		if err := c.populateBlockerReasons(ctx, issues); err != nil {
			return err
		}
	}
	if err := c.hydrateBlockedByRefs(ctx, issues); err != nil {
		return err
	}
	if err := c.resolveBlockedByProjectState(ctx, issues); err != nil {
		return err
	}

	return c.attachStatePullRequests(ctx, issues, true)
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
