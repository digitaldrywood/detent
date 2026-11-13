package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
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

func (w *workspaceService) workspaceCreationSubject(ctx context.Context, r tenantstore.Reader, scope nativeScope, request workspaceRequest) (corestore.WorkItem, bool, error) {
	item := request.WorkItemID
	running := false
	if request.AttemptID != "" {
		attempt, err := r.Attempt(ctx, strings.TrimSpace(request.AttemptID))
		if err != nil {
			return corestore.WorkItem{}, false, projectRecordError(err)
		}
		subject, err := r.WorkItemByID(ctx, scope.project, attempt.IssueID)
		if err != nil {
			return corestore.WorkItem{}, false, projectRecordError(err)
		}
		if item != "" && item != string(subject.NativeID) {
			return corestore.WorkItem{}, false, nativePublicInvalid("attempt_id belongs to a different work item")
		}
		item, running = string(subject.NativeID), attempt.Status == "running"
	}
	subject, err := r.WorkItem(ctx, scope.project, tracker.NativeWorkItemID(item))
	if err != nil {
		return subject, false, projectRecordError(err)
	}
	if !scope.reporterOwns(tracker.Actor{PrincipalID: stringValue(subject.ActorPrincipalID)}) {
		return subject, false, nativeNotFound()
	}
	reader, ok := r.(postgres.WorkspaceReader)
	if !ok {
		return subject, false, workspaceUnavailable()
	}
	workspace, err := reader.IsWorkspaceWorkItem(ctx, scope.project, subject.NativeID)
	if err != nil {
		return subject, false, err
	}
	if workspace {
		return subject, false, nativeNotFound()
	}
	if running {
		for _, name := range request.Requires {
			if name == workspacesession.CapabilityTerminal {
				return subject, false, &nativeError{Code: "forbidden", Message: "A terminal is refused while the attempt is running", status: http.StatusForbidden, publicMessage: true}
			}
		}
	}
	if err := w.authorizeWorkspaceCreationRequires(ctx, r, scope, request.Requires); err != nil {
		return subject, false, err
	}
	return subject, running, nil
}

