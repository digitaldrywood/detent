package hubserver

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func nativeExecutionConflict(message string) error {
	return &nativeError{Code: "run_sequence_conflict", Message: message, status: http.StatusConflict}
}

// nativeStaleExecution is section 5's stale_execution: the expected owner
// generation is no longer the current one. It lives here rather than beside
// one of its callers because every path that is fenced by an ownership
// generation has to report the same code for the same reason.
func nativeStaleExecution(message string) error {
	return &nativeError{Code: "stale_execution", Message: message, status: http.StatusConflict}
}

// nativeStaleLease maps a tracker fencing failure onto that vocabulary, so a
// lease that is no longer current reads as stale_execution rather than as the
// tracker's own error.
func nativeStaleLease(err error, message string) error {
	if errors.Is(err, tracker.ErrStaleFencingToken) {
		return nativeStaleExecution(message)
	}
	return err
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
	if f := data.TerminalFailure; f != nil {
		if eventType != "run.finished" || data.Sequence <= 0 || f.ObservedAt.IsZero() || data.Outcome == "succeeded" {
			return nativeInvalid("Execution failures require an ordered failed or interrupted terminal event")
		}
		*f = f.Public()
	}
	if f := data.Finalization; f != nil {
		if eventType != "run.finished" || data.Sequence <= 0 || f.ObservedAt.IsZero() || f.Files < 0 ||
			f.ChangeID != "" && !validNativeID(f.ChangeID, "change") || f.VersionID != "" && !validNativeID(f.VersionID, "version") ||
			f.BaseSHA != "" && !validCommitID(f.BaseSHA) || f.HeadSHA != "" && !validCommitID(f.HeadSHA) ||
			f.SourceAttemptID != "" && !validNativeID(f.SourceAttemptID, "attempt") || f.VersionCode != "" && (!validExecutionName(f.VersionCode) || strings.ContainsAny(f.VersionCode, "/:\\")) ||
			len(f.VersionError) > tracker.NativeFinalizationTextLimit || len(f.Error) > tracker.NativeFinalizationTextLimit || !utf8.ValidString(f.VersionError) || !utf8.ValidString(f.Error) {
			return nativeInvalid("Finalizer observations require bounded identity and text on an ordered terminal event")
		}
		if f.VersionID != "" && f.ChangeID == "" || f.SourceAttemptID != "" && f.SourceVersion == nil {
			return nativeInvalid("Finalizer source identity requires its owning Change and version")
		}
		if v := f.SourceVersion; v != nil && (v.ChangeID != f.ChangeID || !validNativeID(v.ChangeID, "change") || !validNativeID(v.VersionID, "version") || !validCommitID(v.HeadSHA)) {
			return nativeInvalid("Invalid finalizer source version identity")
		}
		if p := f.Publication; p != nil && (!f.Settled || !f.Changed || f.VersionError != "" || f.Error != "" || p.ChangeID != f.ChangeID || p.VersionID != f.VersionID || p.HeadSHA != f.HeadSHA || !validNativeID(p.PolicyID, "policy") ||
			p.SourceAttemptID != "" && !validNativeID(p.SourceAttemptID, "attempt") || len(p.BaseRef) > 255 || len(p.Branch) > 255 || strings.ContainsAny(p.BaseRef+p.Branch, " \t\r\n~^:?*[\\") || strings.Contains(p.BaseRef+p.Branch, "..")) {
			return nativeInvalid("PR publication requires a settled exact version, policy and verified delivery identity")
		}
		*f = f.Public()
	}
	if len(data.Evidence) != 0 {
		return nativeInvalid("Attempt evidence is owned by attachment uploads")
	}
	if data.CompletionBody != "" && (eventType != "run.finished" || data.Sequence <= 0 || len(data.CompletionBody) > 64<<10 || !utf8.ValidString(data.CompletionBody)) {
		return nativeInvalid("Completion responses require a bounded ordered terminal event")
	}
	if data.Disposition != nil && (eventType != "run.finished" || data.Sequence <= 0 || !slices.Contains([]string{workpad.StatusInProgress, workpad.StatusBlocked, workpad.StatusComplete}, data.Disposition.Status)) {
		return nativeInvalid("Final disposition requires an ordered terminal event and a supported workpad status")
	}
	if data.Disposition != nil && (len(data.Disposition.FinalSummary) > workpad.MaxFinalSummaryBytes || !utf8.ValidString(data.Disposition.FinalSummary)) {
		return nativeInvalid("Final summary must be bounded UTF-8 text")
	}
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

func recordNativeAttempt(ctx context.Context, tx *sql.Tx, scope nativeScope, item tracker.NativeWorkItemID, event tracker.NativeRunEvent, now time.Time) (recorded, history bool, resultErr error) {
	data := event.Data
	publish := true
	encoded, err := marshalNative(data)
	if err != nil {
		return false, false, err
	}
	hash := sha256.Sum256([]byte(event.Type + " " + encoded))
	digest := hex.EncodeToString(hash[:])
	var previousHash string
	err = tx.QueryRowContext(ctx, "SELECT request_hash FROM native_attempt_events WHERE attempt_id = ? AND sequence = ?", data.AttemptID, data.Sequence).Scan(&previousHash)
	if err == nil {
		if previousHash != digest {
			return false, false, nativeExecutionConflict("Attempt sequence already contains different content")
		}
		return false, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, false, err
	}
	if f := data.Finalization; f != nil && f.Publication != nil {
		if err := requireNativeMutationLease(ctx, tx, scope, string(item), tracker.Mutation{LeaseID: data.LeaseID, FencingToken: data.FencingToken}, now); err != nil {
			return false, false, err
		}
		change, err := readChange(ctx, tx, scope, string(item), f.ChangeID)
		if err != nil {
			return false, false, err
		}
		version, err := readChangeVersion(ctx, tx, f.ChangeID, f.VersionID)
		if err != nil {
			return false, false, err
		}
		if change.CurrentVersion != f.VersionID || data.PolicyID != f.Publication.PolicyID || !version.Policy.Gates.GitHubPullRequest || !f.Publication.Matches(f.ChangeID, version) {
			return false, false, nativeExecutionConflict("PR publication no longer matches the current Change version, source or approved policy")
		}
		var raw string
		if err := tx.QueryRowContext(ctx, "SELECT checkpoint_json FROM native_attempts WHERE id=?", data.AttemptID).Scan(&raw); err != nil {
			return false, false, err
		}
		var checkpoint tracker.NativeCheckpoint
		if err := json.Unmarshal([]byte(raw), &checkpoint); err != nil {
			return false, false, err
		}
		if checkpoint.ExternalEffect != "pr_create" || checkpoint.EffectState != "confirmed" || checkpoint.EffectID != f.Publication.EffectID("pr_create") || checkpoint.Change == nil || *checkpoint.Change != f.Publication.SourceVersion || checkpoint.HeadSHA != f.HeadSHA {
			return false, false, nativeExecutionConflict("PR publication requires its exact confirmed checkpoint; reconcile pending or ambiguous effects before completion")
		}
		var head string
		if err := tx.QueryRowContext(ctx, `SELECT head_sha FROM attempt_diffs WHERE organization_id=? AND project_id=? AND work_item_id=?
AND attempt_id=? AND source='attempt' AND seq=? AND producer_lease_id=? AND producer_fencing_token=?`, scope.organization, scope.project, item, data.AttemptID, data.Sequence, data.LeaseID, data.FencingToken).Scan(&head); err != nil {
			return false, false, nativeExecutionConflict("PR publication requires the current fenced final diff; retain source and reconcile its head before completion")
		}
		if head != f.HeadSHA {
			return false, false, nativeExecutionConflict("PR publication head changed after verification; reconcile the current source before completion")
		}
	}
	var previousJSON, status string
	var sequence int64
	err = tx.QueryRowContext(ctx, "SELECT data_json, sequence, status FROM native_attempts WHERE id = ?", data.AttemptID).Scan(&previousJSON, &sequence, &status)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if event.Type != "run.started" || data.Sequence != 1 {
			return false, false, nativeExecutionConflict("An attempt must begin with run.started at sequence 1")
		}
		if err := validateProviderAttempt(ctx, tx, scope, data, now); err != nil {
			return false, false, err
		}
		var conflicts int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM native_attempts WHERE lease_id = ? OR (run_id = ? AND work_item_id != ?)`, data.LeaseID, data.RunID, item).Scan(&conflicts); err != nil {
			return false, false, err
		}
		if conflicts != 0 {
			return false, false, nativeExecutionConflict("Lease or run is already bound to another attempt or issue")
		}
		// The attempt records the item revision its lease was granted
		// against, captured in the claim transaction before the runner
		// hydrated the item. Reading the item's revision here instead would
		// credit an edit made between the claim and the start to an attempt
		// that never saw it. The change review surface compares it back to
		// tell whether a recorded change covers the item as it stands.
		var revision tracker.Revision
		if err := tx.QueryRowContext(ctx, "SELECT work_item_revision FROM leases WHERE lease_id = ?", data.LeaseID).Scan(&revision); err != nil {
			return false, false, fmt.Errorf("read attempt work item revision: %w", err)
		}
		// The dispatch generation is the last explicit request for another
		// attempt; the claim predicate compares it back so a continuation that
		// bumps no revision is still offered.
		dispatch, err := readNativeDispatchState(ctx, tx, scope, string(item))
		if err != nil {
			return false, false, fmt.Errorf("read attempt dispatch state: %w", err)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO native_attempts (id, organization_id, project_id, work_item_id, lease_id, fencing_token, run_id, sequence, status, data_json, started_at, updated_at, work_item_revision, dispatch_generation)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'running', ?, ?, ?, ?, ?)`, data.AttemptID, scope.organization, scope.project, item, data.LeaseID, data.FencingToken, data.RunID, data.Sequence, encoded, formatHubTime(now), formatHubTime(now), revision, dispatch.generation)
		if err != nil {
			return false, false, err
		}
	case err != nil:
		return false, false, err
	default:
		var previous tracker.NativeRunData
		if err := json.Unmarshal([]byte(previousJSON), &previous); err != nil {
			return false, false, err
		}
		data.Evidence = previous.Evidence
		encoded, err = marshalNative(data)
		if err != nil {
			return false, false, err
		}
		usageCorrection := status != "running" && event.Type == "run.finished" && nativeUsageCorrection(previous, data)
		if previous.LeaseID != data.LeaseID || previous.RunID != data.RunID || previous.PolicyID != data.PolicyID || previous.Identity == nil || *previous.Identity != *data.Identity || status != "running" && !usageCorrection || data.Sequence != sequence+1 || event.Type == "run.started" {
			return false, false, nativeExecutionConflict("Attempt identity, lifecycle or next sequence does not match")
		}
		if previous.Runtime != nil {
			if data.Runtime == nil || previous.Runtime.LocalAttemptID != 0 && (data.Runtime.LocalAttemptID != previous.Runtime.LocalAttemptID || data.Runtime.Generation != previous.Runtime.Generation) || data.Runtime.HeartbeatAt.Before(previous.Runtime.HeartbeatAt) {
				return false, false, nativeExecutionConflict("Runtime attribution or observation order changed during an attempt")
			}
		}
		if event.Type == "run.observed" {
			publish = nativeRuntimeHistoryChanged(previous.Runtime, data.Runtime)
		}
		if event.Type == "run.finished" && !usageCorrection {
			if err := publishAttemptEvidence(ctx, tx, scope, item, data, now); err != nil {
				return false, false, err
			}
			status = data.Outcome
		}
		if usageCorrection {
			publish = false
			if _, err := tx.ExecContext(ctx, "UPDATE native_attempts SET sequence=?,data_json=? WHERE id=?", data.Sequence, encoded, data.AttemptID); err != nil {
				return false, false, err
			}
		} else if _, err := tx.ExecContext(ctx, "UPDATE native_attempts SET sequence = ?, status = ?, data_json = ?, updated_at = ? WHERE id = ?", data.Sequence, status, encoded, formatHubTime(now), data.AttemptID); err != nil {
			return false, false, err
		}
	}
	if data.Runtime != nil && data.Runtime.Landing != nil {
		if err := recordAttemptLanding(ctx, tx, scope, item, data.AttemptID, *data.Runtime.Landing); err != nil {
			return false, false, err
		}
	}
	if data.Handoff != nil {
		checkpoint, err := marshalNative(data.Handoff)
		if err != nil {
			return false, false, err
		}
		artifacts, err := marshalNative(data.ArtifactIDs)
		if err != nil {
			return false, false, err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE native_attempts SET checkpoint_json = ?, artifact_ids_json = ? WHERE id = ?", checkpoint, artifacts, data.AttemptID); err != nil {
			return false, false, err
		}
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO native_attempt_events (attempt_id, sequence, request_hash) VALUES (?, ?, ?)", data.AttemptID, data.Sequence, digest)
	return err == nil, publish, err
}

