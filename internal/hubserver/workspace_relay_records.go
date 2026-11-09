package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func (s *Service) relayWrite(ctx context.Context, scope nativeScope, run func(workspaceWriter, time.Time) error) error {
	return projectRecordError(s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, w, scope, false); err != nil {
			if errors.Is(err, accessstore.ErrDenied) {
				return nativeNotFound()
			}
			return err
		}
		owner, ok := w.(workspaceWriter)
		if !ok {
			return workspaceUnavailable()
		}
		return run(owner, s.config.now().UTC())
	}))
}
func (s *Service) mintRelayTicketRecord(ctx context.Context, scope nativeScope, id, ticket string, principal relayPrincipal) error {
	return s.relayWrite(ctx, scope, func(w workspaceWriter, now time.Time) error {
		record, err := workspaceForActor(ctx, w, scope, id)
		if err != nil {
			return err
		}
		if workspacesession.Terminal(record.State) {
			return nativeStaleExecution("The workspace has ended")
		}
		_, err = w.CreateRelayTicket(ctx, postgres.CreateRelayTicketInput{OrganizationID: scope.organization, ProjectID: scope.project, Digest: relayTicketDigest(ticket), WorkspaceID: id, SessionID: principal.sessionID, PrincipalID: principal.principalID, Subject: principal.subject, SupportReason: principal.supportReason, CreatedAt: now, ExpiresAt: now.Add(workspacesession.TicketLifetime)})
		return err
	})
}
func (s *Service) redeemRelayTicketRecord(ctx context.Context, scope nativeScope, id, ticket string, principal relayPrincipal) (workspaceRecord, error) {
	var record workspaceRecord
	err := s.relayWrite(ctx, scope, func(w workspaceWriter, now time.Time) error {
		var err error
		record, err = workspaceForActor(ctx, w, scope, id)
		if err != nil {
			return err
		}
		if workspacesession.Terminal(record.State) {
			return nativeStaleExecution("The workspace has ended")
		}
		row, err := w.RedeemRelayTicket(ctx, postgres.RedeemRelayTicketInput{OrganizationID: scope.organization, ProjectID: scope.project, Digest: relayTicketDigest(ticket), SessionID: principal.sessionID, PrincipalID: principal.principalID, Subject: principal.subject, Now: now})
		if err != nil {
			return nativeNotFound()
		}
		if row.WorkspaceID != id {
			return nativeNotFound()
		}
		return nil
	})
	return record, err
}
func (w *workspaceService) readWorkspaceIDRecord(ctx context.Context, id string) (workspaceRecord, error) {
	var record workspaceRecord
	err := w.server.config.TenantRepository.ReadTenant(ctx, w.organization(), corestore.Serving, func(r tenantstore.Reader) error {
		row, err := r.(workspaceReader).WorkspaceSessionByID(ctx, id)
		record = workspaceFromRecord(row)
		return err
	})
	return record, projectRecordError(err)
}
func (w *workspaceService) revalidateRelayRecord(ctx context.Context, record workspaceRecord, connection *relayConnection, write, terminal bool) error {
	scope := connection.scope
	scope.organization = record.OrganizationID
	scope.project = record.ProjectID
	return w.server.workspaceRead(ctx, scope, func(r workspaceReader) error {
		row, err := r.WorkspaceSession(ctx, scope.project, record.ID)
		if err != nil {
			return err
		}
		if !workspacesession.Bound(row.State) {
			return nativeStaleExecution("The workspace is no longer bound to a runner")
		}
		if row.LeaseID != "" {
			lease, err := r.Lease(ctx, row.LeaseID)
			if err != nil {
				return err
			}
			if lease.ReleasedAt != nil || lease.FencingToken != row.FencingToken || !lease.ExpiresAt.After(w.now()) {
				return nativeStaleExecution("The workspace lease is no longer current")
			}
		}
		if connection.sessionHash == "" {
			return nil
		}
		session, err := r.AccessSession(ctx, connection.sessionHash)
		if err != nil {
			return err
		}
		if session.RevokedAt != nil || !session.ExpiresAt.After(w.now()) || session.Subject != scope.credential.Hosted.Subject {
			return nativeNotFound()
		}
		member, err := r.AccessMember(ctx, scope.credential.Hosted.Subject)
		if err != nil {
			return err
		}
		if !member.Active {
			return nativeNotFound()
		}
		grant, err := r.AccessProjectGrant(ctx, scope.credential.Hosted.Subject, scope.project)
		if err != nil {
			return err
		}
		if write && (!grant.CanWrite || member.Role == "viewer") {
			return nativeNotFound()
		}
		if terminal && !grant.ManageRunner {
			return nativeNotFound()
		}
		return nil
	})
}

