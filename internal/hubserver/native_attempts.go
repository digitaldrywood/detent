package hubserver

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
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
			return nativeRejection(nativePublicInvalid("Current run events require a positive sequence"), "execution_validation", "data.sequence")
		}
		return nil
	}
	if data.Identity == nil && eventType == "run.finished" && data.Sequence == 1 && data.TerminalFailure != nil &&
		(data.Runtime == nil && data.Outcome == "cancelled" && data.TerminalFailure.ErrorClass == "lease_released" ||
			data.Runtime != nil && data.Runtime.LocalAttemptID >= 0 && data.Runtime.Generation > 0 &&
				data.Runtime.Activity == nil && data.Runtime.Validation == nil && data.Runtime.Landing == nil && data.Runtime.Completion == nil) &&
		(len(data.Usage) == 0 || data.TerminalFailure.Decision != nil && data.TerminalFailure.Decision.Valid()) && data.Handoff == nil && data.Finalization == nil && data.Disposition == nil && data.CompletionBody == "" {
		data.TerminalFailure.Stage = "pre_provider"
		*data.TerminalFailure = data.TerminalFailure.Public()
		return nil
	}
	if eventType == "run.finished" && data.Identity == nil && data.Sequence > 1 && data.Outcome == "cancelled" && nativeTerminationData(data) {
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

func nativeRuntimeHistoryChanged(previous, observation *tracker.NativeRuntimeObservation) bool {
	if previous == nil || observation == nil {
		return previous != observation
	}
	if nativeRuntimeSession(previous) != nativeRuntimeSession(observation) {
		return true
	}
	left, right := *previous, *observation
	left.Activity, right.Activity = nil, nil
	left.REST, right.REST = nil, nil
	left.GitHub, right.GitHub = nil, nil
	left.HeartbeatAt, right.HeartbeatAt = time.Time{}, time.Time{}
	if len(left.Pipeline) == 0 && len(right.Pipeline) == 0 {
		left.Pipeline, right.Pipeline = nil, nil
	}
	if len(left.Phases) == 0 && len(right.Phases) == 0 {
		left.Phases, right.Phases = nil, nil
	}
	if len(left.RepairDependencies) == 0 && len(right.RepairDependencies) == 0 {
		left.RepairDependencies, right.RepairDependencies = nil, nil
	}
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
	if params.Get("view") == "" && c.Request().Header.Get("X-Detent-Attempt-Detail") == "full" {
		params.Set("view", "detail")
	}
	result, err := s.readNativeAttemptPageTenant(ctx, scope, c.Param("item"), params, json.Marshal)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, result.encoded)
}

type nativeAttemptPage struct {
	tracker.Page[tracker.NativeAttempt]
	encoded []byte
}

func (p nativeAttemptPage) candidateBytes(encoded []byte, cursor string) int {
	separator := 0
	if len(p.Items) > 0 {
		separator = 1
	}
	return len(p.encoded) + separator + len(encoded) + len(`],"next_cursor":"`) + len(cursor) + len("\"}\n")
}

func (p *nativeAttemptPage) appendAttempt(attempt tracker.NativeAttempt, encoded []byte) {
	if len(p.Items) > 0 {
		p.encoded = append(p.encoded, ',')
	}
	p.encoded = append(p.encoded, encoded...)
	p.Items = append(p.Items, attempt)
}

func (p *nativeAttemptPage) finish() {
	p.encoded = append(p.encoded, ']')
	if p.NextCursor != "" {
		p.encoded = append(p.encoded, `,"next_cursor":"`...)
		p.encoded = append(p.encoded, p.NextCursor...)
		p.encoded = append(p.encoded, '"')
	}
	p.encoded = append(p.encoded, '}', '\n')
}

func (s *Service) readAttempts(ctx context.Context, scope nativeScope, item string, params url.Values) (tracker.Page[tracker.NativeAttempt], error) {
	result, err := s.readNativeAttemptPage(ctx, scope, item, params, json.Marshal)
	return result.Page, err
}

