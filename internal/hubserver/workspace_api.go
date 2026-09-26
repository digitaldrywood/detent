package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

// Operator endpoints for workspace sessions (decisions section 18.1). The
// actor needs write on the project; terminal in requires also needs the
// grant's runners flag.

// workspaceRequest is the body of POST /workspaces.
type workspaceRequest struct {
	tracker.Mutation
	// WorkItemID and AttemptID name what to open; at least one is required,
	// and naming both is only allowed when they agree. Ref narrows what the
	// runner checks out; empty means the runner's own branch for the issue,
	// falling back to the project's default branch, because the branch name
	// is the runner's construction and the hub does not hold it.
	WorkItemID string   `json:"work_item_id,omitempty"`
	AttemptID  string   `json:"attempt_id,omitempty"`
	Ref        string   `json:"ref,omitempty"`
	RunnerID   string   `json:"runner_id,omitempty"`
	Requires   []string `json:"requires,omitempty"`
}

// workspaceListResponse is the listing shape.
type workspaceListResponse struct {
	Workspaces []workspacesession.Session `json:"workspaces"`
}

// workspaceListLimit bounds one listing. The per-organization open cap is 20,
// so a page this size shows every open workspace of a project and a useful
// amount of its history.
const workspaceListLimit = 100

// maxWorkspaceRefBytes bounds a requested ref. Git's own limit is far smaller;
// this only stops a request from becoming a storage cost.
const maxWorkspaceRefBytes = 512

// workspaces returns the service, or the refusal a hub without one gives.
func (s *Service) requireWorkspaces() (*workspaceService, error) {
	if s.workspaces == nil {
		return nil, workspaceUnavailable()
	}
	return s.workspaces, nil
}

// createWorkspace implements POST {nativeBase}/workspaces.
func (s *Service) createWorkspace(c echo.Context) error {
	var request workspaceRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	service, err := s.requireWorkspaces()
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	requires, err := workspacesession.NormalizeRequires(request.Requires)
	if err != nil {
		return s.nativeAPIError(c, nativeInvalid(err.Error()))
	}
	request.Requires = requires
	if len(request.Ref) > maxWorkspaceRefBytes {
		return s.nativeAPIError(c, nativeInvalid("ref is too long"))
	}
	if request.WorkItemID == "" && request.AttemptID == "" {
		return s.nativeAPIError(c, nativeInvalid("A work item or an attempt is required"))
	}
	if request.AttemptID != "" && !validNativeID(request.AttemptID, "attempt") {
		return s.nativeAPIError(c, nativeInvalid("attempt_id must be a typed attempt identifier"))
	}
	var created workspaceRecord
	err = s.nativeMutationStatus(c, http.StatusCreated, request.Mutation, request,
		func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
			record, err := service.openWorkspace(ctx, tx, scope, request, now)
			if err != nil {
				return nil, err
			}
			created = record
			return record.resource(), nil
		})
	if created.ID != "" {
		service.committed(c.Request().Context(), created)
	}
	return err
}

