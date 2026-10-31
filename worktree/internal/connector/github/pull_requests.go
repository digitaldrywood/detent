package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/citrigger"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/reviewseverity"
)

type pullRequestDiffFingerprintCacheKey struct {
	Repository string
	Number     int
	HeadSHA    string
	BaseSHA    string
}

type pullRequestRepo struct {
	Owner string
	Name  string
}

const (
	codexReviewSummaryMarker             = "<!-- codex-pull-request-review-summary -->"
	codexReviewSummaryHeading            = "## Codex Review Summary"
	codexReviewSummaryTableHeader        = "| Review | Status | Commit | Review trigger |"
	codexReviewSummaryTableSeparator     = "| --- | --- | --- | --- |"
	minimumCodexReviewCommitPrefixLength = 7
)

const pullRequestReviewThreadsQuery = `
query DetentGitHubPullRequestReviewThreads($owner: String!, $name: String!, $number: Int!, $after: String) {
	repository(owner: $owner, name: $name) {
		pullRequest(number: $number) {
			headRefOid
			reviewThreads(first: 100, after: $after) {
				pageInfo { hasNextPage endCursor }
				nodes { isResolved isOutdated path line originalLine comments(first: 1) { nodes { body } } }
			}
		}
	}
	rateLimit { limit used remaining cost resetAt }
}`

var (
	codexReviewSummaryStatusPattern = regexp.MustCompile(`^✅\s+\*\*Completed\*\*\s+<relative-time datetime="([^"]+)">([^<]+)</relative-time>$`)
	codexReviewSummaryCommitPattern = regexp.MustCompile("^`([0-9a-fA-F]{7,40})`$")
)

func (c *Connector) fetchRepositoryPullRequest(ctx context.Context, repo pullRequestRepo, number int) (pullRequestNode, error) {
	var response restPullRequest
	if err := c.client.REST(ctx, http.MethodGet, restPullRequestPath(repo, number), nil, &response); err != nil {
		return pullRequestNode{}, fmt.Errorf("fetch github pull request: %w", err)
	}
	return pullRequestNodeFromREST(response), nil
}

func (c *Connector) HydratePullRequest(ctx context.Context, issue connector.Issue) (connector.Issue, error) {
	return c.hydratePullRequest(ctx, issue, true)
}

func (c *Connector) hydratePullRequest(ctx context.Context, issue connector.Issue, includeStatus bool) (connector.Issue, error) {
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
	if includeStatus {
		if err := c.populatePullRequestStatus(ctx, repo, &pullRequest, false); err != nil {
			if state := c.pullRequestHydrationStateForError(repo, err); state.Reason != "" {
				applyPullRequestHydrationUnavailableState(&pullRequest, state)
			} else {
				return issue, fmt.Errorf("hydrate github pull request status: %w", err)
			}
		}
	}
	attachPullRequestToIssue(&issue, repo, pullRequest)
	if !includeStatus {
		if pullRequest.Number != number {
			return issue, fmt.Errorf("hydrate github pull request: %w: pull request number mismatch", ErrInvalidResponse)
		}
		issue.PRNumber = new(number)
		return issue, nil
	}
	if err := c.attachRequiredBranchChecks(ctx, &issue); err != nil {
		return issue, err
	}
	return issue, nil
}

func (c *Connector) LookupBranchHead(ctx context.Context, repository, branch string) (string, error) {
	repo, ok := pullRequestRepoFromName(repository)
	branch = strings.TrimSpace(branch)
	if !ok || branch == "" {
		return "", errors.New("lookup github branch head: invalid repository or branch")
	}
	const query = `query BranchHead($owner: String!, $name: String!, $ref: String!) {
  repository(owner: $owner, name: $name) { ref(qualifiedName: $ref) { target { oid } } }
  rateLimit { limit used remaining cost resetAt }
 }`
	var response struct {
		Repository *struct {
			Ref *struct {
				Target struct {
					OID string `json:"oid"`
				} `json:"target"`
			} `json:"ref"`
		} `json:"repository"`
	}
	if err := c.client.GraphQLWithType(ctx, "merge", query, map[string]any{"owner": repo.Owner, "name": repo.Name, "ref": "refs/heads/" + branch}, &response); err != nil {
		return "", fmt.Errorf("lookup github branch head: %w", err)
	}
	if response.Repository == nil {
		return "", fmt.Errorf("lookup github branch head: %w", ErrInvalidResponse)
	}
	if response.Repository.Ref == nil {
		return "", nil
	}
	head := strings.TrimSpace(response.Repository.Ref.Target.OID)
	if head == "" {
		return "", fmt.Errorf("lookup github branch head: %w", ErrInvalidResponse)
	}
	return head, nil
}

func (c *Connector) LookupPullRequestByHead(
	ctx context.Context,
	repository string,
	branch string,
	headSHA string,
) (connector.PullRequest, bool, error) {
	repo, ok := pullRequestRepoFromName(repository)
	branch = strings.TrimSpace(branch)
	headSHA = strings.TrimSpace(headSHA)
	if !ok || branch == "" || headSHA == "" {
		return connector.PullRequest{}, false, errors.New("lookup github pull request by head: invalid repository, branch, or head SHA")
	}

	var response []restPullRequest
	if err := c.client.REST(ctx, http.MethodGet, restPullRequestsByHeadPath(repo, branch), nil, &response); err != nil {
		return connector.PullRequest{}, false, fmt.Errorf("lookup github pull request by head: %w", err)
	}
	for _, candidate := range response {
		pullRequest := pullRequestNodeFromREST(candidate)
		if strings.TrimSpace(pullRequest.HeadRefName) != branch || strings.TrimSpace(pullRequest.HeadSHA) != headSHA {
			continue
		}
		issue := connector.Issue{}
		attachPullRequestToIssue(&issue, repo, pullRequest)
		return *issue.PullRequest, true, nil
	}
	return connector.PullRequest{}, false, nil
}