func (s *Service) readNativeAttemptPage(ctx context.Context, scope nativeScope, item string, params url.Values, marshal func(any) ([]byte, error)) (nativeAttemptPage, error) {
	return s.readNativeAttemptPageTenant(ctx, scope, item, params, marshal)
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

func (s *Service) getNativeAttempt(c echo.Context) error {
	if c.QueryParam("view") == "cleanup" {
		result, err := s.readNativeCleanupAttempt(c.Request().Context(), nativeRequestScope(c), c.Param("item"), c.Param("attempt"))
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		return c.JSON(http.StatusOK, result)
	}
	result, err := s.readNativeAttempt(c.Request().Context(), nativeRequestScope(c), c.Param("item"), c.Param("attempt"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return nativeArchiveJSON(c, http.StatusOK, result)
}

func (s *Service) readNativeCleanupAttempt(ctx context.Context, scope nativeScope, item, id string) (tracker.NativeAttempt, error) {
	if !scope.credential.Runner.Valid() {
		return tracker.NativeAttempt{}, nativeNotFound()
	}
	ctx = s.runDetailContext(ctx)
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return tracker.NativeAttempt{}, nativeRunEventStorageUnavailable()
	}
	var snapshot tenantAttemptSnapshot
	err := repository.ReadTenantSnapshot(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		project, err := r.Project(ctx, scope.project)
		if err != nil {
			return err
		}
		if project.DeletedAt != nil {
			return corestore.ErrNotFound
		}
		issue, err := r.WorkItem(ctx, scope.project, tracker.NativeWorkItemID(item))
		if err != nil {
			return err
		}
		snapshot.attempt, err = r.Attempt(ctx, id)
		if err != nil {
			return err
		}
		if snapshot.attempt.IssueID != issue.ID {
			return corestore.ErrNotFound
		}
		snapshot.lease, err = r.Lease(ctx, snapshot.attempt.LeaseID)
		if err != nil {
			return err
		}
		if snapshot.lease.IssueID != issue.ID || snapshot.lease.FencingToken != snapshot.attempt.FencingToken || snapshot.lease.MachineID != string(scope.credential.Runner.MachineID) || snapshot.lease.ReleasedAt == nil {
			return corestore.ErrNotFound
		}
		runner, err := r.RunnerForLease(ctx, snapshot.lease.LeaseID)
		if err != nil {
			return err
		}
		if runner.RunnerID != scope.credential.Runner.RunnerID {
			return corestore.ErrNotFound
		}
		snapshot.event, err = r.LatestRunEvent(ctx, scope.project, id)
		if err != nil {
			return err
		}
		if snapshot.event.WorkItemID != tracker.NativeWorkItemID(item) {
			return corestore.ErrNotFound
		}
		return readTenantAttemptPayload(ctx, r, &snapshot)
	})
	if err != nil {
		return tracker.NativeAttempt{}, projectRecordError(err)
	}
	if s.runDetailStore() == nil {
		return tracker.NativeAttempt{}, nativeRunEventStorageUnavailable()
	}
	data, err := s.readNativeAttemptRunTenant(ctx, snapshot)
	if err != nil {
		return tracker.NativeAttempt{}, err
	}
	if !nativeAttemptRunMatches(snapshot, data) || data.RunnerID != scope.credential.Runner.RunnerID {
		return tracker.NativeAttempt{}, artifact.ErrIntegrity
	}
	if !nativeRunCleanupRequired(data) || data.MachineID != scope.credential.Runner.MachineID {
		return tracker.NativeAttempt{}, nativeNotFound()
	}
	return tracker.NativeAttempt{
		NativeRunData: tracker.NativeRunData{AttemptID: data.AttemptID, RunID: data.RunID, LeaseID: data.LeaseID,
			FencingToken: data.FencingToken, PolicyID: data.PolicyID, Identity: data.Identity, MachineID: data.MachineID,
			SessionID: data.SessionID, RunnerID: data.RunnerID, Sequence: snapshot.attempt.Sequence, Outcome: data.Outcome,
			Runtime: &tracker.NativeRuntimeObservation{LocalAttemptID: data.Runtime.LocalAttemptID, Generation: data.Runtime.Generation, WorkerLaunch: data.Runtime.WorkerLaunch}},
		Status: snapshot.attempt.Status, StartedAt: snapshot.attempt.StartedAt, ClaimReleasedAt: snapshot.lease.ReleasedAt,
		CleanupAcknowledgedAt: snapshot.attempt.CleanupAcknowledgedAt,
	}, nil
}

func (s *Service) readNativeAttempt(ctx context.Context, scope nativeScope, item, id string) (tracker.NativeAttempt, error) {
	ctx = s.runDetailContext(ctx)
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	return s.readNativeAttemptTenant(ctx, scope, item, id)
}

func nativeLeaseReleaseFailure(reason string, now time.Time) tracker.NativeTerminalFailure {
	failure := tracker.NativeTerminalFailure{ErrorClass: "lease_released", Stage: "lease_release", ObservedAt: now.UTC(),
		Error: "attempt lease was released before the attempt finished; release reason unavailable (lease expired, replaced or its runner was revoked)"}
	if validExecutionName(reason) {
		failure.Error = "attempt lease was released before the attempt finished; release reason: " + reason
	}
	return failure.Public()
}