func (w *workspaceService) authorizeWorkspaceCreationRequires(ctx context.Context, r tenantstore.Reader, scope nativeScope, requires []string) error {
	terminal := false
	for _, name := range requires {
		terminal = terminal || name == workspacesession.CapabilityTerminal
	}
	if !terminal {
		return nil
	}
	if !w.config.Terminal.Enabled {
		return &nativeError{Code: "forbidden", Message: "Terminals are turned off for this organization", status: http.StatusForbidden, publicMessage: true}
	}
	if scope.credential.Hosted == nil {
		return &nativeError{Code: "forbidden", Message: "A terminal requires a hosted session", status: http.StatusForbidden, publicMessage: true}
	}
	grant, err := r.AccessProjectGrant(ctx, scope.credential.Hosted.Subject, scope.project)
	if errors.Is(err, corestore.ErrNotFound) || err == nil && !grant.ManageRunner {
		return &nativeError{Code: "forbidden", Message: "A terminal requires the project's runner grant", status: http.StatusForbidden, publicMessage: true}
	}
	if err != nil {
		return err
	}
	if w.config.Terminal.Isolation == workspacesession.IsolationUser && scope.credential.HostedRole != "owner" && scope.credential.HostedRole != "admin" {
		return &nativeError{Code: "forbidden", Message: "This organization runs terminals as the runner's own user, which only an owner or admin may open", status: http.StatusForbidden, publicMessage: true}
	}
	if scope.credential.Hosted.SupportReason != "" && w.config.Terminal.Isolation != workspacesession.IsolationContainer {
		return &nativeError{Code: "forbidden", Message: "A support session may open a terminal only at container isolation", status: http.StatusForbidden, publicMessage: true}
	}
	return nil
}

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
	service, err := s.requireWorkspaces()
	if err != nil {
		return nil, err
	}
	if s.config.TenantRepository == nil || s.config.AccessRepository == nil {
		return nil, workspaceUnavailable()
	}
	ctx = allowWorkspaceLabel(ctx)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
			return nil, err
		}
		var current postgres.WorkspaceSession
		var dispatchItem string
		err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
			if err := s.authorizeWorkItemRecordScope(ctx, r, scope); err != nil {
				return err
			}
			owner, ok := r.(postgres.WorkspaceReader)
			if !ok {
				return workspaceUnavailable()
			}
			var err error
			current, err = owner.WorkspaceSession(ctx, scope.project, id)
			if err != nil {
				return projectRecordError(err)
			}
			if err := workspaceReadableSubject(ctx, r, scope, current.SubjectWorkItemID); err != nil {
				return projectRecordError(err)
			}
			if workspacesession.Terminal(current.State) {
				return nil
			}
			item, err := owner.WorkspaceDispatchItem(ctx, scope.project, id)
			if errors.Is(err, corestore.ErrNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			if item.ClosedAt == nil {
				dispatchItem = string(item.WorkItemID)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		if workspacesession.Terminal(current.State) {
			return json.RawMessage(`{"deleted":true}`), nil
		}
		now := s.config.now().UTC().Truncate(time.Microsecond)
		closure, err := s.prepareWorkspaceIssueClose(ctx, scope, dispatchItem, now)
		if err != nil {
			return nil, err
		}
		authority, err := s.beginProjectRecordAuthority(ctx, scope)
		if err != nil {
			return nil, err
		}
		err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.LockAccessOrganization(ctx); err != nil {
				return err
			}
			if err := w.LockWorkItemProject(ctx, scope.project); err != nil {
				return err
			}
			if err := s.authorizeWorkItemRecordScope(ctx, w, scope); err != nil {
				return err
			}
			owner, ok := w.(postgres.WorkspaceWriter)
			if !ok {
				return workspaceUnavailable()
			}
			row, err := owner.WorkspaceSession(ctx, scope.project, id)
			if err != nil {
				return projectRecordError(err)
			}
			if row.Revision != current.Revision || row.LeaseID != current.LeaseID || row.FencingToken != current.FencingToken || row.RunnerID != current.RunnerID {
				return corestore.ErrSerializationFailure
			}
			if err := workspaceReadableSubject(ctx, w, scope, row.SubjectWorkItemID); err != nil {
				return projectRecordError(err)
			}
			item, err := owner.WorkspaceDispatchItem(ctx, scope.project, id)
			if err != nil && !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			if dispatchItem == "" && err == nil && item.ClosedAt == nil {
				return corestore.ErrSerializationFailure
			}
			if dispatchItem != "" && (err != nil || item.ClosedAt != nil || string(item.WorkItemID) != dispatchItem) {
				return corestore.ErrSerializationFailure
			}
			if err := closure.write(ctx, w); err != nil {
				return err
			}
			if dispatchItem != "" {
				if err := owner.CloseWorkspaceItem(ctx, scope.project, item.WorkItemID, now); err != nil {
					return err
				}
			}
			for !workspacesession.Terminal(row.State) {
				state := workspacesession.StateClosed
				if !workspacesession.Transition(row.State, state) {
					state = workspacesession.StateClosing
				}
				row, err = owner.UpdateWorkspaceSession(ctx, postgres.WorkspaceUpdate{OrganizationID: scope.organization, ProjectID: scope.project, ID: id, ExpectedRevision: row.Revision, ExpectedRunnerID: row.RunnerID, ExpectedLeaseID: row.LeaseID, ExpectedFencingToken: row.FencingToken, State: state, Reason: workspacesession.ReasonClosedByActor, RunnerID: row.RunnerID, MachineID: row.MachineID, LeaseID: row.LeaseID, FencingToken: row.FencingToken, HeadSHA: row.HeadSHA, Capabilities: row.Capabilities, Isolation: row.Isolation, Worktree: row.Worktree, MachineHostname: row.MachineHostname, WorktreePath: row.WorktreePath, ReadOnly: row.ReadOnly, OpenedAt: row.OpenedAt, LastActivityAt: row.LastActivityAt, LastHeartbeatAt: row.LastHeartbeatAt, RebindDeadline: row.RebindDeadline, UpdatedAt: now})
				if err != nil {
					return err
				}
			}
			if row.RunnerID != "" {
				if err := owner.EndWorkspaceOccupancy(ctx, postgres.WorkspaceOccupancyEnd{OrganizationID: scope.organization, ProjectID: scope.project, WorkspaceID: id, RunnerID: row.RunnerID, EndedAt: &now}); err != nil {
					return err
				}
			}
			return s.recordEntryAccessKeyAuditWith(ctx, w, entryKeyAuditFrom(ctx), http.StatusNoContent)
		})
		err = errors.Join(err, releaseWorkItemAuthority(authority))
		if errors.Is(err, corestore.ErrSerializationFailure) {
			continue
		}
		if err != nil {
			return nil, projectRecordError(err)
		}
		service.committed(ctx, workspaceRecord{ID: id, OrganizationID: scope.organization, ProjectID: scope.project, State: workspacesession.StateClosed, Reason: workspacesession.ReasonClosedByActor, RunnerID: current.RunnerID, MachineID: tracker.MachineID(current.MachineID), LeaseID: tracker.LeaseID(current.LeaseID), FencingToken: tracker.FencingToken(current.FencingToken)})
		return json.RawMessage(`{"deleted":true}`), nil
	}
}

