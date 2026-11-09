package hubserver

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

// Operator endpoints for workspace sessions. The
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
	ChangeSequence int64                      `json:"change_sequence"`
	Workspaces     []workspacesession.Session `json:"workspaces"`
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

// relayedRequires drops the capabilities this hub never relays to a runner:
// diff and preview, which have no channel protocol. Requiring one of them would
// only keep every runner from claiming the workspace, while the surface itself
// is refused by the relay whatever the runner reports.
func relayedRequires(requires []string) []string {
	relayed := make([]string, 0, len(requires))
	for _, name := range requires {
		switch name {
		case workspacesession.CapabilityDiff, workspacesession.CapabilityPreview:
			continue
		}
		relayed = append(relayed, name)
	}
	return relayed
}

// createWorkspace implements POST {nativeBase}/workspaces.
func (s *Service) createWorkspace(c echo.Context) error {
	var request workspaceRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	value, err := s.commandCreateWorkspace(c.Request().Context(), nativeRequestScope(c), request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusCreated, value)
}

// openWorkspace validates the request, applies the limits and creates the
// workspace together with the work item that dispatches it.

// A workspace on a workspace item, or on a coordinator item, would be a
// surface onto a dispatch record rather than onto work.

// A workspace on an attempt that is still running is allowed with files
// and diff only, read-only, and refuses terminal until the attempt
// finishes, so a person cannot type into a worktree the model is editing.

// authorizeRequires applies the extra authority a requirement needs beyond
// write on the project. Only the terminal has one today: the grant's runners
// flag, because a terminal is the runner account's authority handed to a
// person.

// A worker or operator token is not a person, and a terminal is a
// person's surface.

// The isolation rule. `user` runs the shell as the runner's
// own account, with its credential store, its provider logins and its other
// checkouts in reach; it is allowed "only when the organization has set
// workspaces.terminal.isolation: user explicitly and the person is an owner
// or admin with the runners grant". `container` is the level a member with
// write and the runners grant may use, and it is the one the setting
// recommends.

// A support actor gets a terminal "only with a support reason recorded and
// only at container". The reason is recorded by the session
// itself; this is the level.

// workspaceAttemptItem resolves the work item an attempt belongs to inside the
// caller's scope, so an attempt from another project is simply not found.

// listWorkspaces implements GET {nativeBase}/workspaces.
func (s *Service) listWorkspaces(c echo.Context) error {
	if err := validateNativeQuery(c.QueryParams(), "work_item", "state"); err != nil {
		return s.nativeAPIError(c, err)
	}
	value, err := s.readWorkspaces(c.Request().Context(), nativeRequestScope(c), c.QueryParam("work_item"), c.QueryParam("state"), workspaceListLimit)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, value)
}

// getWorkspace implements GET {nativeBase}/workspaces/:workspace.
func (s *Service) getWorkspace(c echo.Context) error {
	value, err := s.readWorkspace(c.Request().Context(), nativeRequestScope(c), c.Param("workspace"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, value)
}

// deleteWorkspace implements DELETE {nativeBase}/workspaces/:workspace. It
// closes the workspace; closing one never deletes the attempt's artifacts.
func (s *Service) deleteWorkspace(c echo.Context) error {
	_, err := s.commandCloseWorkspace(c.Request().Context(), nativeRequestScope(c), c.Param("workspace"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

// presentWorkspace projects a record for one reader, attaching the relay
// session rows only for an owner or an admin. A reader who is
// neither sees the workspace and not who has been inside it.
func (w *workspaceService) presentWorkspace(ctx context.Context, q nativeQueryer, scope nativeScope, record workspaceRecord) workspacesession.Session {
	resource := record.resource()
	if !workspaceAuditReadable(scope, record) {
		return resource
	}
	var sessions []workspacesession.RelaySession
	err := w.server.workspaceRead(ctx, scope, func(r workspaceReader) error {
		rows, err := r.RecentRelaySessions(ctx, postgres.RelaySessionsInput{OrganizationID: scope.organization, ProjectID: scope.project, WorkspaceID: record.ID, Limit: workspaceRelaySessionPage})
		if err != nil {
			return err
		}
		for _, row := range rows {
			sessions = append(sessions, workspacesession.RelaySession{ID: row.ID, WorkspaceID: row.WorkspaceID, PrincipalID: row.PrincipalID, Subject: row.Subject, ConnectionID: row.ConnectionID, Channels: row.Channels, OpenedAt: row.OpenedAt, ClosedAt: row.ClosedAt, CloseReason: row.CloseReason, BytesIn: row.BytesIn, BytesOut: row.BytesOut, SupportReason: row.SupportReason})
		}
		return nil
	})
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
// The workspace session contract says owners and admins, and that is taken literally rather than
// widened to the workspace's creator. A row names who has been inside a
// worktree and what they moved through it, which is a different question from
// who opened it; the terminal contract widens the equivalent audience for a terminal recording
// to "the person who ran the session" in so many words, and the absence of that
// clause here is the contract, not an omission. A surface never widens an
// audience on its own.
func workspaceAuditReadable(scope nativeScope, _ workspaceRecord) bool {
	return scope.credential.HostedRole == "owner" || scope.credential.HostedRole == "admin"
}

func normalizeWorkspaceRequest(request workspaceRequest) (workspaceRequest, error) {
	requires, err := workspacesession.NormalizeRequires(request.Requires)
	if err != nil {
		return workspaceRequest{}, nativeInvalid(err.Error())
	}
	request.Requires = relayedRequires(requires)
	if len(request.Ref) > maxWorkspaceRefBytes {
		return workspaceRequest{}, nativePublicInvalid("ref is too long")
	}
	if request.WorkItemID == "" && request.AttemptID == "" {
		return workspaceRequest{}, nativePublicInvalid("A work item or an attempt is required")
	}
	if request.AttemptID != "" && !validNativeID(request.AttemptID, "attempt") {
		return workspaceRequest{}, nativePublicInvalid("attempt_id must be a typed attempt identifier")
	}
	return request, nil
}

func (s *Service) commandCloseWorkspace(ctx context.Context, scope nativeScope, id string) (json.RawMessage, error) {

	return s.closeWorkspaceRecord(ctx, scope, id)

}

func (s *Service) readWorkspaces(ctx context.Context, scope nativeScope, item, state string, limit int) (json.RawMessage, error) {

	return s.readWorkspaceRecords(ctx, scope, "", item, state, limit)

	// The listing follows the issue's read rule one row at a time, so
	// a reader who can see some of a project's issues sees exactly
	// the workspaces on those.

}

func (s *Service) readWorkspace(ctx context.Context, scope nativeScope, id string) (json.RawMessage, error) {

	return s.readWorkspaceRecords(ctx, scope, id, "", "", 1)

}
