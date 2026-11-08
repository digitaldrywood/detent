package hubgithub

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver"
)

func outboundPullReady(pull restPullRequest, desired hubserver.MergePullRequestDesired) (bool, error) {
	if strings.TrimSpace(pull.Head.SHA) != strings.TrimSpace(desired.HeadSHA) {
		return false, hubserver.Permanent(errors.New("GitHub pull request head changed"))
	}
	if !strings.EqualFold(pull.State, "open") {
		if pull.Merged {
			return true, nil
		}
		return false, hubserver.Permanent(errors.New("GitHub pull request is not open"))
	}
	if pull.Draft || pull.Mergeable == nil || !*pull.Mergeable || !strings.EqualFold(pull.MergeableState, "clean") {
		return false, errors.New("GitHub pull request is not ready to merge")
	}
	return false, nil
}

func (w *Writer) stepMerge(ctx context.Context, item hubserver.OutboxItem) (hubserver.OutboxStepResult, error) {
	result := hubserver.OutboxStepResult{Progress: item.Progress}
	var desired hubserver.MergePullRequestDesired
	if err := decodeDesired(item, &desired); err != nil {
		return result, err
	}
	if desired.PullRequestNumber <= 0 || strings.TrimSpace(desired.HeadSHA) == "" {
		return result, hubserver.Permanent(errors.New("GitHub merge target is incomplete"))
	}
	method := strings.ToLower(strings.TrimSpace(desired.MergeMethod))
	if method != "merge" && method != "rebase" && method != "squash" {
		return result, hubserver.Permanent(errors.New("GitHub merge method is invalid"))
	}
	pullPath := repositoryPath(item) + "/pulls/" + strconv.Itoa(desired.PullRequestNumber)
	commitPath := repositoryPath(item) + "/commits/" + url.PathEscape(strings.TrimSpace(desired.HeadSHA))
	switch result.Progress.Stage {
	case "", "verify":
		var pull restPullRequest
		if err := w.client.REST(ctx, http.MethodGet, pullPath, nil, &pull); err != nil {
			return result, err
		}
		done, err := outboundPullReady(pull, desired)
		if err != nil {
			result.Progress.Stage, result.Progress.Cursor = "", ""
			return result, err
		}
		if done {
			result.Done = true
			return result, nil
		}
		if item.Uncertain {
			result.Progress.Stage, result.Progress.Cursor = "", ""
			return result, hubserver.ErrOutboxUncertain
		}
		if pull.UpdatedAt.IsZero() {
			return result, errors.New("GitHub pull request observation is incomplete")
		}
		if result.Progress.Stage == "" || !pull.UpdatedAt.Equal(result.Progress.SourceUpdatedAt) {
			result.Progress = hubserver.OutboxProgress{Stage: "reviews", Verification: result.Progress.Verification + 1, SourceUpdatedAt: pull.UpdatedAt}
			return result, nil
		}
		if item.Blocked {
			result.Progress.Stage, result.Progress.Cursor = "", ""
			return result, errors.New("GitHub source verification does not permit merge")
		}
		var response restMergeResponse
		err = w.writeOutbound(ctx, item, http.MethodPut, pullPath+"/merge", map[string]string{"merge_method": method, "sha": strings.TrimSpace(desired.HeadSHA)}, &response)
		if err != nil {
			return result, err
		}
		if !response.Merged {
			return result, hubserver.RejectedOutboxWrite(errors.New("GitHub rejected the requested merge"))
		}
		result.Done = true
		return result, nil
	case "reviews":
		path := result.Progress.Cursor
		if path == "" {
			path = pullPath + "/reviews?per_page=100"
		}
		var page []restReview
		next, err := w.client.RESTPage(ctx, path, &page)
		if err != nil {
			return result, err
		}
		if len(page) > 100 {
			return result, errors.New("GitHub review page exceeded its budget")
		}
		for index, review := range page {
			key, state := strings.ToLower(strings.TrimSpace(review.User.Login)), strings.ToUpper(strings.TrimSpace(review.State))
			if key == "" || state == "COMMENTED" || state == "PENDING" {
				continue
			}
			result.Observations = append(result.Observations, hubserver.OutboxObservation{Kind: "review", Key: key, Sequence: result.Progress.Page*100 + int64(index) + 1, Ready: state != "CHANGES_REQUESTED"})
		}
		result.Progress.Cursor = next
		result.Progress.Page++
		if next == "" {
			result.Progress.Stage, result.Progress.Page = "checks", 0
		}
	case "checks":
		path := result.Progress.Cursor
		if path == "" {
			path = commitPath + "/check-runs?per_page=100"
		}
		var page restCheckRuns
		next, err := w.client.RESTPage(ctx, path, &page)
		if err != nil {
			return result, err
		}
		if len(page.CheckRuns) > 100 {
			return result, errors.New("GitHub check page exceeded its budget")
		}
		for _, check := range page.CheckRuns {
			key := strings.ToLower(strings.TrimSpace(check.Name))
			if key != "" {
				result.Observations = append(result.Observations, hubserver.OutboxObservation{Kind: "check", Key: key, Sequence: check.ID, Ready: unreadyCheck([]restCheckRun{check}) == ""})
			}
		}
		result.Progress.Cursor = next
		if next == "" {
			result.Progress.Stage, result.Progress.Page = "statuses", 0
		}
	case "statuses":
		path := result.Progress.Cursor
		if path == "" {
			path = commitPath + "/statuses?per_page=100"
		}
		var page []restStatus
		next, err := w.client.RESTPage(ctx, path, &page)
		if err != nil {
			return result, err
		}
		if len(page) > 100 {
			return result, errors.New("GitHub status page exceeded its budget")
		}
		for index, status := range page {
			key := strings.ToLower(strings.TrimSpace(status.Context))
			if key != "" {
				result.Observations = append(result.Observations, hubserver.OutboxObservation{Kind: "status", Key: key, Sequence: -(result.Progress.Page*100 + int64(index) + 1), Ready: strings.EqualFold(strings.TrimSpace(status.State), "success")})
			}
		}
		result.Progress.Cursor = next
		result.Progress.Page++
		if next == "" {
			result.Progress.Stage, result.Progress.Page = "verify", 0
		}
	default:
		return result, hubserver.Permanent(errors.New("GitHub outbound verification phase is invalid"))
	}
	return result, nil
}
