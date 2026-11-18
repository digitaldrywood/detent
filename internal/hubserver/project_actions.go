package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubcontent"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

// Project actions and their runs.
//
// An action is a command the project wrote down once: every conversation in
// the project offers the same actions, and a worktree opened tomorrow runs the
// same setup as one opened today. Authoring is a project write; running one is
// a project write too, because a run executes the project's command inside a
// worktree.
//
// A run is a row before it is a process. The row is written by POST .../runs
// and the relay's exec channel moves it through running and then exactly one
// of succeeded or failed, so a run that never reaches a runner is visible as
// queued rather than absent.

// projectActionList is the listing shape.
type projectActionList struct {
	Items []workspacesession.Action `json:"items"`
}

// projectActionRequest is the body of POST {nativeBase}/actions.
type projectActionRequest struct {
	tracker.Mutation
	Name                  string `json:"name"`
	Command               string `json:"command"`
	Keybinding            string `json:"keybinding,omitempty"`
	Icon                  string `json:"icon,omitempty"`
	PreviewURL            string `json:"preview_url,omitempty"`
	OpenPreview           bool   `json:"open_preview,omitempty"`
	RunOnWorktreeCreation bool   `json:"run_on_worktree_creation,omitempty"`
}

// projectActionPatch is the body of PATCH {nativeBase}/actions/:action.
//
// Every field is a pointer because "absent" and "cleared" are different
// requests and a plain string cannot hold both: a dialog that only changed the
// name must not silently drop the chord the author set last week, and an
// author who removed the chord must be able to say so. An absent field keeps
// what is stored; a present one replaces it, empty included.
type projectActionPatch struct {
	tracker.Mutation
	ExpectedRevision tracker.Revision `json:"expected_revision"`
	Name             *string          `json:"name,omitempty"`
	Command          *string          `json:"command,omitempty"`
	Keybinding       *string          `json:"keybinding,omitempty"`
	Icon             *string          `json:"icon,omitempty"`
	PreviewURL       *string          `json:"preview_url,omitempty"`

	OpenPreview           *bool `json:"open_preview,omitempty"`
	RunOnWorktreeCreation *bool `json:"run_on_worktree_creation,omitempty"`
}

// projectActionRunRequest is the body of POST {nativeBase}/actions/:action/runs.
type projectActionRunRequest struct {
	tracker.Mutation
	WorkspaceID            string           `json:"workspace_id"`
	ExpectedActionRevision tracker.Revision `json:"expected_action_revision,omitempty"`
}

// projectActionRunList is the run listing shape.
type projectActionRunList struct {
	ChangeSequence int64                  `json:"change_sequence"`
	Items          []workspacesession.Run `json:"items"`
}

// projectActionRunReceipt answers a queued run with its id; the run itself is
// read back through GET .../runs/:run.
type projectActionRunReceipt struct {
	RunID string `json:"run_id"`
}

func actionKeybindingTaken() error {
	return hubapi.NativePublicInvalid("The keybinding is already used by another action")
}

// actionRunReadOnly refuses a run on a workspace held read-only while its
// attempt runs. A command writes into the worktree, and the worktree is the
// one the model is editing (the workspace session contract's read-only rule, applied to a
// command rather than to a terminal).
func actionRunReadOnly() error {
	return &hubapi.NativeError{
		Code: "read_only", Message: "A run is refused while the attempt is still using this worktree",
		Status:        http.StatusConflict,
		PublicMessage: true,
	}
}

// actionRunCapabilityMissing refuses a run on a workspace whose runner does
// not serve the exec surface. It is phrased the way the workspace session contract's other
// surface refusals are: the workspace is real and readable, and this one
// surface is not available on it.
func actionRunCapabilityMissing() error {
	return &hubapi.NativeError{
		Code: "capability_missing", Message: "The workspace does not serve project actions",
		Status:        http.StatusConflict,
		PublicMessage: true,
	}
}

// listProjectActions implements GET {nativeBase}/actions.
func (s *Service) listProjectActions(c echo.Context) error {
	if err := hubapi.ValidateNativeQuery(c.QueryParams()); err != nil {
		return s.nativeAPIError(c, err)
	}
	value, err := s.readActions(c.Request().Context(), hubscope.NativeRequestScope(c))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, value)
}

// createProjectAction implements POST {nativeBase}/actions.
func (s *Service) createProjectAction(c echo.Context) error {
	var request projectActionRequest
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	value, err := s.commandCreateAction(c.Request().Context(), hubscope.NativeRequestScope(c), request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusCreated, value)
}