func (c *Connector) CreateDraftPullRequest(
	ctx context.Context,
	repository string,
	branch string,
	title string,
	body string,
) (connector.PullRequest, error) {
	repo, ok := pullRequestRepoFromName(repository)
	if !ok || strings.TrimSpace(branch) == "" || strings.TrimSpace(title) == "" {
		return connector.PullRequest{}, errors.New("create draft github pull request: invalid repository, branch, or title")
	}
	info, err := c.FetchRepositoryInfo(ctx, repository)
	if err != nil {
		return connector.PullRequest{}, fmt.Errorf("create draft github pull request: %w", err)
	}
	base := strings.TrimSpace(info.DefaultBranch)
	if base == "" {
		return connector.PullRequest{}, errors.New("create draft github pull request: repository default branch is unavailable")
	}
	payload := struct {
		Title string `json:"title"`
		Head  string `json:"head"`
		Base  string `json:"base"`
		Body  string `json:"body"`
		Draft bool   `json:"draft"`
	}{
		Title: strings.TrimSpace(title),
		Head:  strings.TrimSpace(branch),
		Base:  base,
		Body:  strings.TrimSpace(body),
		Draft: true,
	}
	var response restPullRequest
	if err := c.client.REST(ctx, http.MethodPost, restPullRequestsCreatePath(repo), payload, &response); err != nil {
		return connector.PullRequest{}, fmt.Errorf("create draft github pull request: %w", err)
	}
	issue := connector.NewIssue()
	attachPullRequestToIssue(&issue, repo, pullRequestNodeFromREST(response))
	if issue.PullRequest == nil || issue.PullRequest.Number <= 0 {
		return connector.PullRequest{}, fmt.Errorf("create draft github pull request: %w", ErrInvalidResponse)
	}
	return *issue.PullRequest, nil
}

func (c *Connector) PullRequestDiffFingerprint(ctx context.Context, issue connector.Issue) (string, error) {
	repo, number, ok := hydratedPullRequestRef(issue)
	if !ok || issue.PullRequest == nil {
		return "", errors.New("pull request diff fingerprint requires a linked pull request")
	}
	key := pullRequestDiffFingerprintCacheKey{
		Repository: pullRequestRepoName(repo),
		Number:     number,
		HeadSHA:    strings.TrimSpace(issue.PullRequest.HeadSHA),
		BaseSHA:    strings.TrimSpace(issue.PullRequest.BaseSHA),
	}
	if key.Repository == "" || key.Number <= 0 || key.HeadSHA == "" || key.BaseSHA == "" {
		return "", errors.New("pull request diff fingerprint requires current head and base OIDs")
	}
	if fingerprint := c.cachedPullRequestDiffFingerprint(key); fingerprint != "" {
		return fingerprint, nil
	}
	files, err := fetchRESTList[restPullRequestFile](ctx, c.client, restPullRequestFilesPath(repo, number))
	if err != nil {
		return "", fmt.Errorf("fetch github pull request files: %w", err)
	}
	fingerprint := pullRequestDiffFingerprint(files)
	c.cachePullRequestDiffFingerprint(key, fingerprint)
	return fingerprint, nil
}

// PullRequestValidationDiff collects one PR's files and patch between two
// metadata reads. A moving head or inconsistent file response is retryable.
func (c *Connector) PullRequestValidationDiff(ctx context.Context, issue connector.Issue) (connector.ValidationDiff, error) {
	repo, number, ok := hydratedPullRequestRef(issue)
	if !ok || issue.PullRequest == nil {
		return connector.ValidationDiff{}, connector.NewRetryableError("validator PR identity is missing")
	}
	path := restPullRequestPath(repo, number)
	var before, after restPullRequest
	if err := c.client.REST(ctx, http.MethodGet, path, nil, &before); err != nil {
		return connector.ValidationDiff{}, fmt.Errorf("fetch validator PR metadata: %w", err)
	}
	base, head := strings.TrimSpace(before.Base.SHA), strings.TrimSpace(before.Head.SHA)
	if base == "" || head == "" || base != strings.TrimSpace(issue.PullRequest.BaseSHA) || head != strings.TrimSpace(issue.PullRequest.HeadSHA) {
		return connector.ValidationDiff{}, connector.NewRetryableError("validator PR base/head differs from hydrated issue")
	}
	files, err := fetchRESTList[restPullRequestFile](ctx, c.client, restPullRequestFilesPath(repo, number))
	if err != nil {
		return connector.ValidationDiff{}, fmt.Errorf("fetch validator PR files: %w", err)
	}
	patch, truncated, _, err := c.client.RESTTextWithSize(ctx, path, "application/vnd.github.diff", 8<<20)
	if err != nil {
		return connector.ValidationDiff{}, fmt.Errorf("fetch validator PR patch: %w", err)
	}
	if truncated {
		return connector.ValidationDiff{}, connector.NewRetryableError("validator PR patch exceeds provenance limit")
	}
	if err := c.client.REST(ctx, http.MethodGet, path, nil, &after); err != nil {
		return connector.ValidationDiff{}, fmt.Errorf("refresh validator PR metadata: %w", err)
	}
	if before.Base.SHA != after.Base.SHA || before.Head.SHA != after.Head.SHA {
		return connector.ValidationDiff{}, connector.NewRetryableError("validator PR head changed while collecting diff")
	}
	paths := make([]string, 0, len(files))
	for _, file := range files {
		paths = append(paths, file.Filename)
		if file.Patch != "" && !strings.Contains(patch, file.Patch) {
			return connector.ValidationDiff{}, connector.NewRetryableError("validator PR patch differs from file patches")
		}
	}
	if !sameValidationFiles(paths, patch) {
		return connector.ValidationDiff{}, connector.NewRetryableError("validator PR patch file list differs from PR files")
	}
	sum := sha256.Sum256([]byte(patch))
	return connector.ValidationDiff{Repository: pullRequestRepoName(repo), PRNumber: number, BaseSHA: base, HeadSHA: head, Files: paths, Patch: patch, Digest: hex.EncodeToString(sum[:])}, nil
}

