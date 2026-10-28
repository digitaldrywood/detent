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

// nativeStaleExecution is the conversation contract's stale_execution: the expected owner
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

func requireNativeMutationLease(ctx context.Context, tx nativeQueryer, scope nativeScope, item string, mutation tracker.Mutation, now time.Time) error {
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
		return nativeNotFoundAt("mutation_lease_missing")
	}
	id, err := nativeIssueRowID(ctx, tx, scope, item)
	if err != nil {
		return err
	}
	if lease.issueID != id {
		return nativeNotFoundAt("mutation_lease_other_item")
	}
	if err := requireCurrentLease(lease, mutation.FencingToken, now); err != nil {
		return err
	}
	return requireLeaseRouting(ctx, tx, mutation.LeaseID)
}

// nativeIssueRowID resolves a work item to its internal row ID with the same
// scope join as readNativeIssue, without hydrating the issue. Fencing checks
// only compare row identity.
func nativeIssueRowID(ctx context.Context, query nativeQueryer, scope nativeScope, item string) (tracker.WorkItemID, error) {
	var id tracker.WorkItemID
	err := query.QueryRowContext(ctx, `SELECT i.id FROM issues i JOIN projects p ON p.id = i.project_id AND p.organization_id = i.organization_id
WHERE i.organization_id = ? AND i.project_id = ? AND i.native_id = ?`, scope.organization, scope.project, item).Scan(&id)
	return id, err
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

func validCheckpointRef(ref string) bool {
	name, ok := strings.CutPrefix(ref, tracker.CheckpointRefPrefix)
	return ok && name != "" && len(name) <= 128 && strings.Trim(name, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-") == "" && !strings.HasPrefix(name, ".") && !strings.Contains(name, "..") && !strings.HasSuffix(name, ".lock")
}

func validateNativeExecution(data tracker.NativeRunData, eventType string) error {
	if len(data.Steps) > 8 {
		return nativePublicInvalid("Execution step receipts exceed the bounded step set")
	}
	seenSteps := map[string]bool{}
	for _, receipt := range data.Steps {
		if receipt.ReviewEpoch < 0 || receipt.ReviewEpoch > data.FencingToken {
			return nativePublicInvalid("Execution step review revision exceeds its owner")
		}
		if !slices.Contains([]string{"agent_turn", "commit", "validation", "publication", "security_audit", "review", "validator", "landing"}, receipt.Step) || seenSteps[receipt.Step] || receipt.PolicyID != data.PolicyID || receipt.CompletedAt.IsZero() || len(receipt.ReviewState) != 64 || receipt.Source.HeadSHA == "" || receipt.Source.WorkspaceDigest == "" || len(receipt.CompletionBody) > 64<<10 || !utf8.ValidString(receipt.CompletionBody) {
			return nativePublicInvalid("Execution step receipt identity is invalid")
		}
		seenSteps[receipt.Step] = true
		encoded, err := json.Marshal(receipt)
		if err != nil || len(encoded) > tracker.NativeStepReceiptBytes {
			field := "data.steps"
			largest := 0
			for _, part := range []struct {
				name  string
				value any
			}{{"validator", receipt.Validator}, {"validation", receipt.Validation}, {"security_audit", receipt.SecurityAudit}, {"completion_body", receipt.CompletionBody}, {"source", receipt.Source}, {"payload_artifact", receipt.PayloadArtifact}, {"payload_attachment", receipt.PayloadAttachment}} {
				raw, encodeErr := json.Marshal(part.value)
				if encodeErr != nil {
					field = "data.steps." + part.name
					break
				}
				if len(raw) > largest {
					largest, field = len(raw), "data.steps."+part.name
				}
			}
			return nativeRejection(nativePublicInvalid("Execution step receipt exceeds its bounded payload"), "event_validation", field)
		}
	}
	if data.Runtime != nil && data.Runtime.WorkerLaunch != nil && data.Runtime.WorkerLaunch.PolicyID != data.PolicyID {
		return nativeRejection(nativePublicInvalid("Execution worker policy differs from its lease"), "execution_validation", "data.runtime.worker_launch.policy_id")
	}
	if f := data.TerminalFailure; f != nil {
		if eventType != "run.finished" || data.Sequence <= 0 || f.ObservedAt.IsZero() || data.Outcome == "succeeded" {
			return nativeRejection(nativePublicInvalid("Execution failures require an ordered failed or interrupted terminal event"), "execution_validation", "data.terminal_failure")
		}
		*f = f.Public()
	}
	if f := data.Finalization; f != nil {
		if eventType != "run.finished" || data.Sequence <= 0 || f.ObservedAt.IsZero() || f.Files < 0 ||
			f.ChangeID != "" && !validNativeID(f.ChangeID, "change") || f.VersionID != "" && !validNativeID(f.VersionID, "version") ||
			f.BaseSHA != "" && !validCommitID(f.BaseSHA) || f.HeadSHA != "" && !validCommitID(f.HeadSHA) ||
			f.SourceAttemptID != "" && !validNativeID(f.SourceAttemptID, "attempt") || f.VersionCode != "" && (!validExecutionName(f.VersionCode) || strings.ContainsAny(f.VersionCode, "/:\\")) ||
			len(f.VersionError) > tracker.NativeFinalizationTextLimit || len(f.Error) > tracker.NativeFinalizationTextLimit || !utf8.ValidString(f.VersionError) || !utf8.ValidString(f.Error) {
			return nativeRejection(nativePublicInvalid("Finalizer observations require bounded identity and text on an ordered terminal event"), "execution_validation", "data.finalization")
		}
		if f.VersionID != "" && f.ChangeID == "" || f.SourceAttemptID != "" && f.SourceVersion == nil {
			return nativeRejection(nativePublicInvalid("Finalizer source identity requires its owning Change and version"), "execution_validation", "data.finalization")
		}
		if v := f.SourceVersion; v != nil && (v.ChangeID != f.ChangeID || !validNativeID(v.ChangeID, "change") || !validNativeID(v.VersionID, "version") || !validCommitID(v.HeadSHA)) {
			return nativeRejection(nativePublicInvalid("Invalid finalizer source version identity"), "execution_validation", "data.finalization")
		}
		if p := f.Publication; p != nil && (!f.Settled || !f.Changed || f.VersionError != "" || f.Error != "" || p.ChangeID != f.ChangeID || p.VersionID != f.VersionID || p.HeadSHA != f.HeadSHA || !validNativeID(p.PolicyID, "policy") ||
			p.SourceAttemptID != "" && !validNativeID(p.SourceAttemptID, "attempt") || len(p.BaseRef) > 255 || len(p.Branch) > 255 || strings.ContainsAny(p.BaseRef+p.Branch, " \t\r\n~^:?*[\\") || strings.Contains(p.BaseRef+p.Branch, "..")) {
			return nativeRejection(nativePublicInvalid("PR publication requires a settled exact version, policy and verified delivery identity"), "execution_validation", "data.finalization")
		}
		*f = f.Public()
	}
	if len(data.Evidence) != 0 {
		return nativeRejection(nativePublicInvalid("Attempt evidence is owned by attachment uploads"), "execution_validation", "data.evidence")
	}
	if data.CompletionBody != "" && (eventType != "run.finished" || data.Sequence <= 0 || len(data.CompletionBody) > 64<<10 || !utf8.ValidString(data.CompletionBody)) {
		return nativeRejection(nativePublicInvalid("Completion responses require a bounded ordered terminal event"), "execution_validation", "data.completion_body")
	}
	if data.Disposition != nil && (eventType != "run.finished" || data.Sequence <= 0 || !slices.Contains([]string{workpad.StatusInProgress, workpad.StatusBlocked, workpad.StatusComplete}, data.Disposition.Status)) {
		return nativeRejection(nativePublicInvalid("Final disposition requires an ordered terminal event and a supported workpad status"), "execution_validation", "data.disposition")
	}
	if data.Disposition != nil && (len(data.Disposition.FinalSummary) > workpad.MaxFinalSummaryBytes || !utf8.ValidString(data.Disposition.FinalSummary)) {
		return nativeRejection(nativePublicInvalid("Final summary must be bounded UTF-8 text"), "execution_validation", "data.disposition")
	}
	if data.Sequence == 0 {
		if data.Identity != nil || data.Handoff != nil || data.MachineID != "" || data.RunnerID != "" || data.SessionID != "" {
			return nativeRejection(nativePublicInvalid("Execution metadata requires an ordered event"), "execution_validation", "data")
		}
		return nil
	}
	if data.Identity == nil && eventType == "run.finished" && data.Sequence == 1 && data.TerminalFailure != nil &&
		data.Runtime != nil && data.Runtime.LocalAttemptID > 0 && data.Runtime.Generation > 0 &&
		data.Runtime.Activity == nil && data.Runtime.Validation == nil && data.Runtime.Landing == nil && data.Runtime.Completion == nil && data.SessionID == "" &&
		len(data.Usage) == 0 && data.Handoff == nil && data.Finalization == nil && data.Disposition == nil && data.CompletionBody == "" {
		data.TerminalFailure.Stage = "pre_provider"
		*data.TerminalFailure = data.TerminalFailure.Public()
		return nil
	}
	if data.Sequence < 0 || data.Identity == nil || !validExecutionName(data.Identity.Role) || !validExecutionName(data.Identity.Backend) || !validExecutionName(data.Identity.Model) {
		return nativeRejection(nativePublicInvalid("Ordered events require a positive sequence and bounded execution identity"), "execution_validation", "data")
	}
	if data.Handoff == nil {
		if eventType == "run.checkpointed" {
			return nativeRejection(nativePublicInvalid("Ordered checkpoints require a structured handoff"), "execution_validation", "data.handoff")
		}
		return nil
	}
	c := data.Handoff
	if eventType != "run.checkpointed" || !slices.Contains([]string{"resume_session", "fresh_checkout", "manual_recovery"}, c.Resume) ||
		!slices.Contains([]string{"available", "missing", "inaccessible", "unverified"}, c.Availability) ||
		!slices.Contains([]string{"local_only", "git_ref", "customer_store"}, c.Storage) ||
		!slices.Contains([]string{"clean", "dirty", "unpushed", "unknown"}, c.WorktreeState) ||
		!slices.Contains([]string{"none", "git_push", "pr_create", "provider_turn"}, c.ExternalEffect) ||
		!slices.Contains([]string{"none", "pending", "confirmed", "ambiguous"}, c.EffectState) {
		return nativeRejection(nativePublicInvalid("Checkpoint has an unsupported recovery or effect state"), "execution_validation", "data.handoff")
	}
	if p := c.Publication; p != nil && (c.ExternalEffect != "pr_create" || c.EffectState != "confirmed" || c.Change == nil || *c.Change != p.SourceVersion || c.HeadSHA != p.HeadSHA || c.EffectID != p.EffectID("pr_create") ||
		!validNativeID(p.ChangeID, "change") || !validNativeID(p.VersionID, "version") || !validNativeID(p.PolicyID, "policy") || p.SourceAttemptID != "" && !validNativeID(p.SourceAttemptID, "attempt") ||
		len(p.BaseRef) > 255 || len(p.Branch) > 255 || strings.ContainsAny(p.BaseRef+p.Branch, " \t\r\n~^:?*[\\") || strings.Contains(p.BaseRef+p.Branch, "..")) {
		return nativeRejection(nativeInvalid("PR publication requires its exact confirmed checkpoint and bounded delivery identity"), "execution_validation", "data.handoff.publication")
	}
	if c.HeadSHA != "" && !validCommitID(c.HeadSHA) || c.ExpectedHeadSHA != "" && !validCommitID(c.ExpectedHeadSHA) || c.WorkspaceDigest != "" && !validCommitID(c.WorkspaceDigest) {
		return nativeRejection(nativePublicInvalid("Checkpoint heads must be commit IDs"), "execution_validation", "data.handoff")
	}
	if c.ExternalEffect == "none" && (c.EffectState != "none" || c.EffectID != "") || c.ExternalEffect != "none" && (c.EffectState == "none" || !validNativeID(c.EffectID, "effect")) {
		return nativeRejection(nativePublicInvalid("External effects require a typed reconciliation identity and state"), "execution_validation", "data.handoff")
	}
	if (c.Storage == "git_ref") != (c.Ref != "" || c.CommitSHA != "" || c.TreeSHA != "" || c.BaseSHA != "") || c.Storage == "git_ref" &&
		(!validCheckpointRef(c.Ref) || !validCommitID(c.CommitSHA) || !validCommitID(c.TreeSHA) || c.BaseSHA != "" && !validCommitID(c.BaseSHA)) {
		return nativeRejection(nativePublicInvalid("Git checkpoints require a Detent checkpoint ref and commit, tree and base IDs"), "execution_validation", "data.handoff")
	}
	if c.Storage == "customer_store" && (len(data.ArtifactIDs) == 0 || c.Availability == "available") {
		return nativeRejection(nativePublicInvalid("Customer checkpoint references require artifacts and independent availability verification"), "execution_validation", "data.handoff")
	}
	if c.Change != nil && (!validNativeID(c.Change.ChangeID, "change") || !validNativeID(c.Change.VersionID, "version") || !validCommitID(c.Change.HeadSHA)) {
		return nativeRejection(nativePublicInvalid("Change references require typed immutable version and head identities"), "execution_validation", "data.handoff")
	}
	return nil
}

type nativeAttemptUpdate struct {
	data            tracker.NativeRunData
	digest          string
	status          string
	exists          bool
	duplicate       bool
	history         bool
	usageCorrection bool
}

func prepareNativeAttempt(ctx context.Context, tx nativeQueryer, scope nativeScope, item tracker.NativeWorkItemID, event *tracker.NativeRunEvent, now time.Time, termination bool, prior *nativeAttemptUpdate) (nativeAttemptUpdate, error) {
	update := nativeAttemptUpdate{history: true}
	data := event.Data
	encoded, err := marshalNative(data)
	if err != nil {
		return update, err
	}
	hash := sha256.Sum256([]byte(event.Type + " " + encoded))
	digest := hex.EncodeToString(hash[:])
	var previousHash string
	err = tx.QueryRowContext(ctx, "SELECT request_hash FROM native_attempt_events WHERE attempt_id = ? AND sequence = ?", data.AttemptID, data.Sequence).Scan(&previousHash)
	if err == nil {
		if previousHash != digest {
			return update, nativeExecutionConflict("Attempt sequence already contains different content")
		}
		update.duplicate = true
		return update, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return update, err
	}
	if checkpoint := data.Handoff; !termination && checkpoint != nil && checkpoint.Publication != nil {
		if err := requireNativePRPublication(ctx, tx, scope, item, data, checkpoint.Publication, now); err != nil {
			return update, err
		}
	}
	if f := data.Finalization; !termination && f != nil && f.Publication != nil {
		if err := requireNativePRPublication(ctx, tx, scope, item, data, f.Publication, now); err != nil {
			return update, err
		}
	}
	update.digest = digest
	var previousJSON, status string
	var sequence int64
	if prior != nil {
		err = nil
		sequence, status = prior.data.Sequence, prior.status
	} else {
		err = tx.QueryRowContext(ctx, "SELECT data_json, sequence, status FROM native_attempts WHERE id = ?", data.AttemptID).Scan(&previousJSON, &sequence, &status)
	}
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if (event.Type != "run.started" && (event.Type != "run.finished" || data.Identity != nil)) || data.Sequence != 1 {
			return update, nativeExecutionConflict("An attempt must begin at sequence 1 with a provider start or terminal setup failure")
		}
		if data.Identity != nil {
			if err := validateProviderAttempt(ctx, tx, scope, data, now); err != nil {
				return update, err
			}
		}
		var conflicts int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM native_attempts WHERE lease_id = ? OR (run_id = ? AND work_item_id != ?)`, data.LeaseID, data.RunID, item).Scan(&conflicts); err != nil {
			return update, err
		}
		if conflicts != 0 {
			return update, nativeExecutionConflict("Lease or run is already bound to another attempt or issue")
		}
		status = "running"
		if data.Identity == nil {
			status = data.Outcome
		}
	case err != nil:
		return update, err
	default:
		update.exists = true
		var previous tracker.NativeRunData
		if prior != nil {
			previous = prior.data
		} else if err := decodeRunDetail(ctx, tx, previousJSON, &previous); err != nil {
			return update, err
		}
		if termination {
			identity := tracker.NativeRunData{AttemptID: data.AttemptID, RunID: data.RunID, LeaseID: data.LeaseID,
				FencingToken: data.FencingToken, PolicyID: data.PolicyID, Identity: data.Identity,
				MachineID: data.MachineID, SessionID: data.SessionID, RunnerID: data.RunnerID,
				Sequence: data.Sequence, Outcome: "cancelled"}
			if !reflect.DeepEqual(data, identity) {
				return update, nativeExecutionConflict("Released execution may acknowledge termination only; source and completion evidence require current authority")
			}
			if previous.LeaseID != data.LeaseID || previous.FencingToken != data.FencingToken || previous.RunID != data.RunID || previous.PolicyID != data.PolicyID || previous.Identity == nil || data.Identity == nil || *previous.Identity != *data.Identity {
				return update, nativeExecutionConflict("Termination acknowledgment must match the recorded execution owner")
			}
			next := data.Sequence
			data = previous
			data.Sequence, data.Outcome = next, "cancelled"
			data.Handoff, data.ArtifactIDs = nil, nil
			if data.TerminalFailure == nil {
				failure, err := leaseReleaseFailure(ctx, tx, data, now)
				if err != nil {
					return update, err
				}
				data.TerminalFailure = &failure
			}
			event.Data = data
		}
		data.Evidence = previous.Evidence
		reconnecting := (status == "running" || status == "interrupted") && event.Type == "run.observed" && data.Runtime != nil && data.Runtime.LocalAttemptID == 0 && data.Runtime.Generation == 0 && data.Outcome == ""
		usageCorrection := status != "running" && event.Type == "run.finished" && nativeUsageCorrection(previous, data)
		terminating := termination && status == "interrupted"
		if previous.LeaseID != data.LeaseID || previous.RunID != data.RunID || previous.PolicyID != data.PolicyID || previous.Identity == nil || data.Identity == nil || *previous.Identity != *data.Identity || status != "running" && !usageCorrection && !reconnecting && !terminating || data.Sequence != sequence+1 || event.Type == "run.started" {
			return update, nativeExecutionConflict("Attempt identity, lifecycle or next sequence does not match")
		}
		if previous.Runtime != nil && !reconnecting {
			if data.Runtime == nil || previous.Runtime.LocalAttemptID != 0 && (data.Runtime.LocalAttemptID != previous.Runtime.LocalAttemptID || data.Runtime.Generation != previous.Runtime.Generation) || data.Runtime.HeartbeatAt.Before(previous.Runtime.HeartbeatAt) {
				return update, nativeExecutionConflict("Runtime attribution or observation order changed during an attempt")
			}
			if selected := previous.Runtime.WorkerLaunch; selected != nil {
				if data.Runtime.WorkerLaunch == nil {
					return update, nativeExecutionConflict("Execution worker launch identity was removed")
				}
				before, after := *selected, *data.Runtime.WorkerLaunch
				before.PID, before.GroupID, before.ProcessStart = after.PID, after.GroupID, after.ProcessStart
				if before != after {
					return update, nativeExecutionConflict("Execution worker release or policy changed during an attempt")
				}
			}
		}
		if event.Type == "run.observed" {
			update.history = nativeRuntimeHistoryChanged(previous.Runtime, data.Runtime)
		}
		if reconnecting {
			status = "running"
		}
		if event.Type == "run.finished" && !usageCorrection {
			status = data.Outcome
		}
		update.usageCorrection = usageCorrection
	}
	update.data, update.status = data, status
	if update.usageCorrection {
		update.history = false
	}
	return update, nil
}

func recordNativeAttempt(ctx context.Context, tx *sql.Tx, scope nativeScope, item tracker.NativeWorkItemID, event *tracker.NativeRunEvent, now time.Time, termination bool) (recorded, history bool, resultErr error) {
	update, err := prepareNativeAttempt(ctx, tx, scope, item, event, now, termination, nil)
	if err != nil || update.duplicate {
		return false, false, err
	}
	data := update.data
	if checkpoint := data.Handoff; !termination && checkpoint != nil && checkpoint.Publication != nil {
		if err := recordNativePRPublication(ctx, tx, data, checkpoint.Publication); err != nil {
			return false, false, err
		}
	}
	if f := data.Finalization; !termination && f != nil && f.Publication != nil {
		if err := recordNativePRPublication(ctx, tx, data, f.Publication); err != nil {
			return false, false, err
		}
	}
	storedEncoded, err := encodeRunDetail(ctx, data, false)
	if err != nil {
		return false, false, err
	}
	if !update.exists {
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
		status := "running"
		if data.Identity == nil {
			status = data.Outcome
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO native_attempts (id, organization_id, project_id, work_item_id, lease_id, fencing_token, run_id, sequence, status, data_json, started_at, updated_at, work_item_revision, dispatch_generation, checkpoint_preserves_source)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0)`, data.AttemptID, scope.organization, scope.project, item, data.LeaseID, data.FencingToken, data.RunID, data.Sequence, status, storedEncoded, formatHubTime(now), formatHubTime(now), revision, dispatch.generation)
		if err != nil {
			return false, false, err
		}
	} else {
		if event.Type == "run.finished" && !update.usageCorrection && !termination {
			if err := publishAttemptEvidence(ctx, tx, scope, item, data, now); err != nil {
				return false, false, err
			}
		}
		if update.usageCorrection {
			if _, err := tx.ExecContext(ctx, "UPDATE native_attempts SET sequence=?,data_json=?,analytics_revision=analytics_revision+1 WHERE id=?", data.Sequence, storedEncoded, data.AttemptID); err != nil {
				return false, false, err
			}
		} else if _, err := tx.ExecContext(ctx, "UPDATE native_attempts SET sequence = ?, status = ?, data_json = ?, updated_at = ?, analytics_revision = analytics_revision + 1 WHERE id = ?", data.Sequence, update.status, storedEncoded, formatHubTime(now), data.AttemptID); err != nil {
			return false, false, err
		}
	}
	if !termination && data.Runtime != nil && data.Runtime.Landing != nil {
		if err := recordAttemptLanding(ctx, tx, scope, item, data.AttemptID, *data.Runtime.Landing); err != nil {
			return false, false, err
		}
	}
	if data.Handoff != nil {
		checkpoint, err := encodeRunCheckpoint(ctx, data)
		if err != nil {
			return false, false, err
		}
		artifacts, err := marshalNative(data.ArtifactIDs)
		if err != nil {
			return false, false, err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE native_attempts SET checkpoint_json = ?, artifact_ids_json = ?, checkpoint_preserves_source = ? WHERE id = ?", checkpoint, artifacts, data.Handoff.PreservesSource(), data.AttemptID); err != nil {
			return false, false, err
		}
	}
	if err := updateAttemptClaimFacts(ctx, tx, data.AttemptID, data); err != nil {
		return false, false, err
	}
	if err := writeAttemptUsageFacts(ctx, tx, data.AttemptID, data); err != nil {
		return false, false, err
	}
	if err := updateAttemptHotProjection(ctx, tx, data.AttemptID, update.status, data); err != nil {
		return false, false, err
	}
	if err := recordRunDetailReference(ctx, tx, scope, item, data, formatHubTime(now)); err != nil {
		return false, false, err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO native_attempt_events (attempt_id, sequence, request_hash) VALUES (?, ?, ?)", data.AttemptID, data.Sequence, update.digest)
	return err == nil, update.history, err
}

func requireNativePRPublication(ctx context.Context, tx nativeQueryer, scope nativeScope, item tracker.NativeWorkItemID, data tracker.NativeRunData, publication *tracker.NativePRPublication, now time.Time) error {
	if err := requireNativeMutationLease(ctx, tx, scope, string(item), tracker.Mutation{LeaseID: data.LeaseID, FencingToken: data.FencingToken}, now); err != nil {
		return err
	}
	change, err := readChange(ctx, tx, scope, string(item), publication.ChangeID)
	if err != nil {
		return err
	}
	version, err := readChangeVersion(ctx, tx, publication.ChangeID, publication.VersionID)
	if err != nil {
		return err
	}
	if change.WorkItemID != item || change.CurrentVersion != publication.VersionID || !version.Policy.Gates.GitHubPullRequest || !publication.Matches(publication.ChangeID, version) {
		return nativeExecutionConflict("PR publication no longer matches the current Change version or source")
	}
	var raw string
	if err := tx.QueryRowContext(ctx, "SELECT checkpoint_json FROM native_attempts WHERE id=?", data.AttemptID).Scan(&raw); err != nil {
		return err
	}
	checkpoint, err := decodeRunCheckpoint(ctx, tx, raw)
	if err != nil {
		return err
	}
	if checkpoint == nil || checkpoint.ExternalEffect != "pr_create" || checkpoint.EffectState != "confirmed" || checkpoint.EffectID != publication.EffectID("pr_create") || checkpoint.Change == nil || *checkpoint.Change != publication.SourceVersion || checkpoint.HeadSHA != publication.HeadSHA {
		return nativeExecutionConflict("PR publication requires its exact confirmed checkpoint; reconcile pending or ambiguous effects before completion")
	}
	var head string
	if err := tx.QueryRowContext(ctx, `SELECT head_sha FROM attempt_diffs WHERE organization_id=? AND project_id=? AND work_item_id=?
AND attempt_id=? AND source='attempt' AND seq=? AND producer_lease_id=? AND producer_fencing_token=?`, scope.organization, scope.project, item, data.AttemptID, data.Sequence, data.LeaseID, data.FencingToken).Scan(&head); err != nil {
		return nativeExecutionConflict("PR publication requires the current fenced final diff; retain source and reconcile its head before completion")
	}
	if head != publication.HeadSHA {
		return nativeExecutionConflict("PR publication head changed after verification; reconcile the current source before completion")
	}
	return nil
}

func recordNativePRPublication(ctx context.Context, tx *sql.Tx, data tracker.NativeRunData, publication *tracker.NativePRPublication) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO native_pr_publications
(version_id, change_id, repository, base_ref, branch, head_sha, policy_id, source_attempt_id, recorder_attempt_id, provider, external_id, external_url)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(version_id) DO NOTHING`,
		publication.VersionID, publication.ChangeID, publication.Repository, publication.BaseRef, publication.Branch, publication.HeadSHA, publication.PolicyID, publication.SourceAttemptID, data.AttemptID, publication.External.Provider, publication.External.ID, publication.External.URL)
	return err
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
	if handled, err := s.archiveView(c, c.Param("item"), "attempts", nil); handled {
		return err
	}
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
	ctx = s.runDetailContext(ctx)
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	path := "/api/v2/organizations/" + url.PathEscape(string(scope.organization)) + "/projects/" + url.PathEscape(string(scope.project)) + "/work-items/" + url.PathEscape(item) + "/attempts"

	if err := validateNativeQuery(params, "view", "direction"); err != nil {
		return tracker.Page[tracker.NativeAttempt]{}, err
	}
	view := params.Get("view")
	if view != "" && view != "blockers" && view != "recorded_blockers" {
		return tracker.Page[tracker.NativeAttempt]{}, nativePublicInvalid("view supports blockers or recorded_blockers")
	}
	budget := 1 << 20
	if view != "" {
		budget = operatortool.WorkHistoryPageBytes
	}
	limit, cursor, key, err := s.readNativePage(ctx, scope, path, params)
	if err != nil {
		return tracker.Page[tracker.NativeAttempt]{}, err
	}

	if _, _, err := readNativeIssueProjection(ctx, s.database.reader, scope, item, true); err != nil {
		return tracker.Page[tracker.NativeAttempt]{}, err
	}
	_, after, err := nativePageOrder(params, "a.fencing_token", cursor.After)
	if err != nil {
		return tracker.Page[tracker.NativeAttempt]{}, err
	}
	tx, err := s.database.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return tracker.Page[tracker.NativeAttempt]{}, err
	}
	defer tx.Rollback()
	query := `SELECT a.data_json, a.status, a.started_at, a.updated_at, a.checkpoint_json, a.artifact_ids_json, l.expires_at, l.released_at, l.renewed_at, a.work_item_revision, a.dispatch_generation
FROM native_attempts a JOIN leases l ON l.lease_id = a.lease_id
WHERE a.organization_id = ? AND a.project_id = ? AND a.work_item_id = ? AND a.fencing_token > CAST(? AS INTEGER) ORDER BY a.fencing_token LIMIT ?`
	if params.Get("direction") == "newest" {
		query = `SELECT a.data_json, a.status, a.started_at, a.updated_at, a.checkpoint_json, a.artifact_ids_json, l.expires_at, l.released_at, l.renewed_at, a.work_item_revision, a.dispatch_generation
FROM native_attempts a JOIN leases l ON l.lease_id = a.lease_id
WHERE a.organization_id = ? AND a.project_id = ? AND a.work_item_id = ? AND a.fencing_token < CAST(? AS INTEGER) ORDER BY a.fencing_token DESC LIMIT ?`
	}
	rows, err := tx.QueryContext(ctx, query, scope.organization, scope.project, item, after, limit+1)
	if err != nil {
		return tracker.Page[tracker.NativeAttempt]{}, err
	}
	defer rows.Close()
	var attempts []tracker.NativeAttempt
	for rows.Next() {
		attempt, err := scanNativeAttempt(ctx, tx, rows, s.config.now())
		if err != nil {
			return tracker.Page[tracker.NativeAttempt]{}, err
		}
		if view != "" {
			attempt.Runtime = nil
			attempt.RuntimeFreshness = "unavailable"
		}
		attempts = append(attempts, attempt)
	}
	if err := rows.Err(); err != nil {
		return tracker.Page[tracker.NativeAttempt]{}, err
	}
	if err := rows.Close(); err != nil {
		return tracker.Page[tracker.NativeAttempt]{}, err
	}
	page := tracker.Page[tracker.NativeAttempt]{Items: []tracker.NativeAttempt{}}
	hasMore := false
	for _, attempt := range attempts {
		if len(page.Items) == limit {
			hasMore = true
			break
		}
		if view != "recorded_blockers" {
			if err := readAttachmentEvidence(ctx, tx, scope, item, &attempt.NativeRunData); err != nil {
				return tracker.Page[tracker.NativeAttempt]{}, err
			}
		}
		attempt.Identity = nativeAttemptDisplayIdentity(attempt)
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
		if len(encoded)+1 > budget && len(page.Items) == 0 && view != "" {
			candidate.Items[0] = nativeAttemptAuthorityProjection(attempt)
			encoded, err = json.Marshal(candidate)
			if err != nil {
				return tracker.Page[tracker.NativeAttempt]{}, err
			}
		}
		if len(encoded)+1 > budget {
			if len(page.Items) == 0 {
				return tracker.Page[tracker.NativeAttempt]{}, &nativeError{Code: "attempt_record_too_large", Message: fmt.Sprintf("Work item %s attempt %s exceeds the %d-byte page limit", item, attempt.AttemptID, budget), status: http.StatusUnprocessableEntity, publicMessage: true}
			}
			hasMore = true
			break
		}
		page.Items = candidate.Items
		cursor = next
	}
	if hasMore {
		page.NextCursor, err = encodeNativeCursor(cursor, key)
		if err != nil {
			return tracker.Page[tracker.NativeAttempt]{}, err
		}
	}
	return page, nil
}

func nativeAttemptAuthorityProjection(attempt tracker.NativeAttempt) tracker.NativeAttempt {
	if attempt.CompletionBody != "" {
		attempt.CompletionBody = ""
		attempt.Unavailable = append(attempt.Unavailable, "completion_body")
	}
	if len(attempt.Evidence) > 0 {
		attempt.Evidence = nil
		attempt.Unavailable = append(attempt.Unavailable, "evidence")
	}
	if len(attempt.Steps) > 0 {
		attempt.Steps = nil
		attempt.Unavailable = append(attempt.Unavailable, "steps")
	}
	if attempt.ProcessUsage != nil {
		attempt.ProcessUsage = nil
		attempt.Unavailable = append(attempt.Unavailable, "process_usage")
	}
	if len(attempt.Usage) > 0 {
		attempt.Usage = nil
		attempt.Unavailable = append(attempt.Unavailable, "usage")
	}
	return attempt
}

func nativeAttemptDisplayIdentity(attempt tracker.NativeAttempt) *tracker.NativeExecutionIdentity {
	if attempt.Identity == nil {
		return nil
	}
	identity := *attempt.Identity
	identity.ReasoningEffort = ""
	if attempt.Runtime != nil {
		runtime := attempt.Runtime.Identity
		identity.ReasoningEffort = runtime.ReasoningEffort.Value
		if runtime.ResolvedModel.Value != "" {
			identity.Model = runtime.ResolvedModel.Value
		} else if runtime.RequestedModel.Value != "" {
			identity.Model = runtime.RequestedModel.Value
		}
	}
	return &identity
}

func scanNativeAttempt(ctx context.Context, q nativeQueryer, rows *sql.Rows, now time.Time) (tracker.NativeAttempt, error) {
	var attempt tracker.NativeAttempt
	var data, started, updated, artifacts, expires, renewed string
	var checkpoint, released sql.NullString
	if err := rows.Scan(&data, &attempt.Status, &started, &updated, &checkpoint, &artifacts, &expires, &released, &renewed, &attempt.WorkItemRevision, &attempt.DispatchGeneration); err != nil {
		return attempt, err
	}
	if err := decodeRunDetail(ctx, q, data, &attempt.NativeRunData); err != nil {
		return attempt, err
	}
	if checkpoint.Valid {
		decoded, err := decodeRunCheckpoint(ctx, q, checkpoint.String)
		if err != nil {
			return attempt, err
		}
		attempt.Checkpoint = decoded
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
	if handled, err := s.archiveView(c, c.Param("item"), "attempt:"+c.Param("attempt"), nil); handled {
		return err
	}
	result, err := s.readNativeAttempt(c.Request().Context(), nativeRequestScope(c), c.Param("item"), c.Param("attempt"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return nativeArchiveJSON(c, http.StatusOK, result)
}

func (s *Service) readNativeAttempt(ctx context.Context, scope nativeScope, item, id string) (tracker.NativeAttempt, error) {
	ctx = s.runDetailContext(ctx)
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	tx, err := s.database.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return tracker.NativeAttempt{}, err
	}
	defer tx.Rollback()
	return readNativeAttempt(ctx, tx, scope, item, id, s.config.now())
}

func readNativeAttempt(ctx context.Context, q nativeQueryer, scope nativeScope, item, id string, now time.Time) (tracker.NativeAttempt, error) {
	if _, _, err := readNativeIssue(ctx, q, scope, item); err != nil {
		return tracker.NativeAttempt{}, err
	}
	attempt, err := readNativeAttemptRecord(ctx, q, scope, item, id, now)
	if err != nil {
		return attempt, err
	}
	if explanationRead(ctx) {
		return attempt, nil
	}
	err = readAttachmentEvidence(ctx, q, scope, item, &attempt.NativeRunData)
	return attempt, err
}

func readNativeAttemptRecord(ctx context.Context, q nativeQueryer, scope nativeScope, item, id string, now time.Time) (tracker.NativeAttempt, error) {
	var err error
	q, err = archiveQuery(ctx, q, item, "run", id, 0)
	if err != nil {
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
	return scanNativeAttempt(ctx, q, rows, now)
}

func leaseReleaseFailure(ctx context.Context, tx nativeQueryer, data tracker.NativeRunData, now time.Time) (tracker.NativeTerminalFailure, error) {
	var reason sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT e.payload_json FROM work_events e JOIN leases l ON l.issue_id=e.issue_id AND l.fencing_token=e.fencing_token
WHERE l.lease_id=? AND l.fencing_token=? AND e.kind='lease_released' ORDER BY e.id DESC LIMIT 1`, data.LeaseID, data.FencingToken).Scan(&reason)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return tracker.NativeTerminalFailure{}, fmt.Errorf("read attempt lease release reason: %w", err)
	}
	failure := tracker.NativeTerminalFailure{ErrorClass: "lease_released", Stage: "lease_release", ObservedAt: now.UTC(),
		Error: "attempt lease was released before the attempt finished; release reason unavailable (lease expired, replaced or its runner was revoked)"}
	if reason.Valid {
		var payload struct {
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal([]byte(reason.String), &payload); err != nil {
			return tracker.NativeTerminalFailure{}, err
		}
		reason.String = payload.Reason
	}
	if reason.Valid && validExecutionName(reason.String) {
		failure.Error = "attempt lease was released before the attempt finished; release reason: " + reason.String
	}
	return failure.Public(), nil
}

