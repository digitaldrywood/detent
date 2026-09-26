package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/tracker"
)

// Stored attempt diffs (decisions section 18.5). A diff write names its
// producer and is fenced by the producer's lease, not by the subject
// attempt's: while an attempt runs its own lease is the producer, and after it
// finishes only a workspace lease on that attempt may write. Workspace sessions
// (section 18.1) are not built, so the attempt-producer path is the only one
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
	return &nativeError{Code: "stale_generation", Message: message, status: http.StatusConflict}
}

// attemptDiffTooLarge reports a diff whose patches exceed tracker.MaxDiffBytes.
// The producer answers it by re-posting the same file list without patches.
func attemptDiffTooLarge(limit int64) error {
	return &nativeError{
		Code: "diff_too_large", Message: "Diff patches exceed the stored diff limit", status: http.StatusRequestEntityTooLarge,
		Details: map[string]any{"limit_bytes": limit},
	}
}

// attemptDiffRecord is one stored generation, without its files.
type attemptDiffRecord struct {
	ID         string
	AttemptID  string
	WorkItemID string
	Producer   tracker.DiffProducer
	Generation tracker.DiffGeneration
	BaseSHA    string
	HeadSHA    string
	FileCount  int
	PatchBytes int64
	Truncated  bool
	CreatedAt  time.Time
}

const attemptDiffColumns = `id, attempt_id, work_item_id, source, source_id, seq, base_sha, head_sha,
 producer_kind, producer_id, producer_runner_id, producer_lease_id, producer_fencing_token,
 file_count, patch_bytes, truncated, created_at`

func scanAttemptDiff(row interface{ Scan(...any) error }) (attemptDiffRecord, error) {
	var record attemptDiffRecord
	var created string
	if err := row.Scan(&record.ID, &record.AttemptID, &record.WorkItemID, &record.Generation.Source, &record.Generation.ID, &record.Generation.Seq,
		&record.BaseSHA, &record.HeadSHA, &record.Producer.Kind, &record.Producer.ID, &record.Producer.RunnerID,
		&record.Producer.LeaseID, &record.Producer.FencingToken, &record.FileCount, &record.PatchBytes, &record.Truncated, &created); err != nil {
		return record, err
	}
	var err error
	if record.CreatedAt, err = parseTimeValue(created); err != nil {
		return record, fmt.Errorf("decode attempt diff %s created_at: %w", record.ID, err)
	}
	return record, nil
}

// readAttemptDiffFiles reads one stored diff's files in producer order.
func readAttemptDiffFiles(ctx context.Context, query nativeQueryer, diffID string) ([]tracker.AttemptDiffFile, error) {
	rows, err := query.QueryContext(ctx, `SELECT path, old_path, status, additions, deletions, binary, patch, truncated, denied
FROM attempt_diff_files WHERE diff_id = ? ORDER BY position`, diffID)
	if err != nil {
		return nil, fmt.Errorf("query attempt diff files: %w", err)
	}
	defer rows.Close()
	files := []tracker.AttemptDiffFile{}
	for rows.Next() {
		var file tracker.AttemptDiffFile
		if err := rows.Scan(&file.Path, &file.OldPath, &file.Status, &file.Additions, &file.Deletions,
			&file.Binary, &file.Patch, &file.Truncated, &file.Denied); err != nil {
			return nil, fmt.Errorf("scan attempt diff file: %w", err)
		}
		files = append(files, file)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate attempt diff files: %w", err)
	}
	return files, nil
}

// readAttemptDiffGeneration reads one generation of an attempt. With seq zero
// it reads the latest of that source.
func readAttemptDiffGeneration(ctx context.Context, query nativeQueryer, scope nativeScope, attemptID, source string, seq int64) (attemptDiffRecord, error) {
	statement := `SELECT ` + attemptDiffColumns + ` FROM attempt_diffs
WHERE organization_id = ? AND project_id = ? AND attempt_id = ? AND source = ?`
	args := []any{scope.organization, scope.project, attemptID, source}
	if seq > 0 {
		statement += ` AND seq = ?`
		args = append(args, seq)
	}
	statement += ` ORDER BY seq DESC LIMIT 1`
	return scanAttemptDiff(query.QueryRowContext(ctx, statement, args...))
}

