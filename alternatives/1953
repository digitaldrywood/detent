package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"

	"github.com/digitaldrywood/detent/internal/workpad"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type runDetailContextKey struct{}

type runDetailContext struct {
	mu           sync.Mutex
	store        attachment.DetailStore
	organization string
	reference    *attachment.ArchiveReference
	runs         map[string]tracker.NativeRunData
	cache        map[string]map[string]tracker.NativeRunData
	reuse        map[string]*runDetailReference
}

type runDetailReference struct {
	AttachmentID string `json:"attachment_id"`
	Organization string `json:"organization_id"`
	Entry        string `json:"entry"`
}

func newRunDetailReference(ref attachment.ArchiveReference, organization, entry string) *runDetailReference {
	return &runDetailReference{AttachmentID: "att_" + artifact.Digest([]byte(organization + "\n" + ref.Key))[:32], Organization: organization, Entry: entry}
}

type storedRun struct {
	tracker.NativeRunData
	Detail *runDetailReference `json:"detail,omitempty"`
}

func runDetailEntry(data tracker.NativeRunData) string {
	return data.AttemptID + ":" + strconv.FormatInt(data.Sequence, 10)
}

func (s *Service) runDetailStore() attachment.DetailStore {
	if s.config.RunDetails != nil {
		return s.config.RunDetails
	}
	if s.config.Hosted != nil && s.config.Hosted.SharedEntry != nil {
		return s.config.Hosted.SharedEntry.RunDetails
	}
	return nil
}

func (s *Service) runDetailContext(ctx context.Context) context.Context {
	ctx = s.contentContext(ctx)
	if ctx.Value(runDetailContextKey{}) != nil {
		return ctx
	}
	owner := s.runDetailStore()
	if owner == nil {
		return ctx
	}
	organization := ""
	if s.config.Hosted != nil {
		organization = s.config.Hosted.OrganizationID
	}
	return context.WithValue(ctx, runDetailContextKey{}, &runDetailContext{store: owner, organization: organization, cache: make(map[string]map[string]tracker.NativeRunData), reuse: make(map[string]*runDetailReference)})
}

func (s *Service) runDetailMiddleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		c.SetRequest(c.Request().WithContext(s.runDetailContext(c.Request().Context())))
		return next(c)
	}
}

func decodeRunDetail(ctx context.Context, q nativeQueryer, raw string, result *tracker.NativeRunData) error {
	if prepared := claimPrepared(ctx); prepared != nil && !prepared.hydrating {
		if value, ok := prepared.runs[raw]; ok {
			*result = value
			return nil
		}
		if claimWriting(ctx) {
			return errClaimHintStale
		}
		return &claimHydration{run: func(ctx context.Context, q nativeQueryer) error {
			var value tracker.NativeRunData
			if err := decodeRunDetail(ctx, q, raw, &value); err != nil {
				return err
			}
			if prepared.runs == nil {
				prepared.runs = make(map[string]tracker.NativeRunData)
			}
			prepared.runs[raw] = value
			return nil
		}}
	}

	var stored storedRun
	if err := decodeStorageJSON(ctx, []byte(raw), &stored); err != nil {
		return err
	}
	if stored.Detail == nil || explanationRead(ctx) {
		*result = stored.NativeRunData
		if explanationRead(ctx) {
			*result = explanationRun(*result)
		}
		return nil
	}
	runs, err := readRunDetailSegment(ctx, q, stored.Detail)
	if err != nil {
		return err
	}
	run, found := runs[stored.Detail.Entry]
	if !found || run.AttemptID != stored.AttemptID || run.RunID != stored.RunID || run.LeaseID != stored.LeaseID || run.FencingToken != stored.FencingToken || run.PolicyID != stored.PolicyID {
		return fmt.Errorf("match run detail entry: %w", artifact.ErrIntegrity)
	}
	run.MachineID, run.SessionID, run.RunnerID = stored.MachineID, stored.SessionID, stored.RunnerID
	run.Identity = stored.Identity
	run.Sequence, run.Outcome = stored.Sequence, stored.Outcome
	if stored.Sequence != runs[stored.Detail.Entry].Sequence {
		run.Handoff, run.ArtifactIDs = stored.Handoff, stored.ArtifactIDs
	}
	if stored.Outcome == "cancelled" && stored.TerminalFailure != nil {
		run.TerminalFailure = stored.TerminalFailure
	}
	*result = run
	return nil
}

