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

func restIssueSearchPath(ref issueRef, page int) string {
	values := url.Values{}
	values.Set("q", "user:"+ref.Owner+" is:issue is:open "+strconv.Itoa(ref.Number))
	values.Set("per_page", strconv.Itoa(bodyParentSearchPageSize))
	values.Set("page", strconv.Itoa(page))
	return "/search/issues?" + values.Encode()
}

func restIssueCommentsPath(ref issueRef) string {
	return restIssuePath(ref) + "/comments"
}

func restIssueBlockedByDependenciesPath(ref issueRef) string {
	return restIssuePath(ref) + "/dependencies/blocked_by"
}

func restIssueBlockedByDependenciesListPath(ref issueRef) string {
	return restIssueBlockedByDependenciesPath(ref) + "?per_page=100"
}

func restIssueBlockedByDependencyPath(ref issueRef, dependencyID int) string {
	return restIssueBlockedByDependenciesPath(ref) + "/" + strconv.Itoa(dependencyID)
}

func restIssueCommentsListPath(ref issueRef) string {
	return restIssueCommentsPath(ref) + "?per_page=100"
}

func restIssueTimelinePath(ref issueRef) string {
	return restIssuePath(ref) + "/timeline?per_page=100"
}

func restIssueAssigneesPath(ref issueRef) string {
	return restIssuePath(ref) + "/assignees"
}

func restPullRequestsPath(repo pullRequestRepo, page int) string {
	values := url.Values{}
	values.Set("state", "all")
	values.Set("sort", "updated")
	values.Set("direction", "desc")
	values.Set("per_page", strconv.Itoa(pullRequestsPageSize))
	values.Set("page", strconv.Itoa(page))
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

func githubIssueNodeFromREST(ref issueRef, issue restIssue) githubIssueNode {
	repo := ref.Owner + "/" + ref.Name
	return githubIssueNode{
		TypeName:    "Issue",
		ID:          strings.TrimSpace(issue.NodeID),
		Number:      issue.Number,
		Title:       issue.Title,
		Body:        restStringValue(issue.Body),
		State:       issue.State,
		StateReason: issue.StateReason,
		URL:         issue.HTMLURL,
		CreatedAt:   restTimeString(issue.CreatedAt),
		UpdatedAt:   restTimeString(issue.UpdatedAt),
		Author:      issue.User,
		Assignees:   restAssigneesConnection(issue.Assignees),
		Labels:      nodeConnection[label]{Nodes: issue.Labels},
		Repository: repository{
			NameWithOwner: repo,
		},
	}
}

func restStringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
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

func restAssigneesConnection(values []restAssignee) nodeConnection[assignee] {
	out := make([]assignee, 0, len(values))
	for _, value := range values {
		out = append(out, assignee{
			ID:    strings.TrimSpace(value.NodeID),
			Login: strings.TrimSpace(value.Login),
		})
	}
	return nodeConnection[assignee]{Nodes: out}
}