// readWorkItemDiffGeneration reads the latest diff stored against one work
// item, whichever attempt produced it. Ordering is by creation rather than by
// seq: seq is monotonic within one attempt's generations and says nothing
// across attempts, so the newest write is the honest answer to "what does this
// issue look like now".
func readWorkItemDiffGeneration(ctx context.Context, query nativeQueryer, scope nativeScope, item, source string) (attemptDiffRecord, error) {
	return scanAttemptDiff(query.QueryRowContext(ctx, `SELECT `+attemptDiffColumns+` FROM attempt_diffs
WHERE organization_id = ? AND project_id = ? AND work_item_id = ? AND source = ?
ORDER BY created_at DESC, rowid DESC LIMIT 1`, scope.organization, scope.project, item, source))
}

// resolveAttemptDiffProducer validates the producer of a diff write and
// returns the work item the subject attempt belongs to.
//
// Only an attempt producer can be honoured today. A workspace producer names a
// lease the hub has never issued, so it is refused as stale_execution: that is
// also what a released attempt lease gets, which is the point of the rule —
// once the run is over, the attempt's own generation can no longer write.
func resolveAttemptDiffProducer(ctx context.Context, tx *sql.Tx, scope nativeScope, attemptID string, producer tracker.DiffProducer, now time.Time) (string, error) {
	if producer.Kind != tracker.DiffSourceAttempt {
		return "", nativeStaleExecution("Only the attempt's own lease may write this diff until a workspace session owns it")
	}
	if producer.ID != "" && producer.ID != attemptID {
		return "", nativeStaleExecution("An attempt producer may only write its own attempt's diff")
	}
	if strings.TrimSpace(string(producer.LeaseID)) == "" || producer.FencingToken <= 0 {
		return "", nativeInvalid("A producer lease and fencing token are required")
	}
	if producer.RunnerID != "" && producer.RunnerID != scope.credential.Runner.RunnerID {
		return "", nativeInvalid("Producer identity must match the authenticated lease owner")
	}
	if err := requireRunnerAuthority(ctx, tx, scope, now); err != nil {
		return "", err
	}
	lease, found, err := readLeaseByID(ctx, tx, producer.LeaseID)
	if err != nil {
		return "", err
	}
	if !found {
		return "", nativeNotFound()
	}
	if err := requireCurrentLease(lease, producer.FencingToken, now); err != nil {
		return "", nativeStaleLease(err, "The producer lease is no longer current")
	}
	if err := requireApprovedLeasePolicy(ctx, tx, producer.LeaseID, true); err != nil {
		return "", err
	}
	if err := requireLeaseRunner(ctx, tx, producer.LeaseID, scope); err != nil {
		return "", err
	}
	var item string
	err = tx.QueryRowContext(ctx, `SELECT work_item_id FROM native_attempts
WHERE id = ? AND organization_id = ? AND project_id = ? AND lease_id = ? AND fencing_token = ? AND status = 'running'`,
		attemptID, scope.organization, scope.project, producer.LeaseID, producer.FencingToken).Scan(&item)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nativeStaleExecution("The attempt is not the running owner of the producer lease")
	}
	if err != nil {
		return "", fmt.Errorf("read diff producer attempt: %w", err)
	}
	return item, nil
}