func decodeRunHistory(ctx context.Context, q nativeQueryer, raw string, result *tracker.CollaborationData) error {
	resolved, err := resolveStorageJSON(ctx, []byte(raw))
	if err != nil {
		return err
	}
	raw = string(resolved)
	if err := json.Unmarshal([]byte(raw), result); err != nil {
		return err
	}
	if result.Run == nil {
		return nil
	}
	var envelope struct {
		Run json.RawMessage `json:"run"`
	}
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		return err
	}
	return decodeRunDetail(ctx, q, string(envelope.Run), result.Run)
}

func compactRun(data tracker.NativeRunData) tracker.NativeRunData {
	result := data
	result.Evidence, result.Usage = nil, nil
	result.Steps = nil
	result.CompletionBody = ""
	if data.Disposition != nil {
		disposition := *data.Disposition
		disposition.FinalSummary, disposition.BlockerEvidence = "", nil
		if len(data.Disposition.BlockerEvidence) > 0 {
			disposition.BlockerEvidence = []workpad.Blocker{{}}
			for _, blocker := range data.Disposition.BlockerEvidence {
				if blocker.Predicate != nil && blocker.Predicate.Type == workpad.PredicateIssueState {
					disposition.BlockerEvidence[0].Predicate = &workpad.Predicate{Type: workpad.PredicateIssueState}
					break
				}
			}
		}
		result.Disposition = &disposition
	}
	if data.TerminalFailure != nil {
		failure := data.TerminalFailure.Public()
		result.TerminalFailure = &failure
	}
	if data.Finalization != nil {
		finalization := data.Finalization.Public()
		result.Finalization = &finalization
	}
	if data.Runtime != nil {
		runtime := *data.Runtime
		runtime.Activity, runtime.REST, runtime.GitHub = nil, nil, nil
		if data.Runtime.Activity != nil {
			activity := *data.Runtime.Activity
			activity.Spans, activity.Sources, activity.Summary, activity.CoverageNotes = nil, nil, nil, nil
			runtime.Activity = &activity
		}

		runtime.Phases, runtime.Pipeline, runtime.WorkerLaunch = nil, nil, nil
		if runtime.Validation != nil {
			validation := *runtime.Validation
			validation.Output, validation.Command, validation.Pipeline, validation.Evidence = "", "", nil, nil
			runtime.Validation = &validation
		}
		if runtime.Landing != nil {
			landing := *runtime.Landing
			landing.Pipeline, landing.Packages, landing.SecurityAudit = nil, nil, nil
			if landing.CI != nil {
				ci := *landing.CI
				ci.RequiredChecks, ci.PendingChecks, ci.MissingChecks, ci.FailedChecks = nil, nil, nil, nil
				landing.CI = &ci
			}
			if landing.Gate != nil {
				gate := *landing.Gate
				gate.Output, gate.Command, gate.Pipeline, gate.Evidence = "", "", nil, nil
				landing.Gate = &gate
			}
			runtime.Landing = &landing
		}
		result.Runtime = &runtime
	}
	return result
}

