package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

// Project actions and their runs (decisions section 18.12).
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
// queued rather than absent, and every transition emits action_run.<status> on
// the project event stream with the resource as data -- the same rule section
// 18.1 applies to workspace.<state>, so a client observes a run it did not
// start by subscription and never by polling.

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
	WorkspaceID string `json:"workspace_id"`
}

// projectActionRunList is the run listing shape.
type projectActionRunList struct {
	Items []workspacesession.Run `json:"items"`
}

// projectActionRunReceipt answers a queued run. It carries the id and nothing
// else: the resource arrives on the project event stream, and a client that
// read it from the response as well would have two sources for one fact.
type projectActionRunReceipt struct {
	RunID string `json:"run_id"`
}

// actionKeybindingTaken reports a chord another action in the project already
// claims. It names the conflict, because the author's next move is to change
// one of the two rather than to retry.
func actionKeybindingTaken(keybinding, name string) error {
	return nativeInvalid("The keybinding " + keybinding + " is already used by " + name)
}

// actionRunReadOnly refuses a run on a workspace held read-only while its
// attempt runs. A command writes into the worktree, and the worktree is the
// one the model is editing (section 18.1's read-only rule, applied to a
// command rather than to a terminal).
func actionRunReadOnly() error {
	return &nativeError{
		Code: "read_only", Message: "A run is refused while the attempt is still using this worktree",
		status: http.StatusConflict,
	}
}

// actionRunCapabilityMissing refuses a run on a workspace whose runner does
// not serve the exec surface. It is phrased the way section 18.1's other
// surface refusals are: the workspace is real and readable, and this one
// surface is not available on it.
func actionRunCapabilityMissing() error {
	return &nativeError{
		Code: "capability_missing", Message: "The workspace does not serve project actions",
		status: http.StatusConflict,
	}
}

// listProjectActions implements GET {nativeBase}/actions.
func (s *Service) listProjectActions(c echo.Context) error {
	if err := validateNativeQuery(c.QueryParams()); err != nil {
		return s.nativeAPIError(c, err)
	}
	scope := nativeRequestScope(c)
	records, err := readProjectActions(c.Request().Context(), s.database.db, scope.organization, scope.project)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	response := projectActionList{Items: make([]workspacesession.Action, 0, len(records))}
	for _, record := range records {
		response.Items = append(response.Items, record.resource())
	}
	return c.JSON(http.StatusOK, response)
}

// createProjectAction implements POST {nativeBase}/actions.
func (s *Service) createProjectAction(c echo.Context) error {
	var request projectActionRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	return s.nativeMutationStatus(c, http.StatusCreated, request.Mutation, request,
		func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
			record := actionRecord{
				ID: newNativeID("action"), OrganizationID: scope.organization, ProjectID: scope.project,
				OpenPreview: request.OpenPreview, RunOnWorktreeCreation: request.RunOnWorktreeCreation,
				CreatedBy: scope.actor().PrincipalID, Revision: 1, CreatedAt: now, UpdatedAt: now,
			}
			var err error
			if record.Name, err = workspacesession.NormalizeActionName(request.Name); err != nil {
				return nil, nativeInvalid(err.Error())
			}
			if record.Command, err = workspacesession.NormalizeActionCommand(request.Command); err != nil {
				return nil, nativeInvalid(err.Error())
			}
			if record.Keybinding, err = workspacesession.NormalizeActionKeybinding(request.Keybinding); err != nil {
				return nil, nativeInvalid(err.Error())
			}
			if record.Icon, err = workspacesession.NormalizeActionIcon(request.Icon); err != nil {
				return nil, nativeInvalid(err.Error())
			}
			if record.PreviewURL, err = workspacesession.NormalizeActionPreviewURL(request.PreviewURL); err != nil {
				return nil, nativeInvalid(err.Error())
			}
			count, err := countProjectActions(ctx, tx, scope)
			if err != nil {
				return nil, err
			}
			if count >= workspacesession.MaxActionsPerProject {
				return nil, nativeInvalid("A project may hold at most " +
					strconv.Itoa(workspacesession.MaxActionsPerProject) + " actions")
			}
			// The chord is checked here as well as by the migration's partial
			// unique index. The check is what produces an answer the author
			// can act on -- it names the action holding the chord -- and the
			// index is the backstop that keeps two concurrent creates from
			// both passing it.
			if owner, found, err := projectActionKeybindingOwner(ctx, tx, scope, record.Keybinding, record.ID); err != nil {
				return nil, err
			} else if found {
				return nil, actionKeybindingTaken(record.Keybinding, owner.Name)
			}
			if err := insertProjectAction(ctx, tx, record); err != nil {
				return nil, err
			}
			return record.resource(), nil
		})
}

