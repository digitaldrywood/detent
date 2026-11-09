package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

type workspaceReader interface {
	tenantstore.Reader
	postgres.WorkspaceLifecycleReader
	postgres.RelayActionLifecycleReader
}

type workspaceWriter interface {
	tenantstore.Writer
	workspaceReader
	postgres.WorkspaceWriter
	postgres.RelayActionLifecycleWriter
}

func workspaceFromRecord(row postgres.WorkspaceSession) workspaceRecord {
	record := workspaceRecord{ID: row.ID, OrganizationID: row.OrganizationID, ProjectID: row.ProjectID, SubjectWorkItemID: row.SubjectWorkItemID, AttemptID: row.AttemptID, Ref: row.Ref, HeadSHA: row.HeadSHA, RunnerID: row.RunnerID, MachineID: tracker.MachineID(row.MachineID), MachineHostname: row.MachineHostname, WorktreePath: row.WorktreePath, LeaseID: tracker.LeaseID(row.LeaseID), FencingToken: tracker.FencingToken(row.FencingToken), State: row.State, Reason: row.Reason, Requires: row.Requires, Isolation: row.Isolation, Worktree: row.Worktree, ReadOnly: row.ReadOnly, IdleTimeoutSeconds: int(row.IdleTimeoutSeconds), ExpiresAt: row.ExpiresAt, RequestedExpiresAt: row.RequestedExpiresAt, OpenedAt: row.OpenedAt, LastActivityAt: row.LastActivityAt, LastHeartbeatAt: row.LastHeartbeatAt, RebindDeadline: row.RebindDeadline, CreatedBy: row.CreatedBy, Revision: row.Revision, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
	if row.Capabilities != nil {
		c := workspacesession.Capabilities{Files: false}
		for _, name := range row.Capabilities {
			switch name {
			case "files":
				c.Files = true
			case "diff":
				c.Diff = true
			case "exec":
				c.Exec = true
			case "terminal":
				c.Terminal = true
			case "preview":
				c.Preview = true
			case "git":
				c.Git = true
			}
		}
		record.Capabilities = &c
	}
	return record
}

func workspaceCapabilities(c *workspacesession.Capabilities) []string {
	if c == nil {
		return nil
	}
	result := []string{}
	for _, name := range []string{"files", "diff", "exec", "terminal", "preview", "git"} {
		if c.Has(name) {
			result = append(result, name)
		}
	}
	return result
}

func (s *Service) workspaceRead(ctx context.Context, scope nativeScope, read func(workspaceReader) error) error {
	return projectRecordError(s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			if errors.Is(err, accessstore.ErrDenied) {
				return nativeNotFound()
			}
			return err
		}
		owner, ok := r.(workspaceReader)
		if !ok {
			return workspaceUnavailable()
		}
		return read(owner)
	}))
}
func (s *Service) workspaceWrite(ctx context.Context, scope nativeScope, write func(workspaceWriter, time.Time) error) error {
	return projectRecordError(s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if scope.credential.ID != "" {
			if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
				return err
			}
		}
		owner, ok := w.(workspaceWriter)
		if !ok {
			return workspaceUnavailable()
		}
		return write(owner, s.config.now().UTC())
	}))
}

func workspaceForActor(ctx context.Context, r workspaceReader, scope nativeScope, id string) (workspaceRecord, error) {
	if workspacesession.ValidateID(id) != nil {
		return workspaceRecord{}, nativeNotFound()
	}
	row, err := r.WorkspaceSession(ctx, scope.project, id)
	if err != nil {
		return workspaceRecord{}, projectRecordError(err)
	}
	subject, err := r.WorkItem(ctx, scope.project, tracker.NativeWorkItemID(row.SubjectWorkItemID))
	if err != nil {
		return workspaceRecord{}, projectRecordError(err)
	}
	if !scope.reporterOwns(tracker.Actor{PrincipalID: stringValue(subject.ActorPrincipalID)}) {
		return workspaceRecord{}, nativeNotFound()
	}
	return workspaceFromRecord(row), nil
}

