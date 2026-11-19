package executionapi

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type DiffDeps struct {
	Repository     tenantstore.Repository
	Now            func() time.Time
	HTTPError      func(echo.Context, error, ...any) error
	InvalidRequest func(echo.Context, error) error
	Details        func() attachment.DetailStore
	Unavailable    func(string) error
	ArchiveJSON    func(echo.Context, int, any) error
	HostedShared   func() bool
	QuotaLimit     func(context.Context, tenantstore.Reader) (int64, error)
	ServiceClaims  func(echo.Context) (cloudassert.Claims, bool)
}

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

func normalizeAttemptDiff(scope hubscope.NativeScope, request tracker.AttemptDiffRequest) ([]tracker.AttemptDiffFile, int64, error) {
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
			if err := file.PatchBody.Validate(string(scope.Organization), string(scope.Project), tracker.MaxDiffPatchBytes); err != nil {
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

func (d DiffDeps) CheckAttemptDiff(c echo.Context) error {
	c.Set("attempt_diff_check", true)
	return d.PostAttemptDiff(c)
}

func (d DiffDeps) PostAttemptDiff(c echo.Context) error {
	var request tracker.AttemptDiffRequest
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return d.InvalidRequest(c, err)
	}
	attemptID := c.Param("attempt")
	if !hubapi.ValidID(attemptID, "attempt") {
		return d.HTTPError(c, hubapi.NativeNotFound())
	}
	if err := tracker.ValidateDiffGeneration(request.Generation); err != nil {
		return d.HTTPError(c, hubapi.NativePublicInvalid(err.Error()))
	}
	if request.Generation.Source != request.Producer.Kind {
		return d.HTTPError(c, hubapi.NativePublicInvalid("Generation source and producer kind must agree"))
	}
	if len(request.BaseSHA) > tracker.MaxDiffSHABytes || len(request.HeadSHA) > tracker.MaxDiffSHABytes {
		return d.HTTPError(c, hubapi.NativePublicInvalid("Commit identities are bounded to "+strconv.Itoa(tracker.MaxDiffSHABytes)+" bytes"))
	}
	scope := hubscope.NativeRequestScope(c)
	ctx := c.Request().Context()
	receipt, err := d.PostAttemptDiffTenant(ctx, scope, attemptID, request, c.Get("attempt_diff_check") == true)
	if err != nil {
		return d.HTTPError(c, err)
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

func (d DiffDeps) GetAttemptDiff(c echo.Context) error {

	attemptID := c.Param("attempt")
	if !hubapi.ValidID(attemptID, "attempt") {
		return d.HTTPError(c, hubapi.NativeNotFound())
	}
	if err := hubapi.ValidateNativeQuery(c.QueryParams(), "at", "source"); err != nil {
		return d.HTTPError(c, err)
	}
	var seq int64
	if raw := strings.TrimSpace(c.QueryParam("at")); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed <= 0 {
			return d.HTTPError(c, hubapi.NativePublicInvalid("at must be a positive generation sequence"))
		}
		seq = parsed
	}
	// The default read is the attempt's own diff. ?source=workspace is
	// accepted and answers 404 until workspace sessions can produce one, so
	// the client can ask for it without the shape changing later.
	source := tracker.DiffSourceAttempt
	if raw := strings.TrimSpace(c.QueryParam("source")); raw != "" {
		if raw != tracker.DiffSourceAttempt && raw != tracker.DiffSourceWorkspace {
			return d.HTTPError(c, hubapi.NativePublicInvalid("source must be attempt or workspace"))
		}
		source = raw
	}
	result, err := d.ReadAttemptDiff(c.Request().Context(), hubscope.NativeRequestScope(c), "", attemptID, source, seq)
	if err != nil {
		return d.HTTPError(c, err)
	}
	return d.ArchiveJSON(c, http.StatusOK, result)
}

func (d DiffDeps) ReadAttemptDiff(ctx context.Context, scope hubscope.NativeScope, item, attemptID, source string, seq int64) (tracker.AttemptDiff, error) {
	if d.Repository == nil {
		return tracker.AttemptDiff{}, d.Unavailable("Attempt diff storage is unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	return d.ReadAttemptDiffTenant(ctx, scope, item, attemptID, source, seq)
}

// getWorkItemDiff reads the latest stored diff on one issue, or reports that
// it has none. It follows the issue's read rule: the work item is resolved in
// the caller's scope first, so a reader that cannot see the issue gets the
// same 404 it would get for the issue itself, and an issue the reader can see
// answers 200 with `{"diff": null}` rather than a 404 the client would have to
// tell apart from a permission failure.

func (d DiffDeps) GetWorkItemDiff(c echo.Context) error {
	if err := hubapi.ValidateNativeQuery(c.QueryParams(), "source"); err != nil {
		return d.HTTPError(c, err)
	}
	source := tracker.DiffSourceAttempt
	if raw := strings.TrimSpace(c.QueryParam("source")); raw != "" {
		if raw != tracker.DiffSourceAttempt && raw != tracker.DiffSourceWorkspace {
			return d.HTTPError(c, hubapi.NativePublicInvalid("source must be attempt or workspace"))
		}
		source = raw
	}
	result, err := d.ReadWorkItemDiff(c.Request().Context(), hubscope.NativeRequestScope(c), c.Param("item"), source)
	if err != nil {
		return d.HTTPError(c, err)
	}
	return d.ArchiveJSON(c, http.StatusOK, result)
}

func (d DiffDeps) ReadWorkItemDiff(ctx context.Context, scope hubscope.NativeScope, item, source string) (tracker.WorkItemDiff, error) {
	if d.Repository == nil {
		return tracker.WorkItemDiff{}, d.Unavailable("Attempt diff storage is unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	return d.ReadWorkItemDiffTenant(ctx, scope, item, source)
}
