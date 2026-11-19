package executionapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/workpage"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workpad"
)

type AttemptDeps struct {
	Repository             tenantstore.Repository
	Now                    func() time.Time
	HTTPError              func(echo.Context, error, ...any) error
	Details                func() attachment.DetailStore
	AuthorizeAccess        func(context.Context, accessstore.Reader, hubscope.NativeScope, bool) error
	Logger                 *slog.Logger
	ReadQuery              func(c echo.Context) (url.Values, error)
	ArchiveJSON            func(c echo.Context, status int, value any) error
	DetailContext          func(ctx context.Context) context.Context
	DetailsFor             func(ctx context.Context) attachment.DetailStore
	ReadPage               func(ctx context.Context, scope hubscope.NativeScope, path string, params url.Values) (int, workpage.Cursor, []byte, error)
	PageOrder              func(params url.Values, column, after string) (string, string, error)
	CleanupRequired        func(run tracker.NativeRunData) bool
	ReadLandingObservation func(ctx context.Context, scope hubscope.NativeScope, item tracker.NativeWorkItemID, ref tracker.NativeChangeReference) (*tracker.NativeLandingReceipt, string, error)
	FailureSummary         func(data tracker.NativeRunData, status string) *leasestore.AttemptFailure
}

func NativeExecutionConflict(message string) error {
	return &hubapi.NativeError{Code: "run_sequence_conflict", Message: message, Status: http.StatusConflict}
}

// NativeStaleLease maps a tracker fencing failure onto that vocabulary, so a
// lease that is no longer current reads as stale_execution rather than as the
// tracker's own error.

func NativeStaleLease(err error, message string) error {
	if errors.Is(err, tracker.ErrStaleFencingToken) {
		return hubapi.NativeStaleExecution(message)
	}
	return err
}

func ValidExecutionName(value string) bool {
	return value != "" && len(value) <= 128 && strings.IndexFunc(value, func(r rune) bool {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return false
		}
		return !strings.ContainsRune("-_.:/", r)
	}) < 0
}