func nativeRuntimeHistoryChanged(previous, observation *tracker.NativeRuntimeObservation) bool {
	if previous == nil || observation == nil {
		return previous != observation
	}
	left, right := *previous, *observation
	left.Activity, right.Activity = nil, nil
	left.REST, right.REST = nil, nil
	left.GitHub, right.GitHub = nil, nil
	left.HeartbeatAt, right.HeartbeatAt = time.Time{}, time.Time{}
	if left.Landing != nil {
		landing := *left.Landing
		landing.ObservedAt = time.Time{}
		left.Landing = &landing
	}
	if right.Landing != nil {
		landing := *right.Landing
		landing.ObservedAt = time.Time{}
		right.Landing = &landing
	}
	return !reflect.DeepEqual(left, right)
}

func (s *Service) listNativeAttempts(c echo.Context) error {
	ctx := c.Request().Context()
	scope := nativeRequestScope(c)
	params, err := nativeReadQuery(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	page, err := s.readAttempts(ctx, scope, c.Param("item"), params)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, page)
}

func (s *Service) readAttempts(ctx context.Context, scope nativeScope, item string, params url.Values) (tracker.Page[tracker.NativeAttempt], error) {
	path := "/api/v2/organizations/" + url.PathEscape(string(scope.organization)) + "/projects/" + url.PathEscape(string(scope.project)) + "/work-items/" + url.PathEscape(item) + "/attempts"

	if err := validateNativeQuery(params, "view"); err != nil {
		return tracker.Page[tracker.NativeAttempt]{}, err
	}
	view := params.Get("view")
	if view != "" && view != "blockers" {
		return tracker.Page[tracker.NativeAttempt]{}, nativeInvalid("view supports blockers")
	}
	budget := 1 << 20
	dataColumn := "a.data_json"
	if view == "blockers" {
		budget = operatortool.WorkHistoryPageBytes
		dataColumn = "json_remove(a.data_json, '$.runtime')"
	}
	limit, cursor, key, err := s.readNativePage(ctx, scope, path, params)
	if err != nil {
		return tracker.Page[tracker.NativeAttempt]{}, err
	}

	if _, _, err := readNativeIssueProjection(ctx, s.database.reader, scope, item, true); err != nil {
		return tracker.Page[tracker.NativeAttempt]{}, err
	}
	var after int64
	if cursor.After != "" {
		after, err = strconv.ParseInt(cursor.After, 10, 64)
		if err != nil {
			return tracker.Page[tracker.NativeAttempt]{}, nativeInvalid("Attempt cursor is invalid")
		}
	}
	rows, err := s.database.reader.QueryContext(ctx, `SELECT `+dataColumn+`, a.status, a.started_at, a.updated_at, a.checkpoint_json, a.artifact_ids_json, l.expires_at, l.released_at, l.renewed_at, a.work_item_revision, a.dispatch_generation
FROM native_attempts a JOIN leases l ON l.lease_id = a.lease_id
WHERE a.organization_id = ? AND a.project_id = ? AND a.work_item_id = ? AND a.fencing_token > ? ORDER BY a.fencing_token LIMIT ?`, scope.organization, scope.project, item, after, limit+1)
	if err != nil {
		return tracker.Page[tracker.NativeAttempt]{}, err
	}
	defer rows.Close()
	page := tracker.Page[tracker.NativeAttempt]{Items: []tracker.NativeAttempt{}}
	hasMore := false
	for rows.Next() {
		if len(page.Items) == limit {
			hasMore = true
			break
		}
		attempt, err := scanNativeAttempt(rows, s.config.now())
		if err != nil {
			return tracker.Page[tracker.NativeAttempt]{}, err
		}
		attempt.Runtime = attempt.Runtime.WithoutActivitySpans()
		next := cursor
		next.After = strconv.FormatInt(int64(attempt.FencingToken), 10)
		candidate := tracker.Page[tracker.NativeAttempt]{Items: append(page.Items, attempt)}
		candidate.NextCursor, err = encodeNativeCursor(next, key)
		if err != nil {
			return tracker.Page[tracker.NativeAttempt]{}, err
		}
		encoded, err := json.Marshal(candidate)
		if err != nil {
			return tracker.Page[tracker.NativeAttempt]{}, err
		}
		if len(encoded)+1 > budget {
			if len(page.Items) == 0 {
				return tracker.Page[tracker.NativeAttempt]{}, operatortool.ErrReadUnavailable
			}
			hasMore = true
			break
		}
		page.Items = candidate.Items
		cursor = next
	}
	if err := rows.Err(); err != nil {
		return tracker.Page[tracker.NativeAttempt]{}, err
	}
	if hasMore {
		page.NextCursor, err = encodeNativeCursor(cursor, key)
		if err != nil {
			return tracker.Page[tracker.NativeAttempt]{}, err
		}
	}
	return page, nil
}