func sameValidationFiles(files []string, patch string) bool {
	actual := map[string]bool{}
	lines := strings.Split(patch, "\n")
	for i, line := range lines {
		if !strings.HasPrefix(line, "diff --git ") {
			continue
		}
		path := ""
		if _, right, found := strings.Cut(line, " \"b/"); found {
			quoted := "\"b/" + right
			unquoted, err := strconv.Unquote(quoted)
			if err != nil || !strings.HasPrefix(unquoted, "b/") {
				return false
			}
			path = strings.TrimPrefix(unquoted, "b/")
		} else {
			// Unquoted Git paths may contain " b/" themselves. Match both
			// fields for ordinary changes; renamed paths need Git's explicit
			// destination metadata in the same diff block.
			for _, file := range files {
				if line == "diff --git a/"+file+" b/"+file {
					path = file
					break
				}
				if !strings.HasPrefix(line, "diff --git a/") || !strings.HasSuffix(line, " b/"+file) {
					continue
				}
				for j := i + 1; j < len(lines) && !strings.HasPrefix(lines[j], "diff --git "); j++ {
					if lines[j] == "rename to "+file || lines[j] == "copy to "+file {
						path = file
						break
					}
				}
				if path != "" {
					break
				}
			}
		}
		if path == "" {
			return false
		}
		actual[path] = true
	}
	if len(actual) != len(files) {
		return false
	}
	for _, file := range files {
		if !actual[file] {
			return false
		}
	}
	return true
}

