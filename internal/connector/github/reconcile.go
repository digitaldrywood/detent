package github

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
)

var _ connector.TargetedReconciler = (*Connector)(nil)

func (c *Connector) ReconcileIssue(ctx context.Context, target connector.ReconcileTarget) (connector.ReconcileResult, error) {
	repo, ok := pullRequestRepoFromName(target.Scope)
	if !ok {
		return connector.ReconcileResult{}, nil
	}
	if validPullRequestRepo(c.repository) && !samePullRequestRepo(c.repository, repo) {
		return connector.ReconcileResult{}, nil
	}

	pullRequest, hasPullRequest, err := c.reconcilePullRequest(ctx, repo, target)
	if err != nil {
		return connector.ReconcileResult{}, err
	}
	ref, ok := reconcileIssueRef(repo, target, pullRequest, hasPullRequest)
	if !ok {
		return connector.ReconcileResult{}, nil
	}

	issue, found, err := c.reconcileIssueByRef(ctx, ref)
	if err != nil || !found {
		return connector.ReconcileResult{Issue: issue, Found: found}, err
	}
	if normalizeStateName(issue.State) == normalizeStateName("Blocked") {
		issues := []connector.Issue{issue}
		if err := c.populateBlockerReasons(ctx, issues); err != nil {
			return connector.ReconcileResult{}, err
		}
		c.hydrateBlockedByRefs(ctx, issues)
		if err := c.resolveBlockedByProjectState(ctx, issues); err != nil {
			return connector.ReconcileResult{}, err
		}
		issue = issues[0]
	}
	if hasPullRequest {
		if err := c.populatePullRequestStatus(ctx, repo, &pullRequest, false); err != nil {
			if state := c.pullRequestHydrationStateForError(repo, err); state.Reason != "" {
				applyPullRequestHydrationUnavailableState(&pullRequest, state)
			} else {
				return connector.ReconcileResult{}, fmt.Errorf("reconcile github pull request status: %w", err)
			}
		}
		attachPullRequestToIssue(&issue, repo, pullRequest)
	}
	return connector.ReconcileResult{Issue: issue, Found: true}, nil
}

func (c *Connector) reconcilePullRequest(ctx context.Context, repo pullRequestRepo, target connector.ReconcileTarget) (pullRequestNode, bool, error) {
	if target.ChangeNumber <= 0 {
		return pullRequestNode{}, false, nil
	}
	pullRequest, err := c.fetchRepositoryPullRequest(ctx, repo, target.ChangeNumber)
	if err != nil {
		return pullRequestNode{}, false, err
	}
	return pullRequest, true, nil
}

func (c *Connector) reconcileIssueByRef(ctx context.Context, ref issueRef) (connector.Issue, bool, error) {
	switch {
	case c.usesLabelStatus():
		return c.fetchLabelIssueByRef(ctx, ref)
	case c.usesIssueFieldStatus():
		return c.fetchIssueFieldIssueByRef(ctx, ref)
	default:
		return c.fetchProjectIssueByRef(ctx, ref)
	}
}

func reconcileIssueRef(repo pullRequestRepo, target connector.ReconcileTarget, pullRequest pullRequestNode, hasPullRequest bool) (issueRef, bool) {
	if target.WorkItemNumber > 0 {
		return issueRef{Owner: repo.Owner, Name: repo.Name, Number: target.WorkItemNumber}, true
	}
	branch := strings.TrimSpace(target.Branch)
	if branch == "" && hasPullRequest {
		branch = pullRequest.HeadRefName
	}
	return issueRefFromDetentBranch(repo, branch)
}

func issueRefFromDetentBranch(repo pullRequestRepo, branch string) (issueRef, bool) {
	branch = strings.ToLower(strings.TrimSpace(branch))
	branch, ok := strings.CutPrefix(branch, "detent/")
	if !ok {
		return issueRef{}, false
	}
	branch = strings.TrimPrefix(branch, "detent-")
	repositoryKey := strings.ToLower(branchKeyPattern.ReplaceAllString(pullRequestRepoName(repo), "_"))
	remainder, ok := strings.CutPrefix(branch, repositoryKey+"_")
	if !ok {
		return issueRef{}, false
	}
	end := 0
	for end < len(remainder) && remainder[end] >= '0' && remainder[end] <= '9' {
		end++
	}
	if end == 0 {
		return issueRef{}, false
	}
	number, err := strconv.Atoi(remainder[:end])
	if err != nil || number <= 0 {
		return issueRef{}, false
	}
	return issueRef{Owner: repo.Owner, Name: repo.Name, Number: number}, true
}
