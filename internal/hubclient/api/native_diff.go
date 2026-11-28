package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func NativeAttemptDiffPath(attemptID string) (string, error) {
	if !strings.HasPrefix(attemptID, "attempt_") || strings.ContainsAny(attemptID, "/?#%\\") {
		return "", errors.New("native attempt ID is invalid")
	}
	return "/attempts/" + attemptID + "/diff", nil
}

// PostAttemptDiff stores one generation of an attempt's diff.
//
// A diff whose patches exceed the hub's whole-diff limit is refused with
// diff_too_large, and a body the transport refuses first answers
// payload_too_large; the method answers either by re-posting the same file
// list without patches, which is what the Diff contract says the producer does, so the file
// list and its counts survive a change too large to store in full.
func (c *NativeClient) PostAttemptDiff(ctx context.Context, attemptID string, request tracker.AttemptDiffRequest) (tracker.AttemptDiffReceipt, error) {
	var receipt tracker.AttemptDiffReceipt
	path, err := NativeAttemptDiffPath(attemptID)
	if err != nil {
		return receipt, err
	}
	request.Files, _ = tracker.NormalizeDiffFiles(request.Files)
	err = c.client.request(ctx, http.MethodPost, c.base()+path, request, &receipt)
	if attemptDiffTooLarge(err) {
		request.Files = tracker.StripDiffPatches(request.Files)
		err = c.client.request(ctx, http.MethodPost, c.base()+path, request, &receipt)
	}
	var refusal *APIError
	if request.Generation.Source == tracker.DiffSourceAttempt && errors.As(err, &refusal) && refusal != nil && refusal.Status == http.StatusConflict && refusal.Code == "stale_generation" {
		return c.reconcileAttemptDiff(ctx, path, attemptID, request, err)
	}
	return receipt, err
}

func (c *NativeClient) reconcileAttemptDiff(ctx context.Context, path, attemptID string, request tracker.AttemptDiffRequest, refusal error) (tracker.AttemptDiffReceipt, error) {
	var receipt tracker.AttemptDiffReceipt
	if ctx == nil {
		ctx = context.Background()
	}
	raw, _, err := c.client.download(ctx, c.base()+path+"?at="+strconv.FormatInt(request.Generation.Seq, 10), 6*tracker.MaxDiffBytes+(8<<20))
	if lost := requestFence(ctx).err(); lost != nil {
		return receipt, lost
	}
	if err != nil {
		if NativePublicationUnavailable(err) {
			return receipt, fmt.Errorf("read stored attempt diff: %w", err)
		}
		return receipt, refusal
	}
	var stored tracker.AttemptDiff
	if err := json.Unmarshal(raw, &stored); err != nil {
		return receipt, fmt.Errorf("%w: decode stored attempt diff: %w", ErrUnavailable, err)
	}
	producer := request.Producer
	if producer.ID == "" {
		producer.ID = attemptID
	}
	if producer.RunnerID == "" {
		producer.RunnerID = stored.Producer.RunnerID
	}
	if stored.ID == "" || stored.AttemptID != attemptID || stored.Producer != producer || stored.Generation != request.Generation ||
		stored.BaseSHA != request.BaseSHA || stored.HeadSHA != request.HeadSHA || stored.FileCount != len(request.Files) ||
		!slices.EqualFunc(stored.Files, request.Files, func(stored, intended tracker.AttemptDiffFile) bool { return reflect.DeepEqual(stored, intended) }) {
		return receipt, refusal
	}
	return tracker.AttemptDiffReceipt{Accepted: true, DiffID: stored.ID, Generation: stored.Generation, FileCount: stored.FileCount, PatchBytes: stored.PatchBytes, Truncated: stored.Truncated}, nil
}

// attemptDiffTooLarge reports whether err is the hub refusing the diff for its
// size: the contract's diff_too_large or the transport's payload_too_large.
func attemptDiffTooLarge(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr == nil {
		return false
	}
	return apiErr.Code == "diff_too_large" || apiErr.Code == "payload_too_large"
}