// openWorkspace validates the request, applies the limits and creates the
// workspace together with the work item that dispatches it.
func (w *workspaceService) openWorkspace(ctx context.Context, tx *sql.Tx, scope nativeScope, request workspaceRequest, now time.Time) (workspaceRecord, error) {
	item := request.WorkItemID
	attemptID := strings.TrimSpace(request.AttemptID)
	if attemptID != "" {
		resolved, err := workspaceAttemptItem(ctx, tx, scope, attemptID)
		if err != nil {
			return workspaceRecord{}, err
		}
		if item != "" && item != resolved {
			return workspaceRecord{}, nativeInvalid("attempt_id belongs to a different work item")
		}
		item = resolved
	}
	issue, _, err := readNativeIssue(ctx, tx, scope, item)
	if err != nil {
		return workspaceRecord{}, err
	}
	// A workspace on a workspace item, or on a coordinator item, would be a
	// surface onto a dispatch record rather than onto work.
	if _, found, err := readWorkspaceItem(ctx, tx, string(issue.WorkItemID)); err != nil {
		return workspaceRecord{}, err
	} else if found {
		return workspaceRecord{}, nativeNotFound()
	}
	running := false
	if attemptID != "" {
		if running, err = workspaceAttemptRunning(ctx, tx, scope, attemptID); err != nil {
			return workspaceRecord{}, err
		}
	}
	// A workspace on an attempt that is still running is allowed with files
	// and diff only, read-only, and refuses terminal until the attempt
	// finishes, so a person cannot type into a worktree the model is editing.
	if running {
		for _, name := range request.Requires {
			if name == workspacesession.CapabilityTerminal {
				return workspaceRecord{}, &nativeError{
					Code: "forbidden", Message: "A terminal is refused while the attempt is running",
					status: http.StatusForbidden,
				}
			}
		}
	}
	if err := w.authorizeRequires(ctx, tx, scope, request.Requires); err != nil {
		return workspaceRecord{}, err
	}
	if attemptID != "" {
		if existing, found, err := readOpenWorkspaceForAttempt(ctx, tx, scope, attemptID); err != nil {
			return workspaceRecord{}, err
		} else if found {
			return workspaceRecord{}, workspaceExists(existing.ID)
		}
	}
	total, mine, err := countOpenWorkspaces(ctx, tx, scope.organization, scope.credential.ID)
	if err != nil {
		return workspaceRecord{}, err
	}
	if total >= w.config.PlanMaxOpen {
		return workspaceRecord{}, workspaceLimit("organization", w.config.PlanMaxOpen)
	}
	if mine >= w.config.PersonMaxOpen {
		return workspaceRecord{}, workspaceLimit("person", w.config.PersonMaxOpen)
	}
	ref := strings.TrimSpace(request.Ref)
	record := workspaceRecord{
		ID: workspacesession.NewID(), OrganizationID: scope.organization, ProjectID: scope.project,
		SubjectWorkItemID: string(issue.WorkItemID), AttemptID: attemptID, Ref: ref,
		State: workspacesession.StateRequested, Requires: request.Requires, ReadOnly: running,
		IdleTimeoutSeconds: w.config.idleTimeoutSeconds(),
		ExpiresAt:          now.Add(w.config.MaxLifetime),
		RequestedExpiresAt: now.Add(w.config.RequestTimeout),
		CreatedBy:          scope.credential.ID, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := insertWorkspace(ctx, tx, record); err != nil {
		return workspaceRecord{}, err
	}
	if _, err := w.ensureWorkspaceItem(ctx, tx, scope, record, now); err != nil {
		return workspaceRecord{}, err
	}
	w.logger.Info("workspace.requested", "workspace_id", record.ID, "work_item_id", record.SubjectWorkItemID,
		"attempt_id", record.AttemptID, "requires", strings.Join(record.Requires, ","), "read_only", record.ReadOnly)
	return record, nil
}

// authorizeRequires applies the extra authority a requirement needs beyond
// write on the project. Only the terminal has one today: the grant's runners
// flag, because a terminal is the runner account's authority handed to a
// person (section 18.3).
func (w *workspaceService) authorizeRequires(ctx context.Context, tx *sql.Tx, scope nativeScope, requires []string) error {
	terminal := false
	for _, name := range requires {
		if name == workspacesession.CapabilityTerminal {
			terminal = true
		}
	}
	if !terminal {
		return nil
	}
	if !w.config.Terminal.Enabled {
		return &nativeError{
			Code: "forbidden", Message: "Terminals are turned off for this organization",
			status: http.StatusForbidden,
		}
	}
	if scope.credential.Hosted == nil {
		// A worker or operator token is not a person, and a terminal is a
		// person's surface.
		return &nativeError{Code: "forbidden", Message: "A terminal requires a hosted session", status: http.StatusForbidden}
	}
	if !hostedRunnerGrants(ctx, tx, string(scope.organization), scope.credential.Hosted.Subject, []tracker.ProjectID{scope.project}) {
		return &nativeError{
			Code: "forbidden", Message: "A terminal requires the project's runner grant",
			status: http.StatusForbidden,
		}
	}
	// The isolation rule (section 18.3). `user` runs the shell as the runner's
	// own account, with its credential store, its provider logins and its other
	// checkouts in reach; it is allowed "only when the organization has set
	// workspaces.terminal.isolation: user explicitly and the person is an owner
	// or admin with the runners grant". `container` is the level a member with
	// write and the runners grant may use, and it is the one the setting
	// recommends.
	if w.config.Terminal.Isolation == workspacesession.IsolationUser &&
		scope.credential.HostedRole != "owner" && scope.credential.HostedRole != "admin" {
		return &nativeError{
			Code: "forbidden",
			Message: "This organization runs terminals as the runner's own user, " +
				"which only an owner or admin may open",
			status: http.StatusForbidden,
		}
	}
	// A support actor gets a terminal "only with a support reason recorded and
	// only at container" (section 18.3). The reason is recorded by the session
	// itself; this is the level.
	if scope.credential.Hosted.SupportReason != "" &&
		w.config.Terminal.Isolation != workspacesession.IsolationContainer {
		return &nativeError{
			Code:    "forbidden",
			Message: "A support session may open a terminal only at container isolation",
			status:  http.StatusForbidden,
		}
	}
	return nil
}

// workspaceAttemptItem resolves the work item an attempt belongs to inside the
// caller's scope, so an attempt from another project is simply not found.
func workspaceAttemptItem(ctx context.Context, query nativeQueryer, scope nativeScope, attemptID string) (string, error) {
	var item string
	err := query.QueryRowContext(ctx, `SELECT work_item_id FROM native_attempts
WHERE id = ? AND organization_id = ? AND project_id = ?`, attemptID, scope.organization, scope.project).Scan(&item)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nativeNotFound()
	}
	if err != nil {
		return "", err
	}
	return item, nil
}