func encodeRunDetail(ctx context.Context, data tracker.NativeRunData, history bool) (string, error) {
	owner, ok := ctx.Value(runDetailContextKey{}).(*runDetailContext)
	if !ok {
		return marshalNative(data)
	}
	if ref := owner.reuse[data.AttemptID]; ref != nil {
		compact := compactRun(data)
		if !history {
			compact.Handoff = nil
		}
		compact.TerminalFailure = data.TerminalFailure
		return marshalNative(storedRun{NativeRunData: compact, Detail: ref})
	}
	if owner.reference == nil {
		return marshalNative(data)
	}
	ref := newRunDetailReference(*owner.reference, owner.organization, runDetailEntry(data))
	if _, ok := owner.runs[ref.Entry]; !ok {
		return "", fmt.Errorf("find prepared run detail entry: %w", artifact.ErrIntegrity)
	}
	compact := compactRun(data)
	if !history {
		compact.Handoff = nil
	}
	if data.Outcome == "cancelled" {
		compact.TerminalFailure = data.TerminalFailure
	}
	return marshalNative(storedRun{NativeRunData: compact, Detail: ref})
}

func (s *Service) prepareRunDetail(ctx context.Context, scope nativeScope, item string, request tracker.NativeRunEvent) (context.Context, error) {
	ctx = s.runDetailContext(ctx)
	owner, ok := ctx.Value(runDetailContextKey{}).(*runDetailContext)
	if !ok {
		return ctx, nil
	}
	var previous string
	var stored storedRun
	previousErr := s.database.reader.QueryRowContext(ctx, "SELECT data_json FROM native_attempts WHERE organization_id=? AND project_id=? AND work_item_id=? AND id=?", scope.organization, scope.project, item, request.Data.AttemptID).Scan(&previous)
	if previousErr == nil {
		if err := json.Unmarshal([]byte(previous), &stored); err != nil {
			return ctx, err
		}
		if stored.Detail == nil || explanationRead(ctx) {
			return ctx, nil
		}
		var run tracker.NativeRunData
		if err := decodeRunDetail(ctx, s.database.reader, previous, &run); err != nil {
			return ctx, fmt.Errorf("decode previous run detail: %w", err)
		}
	} else if previousErr != sql.ErrNoRows {
		return ctx, previousErr
	}
	owner.organization = string(scope.organization)
	events := append(append([]tracker.NativeRunEvent{}, request.PriorEvents...), request)
	if len(events) > 8 {
		return ctx, nativeInvalid("Run detail batches are limited to eight events")
	}
	now, err := s.database.currentTime()
	if err != nil {
		return ctx, err
	}
	runs := make(map[string]tracker.NativeRunData, len(events))
	var prior *nativeAttemptUpdate
	for i, event := range events {
		if len(event.PriorEvents) != 0 && i < len(events)-1 || event.Data.AttemptID != request.Data.AttemptID || event.Data.LeaseID != request.Data.LeaseID || event.Data.FencingToken != request.Data.FencingToken || i < len(events)-1 && event.Type != "run.observed" {
			return ctx, nativeInvalid("Run detail batch must contain ordered observations for one execution")
		}
		issue, termination, err := s.validateNativeRunOwner(ctx, s.database.reader, scope, item, &event, now)
		if err != nil {
			return ctx, err
		}
		if event.Data.Sequence <= 0 {
			return ctx, nativeInvalid("Run detail requires an ordered execution")
		}
		update, err := prepareNativeAttempt(ctx, s.database.reader, scope, issue.WorkItemID, &event, now, termination, prior)
		if err != nil {
			return ctx, err
		}
		if update.duplicate {
			continue
		}
		prior = &update
		if termination && stored.Detail != nil {
			owner.reuse[event.Data.AttemptID] = stored.Detail
			continue
		}
		runs[runDetailEntry(event.Data)] = event.Data
	}
	if len(runs) == 0 {
		return ctx, nil
	}
	raw, err := json.Marshal(runs)
	if err != nil {
		return ctx, err
	}
	ref, err := owner.store.StoreDetail(ctx, string(scope.organization), raw)
	if err != nil {
		return ctx, fmt.Errorf("store run detail archive: %w", err)
	}
	if ref.Validate(string(scope.organization)) != nil {
		return ctx, fmt.Errorf("validate stored run detail reference: %w", artifact.ErrIntegrity)
	}
	owner.reference, owner.runs = &ref, runs
	owner.organization = string(scope.organization)
	owner.cache[newRunDetailReference(ref, string(scope.organization), "").AttachmentID] = runs
	return ctx, nil
}

