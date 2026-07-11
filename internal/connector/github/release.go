package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	releasepkg "github.com/digitaldrywood/detent/internal/release"
)

var closingIssuePattern = regexp.MustCompile(`(?i)(?:fix(?:e[sd])?|close[sd]?|resolve[sd]?)\s+(?:[[:alnum:]_.-]+/[[:alnum:]_.-]+)?#([0-9]+)`)

type releaseRESTRepository struct {
	DefaultBranch string `json:"default_branch"`
}

type releaseRESTRef struct {
	Object struct {
		SHA string `json:"sha"`
	} `json:"object"`
}

type releaseRESTTag struct {
	Name string `json:"name"`
}

type releaseRESTCommit struct {
	SHA    string `json:"sha"`
	Commit struct {
		Message   string `json:"message"`
		Committer struct {
			Date time.Time `json:"date"`
		} `json:"committer"`
	} `json:"commit"`
}

type releaseRESTCompare struct {
	Commits []releaseRESTCommit `json:"commits"`
}

type releaseRESTPullRequest struct {
	Number int    `json:"number"`
	Body   string `json:"body"`
}

type releaseRESTCheckRun struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	DetailsURL string `json:"details_url"`
}

type releaseRESTCheckRuns struct {
	CheckRuns []releaseRESTCheckRun `json:"check_runs"`
}

type releaseRESTStatus struct {
	Statuses []struct {
		Context string `json:"context"`
		State   string `json:"state"`
	} `json:"statuses"`
}