func (s *Service) readWorkspaceRecord(ctx context.Context, scope nativeScope, id string) (workspaceRecord, error) {
	var record workspaceRecord
	err := s.workspaceRead(ctx, scope, func(r workspaceReader) error {
		var err error
		record, err = workspaceForActor(ctx, r, scope, id)
		return err
	})
	return record, err
}

func writeWorkspaceRecord(ctx context.Context, w workspaceWriter, record workspaceRecord, expected int64) error {
	current, err := w.WorkspaceSession(ctx, record.ProjectID, record.ID)
	if err != nil {
		return err
	}
	if current.Revision != expected {
		return nativeConflict(tracker.Revision(current.Revision))
	}
	_, err = w.UpdateWorkspaceSession(ctx, postgres.WorkspaceUpdate{OrganizationID: record.OrganizationID, ProjectID: record.ProjectID, ID: record.ID, ExpectedRevision: expected, ExpectedRunnerID: current.RunnerID, ExpectedLeaseID: current.LeaseID, ExpectedFencingToken: current.FencingToken, State: record.State, Reason: record.Reason, RunnerID: record.RunnerID, MachineID: string(record.MachineID), LeaseID: string(record.LeaseID), FencingToken: int64(record.FencingToken), HeadSHA: record.HeadSHA, Capabilities: workspaceCapabilities(record.Capabilities), Isolation: record.Isolation, Worktree: record.Worktree, MachineHostname: record.MachineHostname, WorktreePath: record.WorktreePath, ReadOnly: record.ReadOnly, OpenedAt: record.OpenedAt, LastActivityAt: record.LastActivityAt, LastHeartbeatAt: record.LastHeartbeatAt, RebindDeadline: record.RebindDeadline, UpdatedAt: record.UpdatedAt})
	return projectRecordError(err)
}