// patchProjectAction implements PATCH {nativeBase}/actions/:action.
func (s *Service) patchProjectAction(c echo.Context) error {
	var request projectActionPatch
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	return s.nativeMutation(c, request.Mutation, request,
		func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
			record, err := readProjectAction(ctx, tx, scope, c.Param("action"))
			if errors.Is(err, sql.ErrNoRows) {
				return nil, nativeNotFound()
			}
			if err != nil {
				return nil, err
			}
			if request.ExpectedRevision <= 0 {
				return nil, nativeInvalid("Expected revision must be positive")
			}
			if record.Revision != int64(request.ExpectedRevision) {
				return nil, nativeConflict(tracker.Revision(record.Revision))
			}
			if request.Name != nil {
				if record.Name, err = workspacesession.NormalizeActionName(*request.Name); err != nil {
					return nil, nativeInvalid(err.Error())
				}
			}
			if request.Command != nil {
				if record.Command, err = workspacesession.NormalizeActionCommand(*request.Command); err != nil {
					return nil, nativeInvalid(err.Error())
				}
			}
			if request.Keybinding != nil {
				if record.Keybinding, err = workspacesession.NormalizeActionKeybinding(*request.Keybinding); err != nil {
					return nil, nativeInvalid(err.Error())
				}
			}
			if request.Icon != nil {
				if record.Icon, err = workspacesession.NormalizeActionIcon(*request.Icon); err != nil {
					return nil, nativeInvalid(err.Error())
				}
			}
			if request.PreviewURL != nil {
				if record.PreviewURL, err = workspacesession.NormalizeActionPreviewURL(*request.PreviewURL); err != nil {
					return nil, nativeInvalid(err.Error())
				}
			}
			if request.OpenPreview != nil {
				record.OpenPreview = *request.OpenPreview
			}
			if request.RunOnWorktreeCreation != nil {
				record.RunOnWorktreeCreation = *request.RunOnWorktreeCreation
			}
			if owner, found, err := projectActionKeybindingOwner(ctx, tx, scope, record.Keybinding, record.ID); err != nil {
				return nil, err
			} else if found {
				return nil, actionKeybindingTaken(record.Keybinding, owner.Name)
			}
			expected := record.Revision
			record.Revision++
			record.UpdatedAt = now
			if err := updateProjectActionRow(ctx, tx, record, expected); err != nil {
				return nil, err
			}
			return record.resource(), nil
		})
}

// deleteProjectAction implements DELETE {nativeBase}/actions/:action. Its runs
// go with it, which the migration declares with ON DELETE CASCADE.
func (s *Service) deleteProjectAction(c echo.Context) error {
	scope := nativeRequestScope(c)
	ctx := c.Request().Context()
	err := s.hubTransact(ctx, func(tx *sql.Tx, _ time.Time) error {
		if err := s.recheckHostedMutation(ctx, tx, scope); err != nil {
			return err
		}
		return deleteProjectActionRow(ctx, tx, scope, c.Param("action"))
	})
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
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	service, err := s.requireWorkspaces()
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	var queued actionRunRecord
	err = s.nativeMutationStatus(c, http.StatusAccepted, request.Mutation, request,
		func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
			run, err := service.queueActionRun(ctx, tx, scope, c.Param("action"), request, now)
			if err != nil {
				return nil, err
			}
			queued = run
			return projectActionRunReceipt{RunID: run.ID}, nil
		})
	if queued.ID != "" {
		// The wake happens after the commit, never inside it: a subscriber
		// woken by an uncommitted write would read nothing and sleep again.
		service.notifyRun(queued)
	}
	return err
}