func scanNativeAttempt(rows *sql.Rows, now time.Time) (tracker.NativeAttempt, error) {
	var attempt tracker.NativeAttempt
	var data, started, updated, artifacts, expires, renewed string
	var checkpoint, released sql.NullString
	if err := rows.Scan(&data, &attempt.Status, &started, &updated, &checkpoint, &artifacts, &expires, &released, &renewed, &attempt.WorkItemRevision, &attempt.DispatchGeneration); err != nil {
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
	attempt.LeaseExpiresAt = expiry
	if attempt.LeaseRenewedAt, err = parseTimeValue(renewed); err != nil {
		return attempt, err
	}
	attempt.Current = !released.Valid && !now.Before(attempt.LeaseRenewedAt) && now.Before(expiry)
	if released.Valid {
		at, err := parseTimeValue(released.String)
		if err != nil {
			return attempt, err
		}
		attempt.ClaimReleasedAt = &at
	}
	attempt.FinalizationAvailability = "unavailable"
	attempt.TerminalFailureAvailability = "unavailable"
	if attempt.TerminalFailure != nil {
		public := attempt.TerminalFailure.Public()
		attempt.TerminalFailure = &public
		attempt.TerminalFailureAvailability = "available"
	}
	if attempt.Finalization != nil {
		public := attempt.Finalization.Public()
		attempt.Finalization = &public
		attempt.FinalizationAvailability = "available"
	}
	attempt.RuntimeFreshness = "unavailable"
	if attempt.Runtime != nil {
		attempt.RuntimeFreshness = "available"
		heartbeat := attempt.Runtime.HeartbeatAt
		if heartbeat.IsZero() || now.Before(heartbeat) || attempt.Status == "running" && (!attempt.Current || now.Sub(heartbeat) >= expiry.Sub(attempt.LeaseRenewedAt)) {
			attempt.RuntimeFreshness = "expired"
		}
	}
	if attempt.Status == "running" && !attempt.Current {
		attempt.Status = "interrupted"
	}
	return attempt, nil
}

func (s *Service) getNativeAttempt(c echo.Context) error {
	result, err := s.readNativeAttempt(c.Request().Context(), nativeRequestScope(c), c.Param("item"), c.Param("attempt"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, result)
}

func (s *Service) readNativeAttempt(ctx context.Context, scope nativeScope, item, id string) (tracker.NativeAttempt, error) {
	return readNativeAttempt(ctx, s.database.reader, scope, item, id, s.config.now())
}

func readNativeAttempt(ctx context.Context, q nativeQueryer, scope nativeScope, item, id string, now time.Time) (tracker.NativeAttempt, error) {
	if _, _, err := readNativeIssue(ctx, q, scope, item); err != nil {
		return tracker.NativeAttempt{}, err
	}
	rows, err := q.QueryContext(ctx, `SELECT a.data_json,a.status,a.started_at,a.updated_at,a.checkpoint_json,a.artifact_ids_json,l.expires_at,l.released_at,l.renewed_at,a.work_item_revision,a.dispatch_generation FROM native_attempts a JOIN leases l ON l.lease_id=a.lease_id WHERE a.organization_id=? AND a.project_id=? AND a.work_item_id=? AND a.id=?`, scope.organization, scope.project, item, id)
	if err != nil {
		return tracker.NativeAttempt{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return tracker.NativeAttempt{}, err
		}
		return tracker.NativeAttempt{}, nativeNotFound()
	}
	return scanNativeAttempt(rows, now)
}
