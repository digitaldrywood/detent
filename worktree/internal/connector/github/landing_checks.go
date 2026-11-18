package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func ReadLandingChecks(ctx context.Context, client interface {
	REST(context.Context, string, string, any, any) error
}, repository, head string, required []string) (*tracker.NativeLandingCIReceipt, error) {
	var runs []restCheckRun
	for page := 1; ; page++ {
		var result restCheckRuns
		path := fmt.Sprintf("repos/%s/commits/%s/check-runs?filter=all&per_page=100&page=%d", repository, url.PathEscape(head), page)
		if err := client.REST(ctx, http.MethodGet, path, nil, &result); err != nil {
			return nil, err
		}
		runs = append(runs, result.CheckRuns...)
		if len(result.CheckRuns) < 100 {
			break
		}
	}
	var statuses []restCommitStatus
	for page := 1; ; page++ {
		var result []restCommitStatus
		path := fmt.Sprintf("repos/%s/commits/%s/statuses?per_page=100&page=%d", repository, url.PathEscape(head), page)
		if err := client.REST(ctx, http.MethodGet, path, nil, &result); err != nil {
			return nil, err
		}
		statuses = append(statuses, result...)
		if len(result) < 100 {
			break
		}
	}
	receipt := &tracker.NativeLandingCIReceipt{HeadSHA: head, State: "success", RequiredChecks: normalizeRequiredStatusChecks(required)}
	for _, failure := range requiredStatusCheckFailures(runs, statuses, required) {
		if requiredStatusCheckPending(strings.ToLower(failure.Status), strings.ToLower(failure.Conclusion)) {
			receipt.PendingChecks = append(receipt.PendingChecks, failure.Name)
			if failure.Status == "missing" {
				receipt.MissingChecks = append(receipt.MissingChecks, failure.Name)
			}
		} else {
			receipt.FailedChecks = append(receipt.FailedChecks, failure.Name)
		}
	}
	if len(receipt.PendingChecks) > 0 {
		receipt.State = "pending"
	}
	if len(receipt.FailedChecks) > 0 {
		receipt.State = "failure"
	}
	return receipt, nil
}
