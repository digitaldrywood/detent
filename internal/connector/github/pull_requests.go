package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/digitaldrywood/detent/internal/citrigger"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/reviewseverity"
)

type issuePullRequestCandidate struct {
	Index             int
	Identifier        string
	BranchPrefix      string
	PullRequestNumber int
	PullRequestRepo   pullRequestRepo
}

type pullRequestKey struct {
	Repo   pullRequestRepo
	Number int
}

type pullRequestRepo struct {
	Owner string
	Name  string
}

const (
	linkedPullRequestHydrationConcurrency     = 8
	linkedPullRequestHydrationRequestEstimate = 5
)

type linkedPullRequestHydration struct {
	repo        pullRequestRepo
	number      int
	pullRequest pullRequestNode
	state       pullRequestHydrationState
}

func (c *Connector) attachPullRequests(ctx context.Context, issues []connector.Issue) error {
	return c.attachPullRequestsWithCache(ctx, issues, true)
}

func (c *Connector) attachFreshPullRequests(ctx context.Context, issues []connector.Issue) error {
	return c.attachPullRequestsWithCache(ctx, issues, false)
}

func (c *Connector) attachPullRequestsWithCache(ctx context.Context, issues []connector.Issue, useStatusCache bool) error {
	byRepo := make(map[pullRequestRepo][]issuePullRequestCandidate)
	for index, issue := range issues {
		repo, ok := pullRequestRepoFromIdentifier(issue.Identifier)
		if !ok {
			continue
		}
		branchPrefix := detentIssueBranchPrefix(issue.Identifier)
		pullRequestNumber := 0
		linkedPullRequestRepo := repo
		if issue.PRNumber != nil {
			pullRequestNumber = *issue.PRNumber
		}
		if owner, name, ok := splitRepositoryName(issue.PRRepository); ok {
			linkedPullRequestRepo = pullRequestRepo{Owner: owner, Name: name}
		}
		if normalizeStateName(issue.State) == normalizeStateName("Blocked") && pullRequestNumber <= 0 && !statusLabelConflictIssue(issue) {
			branchPrefix = ""
		}
		if branchPrefix == "" && pullRequestNumber <= 0 {
			continue
		}
		identifier := strings.TrimSpace(issue.Identifier)
		if identifier == "" {
			identifier = strings.TrimSpace(issue.ID)
		}
		byRepo[repo] = append(byRepo[repo], issuePullRequestCandidate{
			Index:             index,
			Identifier:        identifier,
			BranchPrefix:      branchPrefix,
			PullRequestNumber: pullRequestNumber,
			PullRequestRepo:   linkedPullRequestRepo,
		})
	}
	if len(byRepo) == 0 {
		return nil
	}

	repos := make([]pullRequestRepo, 0, len(byRepo))
	for repo := range byRepo {
		repos = append(repos, repo)
	}
	sort.Slice(repos, func(i, j int) bool {
		left := repos[i].Owner + "/" + repos[i].Name
		right := repos[j].Owner + "/" + repos[j].Name
		return left < right
	})

	for _, repo := range repos {
		candidates := c.rotatePullRequestHydrationCandidates(repo, byRepo[repo])
		nextCursor := ""
		branchFirst := firstPullRequestCandidateNeedsBranchHydration(issues, candidates)
		if branchFirst {
			var err error
			nextCursor, err = c.attachBranchPullRequests(ctx, repo, issues, candidates, useStatusCache)
			if err != nil {
				return err
			}
		}

		linkedCursor, err := c.attachLinkedPullRequests(ctx, repo, issues, candidates, useStatusCache)
		if err != nil {
			return err
		}
		if nextCursor == "" {
			nextCursor = linkedCursor
		}
		if !branchFirst {
			branchCursor, err := c.attachBranchPullRequests(ctx, repo, issues, candidates, useStatusCache)
			if err != nil {
				return err
			}
			if nextCursor == "" {
				nextCursor = branchCursor
			}
		}
		c.setPullRequestHydrationCursor(repo, nextCursor)
	}
	return nil
}

func (c *Connector) attachBranchPullRequests(
	ctx context.Context,
	repo pullRequestRepo,
	issues []connector.Issue,
	candidates []issuePullRequestCandidate,
	useStatusCache bool,
) (string, error) {
	if !hasUnattachedBranchPullRequestCandidates(issues, candidates) {
		return "", nil
	}
	if state, ok := c.currentPullRequestHydrationState(repo); ok {
		c.logPullRequestHydrationSkip(ctx, repo, state, "shared_backoff")
		markPullRequestHydrationUnavailableForCandidates(issues, candidates, repo, state)
		return firstUnattachedBranchPullRequestCandidate(issues, candidates), nil
	}
	pullRequests, err := c.fetchRepositoryPullRequests(ctx, repo)
	if err != nil {
		if state := c.pullRequestHydrationStateForError(repo, err); state.Reason != "" {
			cursor := ""
			if errors.Is(err, ErrRESTBudgetReserved) {
				cursor = firstUnattachedBranchPullRequestCandidate(issues, candidates)
			}
			markPullRequestHydrationUnavailableForCandidates(issues, candidates, repo, state)
			return cursor, nil
		}
		return "", err
	}
	return c.attachMatchingPullRequests(ctx, repo, issues, candidates, pullRequests, useStatusCache)
}

func firstPullRequestCandidateNeedsBranchHydration(issues []connector.Issue, candidates []issuePullRequestCandidate) bool {
	if len(candidates) == 0 {
		return false
	}
	candidate := candidates[0]
	return issues[candidate.Index].PullRequest == nil &&
		candidate.PullRequestNumber <= 0 &&
		strings.TrimSpace(candidate.BranchPrefix) != ""
}

