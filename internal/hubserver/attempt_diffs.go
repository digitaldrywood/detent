package hubserver

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// Stored attempt diffs. A diff write names its
// producer and is fenced by the producer's lease, not by the subject
// attempt's: while an attempt runs its own lease is the producer, and after it
// finishes only a workspace lease on that attempt may write. Workspace sessions
// are not built, so the attempt-producer path is the only one
// implemented and every other producer is refused with stale_execution rather
// than guessed at.
//
// The producer is validated the way appendNativeRunEvent validates a run
// event: the lease is the current one, its policy is approved and pinned, and
// it belongs to the authenticated runner.

// attemptDiffStaleGeneration reports a post whose generation is at or below
// the stored one. It is a conflict, not a validation failure: the producer is
// current, the diff it carries is not.
func attemptDiffStaleGeneration(message string) error {
	return &hubapi.NativeError{Code: "stale_generation", Message: message, Status: http.StatusConflict}
}

// attemptDiffTooLarge reports a diff whose patches exceed tracker.MaxDiffBytes.
// The producer answers it by re-posting the same file list without patches.
func attemptDiffTooLarge(limit int64) error {
	return &hubapi.NativeError{
		Code: "diff_too_large", Message: "Diff patches exceed the stored diff limit", Status: http.StatusRequestEntityTooLarge,
		Details:       map[string]any{"limit_bytes": limit},
		PublicMessage: true,
	}
}

func normalizeAttemptDiff(scope nativeScope, request tracker.AttemptDiffRequest) ([]tracker.AttemptDiffFile, int64, error) {
	files, patchBytes := tracker.NormalizeDiffFiles(request.Files)
	if patchBytes > tracker.MaxDiffBytes {
		return nil, 0, attemptDiffTooLarge(tracker.MaxDiffBytes)
	}
	if err := tracker.ValidateDiffFiles(files); err != nil {
		return nil, 0, hubapi.NativeInvalid(err.Error())
	}
	for _, file := range files {
		if file.PatchExpired {
			return nil, 0, hubapi.NativePublicInvalid("A diff write cannot expire its body")
		}
		if file.PatchBody != nil {
			if file.Patch != "" {
				return nil, 0, hubapi.NativePublicInvalid("Diff body must use exactly one storage representation")
			}
			if err := file.PatchBody.Validate(string(scope.organization), string(scope.project), tracker.MaxDiffPatchBytes); err != nil {
				return nil, 0, hubapi.NativeInvalid(err.Error())
			}
		}
	}
	if request.PostedBytes < 0 || request.PostedBytes > hubapi.MaxAttemptDiffRequestBytes || request.PostedBytes > 0 && request.PostedBytes < patchBytes {
		return nil, 0, hubapi.NativePublicInvalid("Invalid posted diff byte count")
	}
	return files, patchBytes, nil
}

func diffPostedBytes(files []tracker.AttemptDiffFile) int64 {
	var total int64
	for _, file := range files {
		total += int64(len(file.Patch))
		if file.PatchBody != nil {
			total += file.PatchBody.Bytes
		}
	}
	return total
}

// Endpoints.

// postAttemptDiff stores one generation of an attempt's diff. It is a worker
// endpoint fenced by the producer's lease, not by the request's idempotency
// key: the generation is the record, and a replay of a stored seq is refused.
func (s *Service) checkAttemptDiff(c echo.Context) error {
	c.Set("attempt_diff_check", true)
	return s.postAttemptDiff(c)
}

func (s *Service) postAttemptDiff(c echo.Context) error {
	var request tracker.AttemptDiffRequest
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	attemptID := c.Param("attempt")
	if !hubapi.ValidID(attemptID, "attempt") {
		return s.nativeAPIError(c, hubapi.NativeNotFound())
	}
	if err := tracker.ValidateDiffGeneration(request.Generation); err != nil {
		return s.nativeAPIError(c, hubapi.NativePublicInvalid(err.Error()))
	}
	if request.Generation.Source != request.Producer.Kind {
		return s.nativeAPIError(c, hubapi.NativePublicInvalid("Generation source and producer kind must agree"))
	}
	if len(request.BaseSHA) > tracker.MaxDiffSHABytes || len(request.HeadSHA) > tracker.MaxDiffSHABytes {
		return s.nativeAPIError(c, hubapi.NativePublicInvalid("Commit identities are bounded to "+strconv.Itoa(tracker.MaxDiffSHABytes)+" bytes"))
	}
	scope := nativeRequestScope(c)
	ctx := c.Request().Context()
	receipt, err := s.postAttemptDiffTenant(ctx, scope, attemptID, request, c.Get("attempt_diff_check") == true)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if c.Get("attempt_diff_check") == true {
		return c.NoContent(http.StatusNoContent)
	}
	return c.JSON(http.StatusAccepted, receipt)
}