// patchProjectAction implements PATCH {nativeBase}/actions/:action.
func (s *Service) patchProjectAction(c echo.Context) error {
	var request projectActionPatch
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	value, err := s.commandPatchAction(c.Request().Context(), hubscope.NativeRequestScope(c), c.Param("action"), request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, value)
}

// deleteProjectAction implements DELETE {nativeBase}/actions/:action. Its runs
// go with it, which the migration declares with ON DELETE CASCADE.
func (s *Service) deleteProjectAction(c echo.Context) error {
	_, err := s.commandDeleteAction(c.Request().Context(), hubscope.NativeRequestScope(c), c.Param("action"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

// createProjectActionRun implements POST {nativeBase}/actions/:action/runs. It
// answers 202 with the run id: the row is queued, and the process starts when
// a client opens the exec channel for it.
func (s *Service) createProjectActionRun(c echo.Context) error {
	var request projectActionRunRequest
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	value, err := s.commandRunAction(c.Request().Context(), hubscope.NativeRequestScope(c), c.Param("action"), request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusAccepted, value)
}

// listProjectActionRuns implements GET {nativeBase}/actions/:action/runs,
// newest first.
//
// It is how a run is found without having seen its 202: a
// run-on-worktree-creation run has no client that asked for it, and "what has
// this action done lately" is the read a person wants anyway. The order and
// the filter are exactly project_action_runs_action_idx's (action_id,
// created_at DESC), so the listing is an index scan rather than a sort.
//
// Unlike the action listing this one is unbounded over time, so it takes the
// house page limit rather than answering whole. There is no cursor yet: the
// panel shows a recent window, and adding one later changes nothing a client
// already reads.
func (s *Service) listProjectActionRuns(c echo.Context) error {
	if err := hubapi.ValidateNativeQuery(c.QueryParams()); err != nil {
		return s.nativeAPIError(c, err)
	}
	limit, err := hubapi.ParsePageLimit(c.QueryParam("limit"))
	if err != nil {
		return s.nativeAPIError(c, hubapi.NativePublicInvalid("Page limit is invalid"))
	}
	value, err := s.readActionRuns(c.Request().Context(), hubscope.NativeRequestScope(c), c.Param("action"), limit)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, value)
}

// getProjectActionRun implements GET {nativeBase}/actions/:action/runs/:run.
func (s *Service) getProjectActionRun(c echo.Context) error {
	run, err := s.readActionRunForRequest(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, run.resource())
}

// getProjectActionRunOutput implements GET
// {nativeBase}/actions/:action/runs/:run/output, the path Run.OutputArtifact
// names. It answers text rather than JSON because the bytes are a log: a
// reader pipes them, greps them or scrolls them, and wrapping them in an
// envelope would only mean every client unwrapped them again.
func (s *Service) getProjectActionRunOutput(c echo.Context) error {
	run, err := s.readActionRunForRequest(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	// An empty body is a legitimate answer: a command that printed nothing
	// and exited zero has no output, and 404 would read as "no such run".
	return c.Blob(http.StatusOK, "text/plain; charset=utf-8", []byte(run.Output))
}

// readActionRunForRequest resolves the run the path names inside the caller's
// scope, so a run of another project is simply not found.
func (s *Service) readActionRunForRequest(c echo.Context) (actionRunRecord, error) {
	if err := hubapi.ValidateNativeQuery(c.QueryParams()); err != nil {
		return actionRunRecord{}, err
	}
	return s.readActionRun(c.Request().Context(), hubscope.NativeRequestScope(c), c.Param("action"), c.Param("run"))
}

// sweepActionRuns fails every run left queued or running on a workspace that
// has ended.
//
// The relay covers the runs it started: every path that tears a stream or a
// connection down writes a terminal row. A run the runner started itself for
// run_on_worktree_creation has no stream and no room entry, so the relay knows
// nothing about it, and its whole record is the report the runner sends. A
// crashed runner, a hub restart, or any refusal the runner does not retry
// would leave that row running for ever -- which is the one outcome the project actions contract
// exists to prevent, so it is swept rather than left to a retry the hub
// cannot see.
//
// It keys on the workspace's terminal state and never on a run's age. That is
// what makes it exact instead of a heuristic: a terminal workspace holds no
// runner and no worktree, so there is no live run it could be racing.
func (w *workspaceService) sweepActionRuns(ctx context.Context) {
	w.relay.mu.Lock()
	cursor := w.relay.actionSweep
	w.relay.mu.Unlock()
	if _, err := w.sweepActionPage(ctx, &cursor, actionRunSweepBatch); err != nil {
		w.logger.Warn("action_run.sweep_failed", "error", err)
	}
	w.relay.mu.Lock()
	w.relay.actionSweep = cursor
	w.relay.mu.Unlock()
}

func (w *workspaceService) sweepActionPage(ctx context.Context, cursor *postgres.MaintenanceCursor, limit int) (int, error) {
	organization := tracker.OrganizationID(w.server.appOrganizationID())
	var page []postgres.ActionRunMaintenanceRow
	err := w.server.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(reader tenantstore.Reader) error {
		owner, ok := reader.(postgres.RelayActionReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		var err error
		page, err = owner.ActiveActionRunPage(ctx, tracker.ProjectID(cursor.Project), cursor.Key, limit)
		return err
	})
	if err != nil {
		return 0, err
	}
	if len(page) == 0 {
		cursor.Project, cursor.Key = "", ""
		return 0, nil
	}
	for _, entry := range page {
		run := entry.ProjectActionRun
		if workspacesession.Terminal(entry.WorkspaceState) {
			err := w.server.config.TenantRepository.WriteTenant(ctx, organization, corestore.Background, func(writer tenantstore.Writer) error {
				owner, ok := writer.(postgres.RelayActionWriter)
				if !ok {
					return hubapi.NativeWorkItemStorageUnavailable()
				}
				_, err := owner.FailOrphanedActionRun(ctx, postgres.OrphanedActionRunInput{OrganizationID: organization, ProjectID: run.ProjectID, ID: run.ID, ExpectedRevision: run.Revision, Now: w.now()})
				return err
			})
			if err != nil && !errors.Is(err, corestore.ErrRevisionConflict) {
				return len(page), err
			}
		}
		cursor.Project, cursor.Key = string(run.ProjectID), run.ID
	}
	return len(page), nil
}

// actionRunReasonFor maps a workspace's own end reason onto the reason its
// runs failed.
//
// It is one function because two paths need the answer -- the relay's teardown
// and the sweep -- and a run that died once must not carry two different
// explanations depending on which of them recorded it. A lost lease is the one
// case worth separating: it says the hub gave up on the worktree, which is a
// different story for the person reading the run from a workspace somebody
// closed.
func actionRunReasonFor(workspaceReason string) string {
	if workspaceReason == workspacesession.ReasonLeaseLost {
		return workspacesession.RunReasonLeaseLost
	}
	return workspacesession.RunReasonWorkspaceEnd
}

// validActionRunReason reports whether value names one of the project actions contract's
// reasons a run failed without exiting. A run that exited carries no reason:
// its exit code is the whole story, which is why an empty reason is legal
// everywhere this is called and is checked by the caller rather than here.
func validActionRunReason(value string) bool {
	switch value {
	case workspacesession.RunReasonLeaseLost, workspacesession.RunReasonStreamClosed,
		workspacesession.RunReasonKilled, workspacesession.RunReasonWorkspaceEnd:
		return true
	}
	return false
}

// forwardRunStatus reports whether a run may move from one status to another.
//
// The lifecycle is queued → running → succeeded | failed, and a terminal
// status never moves again: a run that already failed because its stream
// closed must not be rewritten as succeeded by a report that was in flight
// when it did. A repeat of a non-terminal status is allowed, because a runner
// re-reporting running is a retry rather than a disagreement.
func forwardRunStatus(from, to string) bool {
	if !workspacesession.ValidRunStatus(to) || workspacesession.TerminalRunStatus(from) {
		return false
	}
	switch from {
	case workspacesession.RunQueued:
		return true
	case workspacesession.RunRunning:
		return to != workspacesession.RunQueued
	}
	return false
}

// errActionRunClaimed reports a run another executor already took. It is a
// sentinel rather than a boolean because both callers -- the relay's run frame
// and the hub's own dispatch -- have to tell "somebody else is running this"
// apart from "the write failed", and only the first of those is an ordinary
// outcome.
var errActionRunClaimed = errors.New("the action run is already claimed")

func (s *Service) readActions(ctx context.Context, scope hubscope.NativeScope) (json.RawMessage, error) {
	return s.readActionRecords(ctx, scope)
}

func (s *Service) readActionRuns(ctx context.Context, scope hubscope.NativeScope, actionID string, limit int) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	var rows []postgres.ActionRun
	var sequence int64
	repository, ok := s.config.AccessRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return nil, hubapi.NativeWorkItemStorageUnavailable()
	}
	err := repository.ReadTenantSnapshot(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, reader, scope, false); err != nil {
			return err
		}
		owner, ok := reader.(postgres.RelayActionReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		if _, err := owner.ProjectAction(ctx, scope.Project, actionID); err != nil {
			return err
		}
		var err error
		head, err := reader.BoardSequence(ctx, scope.Project)
		sequence = head.Sequence
		if err != nil {
			return err
		}
		rows, err = owner.RecentActionRuns(ctx, scope.Project, actionID, nil, limit)
		return err
	})
	if err != nil {
		return nil, hubapi.ProjectRecordError(err)
	}
	response := projectActionRunList{ChangeSequence: sequence, Items: make([]workspacesession.Run, 0, len(rows))}
	for _, row := range rows {
		record, err := s.actionRunRecord(ctx, row, false)
		if err != nil {
			return nil, err
		}
		response.Items = append(response.Items, record.resource())
	}
	return json.Marshal(response)
}