func (s *Service) createWorkspaceRecord(ctx context.Context, scope nativeScope, request workspaceRequest) (json.RawMessage, error) {
	service, err := s.requireWorkspaces()
	if err != nil {
		return nil, err
	}
	request, err = normalizeWorkspaceRequest(request)
	if err != nil {
		return nil, err
	}
	if request.IdempotencyKey == "" || len(request.IdempotencyKey) > 128 {
		return nil, nativePublicInvalid("An idempotency key of at most 128 bytes is required")
	}
	if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
		return nil, err
	}
	operation := nativeOperation(scope, http.MethodPost, "/workspaces")
	hash, err := nativeCommandHash(request)
	if err != nil {
		return nil, err
	}
	content := eventstore.NewContent(s.runDetailStore())
	if receipt, found, err := s.workItemRecordReceipt(ctx, scope, operation, request.IdempotencyKey, hash); found || err != nil {
		if err != nil {
			var conflict *nativeError
			if errors.As(err, &conflict) && conflict.Code == "revision_conflict" {
				return nil, workspaceIdempotencyConflict()
			}
			return nil, err
		}
		return content.Receipt(ctx, receipt)
	}
	now := s.config.now().UTC().Truncate(time.Microsecond)
	record := workspaceRecord{ID: newNativeID("ws"), OrganizationID: scope.organization, ProjectID: scope.project, SubjectWorkItemID: request.WorkItemID, AttemptID: request.AttemptID, Ref: strings.TrimSpace(request.Ref), Requires: request.Requires, State: workspacesession.StateRequested, IdleTimeoutSeconds: service.config.idleTimeoutSeconds(), ExpiresAt: now.Add(service.config.MaxLifetime), RequestedExpiresAt: now.Add(service.config.RequestTimeout), CreatedBy: scope.credential.ID, Revision: 1, CreatedAt: now, UpdatedAt: now}
	err = s.workspaceRead(ctx, scope, func(r workspaceReader) error {
		if record.AttemptID != "" {
			attempt, err := r.Attempt(ctx, record.AttemptID)
			if err != nil {
				return err
			}
			issue, err := r.WorkItemByID(ctx, scope.project, attempt.IssueID)
			if err != nil {
				return err
			}
			if record.SubjectWorkItemID != "" && record.SubjectWorkItemID != string(issue.NativeID) {
				return nativePublicInvalid("attempt_id belongs to a different work item")
			}
			record.SubjectWorkItemID = string(issue.NativeID)
			record.ReadOnly = attempt.Status == "running"
		}
		issue, err := r.WorkItem(ctx, scope.project, tracker.NativeWorkItemID(record.SubjectWorkItemID))
		if err != nil {
			return err
		}
		if !scope.reporterOwns(tracker.Actor{PrincipalID: stringValue(issue.ActorPrincipalID)}) {
			return nativeNotFound()
		}
		dispatch, err := r.IsWorkspaceWorkItem(ctx, scope.project, issue.NativeID)
		if err != nil {
			return err
		}
		if dispatch {
			return nativeNotFound()
		}
		return service.authorizeRequiresRecord(ctx, r, scope, record)
	})
	if err != nil {
		return nil, err
	}
	dispatchRequest := tracker.CreateIssue{Title: workspaceItemTitle(record.ID), Body: workspaceItemBody(record), Labels: []string{workspaceItemLabel}}
	plan, err := s.loadWorkItemPlan(allowWorkspaceLabel(ctx), scope, "", "", dispatchRequest)
	if err != nil {
		return nil, err
	}
	for _, state := range plan.project.States {
		if state.Dispatchable {
			dispatchRequest.State = state.Name
			break
		}
	}
	if dispatchRequest.State == "" {
		return nil, nativePublicInvalid("The project has no dispatchable workflow state for a workspace session")
	}
	plan, err = s.planWorkItemRecord(allowWorkspaceLabel(ctx), scope, plan, dispatchRequest)
	if err != nil {
		return nil, err
	}
	plan.issue.IssueContract.Exempt = true
	fields, err := s.prepareWorkItemFields(ctx, scope.organization, plan)
	if err != nil {
		return nil, err
	}
	issueRaw, err := json.Marshal(plan.issue)
	if err != nil {
		return nil, err
	}
	version, err := content.PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: plan.issue.WorkItemID, RecordID: string(plan.issue.WorkItemID), Revision: 1}, issueRaw)
	if err != nil {
		return nil, err
	}
	event, err := s.prepareWorkspaceDispatchEvent(ctx, scope, plan.issue, "issue.created", plan.fields)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(record.resource())
	if err != nil {
		return nil, err
	}
	receipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.organization, ActorID: scope.credential.ID, Operation: operation, CommandKey: request.IdempotencyKey, RequestHash: hash, CreatedAt: now, ResponseReplayUntilNs: now.Add(receiptstore.ReplayRetention).UnixNano()}, raw)
	if err != nil {
		return nil, err
	}
	var replay *eventstore.Receipt
	err = s.workspaceWrite(ctx, scope, func(w workspaceWriter, _ time.Time) error {
		if err := w.LockAccessOrganization(ctx); err != nil {
			return err
		}
		if err := w.LockWorkItemProject(ctx, scope.project); err != nil {
			return err
		}
		previous, e := w.Receipt(ctx, scope.credential.ID, operation, request.IdempotencyKey)
		if e == nil {
			if previous.RequestHash != hash {
				return workspaceIdempotencyConflict()
			}
			replay = &previous
			return nil
		}
		if !errors.Is(e, corestore.ErrNotFound) {
			return e
		}
		total, mine, e := w.OpenWorkspaceCounts(ctx, record.CreatedBy)
		if e != nil {
			return e
		}
		if total >= int64(service.config.PlanMaxOpen) {
			return workspaceLimit("organization", service.config.PlanMaxOpen)
		}
		if mine >= int64(service.config.PersonMaxOpen) {
			return workspaceLimit("person", service.config.PersonMaxOpen)
		}
		if record.AttemptID != "" {
			existing, e := w.OpenWorkspaceAttempt(ctx, scope.project, record.AttemptID)
			if e == nil {
				return workspaceExists(existing.ID)
			}
			if !errors.Is(e, corestore.ErrNotFound) {
				return e
			}
		}
		_, e = w.CreateWorkspaceSession(ctx, postgres.WorkspaceCreate{OrganizationID: scope.organization, ProjectID: scope.project, ID: record.ID, SubjectWorkItemID: record.SubjectWorkItemID, AttemptID: record.AttemptID, Ref: record.Ref, Requires: record.Requires, IdleTimeoutSeconds: int32(record.IdleTimeoutSeconds), ExpiresAt: record.ExpiresAt, RequestedExpiresAt: record.RequestedExpiresAt, CreatedBy: record.CreatedBy, CreatedAt: now, ReadOnly: record.ReadOnly})
		if e != nil {
			return e
		}
		if e = writeWorkItemRecord(ctx, w, plan, fields); e != nil {
			return e
		}
		if _, e = w.CreateCollaborationVersion(ctx, version); e != nil {
			return e
		}
		if _, e = w.AppendEvent(ctx, event); e != nil {
			return e
		}
		if e = w.PutWorkspaceItem(ctx, claimstore.WorkspaceItem{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: plan.issue.WorkItemID, WorkspaceID: record.ID, CreatedAt: now}); e != nil {
			return e
		}
		_, e = w.PutReceipt(ctx, receipt)
		return e
	})
	if err != nil {
		return nil, err
	}
	if replay != nil {
		return content.Receipt(ctx, *replay)
	}
	service.committed(ctx, record)
	return raw, nil
}