func pullRequestDiffFingerprint(files []restPullRequestFile) string {
	files = append([]restPullRequestFile(nil), files...)
	sort.Slice(files, func(i, j int) bool {
		left := files[i]
		right := files[j]
		if left.Filename != right.Filename {
			return left.Filename < right.Filename
		}
		if left.PreviousFilename != right.PreviousFilename {
			return left.PreviousFilename < right.PreviousFilename
		}
		if left.Status != right.Status {
			return left.Status < right.Status
		}
		return left.SHA < right.SHA
	})
	hash := sha256.New()
	for _, file := range files {
		for _, value := range []string{file.Filename, file.PreviousFilename, file.Status, file.SHA} {
			_, _ = hash.Write([]byte(value))
			_, _ = hash.Write([]byte{0})
		}
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func (c *Connector) cachedPullRequestDiffFingerprint(key pullRequestDiffFingerprintCacheKey) string {
	if c == nil {
		return ""
	}
	c.mu.RLock()
	fingerprint := c.prDiffFingerprints[key]
	c.mu.RUnlock()
	return fingerprint
}

func (c *Connector) cachePullRequestDiffFingerprint(key pullRequestDiffFingerprintCacheKey, fingerprint string) {
	if c == nil || strings.TrimSpace(fingerprint) == "" {
		return
	}
	c.mu.Lock()
	if c.prDiffFingerprints == nil {
		c.prDiffFingerprints = map[pullRequestDiffFingerprintCacheKey]string{}
	}
	c.prDiffFingerprints[key] = strings.TrimSpace(fingerprint)
	c.mu.Unlock()
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
		err = classifyPullRequestMergeError(http.MethodPut, restPullRequestMergePath(repo, number), err,
			"head branch is out of date", "base branch was modified")
		if errors.Is(err, ErrRateLimited) {
			return fmt.Errorf("merge github pull request: %w", err)
		}
		var status *StatusError
		if errors.As(err, &status) && status.StatusCode == http.StatusMethodNotAllowed {
			message := strings.ToLower(status.Body)
			if strings.Contains(message, "changes must be made through the merge queue") || strings.Contains(message, "must be merged using the merge queue") {
				// Refresh known branch policies immediately. Inspection resolves
				// the actual base branch when no policy has been cached yet.
				c.mu.Lock()
				var branches []string
				for key, snapshot := range c.branchMergePolicies {
					if strings.HasPrefix(key, strings.ToLower(repository)+"@") {
						branches = append(branches, snapshot.Policy.Branch)
						delete(c.branchMergePolicies, key)
					}
				}
				c.mu.Unlock()
				for _, branch := range branches {
					policy, refreshErr := c.RepositoryBranchMergePolicy(ctx, repository, branch)
					if refreshErr != nil {
						err = errors.Join(err, refreshErr)
						continue
					}
					c.cacheBranchMergePolicy(repository, policy)
				}
				return fmt.Errorf("merge github pull request: %w: %w", connector.ErrPullRequestMergeQueueRequired, err)
			}
		}
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

func ClassifyPullRequestMergeError(method, path string, err error) error {
	return classifyPullRequestMergeError(method, path, err, "base branch was modified")
}

func classifyPullRequestMergeError(method, path string, err error, prefixes ...string) error {
	if err == nil || errors.Is(err, ErrRateLimited) || method != http.MethodPut {
		return err
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) != 6 || parts[0] != "repos" || parts[1] == "" || parts[2] == "" || parts[3] != "pulls" || parts[5] != "merge" {
		return err
	}
	number, parseErr := strconv.Atoi(parts[4])
	if parseErr != nil || number <= 0 || strconv.Itoa(number) != parts[4] {
		return err
	}
	var status *StatusError
	if !errors.As(err, &status) || status.StatusCode != http.StatusMethodNotAllowed {
		return err
	}
	var response struct {
		Message string `json:"message"`
	}
	if decodeErr := json.Unmarshal([]byte(status.Body), &response); decodeErr != nil {
		return err
	}
	message := strings.ToLower(strings.TrimSpace(response.Message))
	for _, prefix := range prefixes {
		if strings.HasPrefix(message, prefix) {
			return fmt.Errorf("%s %s: %w: %w", method, path, connector.ErrPullRequestBaseOutOfDate, err)
		}
	}
	return err
}

func (c *Connector) RerunPullRequestChecks(ctx context.Context, issue connector.Issue, checks []connector.PullRequestCheck) error {
	repo, _, ok := hydratedPullRequestRef(issue)
	if !ok {
		return errors.New("rerun github pull request checks: missing pull request repository")
	}
	if err := c.checkWorkflowRerunsReady(ctx, repo, checks); err != nil {
		return err
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

func (c *Connector) checkWorkflowRerunsReady(ctx context.Context, repo pullRequestRepo, checks []connector.PullRequestCheck) error {
	seen := map[int64]struct{}{}
	for _, check := range checks {
		if check.WorkflowRunID <= 0 {
			continue
		}
		if _, ok := seen[check.WorkflowRunID]; ok {
			continue
		}
		seen[check.WorkflowRunID] = struct{}{}
		var run restWorkflowRun
		if err := c.client.REST(ctx, http.MethodGet, restWorkflowRunPath(repo, check.WorkflowRunID), nil, &run); err != nil {
			return fmt.Errorf("check workflow run %d before rerun: %w", check.WorkflowRunID, err)
		}
		if !strings.EqualFold(strings.TrimSpace(run.Status), "completed") {
			return connector.NewRetryableError(fmt.Sprintf("workflow run %d is not completed; deferring failed-job rerun", check.WorkflowRunID))
		}
	}
	return nil
}

var commitSHAPattern = regexp.MustCompile(`^[0-9a-fA-F]{40}([0-9a-fA-F]{24})?$`)

// PostCommitStatus records a successful commit status for sha. It only ever
// writes success: a gate that did not pass posts nothing.
func (c *Connector) PostCommitStatus(ctx context.Context, repository, sha, statusContext, description string) error {
	repo, ok := pullRequestRepoFromName(repository)
	sha = strings.TrimSpace(sha)
	statusContext = strings.TrimSpace(statusContext)
	if !ok || !commitSHAPattern.MatchString(sha) || statusContext == "" {
		return fmt.Errorf("post github commit status: invalid repository %q, commit %q or context %q", strings.TrimSpace(repository), sha, statusContext)
	}
	var response restCommitStatus
	body := map[string]any{"state": "success", "context": statusContext, "description": description}
	if err := c.client.REST(ctx, http.MethodPost, restCreateCommitStatusPath(repo, sha), body, &response); err != nil {
		return fmt.Errorf("post github commit status: %w", err)
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
		var issue struct {
			Labels []label `json:"labels"`
		}
		err := c.client.REST(ctx, http.MethodGet, restIssuePath(ref), nil, &issue)
		if err != nil {
			return fmt.Errorf("fetch github pull request labels: %w", err)
		}
		if stringSliceContainsFold(labelNames(nodeConnection[label]{Nodes: issue.Labels}), labelName) {
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
		MergedAt:       parseGitHubTime(pullRequest.MergedAt),
		HeadRefName:    pullRequest.Head.Ref,
		BaseRefName:    pullRequest.Base.Ref,
		HeadSHA:        pullRequest.Head.SHA,
		BaseSHA:        pullRequest.Base.SHA,
	}
}

func attachPullRequestToIssue(issue *connector.Issue, repo pullRequestRepo, pullRequest pullRequestNode) {
	committedAt := cloneGitHubTime(pullRequest.HeadCommittedAt)
	if committedAt == nil && issue.PRNumber != nil && *issue.PRNumber == pullRequest.Number && issue.PRHeadSHA != "" && issue.PRHeadSHA == pullRequest.HeadSHA && strings.EqualFold(issue.PRRepository, pullRequestRepoName(repo)) {
		committedAt = cloneGitHubTime(issue.PRHeadCommittedAt)
	}
	if previous := issue.PullRequest; committedAt == nil && previous != nil && previous.Number == pullRequest.Number && previous.HeadSHA != "" && previous.HeadSHA == pullRequest.HeadSHA && strings.EqualFold(issue.PRRepository, pullRequestRepoName(repo)) {
		committedAt = cloneGitHubTime(previous.HeadCommittedAt)
	}
	issue.PullRequest = &connector.PullRequest{
		NodeID:                       strings.TrimSpace(pullRequest.NodeID),
		HeadCommittedAt:              committedAt,
		Number:                       pullRequest.Number,
		URL:                          strings.TrimSpace(pullRequest.URL),
		BranchName:                   strings.TrimSpace(pullRequest.HeadRefName),
		BaseRef:                      strings.TrimSpace(pullRequest.BaseRefName),
		State:                        strings.ToUpper(strings.TrimSpace(pullRequest.State)),
		MergeableState:               strings.ToLower(strings.TrimSpace(pullRequest.MergeableState)),
		Draft:                        pullRequest.Draft,
		Labels:                       append([]string{}, pullRequest.Labels...),
		ActivityAt:                   cloneGitHubTime(pullRequest.ActivityAt),
		MergedAt:                     cloneGitHubTime(pullRequest.MergedAt),
		HeadSHA:                      strings.TrimSpace(pullRequest.HeadSHA),
		BaseSHA:                      strings.TrimSpace(pullRequest.BaseSHA),
		HydrationUnavailableReason:   strings.TrimSpace(pullRequest.HydrationUnavailableReason),
		HydrationDegradedReason:      strings.TrimSpace(pullRequest.HydrationDegradedReason),
		HydrationNextRetryAt:         cloneGitHubTime(pullRequest.HydrationNextRetryAt),
		CIStatus:                     normalizePullRequestCIStatus(pullRequestCIState(pullRequest)),
		Checks:                       append([]connector.PullRequestCheck(nil), pullRequest.CI.Checks...),
		CheckRunCount:                pullRequest.CI.CheckRunCount,
		StatusContextCount:           pullRequest.CI.StatusContextCount,
		CIQueueSeconds:               pullRequest.CI.CIQueueSeconds,
		CIDurationSeconds:            pullRequest.CI.CIDurationSeconds,
		SlowChecks:                   append([]connector.PullRequestCheck(nil), pullRequest.CI.SlowChecks...),
		RunningChecks:                append([]string(nil), pullRequest.CI.RunningChecks...),
		UnstartedCheckCount:          pullRequest.CI.UnstartedCheckCount,
		UnstartedChecks:              append([]connector.PullRequestCheck(nil), pullRequest.CI.UnstartedChecks...),
		StaleSuccessfulChecks:        append([]connector.PullRequestCheck(nil), pullRequest.CI.StaleSuccessfulChecks...),
		RequiredCheckFailures:        append([]connector.PullRequestCheck(nil), pullRequest.CI.RequiredFailures...),
		TransientFailedChecks:        append([]connector.PullRequestCheck(nil), pullRequest.CI.TransientFailures...),
		UnresolvedReviewThreads:      append([]connector.PullRequestReviewThread(nil), pullRequest.UnresolvedReviewThreads...),
		CodexReviewState:             pullRequestCodexReviewState(pullRequest),
		CodexReviewSource:            pullRequestCodexReviewSource(pullRequest),
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

func (c *Connector) pullRequestHydrationStateForError(repo pullRequestRepo, err error) pullRequestHydrationState {
	switch {
	case errors.Is(err, ErrRESTFanoutDeferred):
		return pullRequestHydrationState{Reason: connector.PullRequestHydrationReasonRESTFanoutDeferred}
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

func restFanoutOrReserveDeferred(err error) bool {
	return errors.Is(err, ErrRESTFanoutDeferred) || errors.Is(err, ErrRESTBudgetReserved)
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

func (c *Connector) populatePullRequestStatus(ctx context.Context, repo pullRequestRepo, pullRequest *pullRequestNode, useStatusCache bool) error {
	previousStatus, hadPreviousStatus := pullRequestStatus{}, false
	if c.pullRequests != nil {
		previousStatus, hadPreviousStatus = c.pullRequests.Peek(repo, pullRequest.Number, pullRequest.HeadSHA)
	}
	if useStatusCache && c.pullRequests != nil {
		if status, ok := c.pullRequests.Get(repo, pullRequest.Number, pullRequest.HeadSHA); ok {
			c.logPullRequestCache(ctx, repo, pullRequest, true, false, "")
			applyPullRequestStatus(pullRequest, status)
			return nil
		}
		c.logPullRequestCache(ctx, repo, pullRequest, false, false, "")
	}

	status := pullRequestStatus{}
	checksUnavailable := false
	if strings.TrimSpace(pullRequest.HeadSHA) != "" {
		c.deletePullRequestCIConditionalEntries(repo, pullRequest.HeadSHA)
		ci, err := c.fetchPullRequestCI(ctx, repo, pullRequest.HeadSHA)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				pullRequest.HydrationUnavailableReason = connector.PullRequestHydrationReasonChecksUnavailable
				checksUnavailable = true
			} else {
				state := c.pullRequestHydrationStateForError(repo, err)
				if c.applyCachedPullRequestStatusAfterThrottle(ctx, repo, pullRequest, state) {
					return nil
				}
				return err
			}
		} else {
			status.ci = ci
			if hadPreviousStatus {
				c.logPullRequestCheckReconciliation(ctx, repo, pullRequest, previousStatus.ci, ci)
			}
		}
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
	if c.pullRequests != nil && !checksUnavailable {
		c.pullRequests.Set(repo, pullRequest.Number, pullRequest.HeadSHA, status)
		c.logPullRequestCache(ctx, repo, pullRequest, false, false, "stored")
	}
	applyPullRequestStatus(pullRequest, status)
	return nil
}

func (c *Connector) deletePullRequestCIConditionalEntries(repo pullRequestRepo, headSHA string) {
	if c == nil || c.client == nil {
		return
	}
	c.client.deleteRESTConditionalEntriesForEndpoint(http.MethodGet, restCommitCheckRunsPath(repo, headSHA))
	c.client.deleteRESTConditionalEntriesForEndpoint(http.MethodGet, restCommitStatusesPath(repo, headSHA))
}

func (c *Connector) logPullRequestCheckReconciliation(ctx context.Context, repo pullRequestRepo, pullRequest *pullRequestNode, previous pullRequestCI, current pullRequestCI) {
	if c == nil || c.logger == nil || pullRequest == nil {
		return
	}
	currentNames := make(map[string]struct{}, len(current.Checks))
	for _, check := range current.Checks {
		if name := strings.TrimSpace(check.Name); name != "" {
			currentNames[name] = struct{}{}
		}
	}
	removedChecks := make([]string, 0)
	for _, check := range previous.Checks {
		name := strings.TrimSpace(check.Name)
		if name == "" {
			continue
		}
		if _, ok := currentNames[name]; !ok {
			removedChecks = append(removedChecks, name)
		}
	}
	removedChecks = uniqueNonBlank(removedChecks)
	if len(removedChecks) == 0 {
		return
	}
	removedNames := make(map[string]struct{}, len(removedChecks))
	for _, name := range removedChecks {
		removedNames[name] = struct{}{}
	}
	removedRunningChecks := make([]string, 0)
	for _, name := range previous.RunningChecks {
		if _, ok := removedNames[name]; ok {
			removedRunningChecks = append(removedRunningChecks, name)
		}
	}
	c.logger.InfoContext(ctx, "github pull request checks reconciled",
		"endpoint_family", "check runs",
		"request_purpose", "reconcile_current_head_checks",
		"repository", pullRequestRepoName(repo),
		"pr_number", pullRequest.Number,
		"head_sha", strings.TrimSpace(pullRequest.HeadSHA),
		"authoritative_check_count", len(current.Checks),
		"removed_checks", removedChecks,
		"removed_running_checks", uniqueNonBlank(removedRunningChecks),
	)
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

func (c *Connector) HydratePullRequestReviewThreads(ctx context.Context, issue connector.Issue) (connector.Issue, error) {
	repo, number, ok := hydratedPullRequestRef(issue)
	if !ok || issue.PullRequest == nil || normalizeStateName(issue.PullRequest.State) != "open" {
		return issue, nil
	}
	threads, err := c.fetchPullRequestReviewThreads(ctx, repo, number, issue.PullRequest.HeadSHA)
	if err != nil {
		if state := c.pullRequestHydrationStateForError(repo, err); state.Reason != "" {
			issue = issueWithPullRequestReviewThreads(issue, nil)
			issue.PullRequest.HydrationUnavailableReason = state.Reason
			issue.PullRequest.HydrationNextRetryAt = cloneGitHubTime(state.NextRetryAt)
			return issue, nil
		}
		return issue, err
	}
	issue = issueWithPullRequestReviewThreads(issue, threads)
	return issue, nil
}

func issueWithPullRequestReviewThreads(issue connector.Issue, threads []connector.PullRequestReviewThread) connector.Issue {
	if issue.PullRequest == nil {
		return issue
	}
	pullRequest := *issue.PullRequest
	pullRequest.UnresolvedReviewThreads = append([]connector.PullRequestReviewThread(nil), threads...)
	issue.PullRequest = &pullRequest
	return issue
}

func (c *Connector) fetchPullRequestReviewThreads(
	ctx context.Context,
	repo pullRequestRepo,
	number int,
	headSHA string,
) ([]connector.PullRequestReviewThread, error) {
	headSHA = strings.TrimSpace(headSHA)
	if !validPullRequestRepo(repo) || number <= 0 || headSHA == "" {
		return nil, ErrInvalidResponse
	}
	var after *string
	threads := []connector.PullRequestReviewThread{}
	for {
		var response struct {
			Repository *struct {
				PullRequest *struct {
					HeadSHA       string                                  `json:"headRefOid"`
					ReviewThreads nodeConnection[pullRequestReviewThread] `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		}
		if err := c.client.GraphQLWithType(ctx, graphQLQueryReviewThreads, pullRequestReviewThreadsQuery, map[string]any{
			"owner":  repo.Owner,
			"name":   repo.Name,
			"number": number,
			"after":  after,
		}, &response); err != nil {
			return nil, fmt.Errorf("fetch github pull request review threads: %w", err)
		}
		if response.Repository == nil || response.Repository.PullRequest == nil {
			return nil, ErrInvalidResponse
		}
		if strings.TrimSpace(response.Repository.PullRequest.HeadSHA) != headSHA {
			return nil, fmt.Errorf("%w: pull request head changed while fetching review threads", ErrInvalidResponse)
		}
		connection := response.Repository.PullRequest.ReviewThreads
		for _, thread := range connection.Nodes {
			if thread.IsResolved {
				continue
			}
			line := thread.Line
			if line <= 0 {
				line = thread.OriginalLine
			}
			body := ""
			if len(thread.Comments.Nodes) > 0 {
				body = thread.Comments.Nodes[0].Body
			}
			threads = append(threads, connector.PullRequestReviewThread{
				Body: body,
				Path: strings.TrimSpace(thread.Path),
				Line: line,
			})
		}
		if !connection.PageInfo.HasNextPage {
			return threads, nil
		}
		cursor := strings.TrimSpace(connection.PageInfo.EndCursor)
		if cursor == "" {
			return nil, ErrInvalidResponse
		}
		after = &cursor
	}
}

func (c *Connector) fetchPullRequestReviews(ctx context.Context, repo pullRequestRepo, number int, headSHA string) (pullRequestCodexReviews, error) {
	response, err := fetchRESTList[restReview](ctx, c.client, restPullRequestReviewsPath(repo, number))
	if err != nil {
		return pullRequestCodexReviews{}, fmt.Errorf("fetch github pull request reviews: %w", err)
	}
	if reviews := pullRequestReviewsFromEvidence(response, nil, headSHA); len(reviews.CurrentHead) > 0 {
		return reviews, nil
	}
	comments, err := fetchRESTList[restComment](ctx, c.client, restIssueCommentsListPath(issueRef{
		Owner:  repo.Owner,
		Name:   repo.Name,
		Number: number,
	}))
	if err != nil {
		return pullRequestCodexReviews{}, fmt.Errorf("fetch github pull request review summary comments: %w", err)
	}
	return pullRequestReviewsFromEvidence(response, comments, headSHA), nil
}

func pullRequestReviewsFromEvidence(response []restReview, comments []restComment, headSHA string) pullRequestCodexReviews {
	reviews := pullRequestCodexReviews{}
	if review, ok := latestCodexReview(response, headSHA); ok {
		reviews.CurrentHead = []pullRequestReview{review}
	}
	if review, ok := latestCodexReview(response, ""); ok {
		reviews.Latest = []pullRequestReview{review}
	}
	if len(reviews.CurrentHead) > 0 {
		return reviews
	}

	if review, ok := latestCodexSummaryEvidence(comments); ok && len(reviews.Latest) == 0 {
		reviews.Latest = []pullRequestReview{review}
	} else if _, ok := latestTrustedCodexSummaryComment(comments); ok && len(reviews.Latest) == 0 {
		// An edited in-progress summary is still a review cycle, not an opt-out.
		reviews.Latest = []pullRequestReview{{State: "PENDING", Source: connector.PullRequestReviewSourceSummaryComment}}
	}
	if review, ok := latestCodexSummaryReview(comments, response, headSHA); ok {
		reviews.CurrentHead = []pullRequestReview{review}
		if len(reviews.Latest) == 0 || reviews.Latest[0].Source == connector.PullRequestReviewSourceSummaryComment {
			reviews.Latest = []pullRequestReview{review}
		}
	}
	// A terminal service reply covers only the head of the latest request.
	if review, ok := codexRequestedReviewOutcome(comments); ok && len(reviews.CurrentHead) == 0 {
		if len(reviews.Latest) == 0 {
			reviews.Latest = []pullRequestReview{review}
		}
		if review.State == "COMMENTED" && review.CommitID == headSHA {
			reviews.CurrentHead = []pullRequestReview{review}
			reviews.Latest = []pullRequestReview{review}
			return reviews
		}
	}

	return reviews
}

// codexRequestedReviewOutcome reuses the existing request marker to associate
// service notices with a head; unrelated or older replies cannot complete it.
func codexRequestedReviewOutcome(comments []restComment) (pullRequestReview, bool) {
	const prefix = "<!-- detent:automated-review-head:"
	var request restComment
	var head string
	for _, comment := range comments {
		if !strings.HasPrefix(strings.TrimSpace(comment.Body), "@codex review") {
			continue
		}
		_, tail, ok := strings.Cut(comment.Body, prefix)
		if !ok {
			continue
		}
		sha, _, ok := strings.Cut(tail, " -->")
		if !ok || strings.TrimSpace(sha) == "" {
			continue
		}
		if head == "" || restCommentAfter(comment, request) {
			request, head = comment, strings.TrimSpace(sha)
		}
	}
	if head == "" {
		return pullRequestReview{}, false
	}
	review := pullRequestReview{CommitID: head, Source: connector.PullRequestReviewSourceSummaryComment}
	for _, comment := range comments {
		if !trustedCodexSummaryAuthor(comment.User) || request.CreatedAt == nil || comment.CreatedAt == nil || !comment.CreatedAt.After(*request.CreatedAt) {
			continue
		}
		body := strings.ToLower(strings.TrimSpace(comment.Body))
		if !strings.Contains(body, "usage limit") && !strings.Contains(body, "unavailable") {
			continue
		}
		review.State = "COMMENTED"
		review.Body, review.URL, review.Author, review.SubmittedAt = comment.Body, comment.HTMLURL, comment.User, comment.CreatedAt
	}
	return review, review.State != ""
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
		if headSHA != "" && !strings.EqualFold(strings.TrimSpace(review.CommitID), headSHA) {
			continue
		}
		candidate := pullRequestReview{
			Body:        review.Body,
			URL:         review.HTMLURL,
			State:       review.State,
			Source:      connector.PullRequestReviewSourceFormal,
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

type codexReviewSummary struct {
	commitPrefix string
	completedAt  time.Time
}

func latestCodexSummaryReview(comments []restComment, formalReviews []restReview, headSHA string) (pullRequestReview, bool) {
	review, ok := latestCodexSummaryEvidence(comments)
	if !ok || !codexReviewSummaryMatchesHead(review.CommitID, headSHA, formalReviews) {
		return pullRequestReview{}, false
	}
	review.CommitID = strings.ToLower(strings.TrimSpace(headSHA))
	return review, true
}

func latestCodexSummaryEvidence(comments []restComment) (pullRequestReview, bool) {
	comment, ok := latestTrustedCodexSummaryComment(comments)
	if !ok {
		return pullRequestReview{}, false
	}
	summary, ok := parseCodexReviewSummary(comment.Body)
	if !ok {
		return pullRequestReview{}, false
	}
	if comment.CreatedAt != nil && summary.completedAt.Before(*comment.CreatedAt) {
		return pullRequestReview{}, false
	}
	if comment.UpdatedAt == nil || summary.completedAt.After(comment.UpdatedAt.Add(time.Second)) {
		return pullRequestReview{}, false
	}
	completedAt := summary.completedAt
	return pullRequestReview{
		Body:        comment.Body,
		URL:         comment.HTMLURL,
		State:       "COMMENTED",
		Source:      connector.PullRequestReviewSourceSummaryComment,
		Author:      comment.User,
		CommitID:    summary.commitPrefix,
		SubmittedAt: &completedAt,
	}, true
}

func latestTrustedCodexSummaryComment(comments []restComment) (restComment, bool) {
	var latest restComment
	found := false
	for _, comment := range comments {
		if !trustedCodexSummaryAuthor(comment.User) || !strings.Contains(comment.Body, codexReviewSummaryMarker) {
			continue
		}
		if !found || restCommentAfter(comment, latest) {
			latest = comment
			found = true
		}
	}
	return latest, found
}

func trustedCodexSummaryAuthor(author *actor) bool {
	if author == nil || !strings.EqualFold(strings.TrimSpace(actorType(author)), "Bot") {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(author.Login)) {
	case "chatgpt-codex-connector", "chatgpt-codex-connector[bot]":
		return true
	default:
		return false
	}
}

func restCommentAfter(left restComment, right restComment) bool {
	leftAt := firstNonNilTime(left.UpdatedAt, left.CreatedAt)
	rightAt := firstNonNilTime(right.UpdatedAt, right.CreatedAt)
	if leftAt == nil {
		return rightAt == nil && left.ID > right.ID
	}
	if rightAt == nil {
		return true
	}
	if leftAt.Equal(*rightAt) {
		return left.ID > right.ID
	}
	return leftAt.After(*rightAt)
}

func firstNonNilTime(values ...*time.Time) *time.Time {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func parseCodexReviewSummary(body string) (codexReviewSummary, bool) {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	lines := strings.Split(strings.TrimSpace(body), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != codexReviewSummaryMarker || strings.Count(body, codexReviewSummaryMarker) != 1 {
		return codexReviewSummary{}, false
	}
	headingIndex := -1
	headerIndex := -1
	for index, line := range lines {
		switch strings.TrimSpace(line) {
		case codexReviewSummaryHeading:
			if headingIndex >= 0 {
				return codexReviewSummary{}, false
			}
			headingIndex = index
		case codexReviewSummaryTableHeader:
			if headerIndex >= 0 {
				return codexReviewSummary{}, false
			}
			headerIndex = index
		}
	}
	if headingIndex <= 0 || headerIndex <= headingIndex || headerIndex+2 >= len(lines) || strings.TrimSpace(lines[headerIndex+1]) != codexReviewSummaryTableSeparator {
		return codexReviewSummary{}, false
	}

	var summary codexReviewSummary
	found := false
	for _, line := range lines[headerIndex+2:] {
		line = strings.TrimSpace(line)
		if line == "" {
			if found {
				break
			}
			continue
		}
		if !strings.HasPrefix(line, "|") || !strings.HasSuffix(line, "|") {
			break
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		if len(cells) != 4 {
			return codexReviewSummary{}, false
		}
		if strings.TrimSpace(cells[0]) != "📝 **Code Review**" {
			continue
		}
		if found {
			return codexReviewSummary{}, false
		}
		statusMatches := codexReviewSummaryStatusPattern.FindStringSubmatch(strings.TrimSpace(cells[1]))
		commitMatches := codexReviewSummaryCommitPattern.FindStringSubmatch(strings.TrimSpace(cells[2]))
		if len(statusMatches) != 3 || len(commitMatches) != 2 {
			return codexReviewSummary{}, false
		}
		completedAt, err := time.Parse(time.RFC3339Nano, statusMatches[1])
		if err != nil {
			return codexReviewSummary{}, false
		}
		displayedAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(statusMatches[2]))
		if err != nil || !displayedAt.Equal(completedAt) {
			return codexReviewSummary{}, false
		}
		summary = codexReviewSummary{
			commitPrefix: strings.ToLower(commitMatches[1]),
			completedAt:  completedAt,
		}
		found = true
	}
	return summary, found
}

func codexReviewSummaryMatchesHead(prefix string, headSHA string, formalReviews []restReview) bool {
	prefix = strings.ToLower(strings.TrimSpace(prefix))
	headSHA = strings.ToLower(strings.TrimSpace(headSHA))
	if len(prefix) < minimumCodexReviewCommitPrefixLength || !validFullGitSHA(headSHA) || !strings.HasPrefix(headSHA, prefix) {
		return false
	}
	matches := map[string]struct{}{headSHA: {}}
	for _, review := range formalReviews {
		commitID := strings.ToLower(strings.TrimSpace(review.CommitID))
		if validFullGitSHA(commitID) && strings.HasPrefix(commitID, prefix) {
			matches[commitID] = struct{}{}
		}
	}
	return len(matches) == 1
}

func validFullGitSHA(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, character := range value {
		if !strings.ContainsRune("0123456789abcdef", character) {
			return false
		}
	}
	return true
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

func pullRequestCodexReviewSource(pullRequest pullRequestNode) string {
	for _, review := range pullRequest.LatestReviews.Nodes {
		if source := strings.TrimSpace(review.Source); source != "" {
			return source
		}
	}
	return ""
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
