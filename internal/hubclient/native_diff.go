package hubclient

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/digitaldrywood/detent/internal/tracker"
)

// Stored attempt diffs (decisions section 18.5). The runner posts one
// generation of its worktree before the run event that references it, while it
// still holds the lease. The producer tuple in the body is the fencing: the
// hub validates it the way it validates a run event and refuses anything else
// with stale_execution.

func nativeAttemptDiffPath(attemptID string) (string, error) {
	if !strings.HasPrefix(attemptID, "attempt_") || strings.ContainsAny(attemptID, "/?#%\\") {
		return "", errors.New("native attempt ID is invalid")
	}
	return "/attempts/" + attemptID + "/diff", nil
}

// PostAttemptDiff stores one generation of an attempt's diff.
//
// A diff whose patches exceed the hub's whole-diff limit is refused with
// diff_too_large; the method answers that by re-posting the same file list
// without patches, which is what 18.5 says the producer does, so the file list
// and its counts survive a change too large to store in full.
func (c *NativeClient) PostAttemptDiff(ctx context.Context, attemptID string, request tracker.AttemptDiffRequest) (tracker.AttemptDiffReceipt, error) {
	var receipt tracker.AttemptDiffReceipt
	path, err := nativeAttemptDiffPath(attemptID)
	if err != nil {
		return receipt, err
	}
	request.Files, _ = tracker.NormalizeDiffFiles(request.Files)
	err = c.client.request(ctx, http.MethodPost, c.base()+path, request, &receipt)
	if err == nil || !attemptDiffTooLarge(err) {
		return receipt, err
	}
	request.Files = tracker.StripDiffPatches(request.Files)
	err = c.client.request(ctx, http.MethodPost, c.base()+path, request, &receipt)
	return receipt, err
}

// attemptDiffTooLarge reports whether err is the hub's diff_too_large refusal.
func attemptDiffTooLarge(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr == nil {
		return false
	}
	return apiErr.Code == "diff_too_large"
}