func workspaceIdempotencyConflict() error {
	return &nativeError{Code: "idempotency_conflict", Message: "Idempotency key has different content", status: http.StatusConflict, publicMessage: true}
}

func (w *workspaceService) authorizeRequiresRecord(ctx context.Context, r workspaceReader, scope nativeScope, record workspaceRecord) error {
	for _, name := range record.Requires {
		if name != "terminal" {
			continue
		}
		if record.ReadOnly {
			return &nativeError{Code: "forbidden", Message: "A terminal is refused while the attempt is running", status: http.StatusForbidden, publicMessage: true}
		}
		if !w.config.Terminal.Enabled {
			return &nativeError{Code: "forbidden", Message: "Terminals are turned off for this organization", status: http.StatusForbidden, publicMessage: true}
		}
		if scope.credential.Hosted == nil {
			return &nativeError{Code: "forbidden", Message: "A terminal requires a hosted session", status: http.StatusForbidden, publicMessage: true}
		}
		grant, err := r.AccessProjectGrant(ctx, scope.credential.Hosted.Subject, scope.project)
		if err != nil || !grant.ManageRunner {
			return nativeNotFound()
		}
		if w.config.Terminal.Isolation == workspacesession.IsolationUser && scope.credential.HostedRole != "owner" && scope.credential.HostedRole != "admin" {
			return nativeNotFound()
		}
		if scope.credential.Hosted.SupportReason != "" && w.config.Terminal.Isolation != workspacesession.IsolationContainer {
			return nativeNotFound()
		}
	}
	return nil
}

