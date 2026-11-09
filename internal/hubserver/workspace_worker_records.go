package hubserver

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/actionstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func loadWorkspaceWorkerRecord(ctx context.Context, w workspaceWriter, scope nativeScope, id string, identity workspaceWorkerIdentity, now time.Time) (workspaceRecord, leasestore.Lease, error) {
	return readWorkspaceWorkerRecord(ctx, w, scope, id, identity, now, func(ctx context.Context, id string) (leasestore.Lease, error) {
		locker, ok := w.(leasestore.FencedLeaseLocker)
		if !ok {
			return leasestore.Lease{}, workspaceUnavailable()
		}
		return locker.LockLease(ctx, id)
	})
}

func readWorkspaceWorkerRecord(ctx context.Context, r workspaceReader, scope nativeScope, id string, identity workspaceWorkerIdentity, now time.Time, readLease func(context.Context, string) (leasestore.Lease, error)) (workspaceRecord, leasestore.Lease, error) {
	if workspacesession.ValidateID(id) != nil {
		return workspaceRecord{}, leasestore.Lease{}, nativeNotFound()
	}
	if identity.LeaseID == "" || identity.FencingToken <= 0 {
		return workspaceRecord{}, leasestore.Lease{}, nativePublicInvalid("A lease and fencing token are required")
	}
	if _, err := authorizeRunnerRepositoryReader(ctx, r, scope, now); err != nil {
		return workspaceRecord{}, leasestore.Lease{}, err
	}
	row, err := r.WorkspaceSession(ctx, scope.project, id)
	if err != nil {
		return workspaceRecord{}, leasestore.Lease{}, err
	}
	lease, err := readLease(ctx, string(identity.LeaseID))
	if errors.Is(err, tracker.ErrLeaseNotFound) {
		err = corestore.ErrNotFound
	}
	if err != nil {
		return workspaceRecord{}, lease, err
	}
	if lease.ReleasedAt != nil || lease.FencingToken != int64(identity.FencingToken) || !lease.ExpiresAt.After(now) {
		return workspaceRecord{}, lease, nativeStaleExecution("The workspace lease is no longer current")
	}
	if lease.MachineID != string(scope.credential.Runner.MachineID) {
		return workspaceRecord{}, lease, nativeNotFound()
	}
	item, err := r.WorkspaceDispatchItem(ctx, scope.project, id)
	if err != nil {
		return workspaceRecord{}, lease, err
	}
	issue, err := r.WorkItem(ctx, scope.project, tracker.NativeWorkItemID(item))
	if err != nil {
		return workspaceRecord{}, lease, err
	}
	if issue.ID != lease.IssueID {
		return workspaceRecord{}, lease, nativeStaleExecution("The lease does not hold this workspace's work item")
	}
	return workspaceFromRecord(row), lease, nil
}

func (s *Service) workspaceWorkerWrite(ctx context.Context, scope nativeScope, run func(workspaceWriter, time.Time) error) error {
	return projectRecordError(s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		owner, ok := w.(workspaceWriter)
		if !ok {
			return workspaceUnavailable()
		}
		return run(owner, s.config.now().UTC())
	}))
}