// storeAttemptDiff applies the write-side rules and inserts one generation.
func storeAttemptDiff(ctx context.Context, tx *sql.Tx, scope nativeScope, attemptID, item string, request tracker.AttemptDiffRequest, now time.Time) (tracker.AttemptDiffReceipt, error) {
	var receipt tracker.AttemptDiffReceipt
	files, patchBytes := tracker.NormalizeDiffFiles(request.Files)
	if patchBytes > tracker.MaxDiffBytes {
		return receipt, attemptDiffTooLarge(tracker.MaxDiffBytes)
	}
	if err := tracker.ValidateDiffFiles(files); err != nil {
		return receipt, nativeInvalid(err.Error())
	}
	var stored int64
	err := tx.QueryRowContext(ctx, `SELECT coalesce(max(seq), 0) FROM attempt_diffs
WHERE attempt_id = ? AND source = ? AND source_id = ?`, attemptID, request.Generation.Source, request.Generation.ID).Scan(&stored)
	if err != nil {
		return receipt, fmt.Errorf("read stored diff generation: %w", err)
	}
	if request.Generation.Seq <= stored {
		return receipt, attemptDiffStaleGeneration("Generation " + strconv.FormatInt(request.Generation.Seq, 10) +
			" is not ahead of the stored generation " + strconv.FormatInt(stored, 10))
	}
	truncated := false
	for _, file := range files {
		if file.Truncated {
			truncated = true
			break
		}
	}
	id := newNativeID("diff")
	if _, err := tx.ExecContext(ctx, `INSERT INTO attempt_diffs (id, attempt_id, organization_id, project_id, work_item_id,
source, source_id, seq, base_sha, head_sha, producer_kind, producer_id, producer_runner_id, producer_lease_id, producer_fencing_token,
file_count, patch_bytes, posted_bytes, truncated, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, attemptID, scope.organization, scope.project, item,
		request.Generation.Source, request.Generation.ID, request.Generation.Seq, request.BaseSHA, request.HeadSHA,
		request.Producer.Kind, attemptID, scope.credential.Runner.RunnerID, request.Producer.LeaseID, request.Producer.FencingToken,
		len(files), patchBytes, diffPostedBytes(request.Files), truncated, formatHubTime(now)); err != nil {
		return receipt, fmt.Errorf("insert attempt diff: %w", err)
	}
	for position, file := range files {
		if _, err := tx.ExecContext(ctx, `INSERT INTO attempt_diff_files (diff_id, position, path, old_path, status,
additions, deletions, binary, patch, truncated, denied) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, position, file.Path, file.OldPath, file.Status, file.Additions, file.Deletions,
			file.Binary, file.Patch, file.Truncated, file.Denied); err != nil {
			return receipt, fmt.Errorf("insert attempt diff file: %w", err)
		}
	}
	return tracker.AttemptDiffReceipt{
		Accepted: true, DiffID: id, Generation: request.Generation,
		FileCount: len(files), PatchBytes: patchBytes, Truncated: truncated,
	}, nil
}

// diffPostedBytes is how much patch text the producer sent, before the filter
// and the caps ran. Keeping it beside the stored size is what makes "the
// denylist removed this much" answerable without the removed bytes.
func diffPostedBytes(files []tracker.AttemptDiffFile) int64 {
	var total int64
	for _, file := range files {
		total += int64(len(file.Patch))
	}
	return total
}

// Endpoints.

// postAttemptDiff stores one generation of an attempt's diff. It is a worker
// endpoint fenced by the producer's lease, not by the request's idempotency
// key: the generation is the record, and a replay of a stored seq is refused.
func (s *Service) postAttemptDiff(c echo.Context) error {
	var request tracker.AttemptDiffRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	attemptID := c.Param("attempt")
	if !validNativeID(attemptID, "attempt") {
		return s.nativeAPIError(c, nativeNotFound())
	}
	if err := tracker.ValidateDiffGeneration(request.Generation); err != nil {
		return s.nativeAPIError(c, nativeInvalid(err.Error()))
	}
	if request.Generation.Source != request.Producer.Kind {
		return s.nativeAPIError(c, nativeInvalid("Generation source and producer kind must agree"))
	}
	if len(request.BaseSHA) > tracker.MaxDiffSHABytes || len(request.HeadSHA) > tracker.MaxDiffSHABytes {
		return s.nativeAPIError(c, nativeInvalid("Commit identities are bounded to "+strconv.Itoa(tracker.MaxDiffSHABytes)+" bytes"))
	}
	scope := nativeRequestScope(c)
	ctx := c.Request().Context()
	var receipt tracker.AttemptDiffReceipt
	err := s.hubTransact(ctx, func(tx *sql.Tx, now time.Time) error {
		if err := s.recheckHostedMutation(ctx, tx, scope); err != nil {
			return err
		}
		item, err := resolveAttemptDiffProducer(ctx, tx, scope, attemptID, request.Producer, now)
		if err != nil {
			return err
		}
		receipt, err = storeAttemptDiff(ctx, tx, scope, attemptID, item, request, now)
		return err
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusAccepted, receipt)
}

// getAttemptDiff reads the latest attempt-produced diff of an attempt, or the
// generation named by ?at=<seq>. It follows the issue's read rule: the
// attempt's work item is resolved in the caller's scope, so a reader that
// cannot see the issue cannot see its diff.
func (s *Service) getAttemptDiff(c echo.Context) error {
	attemptID := c.Param("attempt")
	if !validNativeID(attemptID, "attempt") {
		return s.nativeAPIError(c, nativeNotFound())
	}
	if err := validateNativeQuery(c.QueryParams(), "at", "source"); err != nil {
		return s.nativeAPIError(c, err)
	}
	var seq int64
	if raw := strings.TrimSpace(c.QueryParam("at")); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed <= 0 {
			return s.nativeAPIError(c, nativeInvalid("at must be a positive generation sequence"))
		}
		seq = parsed
	}
	// The default read is the attempt's own diff. ?source=workspace is
	// accepted and answers 404 until workspace sessions can produce one, so
	// the client can ask for it without the shape changing later.
	source := tracker.DiffSourceAttempt
	if raw := strings.TrimSpace(c.QueryParam("source")); raw != "" {
		if raw != tracker.DiffSourceAttempt && raw != tracker.DiffSourceWorkspace {
			return s.nativeAPIError(c, nativeInvalid("source must be attempt or workspace"))
		}
		source = raw
	}
	scope := nativeRequestScope(c)
	ctx := c.Request().Context()
	record, err := readAttemptDiffGeneration(ctx, s.database.db, scope, attemptID, source, seq)
	if errors.Is(err, sql.ErrNoRows) {
		return s.nativeAPIError(c, nativeNotFound())
	}
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if _, _, err := readNativeIssue(ctx, s.database.db, scope, record.WorkItemID); err != nil {
		return s.nativeAPIError(c, err)
	}
	files, err := readAttemptDiffFiles(ctx, s.database.db, record.ID)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, tracker.AttemptDiff{
		ID: record.ID, AttemptID: record.AttemptID, Producer: record.Producer, Generation: record.Generation,
		BaseSHA: record.BaseSHA, HeadSHA: record.HeadSHA, Files: files,
		FileCount: record.FileCount, PatchBytes: record.PatchBytes, Truncated: record.Truncated, CreatedAt: record.CreatedAt,
	})
}