func (s *Service) readWorkspaceRecords(ctx context.Context, scope nativeScope, id, item, state string, limit int) (json.RawMessage, error) {
	if _, err := s.requireWorkspaces(); err != nil {
		return nil, err
	}
	if state != "" && !workspacesession.ValidState(state) {
		return nil, nativePublicInvalid("state names no workspace state")
	}
	limit = min(max(limit, 1), workspaceListLimit)
	var records []workspaceRecord
	var sequence int64
	err := s.workspaceRead(ctx, scope, func(r workspaceReader) error {
		var err error
		sequence, err = r.ProjectEventSequence(ctx, scope.project)
		if err != nil {
			return err
		}
		if id != "" {
			record, err := workspaceForActor(ctx, r, scope, id)
			if err != nil {
				return err
			}
			records = []workspaceRecord{record}
			return nil
		}
		if item != "" {
			if _, err := r.WorkItem(ctx, scope.project, tracker.NativeWorkItemID(item)); err != nil {
				return err
			}
		}
		rows, err := r.RecentWorkspaceSessions(ctx, scope.project, item, state, limit)
		if err != nil {
			return err
		}
		visible, err := workspaceVisibleSubjects(ctx, r, scope, rows)
		if err != nil {
			return err
		}
		for _, row := range rows {
			if visible == nil || visible[row.SubjectWorkItemID] {
				records = append(records, workspaceFromRecord(row))
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	response := workspaceListResponse{ChangeSequence: sequence, Workspaces: []workspacesession.Session{}}
	for _, record := range records {
		resource := record.resource()
		if workspaceAuditReadable(scope, record) {
			err = s.workspaceRead(ctx, scope, func(r workspaceReader) error {
				sessions, e := r.RecentRelaySessions(ctx, postgres.RelaySessionsInput{OrganizationID: scope.organization, ProjectID: scope.project, WorkspaceID: record.ID, Limit: workspaceRelaySessionPage})
				if e != nil {
					return e
				}
				for _, row := range sessions {
					resource.RelaySessions = append(resource.RelaySessions, workspacesession.RelaySession{ID: row.ID, WorkspaceID: row.WorkspaceID, PrincipalID: row.PrincipalID, Subject: row.Subject, ConnectionID: row.ConnectionID, Channels: row.Channels, OpenedAt: row.OpenedAt, ClosedAt: row.ClosedAt, CloseReason: row.CloseReason, BytesIn: row.BytesIn, BytesOut: row.BytesOut, SupportReason: row.SupportReason})
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
		}
		response.Workspaces = append(response.Workspaces, resource)
	}
	if id != "" {
		return json.Marshal(struct {
			workspacesession.Session
			ChangeSequence int64 `json:"change_sequence"`
		}{Session: response.Workspaces[0], ChangeSequence: sequence})
	}
	return json.Marshal(response)
}

func workspaceVisibleSubjects(ctx context.Context, r workspaceReader, scope nativeScope, rows []postgres.WorkspaceSession) (map[string]bool, error) {
	if scope.credential.HostedRole != "reporter" {
		return nil, nil
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.SubjectWorkItemID)
	}
	actors, err := r.WorkspaceSubjectActors(ctx, scope.project, ids)
	if err != nil {
		return nil, err
	}
	visible := make(map[string]bool, len(actors))
	for id, principal := range actors {
		visible[id] = scope.reporterOwns(tracker.Actor{PrincipalID: principal})
	}
	return visible, nil
}

func (w *workspaceService) transitionRecord(ctx context.Context, writer workspaceWriter, record workspaceRecord, to, reason string, now time.Time) (workspaceRecord, error) {
	if !workspacesession.Transition(record.State, to) {
		return record, nativeStaleExecution("The workspace state changed")
	}
	if reason != "" && !workspacesession.ValidReason(reason) {
		return record, nativeInvalid("Unknown workspace reason")
	}
	expected := record.Revision
	record.State = to
	record.Reason = reason
	record.Revision++
	record.UpdatedAt = now
	if to == workspacesession.StateReady {
		if record.OpenedAt == nil {
			record.OpenedAt = &now
		}
		record.RebindDeadline = nil
	}
	if to == workspacesession.StateRequested {
		record.Capabilities = nil
	}
	if err := writeWorkspaceRecord(ctx, writer, record, expected); err != nil {
		return record, err
	}
	if to == workspacesession.StateStarting {
		return record, writer.CreateWorkspaceOccupancy(ctx, postgres.WorkspaceOccupancyStart{OrganizationID: record.OrganizationID, ProjectID: record.ProjectID, WorkspaceID: record.ID, RunnerID: record.RunnerID, StartedAt: now})
	}
	if workspacesession.Terminal(to) {
		if record.RunnerID != "" {
			if err := writer.EndWorkspaceOccupancy(ctx, postgres.WorkspaceOccupancyEnd{OrganizationID: record.OrganizationID, ProjectID: record.ProjectID, WorkspaceID: record.ID, RunnerID: record.RunnerID, EndedAt: &now}); err != nil {
				return record, err
			}
		}
		item, err := writer.WorkspaceDispatchItem(ctx, record.ProjectID, record.ID)
		if err != nil {
			return record, err
		}
		return record, writer.CloseWorkspaceItem(ctx, record.ProjectID, tracker.NativeWorkItemID(item), now)
	}
	return record, nil
}

func (w *workspaceService) endWorkspaceRecord(ctx context.Context, writer workspaceWriter, record workspaceRecord, to, reason string, now time.Time) (workspaceRecord, error) {
	if workspacesession.Terminal(record.State) {
		return record, nil
	}
	if workspacesession.Transition(record.State, to) {
		return w.transitionRecord(ctx, writer, record, to, reason, now)
	}
	moved, err := w.transitionRecord(ctx, writer, record, workspacesession.StateClosing, reason, now)
	if err != nil {
		return record, err
	}
	return w.transitionRecord(ctx, writer, moved, to, reason, now)
}

type workspaceDispatchClose func(workspaceWriter) error

func (s *Service) prepareWorkspaceDispatchEvent(ctx context.Context, scope nativeScope, issue tracker.NativeIssue, kind string, fields []string) (eventstore.Event, error) {
	actor := scope.actor()
	if actor.PrincipalID == "" {
		actor = tracker.Actor{Kind: "integration", PrincipalID: "workspace-service"}
	}
	raw, err := json.Marshal(tracker.CollaborationEvent{OrganizationID: scope.organization, ProjectID: scope.project, AggregateID: issue.WorkItemID, ID: newNativeID("evt"), Type: kind, SchemaVersion: 1, Actor: actor, RecordedAt: issue.UpdatedAt, Data: tracker.CollaborationData{Revision: issue.Revision, Fields: fields}})
	if err != nil {
		return eventstore.Event{}, err
	}
	return eventstore.NewContent(s.runDetailStore()).PrepareEvent(ctx, scope.organization, raw)
}

func (s *Service) prepareWorkspaceDispatchClose(ctx context.Context, scope nativeScope, id string) (workspaceDispatchClose, error) {
	var item string
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		owner, ok := r.(workspaceReader)
		if !ok {
			return workspaceUnavailable()
		}
		var err error
		item, err = owner.WorkspaceDispatchItem(ctx, scope.project, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	plan, err := s.loadWorkItemPlan(ctx, scope, item, "", tracker.UpdateIssue{})
	if err != nil {
		return nil, err
	}
	terminal, found := firstTerminalState(tracker.NativeProject{States: plan.project.States})
	if !found || plan.issue.State == terminal {
		return func(workspaceWriter) error { return nil }, nil
	}
	plan.issue.State = terminal
	plan.issue.Terminal = true
	plan.issue.Revision++
	plan.issue.UpdatedAt = s.config.now().UTC()
	plan.fields = []string{"state"}
	fields, err := s.prepareWorkItemFields(ctx, scope.organization, plan)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(plan.issue)
	if err != nil {
		return nil, err
	}
	version, err := eventstore.NewContent(s.runDetailStore()).PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: plan.issue.WorkItemID, RecordID: string(plan.issue.WorkItemID), Revision: int64(plan.issue.Revision)}, raw)
	if err != nil {
		return nil, err
	}
	event, err := s.prepareWorkspaceDispatchEvent(ctx, scope, plan.issue, "issue.edited", plan.fields)
	if err != nil {
		return nil, err
	}
	return func(w workspaceWriter) error {
		if err := writeWorkItemRecord(ctx, w, plan, fields); err != nil {
			return err
		}
		if _, err := w.CreateCollaborationVersion(ctx, version); err != nil {
			return err
		}
		_, err := w.AppendEvent(ctx, event)
		return err
	}, nil
}

func (s *Service) closeWorkspaceRecord(ctx context.Context, scope nativeScope, id string) (json.RawMessage, error) {
	service, err := s.requireWorkspaces()
	if err != nil {
		return nil, err
	}
	record, err := s.readWorkspaceRecord(ctx, scope, id)
	if err != nil {
		return nil, err
	}
	if workspacesession.Terminal(record.State) {
		return json.RawMessage(`{"deleted":true}`), nil
	}
	finish, err := s.prepareWorkspaceDispatchClose(ctx, scope, id)
	if err != nil {
		return nil, err
	}
	var closed workspaceRecord
	err = s.workspaceWrite(ctx, scope, func(w workspaceWriter, now time.Time) error {
		current, err := workspaceForActor(ctx, w, scope, id)
		if err != nil {
			return err
		}
		if current.Revision != record.Revision {
			return nativeConflict(tracker.Revision(current.Revision))
		}
		closed, err = service.endWorkspaceRecord(ctx, w, current, workspacesession.StateClosed, workspacesession.ReasonClosedByActor, now)
		if err != nil {
			return err
		}
		return finish(w)
	})
	if err != nil {
		return nil, err
	}
	service.committed(ctx, closed)
	return json.RawMessage(`{"deleted":true}`), nil
}
