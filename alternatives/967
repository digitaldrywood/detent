package github

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

func restIssuePath(ref issueRef) string {
	return "/repos/" + url.PathEscape(ref.Owner) + "/" + url.PathEscape(ref.Name) + "/issues/" + strconv.Itoa(ref.Number)
}

func restIssueCommentsPath(ref issueRef) string {
	return restIssuePath(ref) + "/comments"
}

func restIssueCommentsListPath(ref issueRef) string {
	return restIssueCommentsPath(ref) + "?per_page=100"
}

func restPullRequestsCreatePath(repo pullRequestRepo) string {
	return "/repos/" + url.PathEscape(repo.Owner) + "/" + url.PathEscape(repo.Name) + "/pulls"
}

func restPullRequestsByHeadPath(repo pullRequestRepo, branch string) string {
	values := url.Values{}
	values.Set("state", "all")
	values.Set("head", repo.Owner+":"+strings.TrimSpace(branch))
	values.Set("sort", "updated")
	values.Set("direction", "desc")
	values.Set("per_page", strconv.Itoa(pullRequestsPageSize))
	return "/repos/" + url.PathEscape(repo.Owner) + "/" + url.PathEscape(repo.Name) + "/pulls?" + values.Encode()
}

func restPullRequestPath(repo pullRequestRepo, number int) string {
	return "/repos/" + url.PathEscape(repo.Owner) + "/" + url.PathEscape(repo.Name) + "/pulls/" + strconv.Itoa(number)
}

func restPullRequestMergePath(repo pullRequestRepo, number int) string {
	return restPullRequestPath(repo, number) + "/merge"
}

func restPullRequestReviewsPath(repo pullRequestRepo, number int) string {
	return "/repos/" + url.PathEscape(repo.Owner) + "/" + url.PathEscape(repo.Name) + "/pulls/" + strconv.Itoa(number) + "/reviews?per_page=100"
}

func restPullRequestFilesPath(repo pullRequestRepo, number int) string {
	values := url.Values{}
	values.Set("per_page", "100")
	return restPullRequestPath(repo, number) + "/files?" + values.Encode()
}

func restCommitCheckRunsPath(repo pullRequestRepo, sha string) string {
	values := url.Values{}
	values.Set("per_page", "100")
	return "/repos/" + url.PathEscape(repo.Owner) + "/" + url.PathEscape(repo.Name) + "/commits/" + url.PathEscape(sha) + "/check-runs?" + values.Encode()
}

func restWorkflowRunPath(repo pullRequestRepo, runID int64) string {
	return "/repos/" + url.PathEscape(repo.Owner) + "/" + url.PathEscape(repo.Name) + "/actions/runs/" + strconv.FormatInt(runID, 10)
}

func restWorkflowRunRerunFailedJobsPath(repo pullRequestRepo, runID int64) string {
	return restWorkflowRunPath(repo, runID) + "/rerun-failed-jobs"
}

func restCheckRunAnnotationsPath(repo pullRequestRepo, checkRunID int64) string {
	values := url.Values{}
	values.Set("per_page", "100")
	return "/repos/" + url.PathEscape(repo.Owner) + "/" + url.PathEscape(repo.Name) + "/check-runs/" + strconv.FormatInt(checkRunID, 10) + "/annotations?" + values.Encode()
}

func restCheckRunRerequestPath(repo pullRequestRepo, checkRunID int64) string {
	return "/repos/" + url.PathEscape(repo.Owner) + "/" + url.PathEscape(repo.Name) + "/check-runs/" + strconv.FormatInt(checkRunID, 10) + "/rerequest"
}

func restCreateCommitStatusPath(repo pullRequestRepo, sha string) string {
	return "/repos/" + url.PathEscape(repo.Owner) + "/" + url.PathEscape(repo.Name) + "/statuses/" + url.PathEscape(sha)
}

func restCommitStatusesPath(repo pullRequestRepo, sha string) string {
	values := url.Values{}
	values.Set("per_page", "100")
	return "/repos/" + url.PathEscape(repo.Owner) + "/" + url.PathEscape(repo.Name) + "/commits/" + url.PathEscape(sha) + "/statuses?" + values.Encode()
}