func (s *Service) readActionRun(ctx context.Context, scope hubscope.NativeScope, actionID, runID string) (actionRunRecord, error) {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	var row postgres.ActionRun
	err := s.accessRead(ctx, scope.Organization, func(reader tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, reader, scope, false); err != nil {
			return err
		}
		owner, ok := reader.(postgres.RelayActionReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		if _, err := owner.ProjectAction(ctx, scope.Project, actionID); err != nil {
			return err
		}
		var err error
		row, err = owner.ActionRun(ctx, postgres.ActionRunInput{OrganizationID: scope.Organization, ProjectID: scope.Project, ID: runID})
		return err
	})
	if err != nil {
		return actionRunRecord{}, hubapi.ProjectRecordError(err)
	}
	if row.ActionID != actionID {
		return actionRunRecord{}, hubapi.NativeNotFound()
	}
	return s.actionRunRecord(ctx, row, true)
}

func (s *Service) actionRunRecord(ctx context.Context, row postgres.ActionRun, output bool) (actionRunRecord, error) {
	command := attachment.ArchiveReference{Key: row.CommandObjectKey, SHA256: row.CommandSha256, CompressedSize: row.CommandBytes, UncompressedSize: row.CommandContentBytes, Codec: "gzip", SchemaVersion: 1, Kind: "archive", Retention: attachment.ArchiveRetention}
	if command.Validate(string(row.OrganizationID)) != nil || command.UncompressedSize > workspacesession.MaxExecCommandBytes || s.runDetailStore() == nil {
		return actionRunRecord{}, artifact.ErrIntegrity
	}
	raw, err := s.runDetailStore().ReadDetail(ctx, string(row.OrganizationID), command)
	if err != nil {
		return actionRunRecord{}, err
	}
	record := actionRunRecord{OrganizationID: row.OrganizationID, ProjectID: row.ProjectID, ID: row.ID, ActionID: row.ActionID, WorkspaceID: row.WorkspaceID, Command: string(raw), Status: row.Status, Reason: row.Reason, StartedAt: row.StartedAt, FinishedAt: row.FinishedAt, OutputBytes: row.OutputContentBytes, Truncated: row.Truncated, ClaimedBy: row.ClaimedBy, Revision: row.Revision, CreatedBy: row.CreatedBy, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
	if row.ExitCode.Valid {
		record.ExitCode = new(int(row.ExitCode.Int32))
	}
	if row.OutputObjectKey != nil {
		ref := attachment.ArchiveReference{Key: *row.OutputObjectKey, SHA256: row.OutputSha256, CompressedSize: row.OutputBytes, UncompressedSize: row.OutputContentBytes, Codec: "gzip", SchemaVersion: 1, Kind: "archive", Retention: attachment.ArchiveRetention}
		if ref.Validate(string(row.OrganizationID)) != nil || ref.UncompressedSize > workspacesession.MaxExecOutputBytes {
			return actionRunRecord{}, artifact.ErrIntegrity
		}
		if output {
			ref, err = s.relayArchiveReference(ctx, row.OrganizationID, ref)
			if err != nil {
				return actionRunRecord{}, err
			}
			raw, err := hubcontent.ReadChangeContent(s.contentContext(ctx), string(row.OrganizationID), ref)
			if err != nil {
				return actionRunRecord{}, err
			}
			record.Output = string(raw)
		}
	} else if row.OutputContentBytes != 0 || row.OutputBytes != 0 || row.OutputSha256 != "" {
		return actionRunRecord{}, artifact.ErrIntegrity
	}
	record.OutputArtifact = actionRunOutputPath(record)
	return record, nil
}

func (s *Service) queueActionRunRecord(ctx context.Context, scope hubscope.NativeScope, actionID string, request projectActionRunRequest) (json.RawMessage, error) {
	if strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 128 {
		return nil, hubapi.NativePublicInvalid("An idempotency key of at most 128 bytes is required")
	}
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
		return nil, err
	}
	operation := nativeOperation(scope, "POST", "/actions/"+url.PathEscape(actionID)+"/runs")
	hash, err := hubapi.CommandHash(request)
	if err != nil {
		return nil, err
	}
	content := eventstore.NewContent(s.runDetailStore())
	if receipt, found, err := s.workItemRecordReceipt(ctx, scope, operation, request.IdempotencyKey, hash); found || err != nil {
		if err != nil {
			return nil, err
		}
		return content.Receipt(ctx, receipt)
	}
	service, err := s.requireWorkspaces()
	if err != nil {
		return nil, err
	}
	id := hubapi.NewID("actionrun")
	raw, err := json.Marshal(projectActionRunReceipt{RunID: id})
	if err != nil {
		return nil, err
	}
	now := s.config.Clock().UTC()
	receipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.Organization, ActorID: scope.Credential.ID, Operation: operation, CommandKey: request.IdempotencyKey, RequestHash: hash, CreatedAt: now, ResponseReplayUntilNs: now.Add(receiptstore.ReplayRetention).UnixNano()}, raw)
	if err != nil {
		return nil, err
	}
	var replay *eventstore.Receipt
	err = s.accessWrite(ctx, scope.Organization, func(writer tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, writer, scope, true); err != nil {
			return err
		}
		if err := writer.LockWorkItemProject(ctx, scope.Project); err != nil {
			return err
		}
		stored, err := writer.Receipt(ctx, scope.Credential.ID, operation, request.IdempotencyKey)
		if err == nil {
			if stored.RequestHash != hash {
				return hubapi.NativeConflict(0)
			}
			replay = &stored
			return nil
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if err := hubapi.FromFeatureError(s.database.plansService().RequireFeatureRecord(ctx, writer, "collaboration", s.config.Clock())); err != nil {
			return err
		}
		owner, ok := writer.(postgres.RelayActionWriter)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		action, err := owner.ProjectAction(ctx, scope.Project, actionID)
		if err != nil {
			return err
		}
		if request.ExpectedActionRevision != 0 && int64(request.ExpectedActionRevision) != action.Action.Revision {
			return hubapi.NativeConflict(tracker.Revision(action.Action.Revision))
		}
		workspace, err := service.readRelayWorkspaceForActor(ctx, writer, scope, strings.TrimSpace(request.WorkspaceID))
		if err != nil {
			return err
		}
		if !workspacesession.Bound(workspace.State) {
			return hubapi.NativeStaleExecution("The workspace is not bound to a runner")
		}
		if workspace.ReadOnly {
			return actionRunReadOnly()
		}
		if workspace.Capabilities == nil || !workspace.Capabilities.Has(workspacesession.CapabilityExec) {
			return actionRunCapabilityMissing()
		}
		run, err := owner.CreateActionRun(ctx, postgres.CreateActionRunInput{OrganizationID: scope.Organization, ProjectID: scope.Project, ID: id, ActionID: actionID, WorkspaceID: workspace.ID, CreatedBy: scope.Actor().PrincipalID, CreatedAt: now})
		if err != nil {
			return err
		}
		if run.CommandObjectKey != action.Command.Key || run.CommandSha256 != action.Command.SHA256 {
			return artifact.ErrIntegrity
		}
		if err := s.retainRelayArchive(ctx, writer, scope.Organization, scope.Project, id, "project-action-run-command", action.Command, now); err != nil {
			return err
		}
		_, err = writer.PutReceipt(ctx, receipt)
		return err
	})
	if err != nil {
		return nil, hubapi.ProjectRecordError(err)
	}
	if replay != nil {
		return content.Receipt(ctx, *replay)
	}
	return raw, nil
}