func firstUnattachedBranchPullRequestCandidate(issues []connector.Issue, candidates []issuePullRequestCandidate) string {
	for _, candidate := range candidates {
		if issues[candidate.Index].PullRequest == nil &&
			candidate.PullRequestNumber <= 0 &&
			strings.TrimSpace(candidate.BranchPrefix) != "" {
			return candidate.Identifier
		}
	}
	for _, candidate := range candidates {
		if issues[candidate.Index].PullRequest == nil && strings.TrimSpace(candidate.BranchPrefix) != "" {
			return candidate.Identifier
		}
	}
	return ""
}

func (c *Connector) attachPullRequestMergeStates(ctx context.Context, issues []connector.Issue) error {
	byRepo := make(map[pullRequestRepo][]issuePullRequestCandidate)
	for index, issue := range issues {
		if issue.PullRequest != nil {
			continue
		}
		repo, ok := pullRequestRepoFromIdentifier(issue.Identifier)
		if !ok {
			continue
		}
		branchPrefix := detentIssueBranchPrefix(issue.Identifier)
		if branchPrefix == "" {
			continue
		}
		byRepo[repo] = append(byRepo[repo], issuePullRequestCandidate{
			Index:        index,
			Identifier:   strings.TrimSpace(issue.Identifier),
			BranchPrefix: branchPrefix,
		})
	}
	if len(byRepo) == 0 {
		return nil
	}

	repos := make([]pullRequestRepo, 0, len(byRepo))
	for repo := range byRepo {
		repos = append(repos, repo)
	}
	sort.Slice(repos, func(i, j int) bool {
		left := repos[i].Owner + "/" + repos[i].Name
		right := repos[j].Owner + "/" + repos[j].Name
		return left < right
	})

	for _, repo := range repos {
		pullRequests, err := c.fetchRepositoryPullRequests(ctx, repo)
		if err != nil {
			if errors.Is(err, ErrRESTBudgetReserved) {
				continue
			}
			return err
		}
		attachMatchingPullRequestMergeStates(repo, issues, byRepo[repo], pullRequests)
	}
	return nil
}

func attachMatchingPullRequestMergeStates(
	repo pullRequestRepo,
	issues []connector.Issue,
	candidates []issuePullRequestCandidate,
	pullRequests []pullRequestNode,
) {
	for _, pullRequest := range pullRequests {
		if normalizeStateName(pullRequest.State) != "merged" {
			continue
		}
		branchName := strings.TrimSpace(pullRequest.HeadRefName)
		if branchName == "" {
			continue
		}
		for _, candidate := range candidates {
			if issues[candidate.Index].PullRequest != nil {
				continue
			}
			if !branchMatchesIssuePrefix(branchName, candidate.BranchPrefix) {
				continue
			}
			issues[candidate.Index].PullRequest = &connector.PullRequest{
				Number:     pullRequest.Number,
				URL:        strings.TrimSpace(pullRequest.URL),
				BranchName: branchName,
				State:      strings.ToUpper(strings.TrimSpace(pullRequest.State)),
				ActivityAt: cloneGitHubTime(pullRequest.ActivityAt),
			}
			if issues[candidate.Index].PRNumber == nil && pullRequest.Number > 0 {
				number := pullRequest.Number
				issues[candidate.Index].PRNumber = &number
			}
			if issues[candidate.Index].PRRepository == "" {
				issues[candidate.Index].PRRepository = pullRequestRepoName(repo)
			}
		}
	}
}

func (c *Connector) fetchRepositoryPullRequests(ctx context.Context, repo pullRequestRepo) ([]pullRequestNode, error) {
	pullRequests := []pullRequestNode{}
	for page := 1; page <= pullRequestsPageLimit; page++ {
		pagePullRequests, err := c.fetchRepositoryPullRequestsPage(ctx, repo, page)
		if err != nil {
			return nil, err
		}
		pullRequests = append(pullRequests, pagePullRequests...)
		if len(pagePullRequests) < pullRequestsPageSize {
			break
		}
	}
	return pullRequests, nil
}

func (c *Connector) fetchRepositoryPullRequest(ctx context.Context, repo pullRequestRepo, number int) (pullRequestNode, error) {
	var response restPullRequest
	if err := c.client.REST(ctx, http.MethodGet, restPullRequestPath(repo, number), nil, &response); err != nil {
		return pullRequestNode{}, fmt.Errorf("fetch github pull request: %w", err)
	}
	return pullRequestNodeFromREST(response), nil
}

func (c *Connector) HydratePullRequest(ctx context.Context, issue connector.Issue) (connector.Issue, error) {
	repo, number, ok := hydratedPullRequestRef(issue)
	if !ok {
		return issue, nil
	}
	pullRequest, err := c.fetchRepositoryPullRequest(ctx, repo, number)
	if err != nil {
		if state := c.pullRequestHydrationStateForError(repo, err); state.Reason != "" {
			attachPullRequestHydrationUnavailableToIssue(&issue, repo, number, state)
			return issue, nil
		}
		return issue, fmt.Errorf("hydrate github pull request: %w", err)
	}
	if err := c.populatePullRequestStatus(ctx, repo, &pullRequest, false); err != nil {
		if state := c.pullRequestHydrationStateForError(repo, err); state.Reason != "" {
			applyPullRequestHydrationUnavailableState(&pullRequest, state)
		} else {
			return issue, fmt.Errorf("hydrate github pull request status: %w", err)
		}
	}
	attachPullRequestToIssue(&issue, repo, pullRequest)
	return issue, nil
}

func (c *Connector) MergePullRequest(ctx context.Context, repository string, number int, headSHA string, mergeMethod string) error {
	repo, ok := pullRequestRepoFromName(repository)
	if !ok || number <= 0 {
		return fmt.Errorf("merge github pull request: invalid pull request %s#%d", strings.TrimSpace(repository), number)
	}
	mergeMethod = strings.ToLower(strings.TrimSpace(mergeMethod))
	if mergeMethod == "" {
		mergeMethod = "squash"
	}
	switch mergeMethod {
	case "squash", "merge", "rebase":
	default:
		return errors.New("merge github pull request: merge method must be one of squash, merge, rebase")
	}
	body := map[string]string{
		"merge_method": mergeMethod,
	}
	if headSHA = strings.TrimSpace(headSHA); headSHA != "" {
		body["sha"] = headSHA
	}
	var response restPullRequestMergeResponse
	if err := c.client.REST(ctx, http.MethodPut, restPullRequestMergePath(repo, number), body, &response); err != nil {
		return fmt.Errorf("merge github pull request: %w", err)
	}
	if !response.Merged {
		message := strings.TrimSpace(response.Message)
		if message == "" {
			message = "github did not merge pull request"
		}
		return fmt.Errorf("merge github pull request: %s", message)
	}
	return nil
}