func (w *workspaceService) touchWorkspaceRecord(ctx context.Context, id string) error {
	record, err := w.readWorkspaceIDRecord(ctx, id)
	if err != nil {
		return err
	}
	return w.server.workspaceWrite(ctx, nativeScope{organization: record.OrganizationID, project: record.ProjectID}, func(writer workspaceWriter, now time.Time) error {
		row, err := writer.WorkspaceSession(ctx, record.ProjectID, id)
		if err != nil {
			return err
		}
		if !workspacesession.Bound(row.State) {
			return nil
		}
		current := workspaceFromRecord(row)
		current.LastActivityAt = &now
		current.UpdatedAt = now
		current.Revision++
		return writeWorkspaceRecord(ctx, writer, current, row.Revision)
	})
}

func (w *workspaceService) openRelayAuditRecord(ctx context.Context, record workspaceRecord, connection *relayConnection, principal relayPrincipal) error {
	connection.auditID = newNativeID("relaysession")
	connection.organizationID = record.OrganizationID
	connection.projectID = record.ProjectID
	return w.server.workspaceWrite(ctx, nativeScope{organization: record.OrganizationID, project: record.ProjectID}, func(writer workspaceWriter, now time.Time) error {
		_, err := writer.CreateRelaySession(ctx, postgres.CreateRelaySessionInput{OrganizationID: record.OrganizationID, ProjectID: record.ProjectID, WorkspaceID: record.ID, ID: connection.auditID, PrincipalID: principal.principalID, Subject: principal.subject, ConnectionID: connection.id, Channels: []string{}, OpenedAt: now, SupportReason: principal.supportReason})
		return err
	})
}
func (w *workspaceService) closeRelayAuditRecord(ctx context.Context, connection *relayConnection, reason string) error {
	if connection.auditID == "" {
		return nil
	}
	in, out := connection.counters()
	return w.server.workspaceWrite(ctx, nativeScope{organization: connection.organizationID, project: connection.projectID}, func(writer workspaceWriter, now time.Time) error {
		return writer.CloseRelaySession(ctx, postgres.CloseRelaySessionInput{OrganizationID: connection.organizationID, ProjectID: connection.projectID, ID: connection.auditID, ConnectionID: connection.id, ClosedAt: &now, CloseReason: reason, BytesIn: in, BytesOut: out})
	})
}

func (w *workspaceService) startExecRunRecord(ctx context.Context, connection, runner *relayConnection, record workspaceRecord, stream *relayStream, frame workspacesession.Frame) string {
	var payload workspacesession.ExecRun
	if json.Unmarshal(frame.Payload, &payload) != nil {
		return workspacesession.CodeInvalidFrame
	}
	run, err := w.server.getActionRunRecord(ctx, connection.scope, strings.TrimSpace(payload.ActionID), strings.TrimSpace(payload.RunID))
	if err != nil || run.WorkspaceID != record.ID {
		return workspacesession.CodeNotFound
	}
	if run.Command != payload.Command {
		return workspacesession.CodeForbidden
	}
	_, err = w.claimActionRunRecord(ctx, nativeScope{organization: record.OrganizationID, project: record.ProjectID}, run.ID, stream.id, runner)
	if err != nil {
		if errors.Is(err, corestore.ErrRevisionConflict) {
			return workspacesession.CodeAlreadyRunning
		}
		return workspacesession.CodeStaleExecution
	}
	if !w.relay.beginExecRun(record.ID, stream.id, run.ID, run.ActionID, runner) {
		w.failExecRuns(ctx, []*execRunState{{runID: run.ID, actionID: run.ActionID}}, workspacesession.RunReasonStreamClosed)
		w.relay.closeStream(ctx, record.ID, stream.id)
		return workspacesession.CodeStaleExecution
	}
	return ""
}

func (w *workspaceService) pruneRelayTicketRecords(ctx context.Context, now time.Time) error {
	return w.server.config.TenantRepository.WriteTenant(ctx, w.organization(), corestore.Background, func(writer tenantstore.Writer) error {
		return writer.(workspaceWriter).PruneRelayTickets(ctx, now, workspaceMaintenanceBatch)
	})
}

func (w *workspaceService) relayGrantRecord(ctx context.Context, record workspaceRecord, connection *relayConnection, terminal bool) error {
	return w.server.config.TenantRepository.ReadTenant(ctx, record.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		member, err := r.AccessMember(ctx, connection.subject)
		if err != nil {
			return err
		}
		if !member.Active || member.Role == "viewer" {
			return nativeNotFound()
		}
		grant, err := r.AccessProjectGrant(ctx, connection.subject, record.ProjectID)
		if err != nil {
			return err
		}
		if !grant.CanWrite || terminal && !grant.ManageRunner {
			return nativeNotFound()
		}
		return nil
	})
}