// getWorkItemDiff reads the latest stored diff on one issue, or reports that
// it has none. It follows the issue's read rule: the work item is resolved in
// the caller's scope first, so a reader that cannot see the issue gets the
// same 404 it would get for the issue itself, and an issue the reader can see
// answers 200 with `{"diff": null}` rather than a 404 the client would have to
// tell apart from a permission failure.
func (s *Service) getWorkItemDiff(c echo.Context) error {
	if err := validateNativeQuery(c.QueryParams(), "source"); err != nil {
		return s.nativeAPIError(c, err)
	}
	source := tracker.DiffSourceAttempt
	if raw := strings.TrimSpace(c.QueryParam("source")); raw != "" {
		if raw != tracker.DiffSourceAttempt && raw != tracker.DiffSourceWorkspace {
			return s.nativeAPIError(c, nativeInvalid("source must be attempt or workspace"))
		}
		source = raw
	}
	scope := nativeRequestScope(c)
	ctx := c.Request().Context()
	issue, _, err := readNativeIssue(ctx, s.database.db, scope, c.Param("item"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	record, err := readWorkItemDiffGeneration(ctx, s.database.db, scope, string(issue.WorkItemID), source)
	if errors.Is(err, sql.ErrNoRows) {
		return c.JSON(http.StatusOK, tracker.WorkItemDiff{})
	}
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	files, err := readAttemptDiffFiles(ctx, s.database.db, record.ID)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, tracker.WorkItemDiff{Diff: &tracker.AttemptDiff{
		ID: record.ID, AttemptID: record.AttemptID, Producer: record.Producer, Generation: record.Generation,
		BaseSHA: record.BaseSHA, HeadSHA: record.HeadSHA, Files: files,
		FileCount: record.FileCount, PatchBytes: record.PatchBytes, Truncated: record.Truncated, CreatedAt: record.CreatedAt,
	}})
}

// hubTransact runs fn in one transaction on the hub pool and commits when it
// returns nil. Only one transaction may be open at a time on that pool, so fn
// must never start another.
func (s *Service) hubTransact(ctx context.Context, fn func(tx *sql.Tx, now time.Time) error) (resultErr error) {
	now, err := s.database.currentTime()
	if err != nil {
		return err
	}
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin hub transaction: %w", err)
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			resultErr = errors.Join(resultErr, err)
		}
	}()
	if err := fn(tx, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit hub transaction: %w", err)
	}
	return nil
}