// getAttemptDiff reads the latest attempt-produced diff of an attempt, or the
// generation named by ?at=<seq>. It follows the issue's read rule: the
// attempt's work item is resolved in the caller's scope, so a reader that
// cannot see the issue cannot see its diff.
func (s *Service) getAttemptDiff(c echo.Context) error {

	attemptID := c.Param("attempt")
	if !hubapi.ValidID(attemptID, "attempt") {
		return s.nativeAPIError(c, hubapi.NativeNotFound())
	}
	if err := hubapi.ValidateNativeQuery(c.QueryParams(), "at", "source"); err != nil {
		return s.nativeAPIError(c, err)
	}
	var seq int64
	if raw := strings.TrimSpace(c.QueryParam("at")); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed <= 0 {
			return s.nativeAPIError(c, hubapi.NativePublicInvalid("at must be a positive generation sequence"))
		}
		seq = parsed
	}
	// The default read is the attempt's own diff. ?source=workspace is
	// accepted and answers 404 until workspace sessions can produce one, so
	// the client can ask for it without the shape changing later.
	source := tracker.DiffSourceAttempt
	if raw := strings.TrimSpace(c.QueryParam("source")); raw != "" {
		if raw != tracker.DiffSourceAttempt && raw != tracker.DiffSourceWorkspace {
			return s.nativeAPIError(c, hubapi.NativePublicInvalid("source must be attempt or workspace"))
		}
		source = raw
	}
	result, err := s.readAttemptDiff(c.Request().Context(), nativeRequestScope(c), "", attemptID, source, seq)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return nativeArchiveJSON(c, http.StatusOK, result)
}

func (s *Service) readAttemptDiff(ctx context.Context, scope nativeScope, item, attemptID, source string, seq int64) (tracker.AttemptDiff, error) {
	if s.config.TenantRepository == nil {
		return tracker.AttemptDiff{}, nativeUnavailable("Attempt diff storage is unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	return s.readAttemptDiffTenant(ctx, scope, item, attemptID, source, seq)
}

// getWorkItemDiff reads the latest stored diff on one issue, or reports that
// it has none. It follows the issue's read rule: the work item is resolved in
// the caller's scope first, so a reader that cannot see the issue gets the
// same 404 it would get for the issue itself, and an issue the reader can see
// answers 200 with `{"diff": null}` rather than a 404 the client would have to
// tell apart from a permission failure.
func (s *Service) getWorkItemDiff(c echo.Context) error {
	if err := hubapi.ValidateNativeQuery(c.QueryParams(), "source"); err != nil {
		return s.nativeAPIError(c, err)
	}
	source := tracker.DiffSourceAttempt
	if raw := strings.TrimSpace(c.QueryParam("source")); raw != "" {
		if raw != tracker.DiffSourceAttempt && raw != tracker.DiffSourceWorkspace {
			return s.nativeAPIError(c, hubapi.NativePublicInvalid("source must be attempt or workspace"))
		}
		source = raw
	}
	result, err := s.readWorkItemDiff(c.Request().Context(), nativeRequestScope(c), c.Param("item"), source)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return nativeArchiveJSON(c, http.StatusOK, result)
}

func (s *Service) readWorkItemDiff(ctx context.Context, scope nativeScope, item, source string) (tracker.WorkItemDiff, error) {
	if s.config.TenantRepository == nil {
		return tracker.WorkItemDiff{}, nativeUnavailable("Attempt diff storage is unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	return s.readWorkItemDiffTenant(ctx, scope, item, source)
}
