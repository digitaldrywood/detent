package hubserver

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func nativeExecutionConflict(message string) error {
	return &nativeError{Code: "run_sequence_conflict", Message: message, status: http.StatusConflict}
}

func requireNativeMutationLease(ctx context.Context, tx *sql.Tx, scope nativeScope, item string, mutation tracker.Mutation, now time.Time) error {
	var ordered int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM native_attempts WHERE organization_id = ? AND project_id = ? AND work_item_id = ?", scope.organization, scope.project, item).Scan(&ordered); err != nil {
		return err
	}
	if ordered == 0 && mutation.LeaseID == "" {
		return nil
	}
	if mutation.LeaseID == "" || mutation.FencingToken <= 0 {
		return tracker.ErrStaleFencingToken
	}
	if err := requireLeaseRunner(ctx, tx, mutation.LeaseID, scope); err != nil {
		return err
	}
	lease, found, err := readLeaseByID(ctx, tx, mutation.LeaseID)
	if err != nil {
		return err
	}
	if !found {
		return nativeNotFound()
	}
	_, id, err := readNativeIssue(ctx, tx, scope, item)
	if err != nil {
		return err
	}
	if lease.issueID != id {
		return nativeNotFound()
	}
	if err := requireCurrentLease(lease, mutation.FencingToken, now); err != nil {
		return err
	}
	return requireApprovedLeasePolicy(ctx, tx, mutation.LeaseID, true)
}

func validExecutionName(value string) bool {
	return value != "" && len(value) <= 128 && strings.IndexFunc(value, func(r rune) bool {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return false
		}
		return !strings.ContainsRune("-_.:/", r)
	}) < 0
}

func validCommitID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validateNativeExecution(data tracker.NativeRunData, eventType string) error {
	if data.Sequence == 0 {
		if data.Identity != nil || data.Handoff != nil || data.MachineID != "" || data.RunnerID != "" || data.SessionID != "" {
			return nativeInvalid("Execution metadata requires an ordered event")
		}
		return nil
	}
	if data.Sequence < 0 || data.Identity == nil || !validExecutionName(data.Identity.Role) || !validExecutionName(data.Identity.Backend) || !validExecutionName(data.Identity.Model) {
		return nativeInvalid("Ordered events require a positive sequence and bounded execution identity")
	}
	if data.Handoff == nil {
		if eventType == "run.checkpointed" {
			return nativeInvalid("Ordered checkpoints require a structured handoff")
		}
		return nil
	}
	c := data.Handoff
	if eventType != "run.checkpointed" || !slices.Contains([]string{"resume_session", "fresh_checkout", "manual_recovery"}, c.Resume) ||
		!slices.Contains([]string{"available", "missing", "inaccessible", "unverified"}, c.Availability) ||
		!slices.Contains([]string{"local_only", "customer_store"}, c.Storage) ||
		!slices.Contains([]string{"clean", "dirty", "unpushed", "unknown"}, c.WorktreeState) ||
		!slices.Contains([]string{"none", "git_push", "pr_create", "provider_turn"}, c.ExternalEffect) ||
		!slices.Contains([]string{"none", "pending", "confirmed", "ambiguous"}, c.EffectState) {
		return nativeInvalid("Checkpoint has an unsupported recovery or effect state")
	}
	if c.HeadSHA != "" && !validCommitID(c.HeadSHA) || c.ExpectedHeadSHA != "" && !validCommitID(c.ExpectedHeadSHA) || c.WorkspaceDigest != "" && !validCommitID(c.WorkspaceDigest) {
		return nativeInvalid("Checkpoint heads must be commit IDs")
	}
	if c.ExternalEffect == "none" && (c.EffectState != "none" || c.EffectID != "") || c.ExternalEffect != "none" && (c.EffectState == "none" || !validNativeID(c.EffectID, "effect")) {
		return nativeInvalid("External effects require a typed reconciliation identity and state")
	}
	if c.Storage == "customer_store" && (len(data.ArtifactIDs) == 0 || c.Availability == "available") {
		return nativeInvalid("Customer checkpoint references require artifacts and independent availability verification")
	}
	if c.Change != nil && (!validNativeID(c.Change.ChangeID, "change") || !validNativeID(c.Change.VersionID, "version") || !validCommitID(c.Change.HeadSHA)) {
		return nativeInvalid("Change references require typed immutable version and head identities")
	}
	return nil
}

