package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type BranchMergePolicy struct {
	Branch         string
	MergeQueue     bool
	Strict         bool
	AdmissionLimit int
}

type branchMergePolicySnapshot struct {
	Repository string
	Policy     BranchMergePolicy
	CheckedAt  time.Time
}

func (c *Connector) RepositoryBranchMergePolicy(ctx context.Context, repository, branch string) (BranchMergePolicy, error) {
	repo, ok := pullRequestRepoFromName(repository)
	if !ok {
		return BranchMergePolicy{}, fmt.Errorf("invalid GitHub repository %q", repository)
	}
	base := "/repos/" + repo.Owner + "/" + repo.Name
	if branch == "" {
		var info struct {
			DefaultBranch string `json:"default_branch"`
		}
		if err := c.client.REST(ctx, http.MethodGet, base, nil, &info); err != nil {
			return BranchMergePolicy{}, err
		}
		branch = strings.TrimSpace(info.DefaultBranch)
	}
	if branch == "" {
		return BranchMergePolicy{}, errors.New("github repository has no default branch")
	}
	policy := BranchMergePolicy{Branch: branch}
	for page := 1; ; page++ {
		var rules []struct {
			Type       string `json:"type"`
			Parameters struct {
				Strict     bool `json:"strict_required_status_checks_policy"`
				MaxEntries int  `json:"max_entries_to_build"`
			} `json:"parameters"`
		}
		path := fmt.Sprintf("%s/rules/branches/%s?per_page=100&page=%d", base, url.PathEscape(branch), page)
		if err := c.client.REST(ctx, http.MethodGet, path, nil, &rules); err != nil {
			return BranchMergePolicy{}, fmt.Errorf("read branch rules: %w", err)
		}
		for _, rule := range rules {
			switch rule.Type {
			case "merge_queue":
				policy.MergeQueue = true
				policy.AdmissionLimit = rule.Parameters.MaxEntries
			case "required_status_checks":
				policy.Strict = policy.Strict || rule.Parameters.Strict
			}
		}
		if len(rules) < 100 {
			break
		}
	}
	return policy, nil
}

func (c *Connector) RepositoryStrictMergePolicy(ctx context.Context, repository string) (BranchMergePolicy, error) {
	policy, err := c.RepositoryBranchMergePolicy(ctx, repository, "")
	if err != nil {
		return policy, err
	}
	var checks struct {
		Strict bool `json:"strict"`
	}
	path := "/repos/" + repository + "/branches/" + url.PathEscape(policy.Branch) + "/protection/required_status_checks"
	if err := c.client.REST(ctx, http.MethodGet, path, nil, &checks); err != nil && !errors.Is(err, ErrNotFound) {
		return policy, fmt.Errorf("read strict branch protection: %w", err)
	}
	policy.Strict = policy.Strict || checks.Strict
	return policy, nil
}

func (c *Connector) RefreshMergeQueuePolicy(ctx context.Context) error {
	repository := pullRequestRepoName(c.repository)
	if strings.Trim(repository, "/ ") == "" {
		return nil
	}
	policy, err := c.RepositoryBranchMergePolicy(ctx, repository, "")
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.branchMergePolicy = branchMergePolicySnapshot{Repository: repository, Policy: policy, CheckedAt: c.now()}
	c.mu.Unlock()
	return nil
}
