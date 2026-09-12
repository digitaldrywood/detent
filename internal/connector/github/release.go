package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	releasepkg "github.com/digitaldrywood/detent/internal/release"
)

var closingIssuePattern = regexp.MustCompile(`(?i)(?:fix(?:e[sd])?|close[sd]?|resolve[sd]?)\s+(?:([[:alnum:]_.-]+/[[:alnum:]_.-]+))?#([0-9]+)`)

type releaseRESTRepository struct {
	DefaultBranch string `json:"default_branch"`
}

type releaseRESTRef struct {
	Object struct {
		SHA  string `json:"sha"`
		Type string `json:"type"`
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
	ID         int64  `json:"id"`
	HeadSHA    string `json:"head_sha"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	DetailsURL string `json:"details_url"`
}

type releaseRESTCheckRuns struct {
	TotalCount int                   `json:"total_count"`
	CheckRuns  []releaseRESTCheckRun `json:"check_runs"`
}

type releaseRESTStatus struct {
	SHA        string `json:"sha"`
	TotalCount int    `json:"total_count"`
	Statuses   []struct {
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
	result := releasepkg.Repository{RequiredCheckNames: append([]string(nil), c.requiredChecks...), Name: pullRequestRepoName(c.repository), HeadSHA: strings.TrimSpace(ref.Object.SHA)}
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

	commits, err := c.releaseCommits(ctx, base, result.HeadSHA, result.LatestTag)
	if err != nil {
		return releasepkg.Repository{}, err
	}
	result.Commits = commits
	if result.LatestSHA == result.HeadSHA {
		result.IssueRefs, err = c.releaseTagOrigins(ctx, base, result.LatestTag, result.HeadSHA)
		if err != nil {
			return releasepkg.Repository{}, err
		}
	}
	checks, err := c.releaseChecks(ctx, base, result.HeadSHA)
	if err != nil {
		return releasepkg.Repository{}, err
	}
	result.Checks = checks
	return result, nil
}

func (c *Connector) releaseTagOrigins(ctx context.Context, base, tag, sha string) ([]string, error) {
	var ref releaseRESTRef
	if err := c.client.REST(ctx, http.MethodGet, base+"/git/ref/tags/"+url.PathEscape(tag), nil, &ref); err != nil {
		return nil, fmt.Errorf("read release origin reference: %w", err)
	}
	if ref.Object.Type == "tag" {
		var object struct {
			Message string `json:"message"`
		}
		if err := c.client.REST(ctx, http.MethodGet, base+"/git/tags/"+url.PathEscape(ref.Object.SHA), nil, &object); err != nil {
			return nil, fmt.Errorf("read release origins: %w", err)
		}
		message := strings.TrimSpace(object.Message)
		line := message[strings.LastIndex(message, "\n")+1:]
		if suffix, found := strings.CutPrefix(line, "<!-- detent-release-origins:"); found {
			value, closed := strings.CutSuffix(suffix, " -->")
			var refs []string
			if err := json.Unmarshal([]byte(value), &refs); err != nil || !closed {
				return nil, errors.New("invalid release origin metadata")
			}
			return refs, nil
		}
	}
	return c.releaseCommitIssueRefs(ctx, base, sha)
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
			if len(match) != 3 {
				continue
			}
			origin := repository
			if match[1] != "" {
				origin = match[1]
			}
			ref := origin + "#" + match[2]
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
	if runs.TotalCount > len(runs.CheckRuns) {
		return nil, errors.New("release check evidence is truncated")
	}
	checks := make([]releasepkg.Check, 0, len(runs.CheckRuns))
	for _, run := range runs.CheckRuns {
		checks = append(checks, releasepkg.Check{
			SHA:        run.HeadSHA,
			Name:       run.Name,
			Status:     run.Status,
			Conclusion: run.Conclusion,
			RunID:      workflowRunID(run.DetailsURL),
			CheckRunID: run.ID,
		})
	}
	var combined releaseRESTStatus
	if err := c.client.REST(ctx, http.MethodGet, base+"/commits/"+url.PathEscape(sha)+"/status", nil, &combined); err != nil {
		return nil, fmt.Errorf("inspect release statuses: %w", err)
	}
	if combined.TotalCount > len(combined.Statuses) {
		return nil, errors.New("release status evidence is truncated")
	}
	for _, status := range combined.Statuses {
		check := releasepkg.Check{SHA: combined.SHA, Name: status.Context}
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
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if found, err := c.releaseTagMatches(ctx, tag); err != nil || found {
		return err
	}
	base := restRepositoryPath(pullRequestRepoName(c.repository))
	var object struct {
		SHA string `json:"sha"`
	}
	message := c.protectPublicationText(ctx, pullRequestRepoName(c.repository), tag.Message)
	payload := map[string]any{"tag": tag.Name, "message": message, "object": tag.SHA, "type": "commit"}
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
	if found, lookupErr := c.releaseTagMatches(ctx, tag); lookupErr != nil {
		return errors.Join(err, lookupErr)
	} else if found {
		return nil
	}
	return fmt.Errorf("publish release tag: %w", err)
}

func (c *Connector) releaseTagMatches(ctx context.Context, tag releasepkg.Tag) (bool, error) {
	var commit releaseRESTCommit
	path := restRepositoryPath(pullRequestRepoName(c.repository)) + "/commits/" + url.PathEscape("refs/tags/"+tag.Name)
	if err := c.client.REST(ctx, http.MethodGet, path, nil, &commit); err != nil {
		var statusErr *StatusError
		if errors.As(err, &statusErr) && statusErr.StatusCode == http.StatusNotFound {
			return false, nil
		}
		return false, fmt.Errorf("reconcile release tag: %w", err)
	}
	if commit.SHA != tag.SHA {
		return false, fmt.Errorf("release tag %s points to %s, expected %s", tag.Name, commit.SHA, tag.SHA)
	}
	return true, nil
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
		var run struct {
			HeadSHA string `json:"head_sha"`
			Attempt int    `json:"run_attempt"`
			Status  string `json:"status"`
		}
		path := restRepositoryPath(pullRequestRepoName(c.repository)) + "/actions/runs/" + strconv.FormatInt(check.RunID, 10)
		if err := c.client.REST(ctx, http.MethodGet, path, nil, &run); err != nil {
			errs = append(errs, fmt.Errorf("reconcile workflow %d: %w", check.RunID, err))
			continue
		}
		if run.HeadSHA != check.SHA || run.Attempt < 1 {
			errs = append(errs, fmt.Errorf("workflow %d has incomplete or stale retry evidence", check.RunID))
			continue
		}
		if run.Status != "completed" {
			continue
		}
		if err := c.client.REST(ctx, http.MethodPost, restWorkflowRunRerunFailedJobsPath(c.repository, check.RunID), nil, nil); err != nil {
			errs = append(errs, fmt.Errorf("rerun workflow %d: %w", check.RunID, err))
		}
	}
	return errors.Join(errs...)
}

func (c *Connector) EnsureReleaseReport(ctx context.Context, failure releasepkg.Report) (bool, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	repository := pullRequestRepoName(c.repository)
	refs := append([]string(nil), failure.IssueRefs...)
	sort.Strings(refs)
	for _, ref := range refs {
		repo, number, ok := strings.Cut(ref, "#")
		n, err := strconv.Atoi(number)
		if !ok || repo != repository || err != nil || n <= 0 {
			continue
		}
		path := restRepositoryPath(repository) + "/issues/" + number + "/comments"
		marker := "<!-- detent-auto-release:" + failure.Fingerprint + " -->"
		if found, err := c.releaseReportExists(ctx, path, marker); err != nil || found {
			return false, err
		}
		body := c.protectPublicationText(ctx, repository, failure.Body)
		if !strings.Contains(body, marker) {
			body += "\n\n" + marker
		}
		if err := c.client.REST(ctx, http.MethodPost, path, map[string]string{"body": body}, nil); err != nil {
			found, lookupErr := c.releaseReportExists(ctx, path, marker)
			if found && lookupErr == nil {
				return false, nil
			}
			return false, errors.Join(err, lookupErr)
		}
		return true, nil
	}
	return false, errors.New("release report has no originating issue in this repository")
}

func (c *Connector) releaseReportExists(ctx context.Context, path, marker string) (bool, error) {
	for page := 1; ; page++ {
		var comments []struct {
			Body string `json:"body"`
		}
		if err := c.client.REST(ctx, http.MethodGet, fmt.Sprintf("%s?per_page=100&page=%d", path, page), nil, &comments); err != nil {
			return false, fmt.Errorf("read release evidence: %w", err)
		}
		for _, comment := range comments {
			if strings.Contains(comment.Body, marker) {
				return true, nil
			}
		}
		if len(comments) < 100 {
			return false, nil
		}
	}
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