func recordNativeAttempt(ctx context.Context, tx *sql.Tx, scope nativeScope, item tracker.NativeWorkItemID, event tracker.NativeRunEvent, now time.Time) (bool, error) {
	data := event.Data
	encoded, err := marshalNative(data)
	if err != nil {
		return false, err
	}
	hash := sha256.Sum256([]byte(event.Type + " " + encoded))
	digest := hex.EncodeToString(hash[:])
	var previousHash string
	err = tx.QueryRowContext(ctx, "SELECT request_hash FROM native_attempt_events WHERE attempt_id = ? AND sequence = ?", data.AttemptID, data.Sequence).Scan(&previousHash)
	if err == nil {
		if previousHash != digest {
			return false, nativeExecutionConflict("Attempt sequence already contains different content")
		}
		return false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	var previousJSON, status string
	var sequence int64
	err = tx.QueryRowContext(ctx, "SELECT data_json, sequence, status FROM native_attempts WHERE id = ?", data.AttemptID).Scan(&previousJSON, &sequence, &status)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if event.Type != "run.started" || data.Sequence != 1 {
			return false, nativeExecutionConflict("An attempt must begin with run.started at sequence 1")
		}
		var conflicts int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM native_attempts WHERE lease_id = ? OR (run_id = ? AND work_item_id != ?)`, data.LeaseID, data.RunID, item).Scan(&conflicts); err != nil {
			return false, err
		}
		if conflicts != 0 {
			return false, nativeExecutionConflict("Lease or run is already bound to another attempt or issue")
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO native_attempts (id, organization_id, project_id, work_item_id, lease_id, fencing_token, run_id, sequence, status, data_json, started_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'running', ?, ?, ?)`, data.AttemptID, scope.organization, scope.project, item, data.LeaseID, data.FencingToken, data.RunID, data.Sequence, encoded, formatHubTime(now), formatHubTime(now))
		if err != nil {
			return false, err
		}
	case err != nil:
		return false, err
	default:
		var previous tracker.NativeRunData
		if err := json.Unmarshal([]byte(previousJSON), &previous); err != nil {
			return false, err
		}
		if previous.LeaseID != data.LeaseID || previous.RunID != data.RunID || previous.PolicyID != data.PolicyID || previous.Identity == nil || *previous.Identity != *data.Identity || status != "running" || data.Sequence != sequence+1 || event.Type == "run.started" {
			return false, nativeExecutionConflict("Attempt identity, lifecycle or next sequence does not match")
		}
		if event.Type == "run.finished" {
			status = data.Outcome
		}
		if _, err := tx.ExecContext(ctx, "UPDATE native_attempts SET sequence = ?, status = ?, data_json = ?, updated_at = ? WHERE id = ?", data.Sequence, status, encoded, formatHubTime(now), data.AttemptID); err != nil {
			return false, err
		}
	}
	if data.Handoff != nil {
		checkpoint, err := marshalNative(data.Handoff)
		if err != nil {
			return false, err
		}
		artifacts, err := marshalNative(data.ArtifactIDs)
		if err != nil {
			return false, err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE native_attempts SET checkpoint_json = ?, artifact_ids_json = ? WHERE id = ?", checkpoint, artifacts, data.AttemptID); err != nil {
			return false, err
		}
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO native_attempt_events (attempt_id, sequence, request_hash) VALUES (?, ?, ?)", data.AttemptID, data.Sequence, digest)
	return err == nil, err
}

func (s *Service) listNativeAttempts(c echo.Context) error {
	if err := validateNativeQuery(c.QueryParams()); err != nil {
		return s.nativeAPIError(c, err)
	}
	limit, cursor, key, err := s.nativePage(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	scope := nativeRequestScope(c)
	ctx := c.Request().Context()
	if _, _, err := readNativeIssue(ctx, s.database.db, scope, c.Param("item")); err != nil {
		return s.nativeAPIError(c, err)
	}
	var after int64
	if cursor.After != "" {
		after, err = strconv.ParseInt(cursor.After, 10, 64)
		if err != nil {
			return s.nativeAPIError(c, nativeInvalid("Attempt cursor is invalid"))
		}
	}
	rows, err := s.database.db.QueryContext(ctx, `SELECT a.data_json, a.status, a.started_at, a.updated_at, a.checkpoint_json, a.artifact_ids_json, l.expires_at, l.released_at
FROM native_attempts a JOIN leases l ON l.lease_id = a.lease_id
WHERE a.organization_id = ? AND a.project_id = ? AND a.work_item_id = ? AND a.fencing_token > ? ORDER BY a.fencing_token LIMIT ?`, scope.organization, scope.project, c.Param("item"), after, limit+1)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	defer rows.Close()
	page := tracker.Page[tracker.NativeAttempt]{Items: []tracker.NativeAttempt{}}
	for rows.Next() {
		attempt, err := scanNativeAttempt(rows, s.config.now())
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		page.Items = append(page.Items, attempt)
	}
	if err := rows.Err(); err != nil {
		return s.nativeAPIError(c, err)
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		cursor.After = strconv.FormatInt(int64(page.Items[len(page.Items)-1].FencingToken), 10)
		page.NextCursor, err = encodeNativeCursor(cursor, key)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
	}
	return c.JSON(http.StatusOK, page)
}

func scanNativeAttempt(rows *sql.Rows, now time.Time) (tracker.NativeAttempt, error) {
	var attempt tracker.NativeAttempt
	var data, started, updated, artifacts, expires string
	var checkpoint, released sql.NullString
	if err := rows.Scan(&data, &attempt.Status, &started, &updated, &checkpoint, &artifacts, &expires, &released); err != nil {
		return attempt, err
	}
	if err := json.Unmarshal([]byte(data), &attempt.NativeRunData); err != nil {
		return attempt, err
	}
	if checkpoint.Valid {
		if err := json.Unmarshal([]byte(checkpoint.String), &attempt.Checkpoint); err != nil {
			return attempt, err
		}
	}
	if err := json.Unmarshal([]byte(artifacts), &attempt.ArtifactIDs); err != nil {
		return attempt, err
	}
	var err error
	if attempt.StartedAt, err = parseTimeValue(started); err != nil {
		return attempt, err
	}
	if attempt.UpdatedAt, err = parseTimeValue(updated); err != nil {
		return attempt, err
	}
	expiry, err := parseTimeValue(expires)
	if err != nil {
		return attempt, err
	}
	if attempt.Status == "running" && (released.Valid || !now.Before(expiry)) {
		attempt.Status = "interrupted"
	}
	return attempt, nil
}