func cancelNativeLeaseAttempt(ctx context.Context, tx *sql.Tx, scope nativeScope, lease leaseRecord, now time.Time) error {
	var raw string
	var item tracker.NativeWorkItemID
	var revision tracker.Revision
	err := tx.QueryRowContext(ctx, `SELECT a.data_json, a.work_item_id, i.revision FROM native_attempts a
JOIN issues i ON i.native_id = a.work_item_id AND i.project_id = a.project_id AND i.organization_id = a.organization_id
WHERE a.lease_id = ? AND a.fencing_token = ? AND a.organization_id = ? AND a.project_id = ? AND a.status IN ('running', 'interrupted')`, lease.session.ID, lease.session.FencingToken, scope.organization, scope.project).Scan(&raw, &item, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var stored storedRun
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		return err
	}
	if owner, ok := ctx.Value(runDetailContextKey{}).(*runDetailContext); ok && stored.Detail != nil {
		owner.reuse[stored.AttemptID] = stored.Detail
	}
	var previous tracker.NativeRunData
	if err := decodeRunDetail(ctx, tx, raw, &previous); err != nil {
		return err
	}
	if previous.Identity == nil {
		return nil
	}
	data := tracker.NativeRunData{AttemptID: previous.AttemptID, RunID: previous.RunID, LeaseID: previous.LeaseID,
		FencingToken: previous.FencingToken, PolicyID: previous.PolicyID, Identity: previous.Identity,
		MachineID: previous.MachineID, SessionID: previous.SessionID, RunnerID: previous.RunnerID,
		Sequence: previous.Sequence + 1, Outcome: "cancelled"}
	event := tracker.NativeRunEvent{Type: "run.finished", SchemaVersion: 1, Data: data}
	_, history, err := recordNativeAttempt(ctx, tx, scope, item, &event, now, true)
	if err != nil || !history {
		return err
	}
	return appendNativeHistory(ctx, tx, scope, string(item), event.Type, tracker.CollaborationData{Run: &event.Data, Revision: revision}, now)
}