// queueActionRun validates the request and writes the queued row together with
// its action_run.queued event.
func (w *workspaceService) queueActionRun(ctx context.Context, tx *sql.Tx, scope nativeScope, actionID string, request projectActionRunRequest, now time.Time) (actionRunRecord, error) {
	action, err := readProjectAction(ctx, tx, scope, actionID)
	if errors.Is(err, sql.ErrNoRows) {
		return actionRunRecord{}, nativeNotFound()
	}
	if err != nil {
		return actionRunRecord{}, err
	}
	workspace, err := w.readWorkspaceForActor(ctx, tx, scope, strings.TrimSpace(request.WorkspaceID))
	if err != nil {
		return actionRunRecord{}, err
	}
	if !workspacesession.Bound(workspace.State) {
		return actionRunRecord{}, nativeStaleExecution("The workspace is not bound to a runner")
	}
	if workspace.ReadOnly {
		return actionRunRecord{}, actionRunReadOnly()
	}
	if workspace.Capabilities == nil || !workspace.Capabilities.Has(workspacesession.CapabilityExec) {
		return actionRunRecord{}, actionRunCapabilityMissing()
	}
	run := actionRunRecord{
		ID: newNativeID("actionrun"), ActionID: action.ID, OrganizationID: scope.organization,
		ProjectID: scope.project, WorkspaceID: workspace.ID,
		// The command is snapshotted onto the row. An action edited afterwards
		// must not rewrite the history of what already ran, which is what
		// reading the command through action_id would do.
		Command: action.Command, Status: workspacesession.RunQueued,
		CreatedBy: scope.actor().PrincipalID, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := insertProjectActionRun(ctx, tx, run); err != nil {
		return actionRunRecord{}, err
	}
	if _, err := appendProjectEvent(ctx, tx, run.OrganizationID, run.ProjectID,
		workspacesession.RunEventType(run.Status), run.ID, run.resource(), now); err != nil {
		return actionRunRecord{}, err
	}
	w.logger.Info("action_run.queued", "run_id", run.ID, "action_id", run.ActionID,
		"workspace_id", run.WorkspaceID, "created_by", run.CreatedBy)
	return run, nil
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
	if err := validateNativeQuery(c.QueryParams()); err != nil {
		return s.nativeAPIError(c, err)
	}
	limit, err := parsePageLimit(c.QueryParam("limit"))
	if err != nil {
		return s.nativeAPIError(c, nativeInvalid("Page limit is invalid"))
	}
	scope := nativeRequestScope(c)
	ctx := c.Request().Context()
	action, err := readProjectAction(ctx, s.database.db, scope, c.Param("action"))
	if errors.Is(err, sql.ErrNoRows) {
		return s.nativeAPIError(c, nativeNotFound())
	}
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	records, err := readProjectActionRuns(ctx, s.database.db, scope, action.ID, limit)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	response := projectActionRunList{Items: make([]workspacesession.Run, 0, len(records))}
	for _, record := range records {
		response.Items = append(response.Items, record.resource())
	}
	return c.JSON(http.StatusOK, response)
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
	if err := validateNativeQuery(c.QueryParams()); err != nil {
		return actionRunRecord{}, err
	}
	scope := nativeRequestScope(c)
	ctx := c.Request().Context()
	if _, err := readProjectAction(ctx, s.database.db, scope, c.Param("action")); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return actionRunRecord{}, nativeNotFound()
		}
		return actionRunRecord{}, err
	}
	run, err := readProjectActionRun(ctx, s.database.db, scope, c.Param("action"), c.Param("run"))
	if errors.Is(err, sql.ErrNoRows) {
		return actionRunRecord{}, nativeNotFound()
	}
	if err != nil {
		return actionRunRecord{}, err
	}
	return run, nil
}

