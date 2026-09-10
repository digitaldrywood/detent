package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type BranchMergePolicy struct {
	RulesUnavailableOnPlan bool
	Branch                 string
	MergeQueue             bool
	Strict                 bool
	AdmissionLimit         int
}

type branchMergePolicySnapshot struct {
	Policy    BranchMergePolicy
	CheckedAt time.Time
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
			if errors.Is(err, errBranchRulesUnavailableOnPlan) {
				return BranchMergePolicy{Branch: branch, RulesUnavailableOnPlan: true}, nil
			}
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
	if err != nil || policy.RulesUnavailableOnPlan {
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
	c.cacheBranchMergePolicy(repository, policy)
	return nil
}

func (c *Connector) branchMergePolicy(ctx context.Context, repository, branch string) (BranchMergePolicy, error) {
	key := strings.ToLower(repository) + "@" + branch
	c.mu.RLock()
	cached, ok := c.branchMergePolicies[key]
	c.mu.RUnlock()
	if ok && c.now().Sub(cached.CheckedAt) < 5*time.Minute {
		return cached.Policy, nil
	}
	policy, err := c.RepositoryBranchMergePolicy(ctx, repository, branch)
	if err != nil {
		return BranchMergePolicy{}, err
	}
	c.cacheBranchMergePolicy(repository, policy)
	return policy, nil
}

func (c *Connector) cacheBranchMergePolicy(repository string, policy BranchMergePolicy) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.branchMergePolicies == nil {
		c.branchMergePolicies = make(map[string]branchMergePolicySnapshot)
	}
	key := strings.ToLower(repository) + "@" + policy.Branch
	c.branchMergePolicies[key] = branchMergePolicySnapshot{Policy: policy, CheckedAt: c.now()}
}

// The plan response is endpoint-specific: other 403s retain their normal
// authentication and rate-limit handling.
var errBranchRulesUnavailableOnPlan = errors.New("branch rules unavailable on this plan")

func branchRulesRepository(method, path string) string {
	if method != http.MethodGet {
		return ""
	}
	parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 6)
	if len(parts) != 6 || parts[0] != "repos" || parts[3] != "rules" || parts[4] != "branches" {
		return ""
	}
	return strings.ToLower(parts[1] + "/" + parts[2])
}

func branchRulesUnavailableOnPlan(status int, raw []byte) bool {
	if status != http.StatusForbidden {
		return false
	}
	var body struct {
		Message string `json:"message"`
	}
	return json.Unmarshal(raw, &body) == nil && body.Message == "Upgrade to GitHub Pro or make this repository public to enable this feature."
}

// Keep informational deduplication across connector recreation on workflow reload.
// Successful probes clear the entry so a later plan change is reported again.
var unavailableBranchRules sync.Map

func (c *Client) recordBranchRulesAvailability(ctx context.Context, repository string, unavailable bool) {
	key := c.restEndpoint + "/" + repository
	if !unavailable {
		unavailableBranchRules.Delete(key)
		return
	}
	if _, loaded := unavailableBranchRules.LoadOrStore(key, http.StatusForbidden); !loaded {
		c.logger.InfoContext(ctx, "github branch rules not available on this plan", "repository", repository, "status", http.StatusForbidden)
	}
}