func recordRunDetailReference(ctx context.Context, tx *sql.Tx, scope nativeScope, item tracker.NativeWorkItemID, data tracker.NativeRunData, now string) error {
	owner, ok := ctx.Value(runDetailContextKey{}).(*runDetailContext)
	if !ok {
		return nil
	}
	var ref attachment.ArchiveReference
	var id string
	if owner.reference != nil {
		ref = *owner.reference
		id = newRunDetailReference(ref, string(scope.organization), "").AttachmentID
	} else if previous := owner.reuse[data.AttemptID]; previous != nil {
		id = previous.AttachmentID
	} else {
		return nil
	}
	var role, phase, heartbeat, failureClass, failureStage, changeID, versionID string
	var local int64
	var generation uint64
	var settled, changed bool
	if data.Identity != nil {
		role = data.Identity.Role
	}
	if data.Runtime != nil {
		phase, local, generation = data.Runtime.Phase, data.Runtime.LocalAttemptID, data.Runtime.Generation
		heartbeat = formatHubTime(data.Runtime.HeartbeatAt)
	}
	if data.TerminalFailure != nil {
		failureClass, failureStage = data.TerminalFailure.ErrorClass, data.TerminalFailure.Stage
	}
	if data.Finalization != nil {
		settled, changed, changeID, versionID = data.Finalization.Settled, data.Finalization.Changed, data.Finalization.ChangeID, data.Finalization.VersionID
	}

	var err error
	if owner.reference != nil {
		_, err = tx.ExecContext(ctx, `INSERT INTO attachments(id,organization_id,project_id,uploader,name,content_type,size,sha256,created_at,kind,retention_class,object_key,uncompressed_size,codec,schema_version)
 VALUES(?,?,?,?,'archive.gz','application/gzip',?,?,?,'archive',?,?,?,?,?) ON CONFLICT DO NOTHING`, id, scope.organization, scope.project, scope.credential.ID, ref.CompressedSize, ref.SHA256, now, ref.Retention, ref.Key, ref.UncompressedSize, ref.Codec, ref.SchemaVersion)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO attachment_archive_references(organization_id,project_id,work_item_id,attachment_id) VALUES(?,?,?,?) ON CONFLICT DO NOTHING`, scope.organization, scope.project, item, id)
		if err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE native_attempts SET detail_attachment_id=?,policy_id=?,machine_id=?,session_id=?,runner_id=?,provider=?,model=?,effort=?,final_outcome=?,execution_role=?,runtime_phase=?,local_attempt_id=?,runtime_generation=?,heartbeat_at=?,failure_class=?,failure_stage=?,finalization_settled=?,finalization_changed=?,finalization_change_id=?,finalization_version_id=? WHERE id=?`, id, data.PolicyID, data.MachineID, data.SessionID, data.RunnerID, runProvider(data), runModel(data), runEffort(data), data.Outcome, role, phase, local, generation, heartbeat, failureClass, failureStage, settled, changed, changeID, versionID, data.AttemptID)
	if err != nil {
		return err
	}
	if data.Handoff != nil {
		_, err = tx.ExecContext(ctx, "UPDATE native_attempts SET checkpoint_attachment_id=?,checkpoint_preserves_source=? WHERE id=?", id, data.Handoff.PreservesSource(), data.AttemptID)
	}
	return err
}

func runProvider(data tracker.NativeRunData) string {
	if data.Runtime != nil && data.Runtime.Identity.Provider.Value != "" {
		return data.Runtime.Identity.Provider.Value
	}
	if data.Identity != nil {
		return data.Identity.Backend
	}
	return ""
}
func runModel(data tracker.NativeRunData) string {
	if data.Runtime != nil {
		if data.Runtime.Identity.ResolvedModel.Value != "" {
			return data.Runtime.Identity.ResolvedModel.Value
		}
		if data.Runtime.Identity.RequestedModel.Value != "" {
			return data.Runtime.Identity.RequestedModel.Value
		}
	}
	if data.Identity != nil {
		return data.Identity.Model
	}
	return ""
}
func runEffort(data tracker.NativeRunData) string {
	if data.Runtime != nil {
		return data.Runtime.Identity.ReasoningEffort.Value
	}
	return ""
}