func (s *Service) bindWorkspaceRecord(c echo.Context) error {
	var request workspaceBindRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	service, err := s.requireWorkspaces()
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if !workspacesession.ValidIsolation(request.Isolation) {
		return s.nativeAPIError(c, nativePublicInvalid("isolation must be sandbox, container or user"))
	}
	ctx := c.Request().Context()
	scope := nativeRequestScope(c)
	var commands []actionstore.Record
	var creationPrepared bool
	err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		owner, ok := r.(workspaceReader)
		if !ok {
			return workspaceUnavailable()
		}
		now := s.config.now().UTC()
		record, _, err := readWorkspaceWorkerRecord(ctx, owner, scope, c.Param("workspace"), request.workspaceWorkerIdentity, now, owner.Lease)
		if err != nil {
			return err
		}
		candidate, target, err := service.bindCandidate(scope, record, request, now)
		if err != nil {
			return err
		}
		checkout, err := service.checkoutRecord(ctx, owner, candidate, now)
		if err != nil {
			return err
		}
		candidate.State = target
		if !createsWorktree(candidate, checkout) {
			return nil
		}
		records, err := owner.ProjectActions(ctx, scope.project)
		if err != nil {
			return err
		}
		for _, record := range records {
			if record.Action.RunOnWorktreeCreation {
				commands = append(commands, record)
			}
		}
		creationPrepared = true
		return nil
	})
	if err != nil {
		return s.nativeAPIError(c, projectRecordError(err))
	}
	var actions []workspacesession.Action
	for _, record := range commands {
		action, err := s.hydrateActionRecord(ctx, scope, record)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		actions = append(actions, action)
	}
	var bound workspaceRecord
	var checkout workspaceCheckout
	err = s.workspaceWorkerWrite(ctx, scope, func(w workspaceWriter, now time.Time) error {
		record, _, err := loadWorkspaceWorkerRecord(ctx, w, scope, c.Param("workspace"), request.workspaceWorkerIdentity, now)
		if err != nil {
			return err
		}
		bound, err = service.bindRecord(ctx, w, scope, record, request, now)
		if err != nil {
			return err
		}
		checkout, err = service.checkoutRecord(ctx, w, bound, now)
		if err != nil {
			return err
		}
		if createsWorktree(bound, checkout) {
			if !creationPrepared {
				return nativeStaleExecution("The workspace checkout changed")
			}
			current, err := w.ProjectActions(ctx, scope.project)
			if err != nil {
				return err
			}
			index := 0
			for _, action := range current {
				if !action.Action.RunOnWorktreeCreation {
					continue
				}
				if index >= len(commands) || action.Action.ID != commands[index].Action.ID || action.Action.Revision != commands[index].Action.Revision || action.Command != commands[index].Command {
					return nativeStaleExecution("The workspace creation actions changed")
				}
				index++
			}
			if index != len(commands) {
				return nativeStaleExecution("The workspace creation actions changed")
			}
		}
		return nil
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	response := workspaceBindResponse{Owner: bound.owner(), Checkout: checkout, Session: bound.resource()}
	if createsWorktree(bound, checkout) {
		response.Actions = actions
	}
	service.committed(ctx, bound)
	return c.JSON(http.StatusOK, response)
}

func (w *workspaceService) checkoutRecord(ctx context.Context, r workspaceReader, record workspaceRecord, now time.Time) (workspaceCheckout, error) {
	checkout := workspaceCheckout{WorkItemID: record.SubjectWorkItemID, AttemptID: record.AttemptID, Ref: record.Ref, HeadSHA: record.HeadSHA, Worktree: workspacesession.WorktreeFresh, ReadOnly: record.ReadOnly, Requires: record.Requires, IdleTimeoutSeconds: record.IdleTimeoutSeconds, HeartbeatSeconds: int(workspacesession.HeartbeatInterval / time.Second), ExpiresAt: record.ExpiresAt, Deny: w.config.FilesDeny}
	if record.AttemptID == "" {
		return checkout, nil
	}
	attempt, err := r.Attempt(ctx, record.AttemptID)
	if err != nil {
		return checkout, err
	}
	lease, err := r.Lease(ctx, attempt.LeaseID)
	if err != nil {
		return checkout, err
	}
	if lease.MachineID == string(record.MachineID) && (attempt.Status == "running" || now.Before(attempt.UpdatedAt.Add(w.config.RetainAfterRun))) {
		checkout.Worktree = workspacesession.WorktreeRetained
	}
	return checkout, nil
}

func (s *Service) heartbeatWorkspaceRecord(c echo.Context) error {
	var request workspaceHeartbeatRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	service, err := s.requireWorkspaces()
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if request.State != "" && !workspacesession.ValidState(request.State) || !workspacesession.ValidIsolation(request.Isolation) || !workspacesession.ValidWorktree(request.Worktree) || request.Reason != "" && !workspacesession.ValidReason(request.Reason) {
		return s.nativeAPIError(c, nativePublicInvalid("Invalid workspace heartbeat"))
	}
	ctx := c.Request().Context()
	scope := nativeRequestScope(c)
	var finish workspaceDispatchClose
	if workspacesession.Terminal(request.State) || request.Reason == workspacesession.ReasonWorktreeMissing {
		finish, err = s.prepareWorkspaceDispatchClose(ctx, scope, c.Param("workspace"))
		if err != nil {
			return s.nativeAPIError(c, err)
		}
	}
	var beat workspaceRecord
	err = s.workspaceWorkerWrite(ctx, scope, func(w workspaceWriter, now time.Time) error {
		record, lease, err := loadWorkspaceWorkerRecord(ctx, w, scope, c.Param("workspace"), request.workspaceWorkerIdentity, now)
		if err != nil {
			return err
		}
		if record.LeaseID != request.LeaseID || record.FencingToken != request.FencingToken || record.RunnerID != scope.credential.Runner.RunnerID {
			return nativeStaleExecution("The workspace owner changed")
		}
		beat, _, err = service.heartbeatRecord(ctx, w, record, request, now)
		if err != nil {
			return err
		}
		fence := leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}
		if workspacesession.Terminal(beat.State) {
			if finish != nil {
				if err := finish(w); err != nil {
					return err
				}
			}
			_, err = w.Release(ctx, fence, unbindReleaseReason(request.Reason, beat))
		} else {
			_, err = w.Renew(ctx, fence, workspacesession.LeaseTTL)
		}
		return err
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	service.committed(ctx, beat)
	return c.JSON(http.StatusOK, workspaceHeartbeatResponse{Session: beat.resource()})
}

func (s *Service) unbindWorkspaceRecord(c echo.Context) error {
	var request workspaceUnbindRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	service, err := s.requireWorkspaces()
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if request.Reason != "" && !workspacesession.ValidReason(request.Reason) {
		return s.nativeAPIError(c, nativePublicInvalid("reason names no workspace reason"))
	}
	ctx := c.Request().Context()
	scope := nativeRequestScope(c)
	finish, err := s.prepareWorkspaceDispatchClose(ctx, scope, c.Param("workspace"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	var ended workspaceRecord
	err = s.workspaceWorkerWrite(ctx, scope, func(w workspaceWriter, now time.Time) error {
		record, lease, err := loadWorkspaceWorkerRecord(ctx, w, scope, c.Param("workspace"), request.workspaceWorkerIdentity, now)
		if err != nil {
			return err
		}
		if record.LeaseID != request.LeaseID || record.FencingToken != request.FencingToken || record.RunnerID != scope.credential.Runner.RunnerID {
			return nativeStaleExecution("The workspace owner changed")
		}
		terminal := workspacesession.StateFailed
		if record.State == workspacesession.StateClosing || request.Reason == workspacesession.ReasonClosedByActor || request.Reason == workspacesession.ReasonExpired {
			terminal = workspacesession.StateClosed
		}
		ended, err = service.endWorkspaceRecord(ctx, w, record, terminal, request.Reason, now)
		if err != nil {
			return err
		}
		if err = finish(w); err != nil {
			return err
		}
		_, err = w.Release(ctx, leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, unbindReleaseReason(request.Reason, ended))
		return err
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	service.committed(ctx, ended)
	return c.NoContent(http.StatusNoContent)
}

func (s *Service) workspaceRecordForWorkItem(c echo.Context) error {
	if _, err := s.requireWorkspaces(); err != nil {
		return s.nativeAPIError(c, err)
	}
	identity, err := workerRelayIdentity(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	scope := nativeRequestScope(c)
	ctx := c.Request().Context()
	var record workspaceRecord
	err = s.workspaceWorkerWrite(ctx, scope, func(w workspaceWriter, now time.Time) error {
		row, err := w.WorkspaceSessionByItem(ctx, scope.project, c.Param("item"))
		if err != nil {
			return err
		}
		record, _, err = loadWorkspaceWorkerRecord(ctx, w, scope, row.ID, identity, now)
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		err = nativeNotFound()
	}
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, record.resource())
}

func (w *workspaceService) bindRecord(ctx context.Context, tx workspaceWriter, scope nativeScope, record workspaceRecord, request workspaceBindRequest, now time.Time) (workspaceRecord, error) {
	candidate, target, err := w.bindCandidate(scope, record, request, now)
	if err != nil {
		return record, err
	}
	return w.transitionRecord(ctx, tx, candidate, target, "", now)
}

func (w *workspaceService) bindCandidate(scope nativeScope, record workspaceRecord, request workspaceBindRequest, now time.Time) (workspaceRecord, string, error) {
	if record.State != workspacesession.StateRequested {
		return record, "", nativeStaleExecution("The workspace is not waiting for a runner")
	}
	runnerID := scope.credential.Runner.RunnerID
	rebinding := record.RebindDeadline != nil
	if rebinding {
		if now.After(*record.RebindDeadline) {
			return record, "", nativeStaleExecution("The workspace re-bind grace has passed")
		}
		if record.RunnerID != "" && record.RunnerID != runnerID {
			return record, "", nativeStaleExecution("The workspace belongs to another runner")
		}
	}
	if !request.Capabilities.Satisfies(record.Requires) {
		return record, "", nativePublicInvalid("The runner does not serve every required surface")
	}
	if !terminalIsolationAllowed(record.Requires, w.config.Terminal.Isolation, request.Isolation) {
		return record, "", workspaceIsolationRefused()
	}
	capabilities := request.Capabilities
	record.RunnerID = runnerID
	record.MachineID = scope.credential.Runner.MachineID
	record.LeaseID = request.LeaseID
	record.FencingToken = request.FencingToken
	record.Capabilities = &capabilities
	record.Isolation = request.Isolation
	heartbeat := now
	record.LastHeartbeatAt = &heartbeat
	if rebinding {
		return record, workspacesession.StateReady, nil
	}
	return record, workspacesession.StateStarting, nil
}

func (w *workspaceService) heartbeatRecord(ctx context.Context, tx workspaceWriter, record workspaceRecord, request workspaceHeartbeatRequest, now time.Time) (workspaceRecord, bool, error) {
	if workspacesession.Terminal(record.State) {
		return record, false, nativeStaleExecution("The workspace has ended")
	}
	isolation := record.Isolation
	if request.Isolation != "" {
		isolation = request.Isolation
	}
	if !terminalIsolationAllowed(record.Requires, w.config.Terminal.Isolation, isolation) {
		return record, false, workspaceIsolationRefused()
	}
	expected := record.Revision
	beat := now
	record.LastHeartbeatAt = &beat
	if request.HeadSHA != "" {
		record.HeadSHA = request.HeadSHA
	}
	if request.Worktree != "" {
		record.Worktree = request.Worktree
	}
	if request.Isolation != "" {
		record.Isolation = request.Isolation
	}
	if request.WorktreePath != "" {
		record.WorktreePath = request.WorktreePath
	}
	if request.MachineHostname != "" {
		record.MachineHostname = request.MachineHostname
	}
	capabilities := request.Capabilities
	record.Capabilities = &capabilities
	target := request.State
	if request.Reason == workspacesession.ReasonWorktreeMissing {
		target = workspacesession.StateFailed
	}
	if target != "" && target != record.State && workspacesession.Transition(record.State, target) {
		moved, err := w.transitionRecord(ctx, tx, record, target, request.Reason, now)
		return moved, true, err
	}
	if record.State == workspacesession.StateUnreachable {
		moved, err := w.transitionRecord(ctx, tx, record, workspacesession.StateReady, "", now)
		return moved, true, err
	}
	record.Revision++
	record.UpdatedAt = now
	if err := writeWorkspaceRecord(ctx, tx, record, expected); err != nil {
		return record, false, err
	}
	return record, false, nil
}