func ValidCheckpointRef(ref string) bool {
	name, ok := strings.CutPrefix(ref, tracker.CheckpointRefPrefix)
	return ok && name != "" && len(name) <= 128 && strings.Trim(name, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-") == "" && !strings.HasPrefix(name, ".") && !strings.Contains(name, "..") && !strings.HasSuffix(name, ".lock")
}

func ValidateNativeExecution(nativeRejection func(error, string, string) error, nativeTerminationData func(tracker.NativeRunData) bool, data tracker.NativeRunData, eventType string) error {
	if len(data.Steps) > 8 {
		return hubapi.NativePublicInvalid("Execution step receipts exceed the bounded step set")
	}
	seenSteps := map[string]bool{}
	for _, receipt := range data.Steps {
		if receipt.ReviewEpoch < 0 || receipt.ReviewEpoch > data.FencingToken {
			return hubapi.NativePublicInvalid("Execution step review revision exceeds its owner")
		}
		if !slices.Contains([]string{"agent_turn", "commit", "validation", "publication", "security_audit", "review", "validator", "landing"}, receipt.Step) || seenSteps[receipt.Step] || receipt.PolicyID != data.PolicyID || receipt.CompletedAt.IsZero() || len(receipt.ReviewState) != 64 || receipt.Source.HeadSHA == "" || receipt.Source.WorkspaceDigest == "" || len(receipt.CompletionBody) > 64<<10 || !utf8.ValidString(receipt.CompletionBody) {
			return hubapi.NativePublicInvalid("Execution step receipt identity is invalid")
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
			return nativeRejection(hubapi.NativePublicInvalid("Execution step receipt exceeds its bounded payload"), "event_validation", field)
		}
	}
	if data.Runtime != nil && data.Runtime.WorkerLaunch != nil && data.Runtime.WorkerLaunch.PolicyID != data.PolicyID {
		return nativeRejection(hubapi.NativePublicInvalid("Execution worker policy differs from its lease"), "execution_validation", "data.runtime.worker_launch.policy_id")
	}
	if f := data.TerminalFailure; f != nil {
		if eventType != "run.finished" || data.Sequence <= 0 || f.ObservedAt.IsZero() || data.Outcome == "succeeded" {
			return nativeRejection(hubapi.NativePublicInvalid("Execution failures require an ordered failed or interrupted terminal event"), "execution_validation", "data.terminal_failure")
		}
		*f = f.Public()
	}
	if f := data.Finalization; f != nil {
		if eventType != "run.finished" || data.Sequence <= 0 || f.ObservedAt.IsZero() || f.Files < 0 ||
			f.ChangeID != "" && !hubapi.ValidID(f.ChangeID, "change") || f.VersionID != "" && !hubapi.ValidID(f.VersionID, "version") ||
			f.BaseSHA != "" && !hubapi.ValidCommitID(f.BaseSHA) || f.HeadSHA != "" && !hubapi.ValidCommitID(f.HeadSHA) ||
			f.SourceAttemptID != "" && !hubapi.ValidID(f.SourceAttemptID, "attempt") || f.VersionCode != "" && (!ValidExecutionName(f.VersionCode) || strings.ContainsAny(f.VersionCode, "/:\\")) ||
			len(f.VersionError) > tracker.NativeFinalizationTextLimit || len(f.Error) > tracker.NativeFinalizationTextLimit || !utf8.ValidString(f.VersionError) || !utf8.ValidString(f.Error) {
			return nativeRejection(hubapi.NativePublicInvalid("Finalizer observations require bounded identity and text on an ordered terminal event"), "execution_validation", "data.finalization")
		}
		if f.VersionID != "" && f.ChangeID == "" || f.SourceAttemptID != "" && f.SourceVersion == nil {
			return nativeRejection(hubapi.NativePublicInvalid("Finalizer source identity requires its owning Change and version"), "execution_validation", "data.finalization")
		}
		if v := f.SourceVersion; v != nil && (v.ChangeID != f.ChangeID || !hubapi.ValidID(v.ChangeID, "change") || !hubapi.ValidID(v.VersionID, "version") || !hubapi.ValidCommitID(v.HeadSHA)) {
			return nativeRejection(hubapi.NativePublicInvalid("Invalid finalizer source version identity"), "execution_validation", "data.finalization")
		}
		if p := f.Publication; p != nil && (!f.Settled || !f.Changed || f.VersionError != "" || f.Error != "" || p.ChangeID != f.ChangeID || p.VersionID != f.VersionID || p.HeadSHA != f.HeadSHA || !hubapi.ValidID(p.PolicyID, "policy") ||
			p.SourceAttemptID != "" && !hubapi.ValidID(p.SourceAttemptID, "attempt") || len(p.BaseRef) > 255 || len(p.Branch) > 255 || strings.ContainsAny(p.BaseRef+p.Branch, " \t\r\n~^:?*[\\") || strings.Contains(p.BaseRef+p.Branch, "..")) {
			return nativeRejection(hubapi.NativePublicInvalid("PR publication requires a settled exact version, policy and verified delivery identity"), "execution_validation", "data.finalization")
		}
		*f = f.Public()
	}
	if len(data.Evidence) != 0 {
		return nativeRejection(hubapi.NativePublicInvalid("Attempt evidence is owned by attachment uploads"), "execution_validation", "data.evidence")
	}
	if data.CompletionBody != "" && (eventType != "run.finished" || data.Sequence <= 0 || len(data.CompletionBody) > 64<<10 || !utf8.ValidString(data.CompletionBody)) {
		return nativeRejection(hubapi.NativePublicInvalid("Completion responses require a bounded ordered terminal event"), "execution_validation", "data.completion_body")
	}
	if data.Disposition != nil && (eventType != "run.finished" || data.Sequence <= 0 || !slices.Contains([]string{workpad.StatusInProgress, workpad.StatusBlocked, workpad.StatusComplete}, data.Disposition.Status)) {
		return nativeRejection(hubapi.NativePublicInvalid("Final disposition requires an ordered terminal event and a supported workpad status"), "execution_validation", "data.disposition")
	}
	if data.Disposition != nil && (len(data.Disposition.FinalSummary) > workpad.MaxFinalSummaryBytes || !utf8.ValidString(data.Disposition.FinalSummary)) {
		return nativeRejection(hubapi.NativePublicInvalid("Final summary must be bounded UTF-8 text"), "execution_validation", "data.disposition")
	}
	if data.Sequence == 0 {
		if data.Identity != nil || data.Handoff != nil || data.MachineID != "" || data.RunnerID != "" || data.SessionID != "" {
			return nativeRejection(hubapi.NativePublicInvalid("Current run events require a positive sequence"), "execution_validation", "data.sequence")
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
	if data.Sequence < 0 || data.Identity == nil || !ValidExecutionName(data.Identity.Role) || !ValidExecutionName(data.Identity.Backend) || !ValidExecutionName(data.Identity.Model) {
		return nativeRejection(hubapi.NativePublicInvalid("Ordered events require a positive sequence and bounded execution identity"), "execution_validation", "data")
	}
	if data.Handoff == nil {
		if eventType == "run.checkpointed" {
			return nativeRejection(hubapi.NativePublicInvalid("Ordered checkpoints require a structured handoff"), "execution_validation", "data.handoff")
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
		return nativeRejection(hubapi.NativePublicInvalid("Checkpoint has an unsupported recovery or effect state"), "execution_validation", "data.handoff")
	}
	if p := c.Publication; p != nil && (c.ExternalEffect != "pr_create" || c.EffectState != "confirmed" || c.Change == nil || *c.Change != p.SourceVersion || c.HeadSHA != p.HeadSHA || c.EffectID != p.EffectID("pr_create") ||
		!hubapi.ValidID(p.ChangeID, "change") || !hubapi.ValidID(p.VersionID, "version") || !hubapi.ValidID(p.PolicyID, "policy") || p.SourceAttemptID != "" && !hubapi.ValidID(p.SourceAttemptID, "attempt") ||
		len(p.BaseRef) > 255 || len(p.Branch) > 255 || strings.ContainsAny(p.BaseRef+p.Branch, " \t\r\n~^:?*[\\") || strings.Contains(p.BaseRef+p.Branch, "..")) {
		return nativeRejection(hubapi.NativeInvalid("PR publication requires its exact confirmed checkpoint and bounded delivery identity"), "execution_validation", "data.handoff.publication")
	}
	if c.HeadSHA != "" && !hubapi.ValidCommitID(c.HeadSHA) || c.ExpectedHeadSHA != "" && !hubapi.ValidCommitID(c.ExpectedHeadSHA) || c.WorkspaceDigest != "" && !hubapi.ValidCommitID(c.WorkspaceDigest) {
		return nativeRejection(hubapi.NativePublicInvalid("Checkpoint heads must be commit IDs"), "execution_validation", "data.handoff")
	}
	if c.ExternalEffect == "none" && (c.EffectState != "none" || c.EffectID != "") || c.ExternalEffect != "none" && (c.EffectState == "none" || !hubapi.ValidID(c.EffectID, "effect")) {
		return nativeRejection(hubapi.NativePublicInvalid("External effects require a typed reconciliation identity and state"), "execution_validation", "data.handoff")
	}
	if (c.Storage == "git_ref") != (c.Ref != "" || c.CommitSHA != "" || c.TreeSHA != "" || c.BaseSHA != "") || c.Storage == "git_ref" &&
		(!ValidCheckpointRef(c.Ref) || !hubapi.ValidCommitID(c.CommitSHA) || !hubapi.ValidCommitID(c.TreeSHA) || c.BaseSHA != "" && !hubapi.ValidCommitID(c.BaseSHA)) {
		return nativeRejection(hubapi.NativePublicInvalid("Git checkpoints require a Detent checkpoint ref and commit, tree and base IDs"), "execution_validation", "data.handoff")
	}
	if c.Storage == "customer_store" && (len(data.ArtifactIDs) == 0 || c.Availability == "available") {
		return nativeRejection(hubapi.NativePublicInvalid("Customer checkpoint references require artifacts and independent availability verification"), "execution_validation", "data.handoff")
	}
	if c.Change != nil && (!hubapi.ValidID(c.Change.ChangeID, "change") || !hubapi.ValidID(c.Change.VersionID, "version") || !hubapi.ValidCommitID(c.Change.HeadSHA)) {
		return nativeRejection(hubapi.NativePublicInvalid("Change references require typed immutable version and head identities"), "execution_validation", "data.handoff")
	}
	return nil
}

func NativeRuntimeHistoryChanged(nativeRuntimeSession func(*tracker.NativeRuntimeObservation) int64, previous, observation *tracker.NativeRuntimeObservation) bool {
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

func (d AttemptDeps) ListNativeAttempts(c echo.Context) error {
	ctx := c.Request().Context()
	scope := hubscope.NativeRequestScope(c)
	params, err := d.ReadQuery(c)
	if err != nil {
		return d.HTTPError(c, err)
	}
	if params.Get("view") == "" && c.Request().Header.Get("X-Detent-Attempt-Detail") == "full" {
		params.Set("view", "detail")
	}
	result, err := d.ReadNativeAttemptPageTenant(ctx, scope, c.Param("item"), params, json.Marshal)
	if err != nil {
		return d.HTTPError(c, err)
	}
	return c.JSONBlob(http.StatusOK, result.Encoded)
}

type NativeAttemptPage struct {
	tracker.Page[tracker.NativeAttempt]
	Encoded []byte `json:"-"`
}

func (p NativeAttemptPage) CandidateBytes(encoded []byte, cursor string) int {
	separator := 0
	if len(p.Items) > 0 {
		separator = 1
	}
	return len(p.Encoded) + separator + len(encoded) + len(`],"next_cursor":"`) + len(cursor) + len("\"}\n")
}

func (p *NativeAttemptPage) AppendAttempt(attempt tracker.NativeAttempt, encoded []byte) {
	if len(p.Items) > 0 {
		p.Encoded = append(p.Encoded, ',')
	}
	p.Encoded = append(p.Encoded, encoded...)
	p.Items = append(p.Items, attempt)
}

func (p *NativeAttemptPage) Finish() {
	p.Encoded = append(p.Encoded, ']')
	if p.NextCursor != "" {
		p.Encoded = append(p.Encoded, `,"next_cursor":"`...)
		p.Encoded = append(p.Encoded, p.NextCursor...)
		p.Encoded = append(p.Encoded, '"')
	}
	p.Encoded = append(p.Encoded, '}', '\n')
}

func (d AttemptDeps) ReadAttempts(ctx context.Context, scope hubscope.NativeScope, item string, params url.Values) (tracker.Page[tracker.NativeAttempt], error) {
	result, err := d.ReadNativeAttemptPage(ctx, scope, item, params, json.Marshal)
	return result.Page, err
}

func (d AttemptDeps) ReadNativeAttemptPage(ctx context.Context, scope hubscope.NativeScope, item string, params url.Values, marshal func(any) ([]byte, error)) (NativeAttemptPage, error) {
	return d.ReadNativeAttemptPageTenant(ctx, scope, item, params, marshal)
}

func NativeAttemptAuthorityProjection(attempt tracker.NativeAttempt) tracker.NativeAttempt {
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

func NativeAttemptDisplayIdentity(attempt tracker.NativeAttempt) *tracker.NativeExecutionIdentity {
	if attempt.Runtime == nil {
		return NativeExecutionDisplayIdentity(attempt.Identity, nil)
	}
	return NativeExecutionDisplayIdentity(attempt.Identity, &attempt.Runtime.Identity)
}

func NativeRecordedAttemptIdentity(attempt leasestore.Attempt, runtime bool) (*tracker.NativeExecutionIdentity, bool) {
	if attempt.IdentitySequence == 0 {
		return nil, false
	}
	if !runtime {
		return NativeExecutionDisplayIdentity(attempt.Identity.Execution, nil), true
	}
	return NativeExecutionDisplayIdentity(attempt.Identity.Execution, &attempt.Identity.Runtime), true
}

func NativeExecutionDisplayIdentity(execution *tracker.NativeExecutionIdentity, runtime *agentidentity.Identity) *tracker.NativeExecutionIdentity {
	if execution == nil {
		return nil
	}
	identity := *execution
	identity.ReasoningEffort = ""
	if runtime != nil {
		identity.ReasoningEffort = runtime.ReasoningEffort.Value
		if runtime.ResolvedModel.Value != "" {
			identity.Model = runtime.ResolvedModel.Value
		} else if runtime.RequestedModel.Value != "" {
			identity.Model = runtime.RequestedModel.Value
		}
	}
	return &identity
}

func (d AttemptDeps) GetNativeAttempt(c echo.Context) error {
	if c.QueryParam("view") == "cleanup" {
		result, err := d.ReadNativeCleanupAttempt(c.Request().Context(), hubscope.NativeRequestScope(c), c.Param("item"), c.Param("attempt"))
		if err != nil {
			return d.HTTPError(c, err)
		}
		return c.JSON(http.StatusOK, result)
	}
	result, err := d.ReadNativeAttempt(c.Request().Context(), hubscope.NativeRequestScope(c), c.Param("item"), c.Param("attempt"))
	if err != nil {
		return d.HTTPError(c, err)
	}
	return d.ArchiveJSON(c, http.StatusOK, result)
}

func (d AttemptDeps) ReadNativeCleanupAttempt(ctx context.Context, scope hubscope.NativeScope, item, id string) (tracker.NativeAttempt, error) {
	if !scope.Credential.Runner.Valid() {
		return tracker.NativeAttempt{}, hubapi.NativeNotFound()
	}
	ctx = d.DetailContext(ctx)
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	repository, ok := d.Repository.(tenantstore.SnapshotRepository)
	if !ok {
		return tracker.NativeAttempt{}, hubapi.NativeRunEventStorageUnavailable()
	}
	var snapshot hubrecords.TenantAttemptSnapshot
	err := repository.ReadTenantSnapshot(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := d.AuthorizeAccess(ctx, r, scope, false); err != nil {
			return err
		}
		project, err := r.Project(ctx, scope.Project)
		if err != nil {
			return err
		}
		if project.DeletedAt != nil {
			return corestore.ErrNotFound
		}
		issue, err := r.WorkItem(ctx, scope.Project, tracker.NativeWorkItemID(item))
		if err != nil {
			return err
		}
		snapshot.Attempt, err = r.Attempt(ctx, id)
		if err != nil {
			return err
		}
		if snapshot.Attempt.IssueID != issue.ID {
			return corestore.ErrNotFound
		}
		snapshot.Lease, err = r.Lease(ctx, snapshot.Attempt.LeaseID)
		if err != nil {
			return err
		}
		if snapshot.Lease.IssueID != issue.ID || snapshot.Lease.FencingToken != snapshot.Attempt.FencingToken || snapshot.Lease.MachineID != string(scope.Credential.Runner.MachineID) || snapshot.Lease.ReleasedAt == nil {
			return corestore.ErrNotFound
		}
		runner, err := r.RunnerForLease(ctx, snapshot.Lease.LeaseID)
		if err != nil {
			return err
		}
		if runner.RunnerID != scope.Credential.Runner.RunnerID {
			return corestore.ErrNotFound
		}
		snapshot.Event, err = r.LatestRunEvent(ctx, scope.Project, id)
		if err != nil {
			return err
		}
		if snapshot.Event.WorkItemID != tracker.NativeWorkItemID(item) {
			return corestore.ErrNotFound
		}
		return hubrecords.ReadTenantAttemptPayload(ctx, r, &snapshot)
	})
	if err != nil {
		return tracker.NativeAttempt{}, hubapi.ProjectRecordError(err)
	}
	if d.Details() == nil {
		return tracker.NativeAttempt{}, hubapi.NativeRunEventStorageUnavailable()
	}
	data, err := d.ReadNativeAttemptRunTenant(ctx, snapshot)
	if err != nil {
		return tracker.NativeAttempt{}, err
	}
	if !NativeAttemptRunMatches(snapshot, data) || data.RunnerID != scope.Credential.Runner.RunnerID {
		return tracker.NativeAttempt{}, artifact.ErrIntegrity
	}
	if !d.CleanupRequired(data) || data.MachineID != scope.Credential.Runner.MachineID {
		return tracker.NativeAttempt{}, hubapi.NativeNotFound()
	}
	return tracker.NativeAttempt{
		NativeRunData: tracker.NativeRunData{AttemptID: data.AttemptID, RunID: data.RunID, LeaseID: data.LeaseID,
			FencingToken: data.FencingToken, PolicyID: data.PolicyID, Identity: data.Identity, MachineID: data.MachineID,
			SessionID: data.SessionID, RunnerID: data.RunnerID, Sequence: snapshot.Attempt.Sequence, Outcome: data.Outcome,
			Runtime: &tracker.NativeRuntimeObservation{LocalAttemptID: data.Runtime.LocalAttemptID, Generation: data.Runtime.Generation, WorkerLaunch: data.Runtime.WorkerLaunch}},
		Status: snapshot.Attempt.Status, StartedAt: snapshot.Attempt.StartedAt, ClaimReleasedAt: snapshot.Lease.ReleasedAt,
		CleanupAcknowledgedAt: snapshot.Attempt.CleanupAcknowledgedAt,
	}, nil
}

func (d AttemptDeps) ReadNativeAttempt(ctx context.Context, scope hubscope.NativeScope, item, id string) (tracker.NativeAttempt, error) {
	ctx = d.DetailContext(ctx)
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	return d.ReadNativeAttemptTenant(ctx, scope, item, id)
}

func NativeLeaseReleaseFailure(reason string, now time.Time) tracker.NativeTerminalFailure {
	failure := tracker.NativeTerminalFailure{ErrorClass: "lease_released", Stage: "lease_release", ObservedAt: now.UTC(),
		Error: "attempt lease was released before the attempt finished; release reason unavailable (lease expired, replaced or its runner was revoked)"}
	if ValidExecutionName(reason) {
		failure.Error = "attempt lease was released before the attempt finished; release reason: " + reason
	}
	return failure.Public()
}