// listWorkspaces implements GET {nativeBase}/workspaces.
func (s *Service) listWorkspaces(c echo.Context) error {
	service, err := s.requireWorkspaces()
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if err := validateNativeQuery(c.QueryParams(), "work_item", "state"); err != nil {
		return s.nativeAPIError(c, err)
	}
	state := strings.TrimSpace(c.QueryParam("state"))
	if state != "" && !workspacesession.ValidState(state) {
		return s.nativeAPIError(c, nativeInvalid("state names no workspace state"))
	}
	item := strings.TrimSpace(c.QueryParam("work_item"))
	scope := nativeRequestScope(c)
	ctx := c.Request().Context()
	if item != "" {
		if _, _, err := readNativeIssue(ctx, s.database.db, scope, item); err != nil {
			return s.nativeAPIError(c, err)
		}
	}
	records, err := listWorkspaceRows(ctx, s.database.db, scope, item, state, workspaceListLimit)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	response := workspaceListResponse{Workspaces: make([]workspacesession.Session, 0, len(records))}
	for _, record := range records {
		if item == "" {
			// The listing follows the issue's read rule one row at a time, so
			// a reader who can see some of a project's issues sees exactly
			// the workspaces on those.
			if _, _, err := readNativeIssue(ctx, s.database.db, scope, record.SubjectWorkItemID); err != nil {
				if isNativeNotFound(err) {
					continue
				}
				return s.nativeAPIError(c, err)
			}
		}
		response.Workspaces = append(response.Workspaces, service.presentWorkspace(ctx, scope, record))
	}
	return c.JSON(http.StatusOK, response)
}

// getWorkspace implements GET {nativeBase}/workspaces/:workspace.
func (s *Service) getWorkspace(c echo.Context) error {
	service, err := s.requireWorkspaces()
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	scope := nativeRequestScope(c)
	ctx := c.Request().Context()
	record, err := service.readWorkspaceForActor(ctx, s.database.db, scope, c.Param("workspace"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, service.presentWorkspace(ctx, scope, record))
}

// deleteWorkspace implements DELETE {nativeBase}/workspaces/:workspace. It
// closes the workspace; closing one never deletes the attempt's artifacts.
func (s *Service) deleteWorkspace(c echo.Context) error {
	service, err := s.requireWorkspaces()
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	scope := nativeRequestScope(c)
	ctx := c.Request().Context()
	var closed workspaceRecord
	err = s.hubTransact(ctx, func(tx *sql.Tx, now time.Time) error {
		if err := s.recheckHostedMutation(ctx, tx, scope); err != nil {
			return err
		}
		record, err := service.readWorkspaceForActor(ctx, tx, scope, c.Param("workspace"))
		if err != nil {
			return err
		}
		if workspacesession.Terminal(record.State) {
			closed = record
			return nil
		}
		closed, err = service.endWorkspace(ctx, tx, record, workspacesession.StateClosed, workspacesession.ReasonClosedByActor, now)
		return err
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	service.committed(ctx, closed)
	return c.NoContent(http.StatusNoContent)
}

// presentWorkspace projects a record for one reader, attaching the relay
// session rows only for an owner or an admin (section 18.1). A reader who is
// neither sees the workspace and not who has been inside it.
func (w *workspaceService) presentWorkspace(ctx context.Context, scope nativeScope, record workspaceRecord) workspacesession.Session {
	resource := record.resource()
	if !workspaceAuditReadable(scope, record) {
		return resource
	}
	sessions, err := readWorkspaceRelaySessions(ctx, w.server.database.db, record.ID)
	if err != nil {
		w.logger.Warn("workspace.relay_sessions_unavailable", "workspace_id", record.ID, "error", err)
		return resource
	}
	resource.RelaySessions = sessions
	return resource
}

// workspaceAuditReadable reports whether this actor may see the workspace's
// relay session rows.
//
// Section 18.1 says owners and admins, and that is taken literally rather than
// widened to the workspace's creator. A row names who has been inside a
// worktree and what they moved through it, which is a different question from
// who opened it; §18.3 widens the equivalent audience for a terminal recording
// to "the person who ran the session" in so many words, and the absence of that
// clause here is the contract, not an omission. A surface never widens an
// audience on its own.
func workspaceAuditReadable(scope nativeScope, _ workspaceRecord) bool {
	return scope.credential.HostedRole == "owner" || scope.credential.HostedRole == "admin"
}