func (c *Connector) RerunPullRequestChecks(ctx context.Context, issue connector.Issue, checks []connector.PullRequestCheck) error {
	repo, _, ok := hydratedPullRequestRef(issue)
	if !ok {
		return errors.New("rerun github pull request checks: missing pull request repository")
	}
	seenRuns := map[int64]struct{}{}
	seenChecks := map[int64]struct{}{}
	var errs []error
	for _, check := range checks {
		if check.WorkflowRunID > 0 {
			if _, ok := seenRuns[check.WorkflowRunID]; ok {
				continue
			}
			seenRuns[check.WorkflowRunID] = struct{}{}
			if err := c.client.REST(ctx, http.MethodPost, restWorkflowRunRerunFailedJobsPath(repo, check.WorkflowRunID), nil, nil); err != nil {
				errs = append(errs, fmt.Errorf("rerun workflow run %d: %w", check.WorkflowRunID, err))
			}
			continue
		}
		if check.ID <= 0 {
			continue
		}
		if _, ok := seenChecks[check.ID]; ok {
			continue
		}
		seenChecks[check.ID] = struct{}{}
		if err := c.client.REST(ctx, http.MethodPost, restCheckRunRerequestPath(repo, check.ID), nil, nil); err != nil {
			errs = append(errs, fmt.Errorf("rerequest check run %d: %w", check.ID, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("rerun github pull request checks: %w", errors.Join(errs...))
	}
	return nil
}

func (c *Connector) ReapplyPullRequestLabel(ctx context.Context, repository string, number int, labelName string, stagger time.Duration) error {
	repo, ok := pullRequestRepoFromName(repository)
	labelName = strings.TrimSpace(labelName)
	if !ok || number <= 0 || labelName == "" {
		return fmt.Errorf("reapply github pull request label: invalid pull request %s#%d or label", strings.TrimSpace(repository), number)
	}
	return citrigger.Reapply(ctx, citrigger.Options{
		CoordinationDir: c.triggerLabelDir,
		Repository:      repository,
		Stagger:         stagger,
	}, citrigger.Dependencies{}, func(ctx context.Context) error {
		ref := issueRef{Owner: repo.Owner, Name: repo.Name, Number: number}
		issue, err := c.fetchRESTIssue(ctx, ref)
		if err != nil {
			return fmt.Errorf("fetch github pull request labels: %w", err)
		}
		if stringSliceContainsFold(labelNames(issue.Labels), labelName) {
			if err := c.client.REST(ctx, http.MethodDelete, restIssueLabelPath(ref, labelName), nil, nil); err != nil {
				return fmt.Errorf("remove github pull request label: %w", err)
			}
		}
		var response []label
		if err := c.client.REST(ctx, http.MethodPost, restIssueLabelsPath(ref), map[string]any{"labels": []string{labelName}}, &response); err != nil {
			return fmt.Errorf("add github pull request label: %w", err)
		}
		if !stringSliceContainsFold(labelNames(nodeConnection[label]{Nodes: response}), labelName) {
			return errors.New("add github pull request label: response did not include label")
		}
		return nil
	})
}

func hydratedPullRequestRef(issue connector.Issue) (pullRequestRepo, int, bool) {
	number := 0
	if issue.PullRequest != nil && issue.PullRequest.Number > 0 {
		number = issue.PullRequest.Number
	}
	if number <= 0 && issue.PRNumber != nil {
		number = *issue.PRNumber
	}
	if number <= 0 {
		return pullRequestRepo{}, 0, false
	}
	if repo, ok := pullRequestRepoFromName(issue.PRRepository); ok {
		return repo, number, true
	}
	if repo, ok := pullRequestRepoFromIdentifier(issue.Identifier); ok {
		return repo, number, true
	}
	return pullRequestRepo{}, 0, false
}

func (c *Connector) fetchRepositoryPullRequestsPage(
	ctx context.Context,
	repo pullRequestRepo,
	page int,
) ([]pullRequestNode, error) {
	var response []restPullRequest
	if err := c.client.REST(ctx, http.MethodGet, restPullRequestsPath(repo, page), nil, &response); err != nil {
		return nil, fmt.Errorf("fetch github pull requests: %w", err)
	}
	pullRequests := make([]pullRequestNode, 0, len(response))
	for _, pullRequest := range response {
		pullRequests = append(pullRequests, pullRequestNodeFromREST(pullRequest))
	}
	return pullRequests, nil
}

func (c *Connector) attachLinkedPullRequests(
	ctx context.Context,
	repo pullRequestRepo,
	issues []connector.Issue,
	candidates []issuePullRequestCandidate,
	useStatusCache bool,
) (string, error) {
	hydrations := make([]linkedPullRequestHydration, 0, len(candidates))
	hydrationByKey := make(map[pullRequestKey]int, len(candidates))
	hydrationByCandidate := make(map[int]int, len(candidates))
	for _, candidate := range candidates {
		if issues[candidate.Index].PullRequest != nil || candidate.PullRequestNumber <= 0 {
			continue
		}
		pullRequestRepo := candidate.PullRequestRepo
		if strings.TrimSpace(pullRequestRepo.Owner) == "" || strings.TrimSpace(pullRequestRepo.Name) == "" {
			pullRequestRepo = repo
		}
		if state, ok := c.currentPullRequestHydrationState(pullRequestRepo); ok {
			c.logPullRequestHydrationSkip(ctx, pullRequestRepo, state, "linked_pull_request")
			attachPullRequestHydrationUnavailableToIssue(&issues[candidate.Index], pullRequestRepo, candidate.PullRequestNumber, state)
			continue
		}
		key := pullRequestKey{Repo: pullRequestRepo, Number: candidate.PullRequestNumber}
		hydrationIndex, ok := hydrationByKey[key]
		if !ok {
			hydrationIndex = len(hydrations)
			hydrationByKey[key] = hydrationIndex
			hydrations = append(hydrations, linkedPullRequestHydration{
				repo:   pullRequestRepo,
				number: candidate.PullRequestNumber,
			})
		}
		hydrationByCandidate[candidate.Index] = hydrationIndex
	}

	concurrentStart := 0
	if c.linkedPullRequestHydrationUsesFiniteFanoutCap() && len(hydrations) > 0 {
		if err := c.hydrateLinkedPullRequest(ctx, &hydrations[0], useStatusCache); err != nil {
			return "", err
		}
		concurrentStart = 1
	}

	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(c.linkedPullRequestHydrationLimit())
	for index := concurrentStart; index < len(hydrations); index++ {
		group.Go(func() error {
			return c.hydrateLinkedPullRequest(groupCtx, &hydrations[index], useStatusCache)
		})
	}
	if err := group.Wait(); err != nil {
		return "", err
	}

	nextCursor := ""
	for _, candidate := range candidates {
		hydrationIndex, ok := hydrationByCandidate[candidate.Index]
		if !ok {
			continue
		}
		hydration := hydrations[hydrationIndex]
		if hydration.state.Reason != "" {
			if hydration.state.Reason == connector.PullRequestHydrationReasonRESTBudgetReserved && nextCursor == "" {
				nextCursor = candidate.Identifier
			}
			if hydration.pullRequest.Number <= 0 {
				attachPullRequestHydrationUnavailableToIssue(&issues[candidate.Index], hydration.repo, candidate.PullRequestNumber, hydration.state)
				continue
			}
		}
		attachPullRequestToIssue(&issues[candidate.Index], hydration.repo, hydration.pullRequest)
	}
	return nextCursor, nil
}

func (c *Connector) hydrateLinkedPullRequest(ctx context.Context, hydration *linkedPullRequestHydration, useStatusCache bool) error {
	pullRequest, err := c.fetchRepositoryPullRequest(ctx, hydration.repo, hydration.number)
	if err != nil {
		state := c.pullRequestHydrationStateForError(hydration.repo, err)
		if state.Reason == "" {
			return err
		}
		hydration.state = state
		return nil
	}
	if err := c.populatePullRequestStatus(ctx, hydration.repo, &pullRequest, useStatusCache); err != nil {
		state := c.pullRequestHydrationStateForError(hydration.repo, err)
		if state.Reason == "" {
			return err
		}
		hydration.state = state
		applyPullRequestHydrationUnavailableState(&pullRequest, state)
	}
	hydration.pullRequest = pullRequest
	return nil
}

func (c *Connector) linkedPullRequestHydrationUsesFiniteFanoutCap() bool {
	return c != nil && c.client != nil && c.client.restPolicy.FanoutMaxRequests > 0
}

func (c *Connector) linkedPullRequestHydrationLimit() int {
	limit := linkedPullRequestHydrationConcurrency
	if c == nil || c.client == nil {
		return limit
	}
	if fanoutLimit := c.client.restPolicy.FanoutMaxRequests; fanoutLimit > 0 {
		limit = int(fanoutLimit) / linkedPullRequestHydrationRequestEstimate
		if limit < 1 {
			return 1
		}
		if limit > linkedPullRequestHydrationConcurrency {
			return linkedPullRequestHydrationConcurrency
		}
	}
	return limit
}

func (c *Connector) attachMatchingPullRequests(
	ctx context.Context,
	repo pullRequestRepo,
	issues []connector.Issue,
	candidates []issuePullRequestCandidate,
	pullRequests []pullRequestNode,
	useStatusCache bool,
) (string, error) {
	hydrated := map[int]pullRequestNode{}
	for _, candidate := range candidates {
		for _, pullRequest := range pullRequests {
			if issues[candidate.Index].PullRequest != nil {
				break
			}
			branchName := strings.TrimSpace(pullRequest.HeadRefName)
			if branchName == "" {
				continue
			}
			if !branchMatchesIssuePrefix(branchName, candidate.BranchPrefix) {
				continue
			}

			hydratedPullRequest, ok := hydrated[pullRequest.Number]
			if !ok {
				var err error
				hydratedPullRequest, err = c.fetchRepositoryPullRequest(ctx, repo, pullRequest.Number)
				if err != nil {
					if state := c.pullRequestHydrationStateForError(repo, err); state.Reason != "" {
						applyPullRequestHydrationUnavailableState(&pullRequest, state)
						hydrated[pullRequest.Number] = pullRequest
						attachPullRequestToIssue(&issues[candidate.Index], repo, pullRequest)
						markPullRequestHydrationUnavailableForCandidates(issues, candidates, repo, state)
						if errors.Is(err, ErrRESTBudgetReserved) {
							return candidate.Identifier, nil
						}
						return "", nil
					}
					return "", err
				}
				if err := c.populatePullRequestStatus(ctx, repo, &hydratedPullRequest, useStatusCache); err != nil {
					if state := c.pullRequestHydrationStateForError(repo, err); state.Reason != "" {
						applyPullRequestHydrationUnavailableState(&hydratedPullRequest, state)
						hydrated[pullRequest.Number] = hydratedPullRequest
						attachPullRequestToIssue(&issues[candidate.Index], repo, hydratedPullRequest)
						markPullRequestHydrationUnavailableForCandidates(issues, candidates, repo, state)
						if errors.Is(err, ErrRESTBudgetReserved) {
							return candidate.Identifier, nil
						}
						return "", nil
					} else {
						return "", err
					}
				}
				hydrated[pullRequest.Number] = hydratedPullRequest
			}
			attachPullRequestToIssue(&issues[candidate.Index], repo, hydratedPullRequest)
			break
		}
	}
	return "", nil
}

func (c *Connector) rotatePullRequestHydrationCandidates(repo pullRequestRepo, candidates []issuePullRequestCandidate) []issuePullRequestCandidate {
	if c == nil || len(candidates) < 2 {
		return candidates
	}
	key := pullRequestRepoName(repo)
	c.mu.RLock()
	cursor := c.prHydrationCursor[key]
	c.mu.RUnlock()
	if cursor == "" {
		return candidates
	}
	for index, candidate := range candidates {
		if candidate.Identifier != cursor || index == 0 {
			continue
		}
		rotated := make([]issuePullRequestCandidate, 0, len(candidates))
		rotated = append(rotated, candidates[index:]...)
		rotated = append(rotated, candidates[:index]...)
		return rotated
	}
	return candidates
}

func (c *Connector) setPullRequestHydrationCursor(repo pullRequestRepo, identifier string) {
	if c == nil {
		return
	}
	key := pullRequestRepoName(repo)
	if key == "" {
		return
	}
	c.mu.Lock()
	if identifier == "" {
		delete(c.prHydrationCursor, key)
	} else {
		if c.prHydrationCursor == nil {
			c.prHydrationCursor = make(map[string]string)
		}
		c.prHydrationCursor[key] = identifier
	}
	c.mu.Unlock()
}

func hasUnattachedBranchPullRequestCandidates(issues []connector.Issue, candidates []issuePullRequestCandidate) bool {
	for _, candidate := range candidates {
		if issues[candidate.Index].PullRequest == nil && strings.TrimSpace(candidate.BranchPrefix) != "" {
			return true
		}
	}
	return false
}

func pullRequestNodeFromREST(pullRequest restPullRequest) pullRequestNode {
	return pullRequestNode{
		NodeID:         pullRequest.NodeID,
		Number:         pullRequest.Number,
		URL:            pullRequest.HTMLURL,
		State:          restPullRequestState(pullRequest),
		MergeableState: strings.ToLower(strings.TrimSpace(pullRequest.MergeableState)),
		Draft:          pullRequest.Draft,
		Labels:         labelNames(nodeConnection[label]{Nodes: pullRequest.Labels}),
		ActivityAt:     cloneGitHubTime(pullRequest.UpdatedAt),
		HeadRefName:    pullRequest.Head.Ref,
		BaseRefName:    pullRequest.Base.Ref,
		HeadSHA:        pullRequest.Head.SHA,
		BaseSHA:        pullRequest.Base.SHA,
	}
}

func attachPullRequestToIssue(issue *connector.Issue, repo pullRequestRepo, pullRequest pullRequestNode) {
	issue.PullRequest = &connector.PullRequest{
		NodeID:                       strings.TrimSpace(pullRequest.NodeID),
		Number:                       pullRequest.Number,
		URL:                          strings.TrimSpace(pullRequest.URL),
		BranchName:                   strings.TrimSpace(pullRequest.HeadRefName),
		BaseRef:                      strings.TrimSpace(pullRequest.BaseRefName),
		State:                        strings.ToUpper(strings.TrimSpace(pullRequest.State)),
		MergeableState:               strings.ToLower(strings.TrimSpace(pullRequest.MergeableState)),
		Draft:                        pullRequest.Draft,
		Labels:                       append([]string{}, pullRequest.Labels...),
		ActivityAt:                   cloneGitHubTime(pullRequest.ActivityAt),
		HeadSHA:                      strings.TrimSpace(pullRequest.HeadSHA),
		BaseSHA:                      strings.TrimSpace(pullRequest.BaseSHA),
		HydrationUnavailableReason:   strings.TrimSpace(pullRequest.HydrationUnavailableReason),
		HydrationDegradedReason:      strings.TrimSpace(pullRequest.HydrationDegradedReason),
		HydrationNextRetryAt:         cloneGitHubTime(pullRequest.HydrationNextRetryAt),
		CIStatus:                     normalizePullRequestCIStatus(pullRequestCIState(pullRequest)),
		CheckRunCount:                pullRequest.CI.CheckRunCount,
		StatusContextCount:           pullRequest.CI.StatusContextCount,
		CIQueueSeconds:               pullRequest.CI.CIQueueSeconds,
		CIDurationSeconds:            pullRequest.CI.CIDurationSeconds,
		SlowChecks:                   append([]connector.PullRequestCheck(nil), pullRequest.CI.SlowChecks...),
		RunningChecks:                append([]string(nil), pullRequest.CI.RunningChecks...),
		StaleSuccessfulChecks:        append([]connector.PullRequestCheck(nil), pullRequest.CI.StaleSuccessfulChecks...),
		RequiredCheckFailures:        append([]connector.PullRequestCheck(nil), pullRequest.CI.RequiredFailures...),
		TransientFailedChecks:        append([]connector.PullRequestCheck(nil), pullRequest.CI.TransientFailures...),
		CodexReviewState:             pullRequestCodexReviewState(pullRequest),
		CodexReviewAPIState:          pullRequestCodexReviewAPIState(pullRequest),
		CodexReviewBodySeverity:      pullRequestCodexReviewBodySeverity(pullRequest),
		CodexReviewSubmittedAt:       pullRequestCodexReviewSubmittedAt(pullRequest),
		CodexReviewFindings:          pullRequestCodexReviewFindings(pullRequest),
		LatestCodexReviewState:       pullRequestLatestCodexReviewState(pullRequest),
		LatestCodexReviewCommitSHA:   pullRequestLatestCodexReviewCommitSHA(pullRequest),
		LatestCodexReviewSubmittedAt: pullRequestLatestCodexReviewSubmittedAt(pullRequest),
	}
	if issue.PRNumber == nil && pullRequest.Number > 0 {
		number := pullRequest.Number
		issue.PRNumber = &number
	}
	if issue.PRRepository == "" {
		issue.PRRepository = pullRequestRepoName(repo)
	}
}

func markPullRequestHydrationUnavailableForCandidates(
	issues []connector.Issue,
	candidates []issuePullRequestCandidate,
	defaultRepo pullRequestRepo,
	state pullRequestHydrationState,
) {
	for _, candidate := range candidates {
		if issues[candidate.Index].PullRequest != nil {
			continue
		}
		repo := candidate.PullRequestRepo
		if strings.TrimSpace(repo.Owner) == "" || strings.TrimSpace(repo.Name) == "" {
			repo = defaultRepo
		}
		attachPullRequestHydrationUnavailableToIssue(&issues[candidate.Index], repo, candidate.PullRequestNumber, state)
	}
}

func attachPullRequestHydrationUnavailableToIssue(issue *connector.Issue, repo pullRequestRepo, number int, state pullRequestHydrationState) {
	if strings.TrimSpace(state.Reason) == "" {
		return
	}
	if issue.PullRequest == nil {
		issue.PullRequest = &connector.PullRequest{}
	}
	if number > 0 {
		issue.PullRequest.Number = number
	}
	issue.PullRequest.HydrationUnavailableReason = strings.TrimSpace(state.Reason)
	issue.PullRequest.HydrationNextRetryAt = cloneGitHubTime(state.NextRetryAt)
	if issue.PRNumber == nil && number > 0 {
		issue.PRNumber = &number
	}
	if issue.PRRepository == "" {
		issue.PRRepository = pullRequestRepoName(repo)
	}
}

func applyPullRequestHydrationUnavailableState(pullRequest *pullRequestNode, state pullRequestHydrationState) {
	if pullRequest == nil || strings.TrimSpace(state.Reason) == "" {
		return
	}
	pullRequest.HydrationUnavailableReason = strings.TrimSpace(state.Reason)
	pullRequest.HydrationNextRetryAt = cloneGitHubTime(state.NextRetryAt)
}

func (c *Connector) currentPullRequestHydrationState(repo pullRequestRepo) (pullRequestHydrationState, bool) {
	if c == nil || c.prHydration == nil {
		return pullRequestHydrationState{}, false
	}
	return c.prHydration.Current(repo)
}

func (c *Connector) pullRequestHydrationStateForError(repo pullRequestRepo, err error) pullRequestHydrationState {
	switch {
	case errors.Is(err, ErrRESTBudgetReserved):
		return pullRequestHydrationState{Reason: connector.PullRequestHydrationReasonRESTBudgetReserved}
	case errors.Is(err, ErrRateLimited):
		var statusErr *StatusError
		if errors.As(err, &statusErr) {
			switch statusErr.RateLimitKind {
			case restRateLimitKindSecondaryThrottled:
				return c.tripPullRequestHydrationCircuit(repo, statusErr.RetryAfter)
			case restRateLimitKindPrimaryExhausted:
				return newPullRequestHydrationState(
					connector.PullRequestHydrationReasonPrimaryExhausted,
					c.pullRequestHydrationRetryAt(statusErr),
				)
			}
		}
		return pullRequestHydrationState{Reason: connector.PullRequestHydrationReasonRateLimited}
	default:
		return pullRequestHydrationState{}
	}
}

func (c *Connector) tripPullRequestHydrationCircuit(repo pullRequestRepo, retryAfter time.Duration) pullRequestHydrationState {
	reason := connector.PullRequestHydrationReasonSecondaryThrottled
	if c == nil || c.prHydration == nil {
		return pullRequestHydrationState{Reason: reason}
	}
	state := c.prHydration.Trip(repo, reason, retryAfter)
	if strings.TrimSpace(state.Reason) == "" {
		return pullRequestHydrationState{Reason: reason}
	}
	return state
}

func (c *Connector) pullRequestHydrationRetryAt(statusErr *StatusError) time.Time {
	if statusErr == nil {
		return time.Time{}
	}
	now := time.Now()
	if c != nil && c.prHydration != nil && c.prHydration.now != nil {
		now = c.prHydration.now()
	}
	if statusErr.RetryAfter > 0 {
		return now.Add(statusErr.RetryAfter)
	}
	if statusErr.ResetAt.After(now) {
		return statusErr.ResetAt
	}
	return time.Time{}
}

func pullRequestRepoName(repo pullRequestRepo) string {
	owner := strings.TrimSpace(repo.Owner)
	name := strings.TrimSpace(repo.Name)
	if owner == "" || name == "" {
		return ""
	}
	return owner + "/" + name
}

func samePullRequestRepo(left pullRequestRepo, right pullRequestRepo) bool {
	return strings.EqualFold(left.Owner, right.Owner) && strings.EqualFold(left.Name, right.Name)
}

func (c *Connector) populatePullRequestStatus(ctx context.Context, repo pullRequestRepo, pullRequest *pullRequestNode, useStatusCache bool) error {
	if useStatusCache && c.pullRequests != nil {
		if status, ok := c.pullRequests.Get(repo, pullRequest.Number, pullRequest.HeadSHA); ok {
			c.logPullRequestCache(ctx, repo, pullRequest, true, false, "")
			applyPullRequestStatus(pullRequest, status)
			return nil
		}
		c.logPullRequestCache(ctx, repo, pullRequest, false, false, "")
	}

	status := pullRequestStatus{}
	if strings.TrimSpace(pullRequest.HeadSHA) != "" {
		ci, err := c.fetchPullRequestCI(ctx, repo, pullRequest.HeadSHA)
		if err != nil {
			state := c.pullRequestHydrationStateForError(repo, err)
			if c.applyCachedPullRequestStatusAfterThrottle(ctx, repo, pullRequest, state) {
				return nil
			}
			return err
		}
		status.ci = ci
	}
	reviews, err := c.fetchPullRequestReviews(ctx, repo, pullRequest.Number, pullRequest.HeadSHA)
	if err != nil {
		state := c.pullRequestHydrationStateForError(repo, err)
		if c.applyCachedPullRequestStatusAfterThrottle(ctx, repo, pullRequest, state) {
			return nil
		}
		return err
	}
	status.reviews = reviews
	if c.pullRequests != nil {
		c.pullRequests.Set(repo, pullRequest.Number, pullRequest.HeadSHA, status)
		c.logPullRequestCache(ctx, repo, pullRequest, false, false, "stored")
	}
	applyPullRequestStatus(pullRequest, status)
	return nil
}

func (c *Connector) applyCachedPullRequestStatusAfterThrottle(ctx context.Context, repo pullRequestRepo, pullRequest *pullRequestNode, state pullRequestHydrationState) bool {
	if c.pullRequests == nil || pullRequest == nil {
		return false
	}
	if strings.TrimSpace(state.Reason) == "" {
		return false
	}
	status, ok := c.pullRequests.Get(repo, pullRequest.Number, pullRequest.HeadSHA)
	if !ok {
		return false
	}
	c.logPullRequestCache(ctx, repo, pullRequest, true, true, state.Reason)
	applyPullRequestStatus(pullRequest, status)
	pullRequest.HydrationDegradedReason = connector.PullRequestHydrationReasonStaleCachedPullData
	pullRequest.HydrationNextRetryAt = cloneGitHubTime(state.NextRetryAt)
	return true
}

func (c *Connector) logPullRequestHydrationSkip(ctx context.Context, repo pullRequestRepo, state pullRequestHydrationState, purpose string) {
	if c == nil || c.logger == nil {
		return
	}
	c.logger.DebugContext(ctx, "github pull request hydration skipped",
		"endpoint_family", "pull requests",
		"request_purpose", "hydrate_pull_request",
		"repository", pullRequestRepoName(repo),
		"cache_hit", true,
		"avoidable_request", true,
		"backoff_reason", strings.TrimSpace(state.Reason),
		"purpose", strings.TrimSpace(purpose),
		"retry_at", state.NextRetryAt,
	)
}

func (c *Connector) logPullRequestCache(ctx context.Context, repo pullRequestRepo, pullRequest *pullRequestNode, hit bool, staleFallback bool, reason string) {
	if c == nil || c.logger == nil || pullRequest == nil {
		return
	}
	c.logger.DebugContext(ctx, "github pull request status cache",
		"endpoint_family", "pull_request_status_cache",
		"request_purpose", "hydrate_pull_request_status",
		"repository", pullRequestRepoName(repo),
		"pr_number", pullRequest.Number,
		"head_sha_known", strings.TrimSpace(pullRequest.HeadSHA) != "",
		"cache_hit", hit,
		"avoidable_request", hit,
		"stale_fallback", staleFallback,
		"backoff_reason", strings.TrimSpace(reason),
	)
}

func applyPullRequestStatus(pullRequest *pullRequestNode, status pullRequestStatus) {
	pullRequest.CI = clonePullRequestCI(status.ci)
	pullRequest.Commits = nodeConnection[pullRequestCommit]{Nodes: []pullRequestCommit{{
		Commit: commitNode{StatusCheckRollup: &statusCheckRollup{State: status.ci.State}},
	}}}
	pullRequest.LatestReviews = nodeConnection[pullRequestReview]{Nodes: clonePullRequestReviews(status.reviews.CurrentHead)}
	pullRequest.CodexReviews = clonePullRequestCodexReviews(status.reviews)
}

func (c *Connector) fetchPullRequestReviews(ctx context.Context, repo pullRequestRepo, number int, headSHA string) (pullRequestCodexReviews, error) {
	response, err := fetchRESTList[restReview](ctx, c.client, restPullRequestReviewsPath(repo, number))
	if err != nil {
		return pullRequestCodexReviews{}, fmt.Errorf("fetch github pull request reviews: %w", err)
	}
	reviews := pullRequestCodexReviews{}
	if review, ok := latestCodexReview(response, headSHA); ok {
		reviews.CurrentHead = []pullRequestReview{review}
	}
	if review, ok := latestCodexReview(response, ""); ok {
		reviews.Latest = []pullRequestReview{review}
	}
	return reviews, nil
}

type pullRequestReference struct {
	Number     int
	Repository string
	UpdatedAt  *time.Time
}

func firstPullRequestReference(pullRequests nodeConnection[pullRequest]) (pullRequestReference, bool) {
	var fallback pullRequestReference
	fallbackOK := false
	var open pullRequestReference
	openOK := false
	var merged pullRequestReference
	mergedOK := false
	for _, pullRequest := range pullRequests.Nodes {
		if pullRequest.Number <= 0 {
			continue
		}
		ref := pullRequestReferenceFromNode(pullRequest)
		if !fallbackOK {
			fallback = ref
			fallbackOK = true
		}
		switch normalizeStateName(pullRequest.State) {
		case "open":
			if !openOK || pullRequestReferenceAfter(ref, open) {
				open = ref
				openOK = true
			}
		case "merged":
			if !mergedOK || pullRequestReferenceAfter(ref, merged) {
				merged = ref
				mergedOK = true
			}
		}
	}
	if openOK {
		return open, true
	}
	if mergedOK {
		return merged, true
	}
	return fallback, fallbackOK
}

func pullRequestReferenceFromNode(pullRequest pullRequest) pullRequestReference {
	return pullRequestReference{
		Number:     pullRequest.Number,
		Repository: strings.TrimSpace(pullRequest.Repository.NameWithOwner),
		UpdatedAt:  parseGitHubTime(pullRequest.UpdatedAt),
	}
}

func pullRequestReferenceAfter(left, right pullRequestReference) bool {
	if left.UpdatedAt != nil && right.UpdatedAt != nil && !left.UpdatedAt.Equal(*right.UpdatedAt) {
		return left.UpdatedAt.After(*right.UpdatedAt)
	}
	if left.UpdatedAt != nil && right.UpdatedAt == nil {
		return true
	}
	if left.UpdatedAt == nil && right.UpdatedAt != nil {
		return false
	}
	return left.Number > right.Number
}

func pullRequestCIState(pullRequest pullRequestNode) string {
	for _, commit := range pullRequest.Commits.Nodes {
		if commit.Commit.StatusCheckRollup != nil {
			return commit.Commit.StatusCheckRollup.State
		}
	}
	return ""
}

func restPullRequestState(pullRequest restPullRequest) string {
	if pullRequest.MergedAt != nil && strings.TrimSpace(*pullRequest.MergedAt) != "" {
		return "MERGED"
	}
	return strings.ToUpper(strings.TrimSpace(pullRequest.State))
}

func latestCodexReview(reviews []restReview, headSHA string) (pullRequestReview, bool) {
	headSHA = strings.TrimSpace(headSHA)
	var latest pullRequestReview
	found := false
	for _, review := range reviews {
		if !codexReviewAuthor(review.User) || strings.EqualFold(strings.TrimSpace(review.State), "DISMISSED") {
			continue
		}
		if headSHA != "" && strings.TrimSpace(review.CommitID) != "" && review.CommitID != headSHA {
			continue
		}
		candidate := pullRequestReview{
			Body:        review.Body,
			URL:         review.HTMLURL,
			State:       review.State,
			Author:      review.User,
			CommitID:    review.CommitID,
			SubmittedAt: review.SubmittedAt,
		}
		if !found || pullRequestReviewAfter(candidate, latest) {
			latest = candidate
			found = true
		}
	}
	return latest, found
}

func codexReviewAuthor(author *actor) bool {
	if author == nil {
		return false
	}
	return strings.Contains(strings.ToLower(strings.TrimSpace(author.Login)), "codex")
}

func pullRequestReviewAfter(left pullRequestReview, right pullRequestReview) bool {
	if left.SubmittedAt == nil {
		return right.SubmittedAt == nil
	}
	if right.SubmittedAt == nil {
		return true
	}
	return left.SubmittedAt.After(*right.SubmittedAt)
}

func pullRequestCodexReviewState(pullRequest pullRequestNode) string {
	return pullRequestCodexReviewStateFromReviews(pullRequest.LatestReviews.Nodes)
}

func pullRequestLatestCodexReviewState(pullRequest pullRequestNode) string {
	return pullRequestCodexReviewStateFromReviews(pullRequest.CodexReviews.Latest)
}

func pullRequestCodexReviewStateFromReviews(reviews []pullRequestReview) string {
	reviewState, bodySeverity := pullRequestCodexReviewStateInputsFromReviews(reviews)
	if bodySeverity != "" {
		return bodySeverity
	}
	return reviewState
}

func pullRequestCodexReviewAPIState(pullRequest pullRequestNode) string {
	return pullRequestCodexReviewAPIStateFromReviews(pullRequest.LatestReviews.Nodes)
}

func pullRequestCodexReviewBodySeverity(pullRequest pullRequestNode) string {
	return pullRequestCodexReviewBodySeverityFromReviews(pullRequest.LatestReviews.Nodes)
}

func pullRequestCodexReviewAPIStateFromReviews(reviews []pullRequestReview) string {
	reviewState, _ := pullRequestCodexReviewStateInputsFromReviews(reviews)
	return reviewState
}

func pullRequestCodexReviewBodySeverityFromReviews(reviews []pullRequestReview) string {
	_, bodySeverity := pullRequestCodexReviewStateInputsFromReviews(reviews)
	return bodySeverity
}

func pullRequestCodexReviewStateInputsFromReviews(reviews []pullRequestReview) (string, string) {
	bodySeverity := ""
	reviewState := ""
	for _, review := range reviews {
		switch reviewBodySeverity(review.Body) {
		case "P1":
			bodySeverity = "P1"
		case "P2":
			if bodySeverity == "" {
				bodySeverity = "P2"
			}
		}
		if state := strings.ToUpper(strings.TrimSpace(review.State)); state != "" {
			reviewState = state
		}
	}
	return reviewState, bodySeverity
}

func pullRequestCodexReviewSubmittedAt(pullRequest pullRequestNode) *time.Time {
	return pullRequestCodexReviewSubmittedAtFromReviews(pullRequest.LatestReviews.Nodes)
}

func pullRequestLatestCodexReviewSubmittedAt(pullRequest pullRequestNode) *time.Time {
	return pullRequestCodexReviewSubmittedAtFromReviews(pullRequest.CodexReviews.Latest)
}

func pullRequestCodexReviewSubmittedAtFromReviews(reviews []pullRequestReview) *time.Time {
	var latest *time.Time
	for _, review := range reviews {
		if review.SubmittedAt == nil {
			continue
		}
		if latest == nil || review.SubmittedAt.After(*latest) {
			value := *review.SubmittedAt
			latest = &value
		}
	}
	return latest
}

func pullRequestLatestCodexReviewCommitSHA(pullRequest pullRequestNode) string {
	for _, review := range pullRequest.CodexReviews.Latest {
		if commitID := strings.TrimSpace(review.CommitID); commitID != "" {
			return commitID
		}
	}
	return ""
}

func pullRequestCodexReviewFindings(pullRequest pullRequestNode) []connector.PullRequestFinding {
	findings := []connector.PullRequestFinding{}
	for _, review := range pullRequest.LatestReviews.Nodes {
		if !containsReviewSeverity(review.Body, "P1") {
			continue
		}
		findings = append(findings, connector.PullRequestFinding{
			Body: strings.TrimSpace(review.Body),
			URL:  strings.TrimSpace(review.URL),
		})
	}
	return findings
}

func containsReviewSeverity(body string, severity string) bool {
	return reviewseverity.Contains(body, severity)
}

func reviewBodySeverity(body string) string {
	return reviewseverity.BodySeverity(body)
}

func pullRequestCheckNames(checks []connector.PullRequestCheck) []string {
	names := make([]string, 0, len(checks))
	for _, check := range checks {
		names = append(names, check.Name)
	}
	return uniqueNonBlank(names)
}