func workspaceReadableSubject(ctx context.Context, r tenantstore.Reader, scope nativeScope, id string) error {
	row, err := r.WorkItem(ctx, scope.project, tracker.NativeWorkItemID(id))
	if err != nil {
		return err
	}
	if !scope.reporterOwns(tracker.Actor{PrincipalID: stringValue(row.ActorPrincipalID)}) {
		return corestore.ErrNotFound
	}
	return nil
}

func (w *workspaceService) presentWorkspaceRecord(ctx context.Context, r tenantstore.Reader, scope nativeScope, row postgres.WorkspaceSession) (workspacesession.Session, error) {
	record := workspaceTenantRecord(row)
	resource := record.resource()
	if !workspaceAuditReadable(scope, record) {
		return resource, nil
	}
	owner, ok := r.(postgres.RelayActionReader)
	if !ok {
		return resource, workspaceUnavailable()
	}
	rows, err := owner.RelaySessions(ctx, postgres.RelaySessionsInput{OrganizationID: scope.organization, ProjectID: scope.project, WorkspaceID: row.ID, Limit: workspaceListLimit})
	if err != nil {
		return resource, err
	}
	for _, session := range rows {
		resource.RelaySessions = append(resource.RelaySessions, workspacesession.RelaySession{ID: session.ID, WorkspaceID: session.WorkspaceID, PrincipalID: session.PrincipalID, Subject: session.Subject, ConnectionID: session.ConnectionID, Channels: session.Channels, OpenedAt: session.OpenedAt, ClosedAt: session.ClosedAt, CloseReason: session.CloseReason, BytesIn: session.BytesIn, BytesOut: session.BytesOut, SupportReason: session.SupportReason})
	}
	return resource, nil
}

func (s *Service) readWorkspaces(ctx context.Context, scope nativeScope, item, state string, limit int) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	service, err := s.requireWorkspaces()
	if err != nil {
		return nil, err
	}
	state, item = strings.TrimSpace(state), strings.TrimSpace(item)
	if state != "" && !workspacesession.ValidState(state) {
		return nil, nativePublicInvalid("state names no workspace state")
	}
	response := workspaceListResponse{Workspaces: make([]workspacesession.Session, 0)}
	repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return nil, workItemStorageUnavailable()
	}
	err = repository.ReadTenantSnapshot(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
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
		head, err := r.BoardSequence(ctx, scope.project)
		response.ChangeSequence = head.Sequence
		if err != nil {
			return err
		}
		if item != "" {
			if err := workspaceReadableSubject(ctx, r, scope, item); err != nil {
				return err
			}
		}
		owner, ok := r.(postgres.WorkspaceReader)
		if !ok {
			return workspaceUnavailable()
		}
		rows, err := owner.WorkspaceListing(ctx, scope.project, item, state, limit)
		if err != nil {
			return err
		}
		for _, row := range rows {
			if item == "" {
				if err := workspaceReadableSubject(ctx, r, scope, row.SubjectWorkItemID); errors.Is(err, corestore.ErrNotFound) {
					continue
				} else if err != nil {
					return err
				}
			}
			resource, err := service.presentWorkspaceRecord(ctx, r, scope, row)
			if err != nil {
				return err
			}
			response.Workspaces = append(response.Workspaces, resource)
		}
		return nil
	})
	if err != nil {
		return nil, projectRecordError(err)
	}
	return json.Marshal(response)
}

func (s *Service) readWorkspace(ctx context.Context, scope nativeScope, id string) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	service, err := s.requireWorkspaces()
	if err != nil {
		return nil, err
	}
	var resource workspacesession.Session
	var watermark int64
	err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
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
		owner, ok := r.(postgres.WorkspaceReader)
		if !ok {
			return workspaceUnavailable()
		}
		row, err := owner.WorkspaceSession(ctx, scope.project, strings.TrimSpace(id))
		if err != nil {
			return err
		}
		if err := workspaceReadableSubject(ctx, r, scope, row.SubjectWorkItemID); err != nil {
			return err
		}
		watermark, err = r.ProjectEventSequence(ctx, scope.project)
		if err != nil {
			return err
		}
		resource, err = service.presentWorkspaceRecord(ctx, r, scope, row)
		return err
	})
	if err != nil {
		return nil, projectRecordError(err)
	}
	return json.Marshal(struct {
		workspacesession.Session
		ChangeSequence int64 `json:"change_sequence"`
	}{resource, watermark})
}