// sweepActionRuns fails every run left queued or running on a workspace that
// has ended (decisions section 18.12).
//
// The relay covers the runs it started: every path that tears a stream or a
// connection down writes a terminal row. A run the runner started itself for
// run_on_worktree_creation has no stream and no room entry, so the relay knows
// nothing about it, and its whole record is the report the runner sends. A
// crashed runner, a hub restart, or any refusal the runner does not retry
// would leave that row running for ever -- which is the one outcome section
// 18.12 exists to prevent, so it is swept rather than left to a retry the hub
// cannot see.
//
// It keys on the workspace's terminal state and never on a run's age. That is
// what makes it exact instead of a heuristic: a terminal workspace holds no
// runner and no worktree, so there is no live run it could be racing.
func (w *workspaceService) sweepActionRuns(ctx context.Context) {
	var failed []actionRunRecord
	err := w.server.hubTransact(ctx, func(tx *sql.Tx, now time.Time) error {
		orphaned, err := readOrphanedActionRuns(ctx, tx, actionRunSweepBatch)
		if err != nil {
			return err
		}
		for _, entry := range orphaned {
			run := entry.run
			run.Status = workspacesession.RunFailed
			// The workspace's own reason, not a blanket one: the relay's
			// teardown reads the same column, and the two paths race for a
			// workspace that ends while a run is still on it. Agreeing means
			// the answer an operator reads does not depend on which got there
			// first.
			run.Reason = actionRunReasonFor(entry.workspaceReason)
			run.FinishedAt = &now
			if run.StartedAt == nil {
				// A run the workspace ended before anything started it never
				// ran, and a finished_at with no started_at would read as a
				// run of negative length.
				run.StartedAt = &now
			}
			moved, err := applyActionRunStatus(ctx, tx, run, now)
			if err != nil {
				return err
			}
			failed = append(failed, moved)
		}
		return nil
	})
	if err != nil {
		w.logger.Warn("action_run.sweep_failed", "error", err)
		return
	}
	for _, run := range failed {
		w.logger.Info("action_run.orphaned", "run_id", run.ID, "action_id", run.ActionID,
			"workspace_id", run.WorkspaceID, "reason", run.Reason)
		w.notifyRun(run)
	}
}

// notifyRun wakes the project's stream subscribers after a run transition
// committed. It is the run half of committed(): call it after the transaction
// that appended the event, never inside it.
func (w *workspaceService) notifyRun(run actionRunRecord) {
	w.broker.notify(run.OrganizationID, run.ProjectID)
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

// validActionRunReason reports whether value names one of section 18.12's
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

// claimActionRun hands one queued run to exactly one executor (decisions
// section 18.12).
//
// A queued run has two of them. A person who opens the exec channel for it is
// the one the contract prefers, because only their stream carries the output as
// it is produced. The hub itself is the other: a run requested through the API
// with nobody watching would otherwise sit queued for ever, which is what the
// eighth dogfood run found, so the hub hands it to the runner already holding
// the workspace. Both can ask at the same moment.
//
// The claim is the whole answer to that race, and it is one write. The row
// moves out of queued and records who took it in the same transaction and under
// the same revision check every other transition uses, so the second claimant
// finds a row that is no longer queued and is refused. It is never two
// processes in one worktree, whichever order they arrived in.
func claimActionRun(ctx context.Context, tx *sql.Tx, run actionRunRecord, claimant string, now time.Time) (actionRunRecord, error) {
	if run.Status != workspacesession.RunQueued || run.ClaimedBy != "" {
		return actionRunRecord{}, errActionRunClaimed
	}
	run.Status = workspacesession.RunRunning
	run.ClaimedBy = claimant
	run.StartedAt = &now
	claimed, err := applyActionRunStatus(ctx, tx, run, now)
	if err != nil {
		// A revision conflict here is the other claimant committing first,
		// which is the race this function exists for rather than a failure the
		// caller should report as one.
		if isNativeConflict(err) {
			return actionRunRecord{}, errActionRunClaimed
		}
		return actionRunRecord{}, err
	}
	return claimed, nil
}

// applyActionRunStatus writes one run transition in the caller's transaction:
// the row, its bumped revision and the action_run.<status> event that carries
// the resource. It is the single exit the relay, the worker report and the
// workspace teardown share, so no caller has to remember the event.
func applyActionRunStatus(ctx context.Context, tx *sql.Tx, run actionRunRecord, now time.Time) (actionRunRecord, error) {
	expected := run.Revision
	run.Revision++
	run.UpdatedAt = now
	run.OutputArtifact = actionRunOutputPath(run)
	if err := updateProjectActionRunRow(ctx, tx, run, expected); err != nil {
		return run, err
	}
	if _, err := appendProjectEvent(ctx, tx, run.OrganizationID, run.ProjectID,
		workspacesession.RunEventType(run.Status), run.ID, run.resource(), now); err != nil {
		return run, err
	}
	return run, nil
}