type releaseRESTWorkflowRuns struct {
	WorkflowRuns []struct {
		ID         int64  `json:"id"`
		Name       string `json:"name"`
		HeadBranch string `json:"head_branch"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		HTMLURL    string `json:"html_url"`
	} `json:"workflow_runs"`
}

func (c *Connector) Inspect(ctx context.Context) (releasepkg.Repository, error) {
	if c == nil || c.client == nil || !validPullRequestRepo(c.repository) {
		return releasepkg.Repository{}, ErrMissingRepository
	}
	base := restRepositoryPath(pullRequestRepoName(c.repository))
	var repository releaseRESTRepository
	if err := c.client.REST(ctx, http.MethodGet, base, nil, &repository); err != nil {
		return releasepkg.Repository{}, fmt.Errorf("inspect release repository: %w", err)
	}
	branch := strings.TrimSpace(repository.DefaultBranch)
	if branch == "" {
		branch = "main"
	}
	var ref releaseRESTRef
	if err := c.client.REST(ctx, http.MethodGet, base+"/git/ref/heads/"+url.PathEscape(branch), nil, &ref); err != nil {
		return releasepkg.Repository{}, fmt.Errorf("inspect release head: %w", err)
	}
	result := releasepkg.Repository{Name: pullRequestRepoName(c.repository), HeadSHA: strings.TrimSpace(ref.Object.SHA)}
	if result.HeadSHA == "" {
		return releasepkg.Repository{}, errors.New("inspect release head: github returned an empty sha")
	}

	var tags []releaseRESTTag
	if err := c.client.REST(ctx, http.MethodGet, base+"/tags?per_page=100", nil, &tags); err != nil {
		return releasepkg.Repository{}, fmt.Errorf("inspect release tags: %w", err)
	}
	tagNames := make([]string, 0, len(tags))
	for _, tag := range tags {
		tagNames = append(tagNames, tag.Name)
	}
	result.LatestTag = releasepkg.LatestTag(tagNames)
	if result.LatestTag != "" {
		var tagged releaseRESTCommit
		if err := c.client.REST(ctx, http.MethodGet, base+"/commits/"+url.PathEscape(result.LatestTag), nil, &tagged); err != nil {
			return releasepkg.Repository{}, fmt.Errorf("inspect latest release: %w", err)
		}
		result.LatestSHA = strings.TrimSpace(tagged.SHA)
		result.TaggedAt = tagged.Commit.Committer.Date
	}

	commits, err := c.releaseCommits(ctx, base, branch, result.LatestTag)
	if err != nil {
		return releasepkg.Repository{}, err
	}
	result.Commits = commits
	checks, err := c.releaseChecks(ctx, base, result.HeadSHA)
	if err != nil {
		return releasepkg.Repository{}, err
	}
	result.Checks = checks
	return result, nil
}

func (c *Connector) releaseCommits(ctx context.Context, base string, branch string, latestTag string) ([]releasepkg.Commit, error) {
	var commits []releaseRESTCommit
	if latestTag == "" {
		if err := c.client.REST(ctx, http.MethodGet, base+"/commits?sha="+url.QueryEscape(branch)+"&per_page=100", nil, &commits); err != nil {
			return nil, fmt.Errorf("inspect unreleased commits: %w", err)
		}
		sort.Slice(commits, func(i, j int) bool { return commits[i].Commit.Committer.Date.Before(commits[j].Commit.Committer.Date) })
	} else {
		var comparison releaseRESTCompare
		path := base + "/compare/" + url.PathEscape(latestTag) + "..." + url.PathEscape(branch) + "?per_page=100"
		if err := c.client.REST(ctx, http.MethodGet, path, nil, &comparison); err != nil {
			return nil, fmt.Errorf("inspect unreleased commits: %w", err)
		}
		commits = comparison.Commits
	}
	result := make([]releasepkg.Commit, 0, len(commits))
	for _, commit := range commits {
		refs, err := c.releaseCommitIssueRefs(ctx, base, commit.SHA)
		if err != nil {
			return nil, err
		}
		result = append(result, releasepkg.Commit{
			SHA:       strings.TrimSpace(commit.SHA),
			Message:   strings.TrimSpace(commit.Commit.Message),
			MergedAt:  commit.Commit.Committer.Date,
			IssueRefs: refs,
		})
	}
	return result, nil
}

func (c *Connector) releaseCommitIssueRefs(ctx context.Context, base string, sha string) ([]string, error) {
	var pulls []releaseRESTPullRequest
	if err := c.client.REST(ctx, http.MethodGet, base+"/commits/"+url.PathEscape(sha)+"/pulls?per_page=10", nil, &pulls); err != nil {
		return nil, fmt.Errorf("inspect commit pull requests: %w", err)
	}
	seen := make(map[string]struct{})
	refs := make([]string, 0, len(pulls))
	repository := pullRequestRepoName(c.repository)
	for _, pull := range pulls {
		for _, match := range closingIssuePattern.FindAllStringSubmatch(pull.Body, -1) {
			if len(match) != 2 {
				continue
			}
			ref := repository + "#" + match[1]
			if _, ok := seen[ref]; ok {
				continue
			}
			seen[ref] = struct{}{}
			refs = append(refs, ref)
		}
	}
	sort.Strings(refs)
	return refs, nil
}

func (c *Connector) releaseChecks(ctx context.Context, base string, sha string) ([]releasepkg.Check, error) {
	var runs releaseRESTCheckRuns
	if err := c.client.REST(ctx, http.MethodGet, base+"/commits/"+url.PathEscape(sha)+"/check-runs?per_page=100", nil, &runs); err != nil {
		return nil, fmt.Errorf("inspect release check runs: %w", err)
	}
	checks := make([]releasepkg.Check, 0, len(runs.CheckRuns))
	for _, run := range runs.CheckRuns {
		checks = append(checks, releasepkg.Check{
			Name:       run.Name,
			Status:     run.Status,
			Conclusion: run.Conclusion,
			RunID:      workflowRunID(run.DetailsURL),
		})
	}
	var combined releaseRESTStatus
	if err := c.client.REST(ctx, http.MethodGet, base+"/commits/"+url.PathEscape(sha)+"/status", nil, &combined); err != nil {
		return nil, fmt.Errorf("inspect release statuses: %w", err)
	}
	for _, status := range combined.Statuses {
		check := releasepkg.Check{Name: status.Context}
		switch strings.ToLower(strings.TrimSpace(status.State)) {
		case "pending":
			check.Status = "in_progress"
		case "success":
			check.Status = "completed"
			check.Conclusion = "success"
		default:
			check.Status = "completed"
			check.Conclusion = status.State
		}
		checks = append(checks, check)
	}
	return checks, nil
}

func (c *Connector) CreateTag(ctx context.Context, tag releasepkg.Tag) error {
	base := restRepositoryPath(pullRequestRepoName(c.repository))
	var object struct {
		SHA string `json:"sha"`
	}
	payload := map[string]any{"tag": tag.Name, "message": tag.Message, "object": tag.SHA, "type": "commit"}
	if err := c.client.REST(ctx, http.MethodPost, base+"/git/tags", payload, &object); err != nil {
		return fmt.Errorf("create annotated release tag: %w", err)
	}
	if strings.TrimSpace(object.SHA) == "" {
		return errors.New("create annotated release tag: github returned an empty tag object sha")
	}
	err := c.client.REST(ctx, http.MethodPost, base+"/git/refs", map[string]any{"ref": "refs/tags/" + tag.Name, "sha": object.SHA}, nil)
	if err == nil {
		return nil
	}
	var statusErr *StatusError
	if errors.As(err, &statusErr) && statusErr.StatusCode == http.StatusUnprocessableEntity {
		return releasepkg.ErrTagExists
	}
	return fmt.Errorf("publish release tag: %w", err)
}

func (c *Connector) ReleaseWorkflow(ctx context.Context, tag string) (releasepkg.WorkflowRun, bool, error) {
	base := restRepositoryPath(pullRequestRepoName(c.repository))
	var response releaseRESTWorkflowRuns
	path := base + "/actions/runs?event=push&branch=" + url.QueryEscape(tag) + "&per_page=100"
	if err := c.client.REST(ctx, http.MethodGet, path, nil, &response); err != nil {
		return releasepkg.WorkflowRun{}, false, fmt.Errorf("inspect release workflow: %w", err)
	}
	for _, run := range response.WorkflowRuns {
		if !strings.EqualFold(strings.TrimSpace(run.Name), "release") || !strings.EqualFold(strings.TrimSpace(run.HeadBranch), strings.TrimSpace(tag)) {
			continue
		}
		return releasepkg.WorkflowRun{ID: run.ID, URL: run.HTMLURL, Status: run.Status, Conclusion: run.Conclusion}, true, nil
	}
	return releasepkg.WorkflowRun{}, false, nil
}

func (c *Connector) RerunFailedChecks(ctx context.Context, checks []releasepkg.Check) error {
	seen := make(map[int64]struct{})
	var errs []error
	for _, check := range checks {
		if check.RunID <= 0 {
			continue
		}
		if _, ok := seen[check.RunID]; ok {
			continue
		}
		seen[check.RunID] = struct{}{}
		if err := c.client.REST(ctx, http.MethodPost, restWorkflowRunRerunFailedJobsPath(c.repository, check.RunID), nil, nil); err != nil {
			errs = append(errs, fmt.Errorf("rerun workflow %d: %w", check.RunID, err))
		}
	}
	return errors.Join(errs...)
}

func (c *Connector) EnsureFailureIssue(ctx context.Context, failure releasepkg.Failure) (bool, error) {
	repository := pullRequestRepoName(c.repository)
	marker := "detent-auto-release:" + failure.Fingerprint
	query := "repo:" + repository + " is:issue \"" + marker + "\""
	var search struct {
		TotalCount int `json:"total_count"`
		Items      []struct {
			NodeID string `json:"node_id"`
		} `json:"items"`
	}
	if err := c.client.REST(ctx, http.MethodGet, "/search/issues?q="+url.QueryEscape(query)+"&per_page=1", nil, &search); err != nil {
		return false, fmt.Errorf("find auto-release failure issue: %w", err)
	}
	if search.TotalCount > 0 {
		if len(search.Items) > 0 && strings.TrimSpace(search.Items[0].NodeID) != "" {
			if err := c.moveReleaseIssueToTodo(ctx, search.Items[0].NodeID); err != nil {
				return false, fmt.Errorf("restore auto-release failure issue to board: %w", err)
			}
		}
		return false, nil
	}
	labels := []string(nil)
	if c.usesLabelStatus() {
		labels = []string{c.statusLabelForState("Todo")}
	}
	issue, err := c.CreateIssue(ctx, connector.IssueDraft{
		Title:  failure.Title,
		Body:   failure.Body,
		Labels: labels,
	})
	if err != nil {
		return false, err
	}
	if err := c.moveReleaseIssueToTodo(ctx, issue.ID); err != nil {
		return false, fmt.Errorf("add auto-release failure issue to board: %w", err)
	}
	return true, nil
}

func (c *Connector) moveReleaseIssueToTodo(ctx context.Context, issueID string) error {
	if !c.usesLabelStatus() && !c.usesIssueFieldStatus() {
		if err := c.addIntakeIssueToProject(ctx, issueID); err != nil {
			return err
		}
	}
	return c.UpdateIssueState(ctx, issueID, "Todo")
}

func workflowRunID(detailsURL string) int64 {
	marker := "/actions/runs/"
	index := strings.Index(detailsURL, marker)
	if index < 0 {
		return 0
	}
	value := detailsURL[index+len(marker):]
	if slash := strings.IndexByte(value, '/'); slash >= 0 {
		value = value[:slash]
	}
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0
	}
	return id
}

var _ releasepkg.Backend = (*Connector)(nil)