type storedRunCheckpoint struct {
	*tracker.NativeCheckpoint
	Detail *runDetailReference `json:"detail,omitempty"`
}

func encodeRunCheckpoint(ctx context.Context, data tracker.NativeRunData) (string, error) {
	owner, ok := ctx.Value(runDetailContextKey{}).(*runDetailContext)
	if !ok || owner.reference == nil {
		return marshalNative(data.Handoff)
	}
	return marshalNative(storedRunCheckpoint{NativeCheckpoint: data.Handoff, Detail: newRunDetailReference(*owner.reference, owner.organization, runDetailEntry(data))})
}

func decodeRunCheckpoint(ctx context.Context, q nativeQueryer, raw string) (*tracker.NativeCheckpoint, error) {
	var stored storedRunCheckpoint
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		return nil, err
	}
	if stored.Detail == nil || explanationRead(ctx) {
		return stored.NativeCheckpoint, nil
	}
	runs, err := readRunDetailSegment(ctx, q, stored.Detail)
	if err != nil {
		return nil, err
	}
	run, ok := runs[stored.Detail.Entry]
	if !ok || runDetailEntry(run) != stored.Detail.Entry || run.Handoff == nil {
		return nil, fmt.Errorf("match run checkpoint detail entry: %w", artifact.ErrIntegrity)
	}
	return run.Handoff, nil
}

var errRunDetailStoreUnavailable = errors.New("run detail store unavailable in this context")

func readRunDetailSegment(ctx context.Context, q nativeQueryer, detail *runDetailReference) (map[string]tracker.NativeRunData, error) {
	owner, ok := ctx.Value(runDetailContextKey{}).(*runDetailContext)
	if !ok {
		return nil, errRunDetailStoreUnavailable
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if owner.organization != "" && owner.organization != detail.Organization {
		return nil, fmt.Errorf("match run detail organization: %w", artifact.ErrIntegrity)
	}
	if owner.reference != nil && detail.AttachmentID == newRunDetailReference(*owner.reference, owner.organization, "").AttachmentID {
		return owner.runs, nil
	}
	if runs := owner.cache[detail.AttachmentID]; runs != nil {
		return runs, nil
	}
	ref, err := scanAttachmentArchive(q.QueryRowContext(ctx, `SELECT object_key,sha256,size,uncompressed_size,codec,schema_version,kind,retention_class,storage_binding_id,customer_owned FROM attachments WHERE id=? AND organization_id=? AND kind='archive'`, detail.AttachmentID, detail.Organization))
	if err != nil {
		return nil, fmt.Errorf("read run detail attachment: %w", errors.Join(artifact.ErrIntegrity, err))
	}
	if err := ref.Validate(detail.Organization); err != nil {
		return nil, fmt.Errorf("validate run detail attachment: %w", errors.Join(artifact.ErrIntegrity, err))
	}
	raw, err := owner.store.ReadDetail(ctx, detail.Organization, ref)
	if err != nil {
		return nil, fmt.Errorf("read run detail archive: %w", err)
	}
	var runs map[string]tracker.NativeRunData
	if err := json.Unmarshal(raw, &runs); err != nil {
		return nil, fmt.Errorf("decode run detail: %w", err)
	}
	if len(owner.cache) >= 2 {
		clear(owner.cache)
	}
	owner.cache[detail.AttachmentID] = runs
	return runs, nil
}

func (s *Service) freshRunDetailContext(ctx context.Context) context.Context {
	return s.runDetailContext(s.contentContext(context.WithValue(ctx, runDetailContextKey{}, nil)))
}