func fetchRESTList[T any](ctx context.Context, client *Client, path string) ([]T, error) {
	values := []T{}
	for path != "" {
		var page []T
		headers, err := client.rest(ctx, http.MethodGet, path, nil, &page)
		if err != nil {
			return nil, err
		}
		values = append(values, page...)
		path, err = client.nextRESTPage(headers)
		if err != nil {
			return nil, err
		}
	}
	return values, nil
}

func fetchRESTCheckRuns(ctx context.Context, client *Client, path string) ([]restCheckRun, error) {
	checkRuns := []restCheckRun{}
	for path != "" {
		var page restCheckRuns
		headers, err := client.rest(ctx, http.MethodGet, path, nil, &page)
		if err != nil {
			return nil, err
		}
		checkRuns = append(checkRuns, page.CheckRuns...)
		path, err = client.nextRESTPage(headers)
		if err != nil {
			return nil, err
		}
	}
	return checkRuns, nil
}

func fetchRESTCheckRunAnnotations(ctx context.Context, client *Client, path string) ([]restCheckRunAnnotation, error) {
	annotations := []restCheckRunAnnotation{}
	for path != "" {
		var page []restCheckRunAnnotation
		headers, err := client.rest(ctx, http.MethodGet, path, nil, &page)
		if err != nil {
			return nil, err
		}
		annotations = append(annotations, page...)
		path, err = client.nextRESTPage(headers)
		if err != nil {
			return nil, err
		}
	}
	return annotations, nil
}

func fetchRESTWorkflowRunsForCheckRuns(ctx context.Context, client *Client, repo pullRequestRepo, checkRuns []restCheckRun) ([]restWorkflowRun, error) {
	runIDs := checkRunWorkflowRunIDs(checkRuns)
	runs := make([]restWorkflowRun, 0, len(runIDs))
	for _, runID := range runIDs {
		var run restWorkflowRun
		_, err := client.rest(ctx, http.MethodGet, restWorkflowRunPath(repo, runID), nil, &run)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, nil
}

func checkRunWorkflowRunIDs(checkRuns []restCheckRun) []int64 {
	seen := map[int64]struct{}{}
	for _, checkRun := range checkRuns {
		runID := checkRunWorkflowRunID(checkRun)
		if runID <= 0 {
			continue
		}
		seen[runID] = struct{}{}
	}
	runIDs := make([]int64, 0, len(seen))
	for runID := range seen {
		runIDs = append(runIDs, runID)
	}
	sort.Slice(runIDs, func(i, j int) bool {
		return runIDs[i] < runIDs[j]
	})
	return runIDs
}

func checkRunWorkflowRunID(checkRun restCheckRun) int64 {
	for _, value := range []string{checkRun.DetailsURL, checkRun.HTMLURL} {
		match := actionRunURLPattern.FindStringSubmatch(strings.TrimSpace(value))
		if len(match) != 2 {
			continue
		}
		runID, err := strconv.ParseInt(match[1], 10, 64)
		if err == nil && runID > 0 {
			return runID
		}
	}
	return 0
}

func restTimeString(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := value.UTC().Format(time.RFC3339)
	return &formatted
}

func restCommentID(comment restComment) string {
	if id := strings.TrimSpace(comment.NodeID); id != "" {
		return id
	}
	if comment.ID > 0 {
		return strconv.FormatInt(comment.ID, 10)
	}
	return ""
}

func actorType(value *actor) string {
	if value == nil {
		return ""
	}
	if actorType := strings.TrimSpace(value.Type); actorType != "" {
		return actorType
	}
	return strings.TrimSpace(value.TypeName)
}

func restIssueLabelPath(ref issueRef, label string) string {
	return restIssueLabelsPath(ref) + "/" + url.PathEscape(label)
}

func restRepositoryPath(repo string) string {
	owner, name, ok := splitRepositoryName(repo)
	if !ok {
		return "/repos/"
	}
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
}

func stringSliceContainsFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), strings.TrimSpace(want)) {
			return true
		}
	}
	return false
}

func restIssueLabelsPath(ref issueRef) string {
	return restIssuePath(ref) + "/labels"
}

func restRepositoryLabelsListPath(repo pullRequestRepo) string {
	values := url.Values{}
	values.Set("per_page", "100")
	return restRepositoryLabelsPath(repo) + "?" + values.Encode()
}

func restRepositoryLabelsPath(repo pullRequestRepo) string {
	return "/repos/" + url.PathEscape(repo.Owner) + "/" + url.PathEscape(repo.Name) + "/labels"
}
