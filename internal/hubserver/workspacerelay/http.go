package workspacerelay

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/workspacehub"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/actionstore"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubcontent"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

// The relay's two sockets: the person's, opened with a
// ticket, and the runner's, opened under the worker token and fenced by the
// workspace tuple.

// acceptRelaySocket upgrades the request. Compression is left off: the frames
// are small JSON envelopes and the payloads that are not small are already
// base64, which does not compress usefully, so the CPU would buy nothing.
func acceptRelaySocket(c echo.Context) (*websocket.Conn, error) {
	socket, err := websocket.Accept(c.Response(), c.Request(), &websocket.AcceptOptions{
		// The Origin check is the hub's own (requireRelayOrigin), run before
		// the upgrade so a refusal is an HTTP error a client can read rather
		// than a socket that closes immediately.
		InsecureSkipVerify: true,
	})
	if err != nil {
		return nil, fmt.Errorf("accept relay socket: %w", err)
	}
	socket.SetReadLimit(ReadLimit)
	return socket, nil
}

// openWorkspaceRelay implements GET .../workspaces/:workspace/relay?ticket=.
func (w *HTTP) OpenWorkspaceRelay(c echo.Context) error {
	if err := w.requireRelayOrigin(c); err != nil {
		return w.NativeAPIError(c, err)
	}
	principal, err := w.relayPrincipalFor(c)
	if err != nil {
		return w.NativeAPIError(c, err)
	}
	ticket := strings.TrimSpace(c.QueryParam("ticket"))
	if ticket == "" {
		return w.NativeAPIError(c, hubapi.NativePublicInvalid("A relay ticket is required"))
	}
	scope := hubscope.NativeRequestScope(c)
	ctx := c.Request().Context()
	var record workspacehub.Record
	err = w.AccessWrite(ctx, scope.Organization, func(writer tenantstore.Writer) error {
		current := scope
		current.Credential = principal.Credential
		found, err := w.ReadRelayWorkspaceForActor(ctx, writer, current, c.Param("workspace"))
		if err != nil {
			return err
		}
		if workspacesession.Terminal(found.State) {
			return hubapi.NativeStaleExecution("The workspace has ended")
		}
		tickets, ok := writer.(postgres.RelayActionWriter)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		redeemed, err := tickets.RedeemRelayTicket(ctx, postgres.RedeemRelayTicketInput{OrganizationID: scope.Organization, ProjectID: scope.Project, Digest: relayTicketDigest(ticket), SessionID: principal.SessionID, PrincipalID: principal.PrincipalID, Subject: principal.Subject, Now: w.now()})
		if errors.Is(err, corestore.ErrRevisionConflict) || errors.Is(err, corestore.ErrNotFound) {
			return hubapi.NativeNotFound()
		}
		if err != nil {
			return err
		}
		if redeemed.WorkspaceID != found.ID {
			return hubapi.NativeNotFound()
		}
		record = found
		return nil
	})
	if err != nil {
		return w.NativeAPIError(c, err)
	}
	socket, err := acceptRelaySocket(c)
	if err != nil {
		return w.NativeAPIError(c, err)
	}
	connection := &Connection{
		ID: hubapi.NewID("relayconn"), WorkspaceID: record.ID, Socket: socket, Scope: w.AuthInput(scope),
		PrincipalID: principal.PrincipalID, SessionID: principal.SessionID,
		Out: make(chan Outbound, WriteQueue), Done: make(chan struct{}),
	}
	if principal.Credential.Hosted != nil {
		connection.Subject = principal.Credential.Hosted.Subject
	}
	connection.SessionHash = principal.Credential.SessionHash
	connection.SupportReason = principal.SupportReason
	connection.HostedRole = principal.Credential.HostedRole
	// The actor is resolved once here rather than per frame. It changes only
	// when the principal does, and a principal change closes the connection
	// (revalidatePerson), so a per-frame lookup would be a database read per
	// keystroke that could never return a different answer.
	connection.Actor = w.RelayActor(ctx, scope.Organization, connection, principal)
	return w.servePerson(c, scope, record, connection, principal)
}

// relayActor is the stamp every person-originated frame carries: who asked,
// from which connection, and -- for the git channel -- who a commit is
// authored as.
//
// The name and the email are the hub's to supply for the same reason the rest
// of the tuple is: a client that could name its own author could attribute a
// commit to anybody, and a commit's author line is permanent in the
// repository's history in a way a relay audit row is not.
func (w *HTTP) RelayActor(ctx context.Context, organization tracker.OrganizationID, connection *Connection, principal Principal) workspacesession.Actor {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	actor := workspacesession.Actor{
		PrincipalID: principal.PrincipalID, Subject: principal.Subject, ConnectionID: connection.ID,
	}
	if principal.Credential.Hosted == nil {
		// An operator token is not a person. It gets the token's name so a
		// runner's log says which credential acted, and no email, which is
		// what makes the runner refuse a commit from it with forbidden. That
		// is the intended outcome rather than a gap: a commit authored by a
		// machine credential names nobody who could answer for it.
		actor.Name = principal.Credential.Name
		return actor
	}
	// principal.Subject, not credential.Hosted.Subject: on a support session
	// the two differ, and the author has to be whoever is actually acting.
	// A support actor holds no hosted_members row, so the lookup finds
	// nothing, the email stays empty and the runner refuses the commit --
	// which is the only correct answer, because attributing the commit to the
	// person being stood in for would put a name in the repository's history
	// that never typed it.
	var email string
	err := w.AccessRead(ctx, organization, func(r tenantstore.Reader) error {
		member, err := r.AccessMember(ctx, principal.Subject)
		if err != nil {
			return err
		}
		if !member.Active {
			return corestore.ErrNotFound
		}
		email = member.Email
		return nil
	})
	switch {
	case err == nil:
		actor.Email = strings.TrimSpace(email)
		actor.Name = ActorName(actor.Email)
	case !errors.Is(err, corestore.ErrNotFound):
		// A missing row is the support actor's expected answer, so only a
		// real failure is worth a line. Either way the connection opens: a
		// status is a read and must not depend on an author line.
		w.ActorLogger().Warn("workspace.relay_actor_unresolved",
			"connection_id", connection.ID, "error", err)
	}
	if actor.Name == "" {
		actor.Name = principal.Subject
	}
	return actor
}

// servePerson runs one person connection for its whole life: the audit row, the
// writer, the authority re-check and the read loop.
func (w *HTTP) servePerson(c echo.Context, scope hubscope.NativeScope, record workspacehub.Record, connection *Connection, principal Principal) error {
	ctx := c.Request().Context()
	if err := w.OpenRelayAudit(ctx, record, connection, principal); err != nil {
		// The upgrade already happened, so there is no HTTP response left to
		// write an error into: the connection has been handed to the
		// WebSocket. Answering with nativeAPIError here would write a second
		// set of headers onto a hijacked connection, which the server reports
		// as a superfluous WriteHeader and the client never sees. Closing the
		// socket is the only way left to say no.
		//
		// And it does have to say no. Every person connection writes an audit
		// row; one that could not is a connection nobody could
		// later account for, which is exactly what the row exists to prevent.
		w.Logger.Warn("workspace.relay_audit_unavailable", "workspace_id", record.ID,
			"connection_id", connection.ID, "error", err)
		w.closeRelaySocket(connection.Socket, "audit_unavailable")
		return nil
	}
	if err := w.Relay.AttachPerson(connection); err != nil {
		w.CloseRelayAudit(context.WithoutCancel(ctx), record, connection, "server_shutdown")
		w.closeRelaySocket(connection.Socket, "server_shutdown")
		return nil
	}
	w.CloseIfEndedSinceUpgrade(ctx, connection)
	socketCtx, cancel := context.WithCancel(ctx)
	var helpers sync.WaitGroup
	defer helpers.Wait()
	defer cancel()
	helpers.Go(func() { connection.WriteLoop(socketCtx) })
	helpers.Go(func() { w.watchPersonAuthority(socketCtx, c, scope, record, connection) })
	helpers.Go(func() {
		select {
		case <-connection.Done:
			cancel()
		case <-socketCtx.Done():
		}
	})
	w.readPerson(socketCtx, connection, record)
	connection.Close("client_closed")
	w.Relay.DetachPerson(ctx, connection, w.now())
	reason := connection.Reason()
	w.CloseRelayAudit(context.WithoutCancel(ctx), record, connection, reason)
	w.closeRelaySocket(connection.Socket, reason)
	return nil
}

// closeRelaySocket ends a socket and records a close that failed. Every caller
// has already answered its own caller, so there is nobody left to return an
// error to; a close that fails still matters, because it means the peer never
// learned why its connection went away.
func (w *HTTP) closeRelaySocket(socket *websocket.Conn, reason string) {
	if err := socket.Close(relayStatusFor(reason), relayCloseText(reason)); err != nil {
		w.Logger.Debug("workspace.relay_socket_not_closed", "reason", reason, "error", err)
	}
}

// relayStatusFor maps a close reason onto a WebSocket close code. A policy
// refusal is distinguishable from a normal close, so a client can tell "you may
// not" from "we are done".
func relayStatusFor(reason string) websocket.StatusCode {
	switch reason {
	case workspacesession.CodeRevoked, workspacesession.CodeSuperseded:
		return websocket.StatusPolicyViolation
	case workspacesession.CodeRelayBusy, workspacesession.CodeOverflow:
		return websocket.StatusTryAgainLater
	case "server_shutdown":
		return websocket.StatusGoingAway
	default:
		return websocket.StatusNormalClosure
	}
}

// relayCloseText is the close frame's reason string, bounded by the protocol to
// 123 bytes.
func relayCloseText(reason string) string {
	if len(reason) > 123 {
		return reason[:123]
	}
	return reason
}

// watchPersonAuthority is the relay protocol's 30-second periodic re-check. Together
// with the re-check on every person-originated frame it is how a membership,
// role, grant or session change closes a connection.
func (w *HTTP) watchPersonAuthority(ctx context.Context, c echo.Context, scope hubscope.NativeScope, record workspacehub.Record, connection *Connection) {
	ticker := w.AuthorityTicker(workspacesession.AuthorityRecheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-connection.Done:
			return
		case <-ticker.C:
			if err := w.RevalidatePerson(ctx, c, scope, record, connection); err != nil {
				connection.Revoke()
				return
			}
		}
	}
}

// revalidatePerson re-runs the whole authority list: the session is still
// valid, the membership is still active, the project is still readable and the
// workspace is still open. It runs on every person-originated frame as well as
// on the timer, because a socket that lives for an hour would otherwise outlive
// the grant that opened it.
func (w *HTTP) RevalidatePerson(ctx context.Context, c echo.Context, scope hubscope.NativeScope, record workspacehub.Record, connection *Connection) error {
	principal, err := w.ResolvePrincipal(ctx, c)
	if err != nil {
		return err
	}
	if principal.PrincipalID != connection.PrincipalID || principal.SessionID != connection.SessionID {
		return errors.New("the relay principal changed")
	}
	if scope.Organization != record.OrganizationID || scope.Project != record.ProjectID {
		return hubapi.NativeNotFound()
	}
	current := scope
	current.Credential = principal.Credential
	if w.Repository() == nil {
		return hubapi.NativeWorkItemStorageUnavailable()
	}
	return w.Repository().ReadTenant(ctx, current.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := w.AuthorizeAccessScope(ctx, r, current, false); err != nil {
			return err
		}
		workspaces, ok := r.(postgres.WorkspaceReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		live, err := workspaces.WorkspaceSession(ctx, current.Project, record.ID)
		if err != nil {
			return err
		}
		if workspacesession.Terminal(live.State) {
			return errors.New("the workspace has ended")
		}
		return workspacehub.ReadableSubject(ctx, r, current, live.SubjectWorkItemID)
	})
}

// revalidatePersonLocally is the per-frame half of the relay protocol's authority
// check: everything the hub can answer from its own database, run before a
// frame is forwarded rather than on a timer.
//
// It deliberately does not ask the identity provider. The full list -- which
// includes a provider round trip for the membership -- runs on the 30-second
// re-check and on authority.changed, because a network call per keystroke
// would make a terminal unusable and would itself become the failure mode. The
// locally answerable facts are the ones that change under a live socket: the
// session was revoked, the membership row was deactivated, the grant was
// withdrawn, the workspace ended, or the runner lost its lease.
func (w *HTTP) RevalidatePersonLocally(ctx context.Context, record workspacehub.Record, connection *Connection) error {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	if w.Repository() == nil {
		return hubapi.NativeWorkItemStorageUnavailable()
	}
	return w.Repository().ReadTenant(ctx, record.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		workspaces, ok := r.(postgres.WorkspaceReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		live, err := workspaces.WorkspaceSession(ctx, record.ProjectID, record.ID)
		if err != nil {
			return fmt.Errorf("read workspace: %w", err)
		}
		if !workspacesession.Bound(live.State) {
			return errors.New("the workspace is no longer bound to a runner")
		}
		if live.LeaseID != "" {
			lease, err := r.Lease(ctx, live.LeaseID)
			if err != nil {
				return fmt.Errorf("read workspace lease: %w", err)
			}
			if lease.ReleasedAt != nil || lease.FencingToken != live.FencingToken || !lease.ExpiresAt.After(w.now()) {
				return errors.New("the workspace lease is no longer current")
			}
		}
		if connection.SessionHash == "" {
			return nil
		}
		session, err := r.AccessSession(ctx, connection.SessionHash)
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return fmt.Errorf("read hosted session: %w", err)
		}
		if errors.Is(err, corestore.ErrNotFound) || session.RevokedAt != nil || !session.ExpiresAt.After(w.now()) {
			return errors.New("the hosted session or membership is no longer active")
		}
		member, err := r.AccessMember(ctx, connection.Subject)
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return fmt.Errorf("read hosted session: %w", err)
		}
		if errors.Is(err, corestore.ErrNotFound) || !member.Active {
			return errors.New("the hosted session or membership is no longer active")
		}
		if _, err := r.AccessProjectGrant(ctx, connection.Subject, live.ProjectID, connection.HostedRole); err != nil {
			if errors.Is(err, corestore.ErrNotFound) {
				return errors.New("the project grant has been withdrawn")
			}
			return fmt.Errorf("read project grant: %w", err)
		}
		return nil
	})
}

// refuseTerminal is the terminal's whole per-frame authority,
// and it is longer than any other channel's because a terminal is the runner
// account's shell handed to a person and nothing else on this relay is.
//
// The terminal contract lists the gate in order and this follows it in order:
//
//  1. the workspace is readable --- the connection proved that on the upgrade;
//  2. `write` on the project;
//  3. the grant's `runners` flag;
//  4. workspaces.terminal.enabled, which an owner turns on and which defaults
//     off;
//  5. the isolation rule --- `user` is owners and admins only;
//  6. the workspace not being on a running attempt.
//
// Every one of them is asked per frame rather than cached from the upgrade, for
// the reason the relay protocol gives about everything else: a socket that lives for
// an hour would otherwise outlive the grant that opened it, and for a terminal
// "outlive" means a person keeps typing into a worktree after their access was
// withdrawn. The `runners` grant is the one that is checked at creation too
// (authorizeRequires), and checking it again here is not redundant: a grant can
// be withdrawn while the shell is open.
//
// It answers "" when the frame may be forwarded and a relay error code
// otherwise.
func (w *HTTP) RefuseTerminal(ctx context.Context, record workspacehub.Record, connection *Connection, frame workspacesession.Frame) string {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	if frame.Channel != workspacesession.ChannelTerminal {
		return ""
	}
	if !workspacesession.ValidTerminalRequest(frame.Type) && frame.Type != workspacesession.TypeClose {
		// The channel's vocabulary is checked before a stream is allocated, so
		// a client sending typos cannot spend its whole stream budget on them.
		return workspacesession.CodeUnknownFrame
	}
	if frame.Type == workspacesession.TypeTerminalOpen {
		// And so is the open's shape. The hub reads this payload itself --- it
		// is what a recording's header declares as the window ---
		// so an open it cannot parse is one it must refuse rather than forward:
		// forwarding it would spend a stream and start a recording claiming a
		// window nobody asked for, and the runner would refuse the frame a
		// moment later anyway.
		if _, err := decodeTerminalOpen(frame); err != nil {
			return workspacesession.CodeInvalidFrame
		}
	}
	if !w.TerminalConfig().Enabled {
		return workspacesession.CodeForbidden
	}
	if record.ReadOnly {
		// A read-only workspace is one whose subject attempt is still running,
		// and the workspace session contract refuses a terminal there outright: a person must not
		// type into a worktree the model is editing. read_only rather than
		// forbidden, because the request was never allowed rather than the
		// person being the wrong person.
		return workspacesession.CodeReadOnly
	}
	if connection.SessionHash == "" {
		// A token principal has no hosted session, no membership role and no
		// project grant row, so none of the checks below can be asked of it.
		// Its authority is the token, which the middleware already proved on
		// the upgrade against the same scope this frame needs --- the same
		// reasoning refuseRelayWrite applies to a commit, which is the other
		// frame on this relay that changes a worktree.
		//
		// The terminal contract's "a terminal requires a hosted session" is enforced
		// where it can actually be enforced: authorizeRequires refuses a token
		// that asks for `requires: ["terminal"]`, so no token can open a
		// workspace with a terminal on it. What is left here is a token acting
		// on a workspace a person opened, on a project the token holds write
		// on, which is an organization-level secret acting within its scope.
		return ""
	}
	if w.TerminalConfig().Isolation == workspacesession.IsolationUser &&
		connection.HostedRole != "owner" && connection.HostedRole != "admin" {
		// `user` isolation runs the shell as the runner's own account, with its
		// credential store and its other checkouts in reach. The terminal contract
		// allows it "only when the organization has set
		// workspaces.terminal.isolation: user explicitly and the person is an
		// owner or admin with the runners grant".
		return workspacesession.CodeForbidden
	}
	if connection.SupportReason != "" && w.TerminalConfig().Isolation != workspacesession.IsolationContainer {
		// A support actor gets a terminal "only with a support reason recorded
		// and only at container". The reason is recorded on the
		// audit row and on the recording already; this is the other half.
		return workspacesession.CodeForbidden
	}
	var permitted bool
	err := w.AccessRead(ctx, record.OrganizationID, func(r tenantstore.Reader) error {
		member, err := r.AccessMember(ctx, connection.Subject)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if role := accessstore.LesserRole(member.Role, connection.HostedRole); !member.Active || role == "" || role == "viewer" {
			return nil
		}
		grant, err := r.AccessProjectGrant(ctx, connection.Subject, record.ProjectID, connection.HostedRole)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		permitted = grant.CanWrite && grant.ManageRunner
		return nil
	})
	if err != nil {
		// The grant cannot be read, so neither `write` nor the runners flag can
		// be proved. Forwarding would be handing out a shell on an authority
		// nobody established, so the refusal stands and the failure is logged
		// rather than resolved in the person's favour.
		w.Logger.Warn("workspace.relay_terminal_grant_unreadable", "workspace_id", record.ID,
			"connection_id", connection.ID, "error", err)
		return workspacesession.CodeForbidden
	}
	if !permitted {
		// Either the grant is read-only, or it lacks the runners flag, or the
		// role is viewer. The viewer half mirrors requireHostedProject: a role
		// demotion must not be survivable by holding an old grant row, and
		// the terminal contract says outright that viewers never get a terminal.
		return workspacesession.CodeForbidden
	}
	return ""
}

// decodeTerminalOpen reads an open frame's window and working directory.
//
// An absent payload is not an error: the terminal contract lets a client that has not
// measured its container yet open at the conventional size, which is what
// ValidateTerminalOpen defaults to.
func decodeTerminalOpen(frame workspacesession.Frame) (workspacesession.TerminalOpen, error) {
	var open workspacesession.TerminalOpen
	if len(frame.Payload) > 0 {
		if err := json.Unmarshal(frame.Payload, &open); err != nil {
			return workspacesession.TerminalOpen{}, fmt.Errorf("decode terminal open: %w", err)
		}
	}
	return workspacesession.ValidateTerminalOpen(open)
}

// refuseRelayWrite is the per-frame half of the relay protocol's `write` rule for the
// channels that write: a git commit or push, and an exec run, which executes
// the project's command inside the worktree. It reports the code the frame is
// refused with, or "" when it may be forwarded.
//
// channelPermitted already gates the channel on the capability the runner
// reported, and that check cannot answer this one: the capability says the
// worktree is a repository, not that this person may change it. A `status` is
// a read and needs nothing beyond the project read the connection proved on
// the upgrade; a `commit` or a `push` needs `write`, which is a different
// question asked per frame because a grant can be withdrawn under a live
// socket.
func (w *HTTP) RefuseRelayWrite(ctx context.Context, record workspacehub.Record, connection *Connection, frame workspacesession.Frame) string {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	gitWrite := frame.Channel == workspacesession.ChannelGit && workspacesession.GitWriteRequest(frame.Type)
	execRun := frame.Channel == workspacesession.ChannelExec && frame.Type == workspacesession.TypeExecRun
	if !gitWrite && !execRun {
		return ""
	}
	if gitWrite && record.ReadOnly {
		// A read-only workspace is one whose subject attempt is still running,
		// and read_only is set at creation and never cleared, so the record
		// captured at the upgrade is as current as a re-read would be.
		//
		// The runner refuses the same frame against its own lease immediately
		// before it acts. The hub refusing first is defence in depth, and it
		// is also the only refusal a reader ever gets when no runner is
		// attached: without it a commit on a read-only workspace would be
		// answered with stale_execution, which says the wrong thing about a
		// request that was never allowed.
		return workspacesession.CodeReadOnly
	}
	if connection.SessionHash == "" {
		// A token principal has no hosted session and no project grant row;
		// its authority is the token, which the middleware already proved on
		// the upgrade against the same `write` scope this frame needs. There
		// is nothing here that could answer the question better, so the frame
		// is forwarded and the runner's own lease check is the next gate.
		return ""
	}
	var permitted bool
	err := w.AccessRead(ctx, record.OrganizationID, func(r tenantstore.Reader) error {
		member, err := r.AccessMember(ctx, connection.Subject)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if role := accessstore.LesserRole(member.Role, connection.HostedRole); !member.Active || role == "" || role == "viewer" {
			return nil
		}
		grant, err := r.AccessProjectGrant(ctx, connection.Subject, record.ProjectID, connection.HostedRole)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		permitted = grant.CanWrite
		return nil
	})
	if err != nil {
		// The grant cannot be read, so `write` cannot be proved. Forwarding
		// the frame would be writing to a repository on an authority nobody
		// established, so the refusal stands and the failure is logged rather
		// than resolved in the person's favour.
		w.Logger.Warn("workspace.relay_write_grant_unreadable", "workspace_id", record.ID,
			"connection_id", connection.ID, "error", err)
		return workspacesession.CodeForbidden
	}
	if !permitted {
		// The grant is read-only, or the role is viewer. The viewer half
		// mirrors requireHostedProject, which refuses a write to a viewer
		// however generous the grant row is: a role demotion must not be
		// survivable by holding an old can_write row.
		return workspacesession.CodeForbidden
	}
	return ""
}

// readPerson is the person side's read loop.
func (w *HTTP) readPerson(ctx context.Context, connection *Connection, record workspacehub.Record) {
	for {
		frame, ok := w.readFrame(ctx, connection)
		if !ok {
			return
		}
		w.HandlePersonFrame(ctx, connection, record, frame)
	}
}

// readFrame reads and decodes one frame, answering a malformed one rather than
// closing: a client bug on one frame must not cost a person their session.
func (w *HTTP) readFrame(ctx context.Context, connection *Connection) (workspacesession.Frame, bool) {
	kind, data, err := connection.Socket.Read(ctx)
	if err != nil {
		connection.Close("client_closed")
		return workspacesession.Frame{}, false
	}
	connection.CountIn(len(data))
	if kind != websocket.MessageText {
		// Binary data rides inside a frame as base64, so a
		// binary message is not a frame at all.
		connection.Send(workspacesession.ErrorFrame("", "", workspacesession.CodeInvalidFrame, "Frames are JSON text"))
		return workspacesession.Frame{}, true
	}
	var frame workspacesession.Frame
	if err := json.Unmarshal(data, &frame); err != nil {
		connection.Send(workspacesession.ErrorFrame("", "", workspacesession.CodeInvalidFrame, "Frame is not valid JSON"))
		return workspacesession.Frame{}, true
	}
	if err := workspacesession.ValidateFrame(frame); err != nil {
		connection.Send(workspacesession.ErrorFrame(frame.Channel, frame.Stream, workspacesession.CodeInvalidFrame, err.Error()))
		return workspacesession.Frame{}, true
	}
	return frame, true
}

// handlePersonFrame applies the authority re-check and routes one frame.
func (w *HTTP) HandlePersonFrame(ctx context.Context, connection *Connection, record workspacehub.Record, frame workspacesession.Frame) {
	if frame.Type == "" {
		return
	}
	if frame.Type == workspacesession.TypeAck {
		// An acknowledgement is not activity. The workspace session contract resets the idle
		// clock on what a person does -- input, requests, resizes -- and a
		// stream that is only receiving runner output would otherwise keep a
		// workspace alive by acknowledging it, which is exactly the "busy
		// shell left alone" the timeout exists to close. It asks the runner
		// for nothing either, so there is nothing to re-authorize.
		w.handlePersonAck(connection, frame)
		return
	}
	// Every other person-originated frame resets the idle clock and re-proves
	// the authority behind it.
	if err := w.RevalidatePersonLocally(ctx, record, connection); err != nil {
		w.Logger.Info("workspace.relay_revoked", "workspace_id", record.ID,
			"connection_id", connection.ID, "reason", err)
		connection.Revoke()
		return
	}
	w.TouchWorkspace(ctx, record)
	if frame.Type == workspacesession.TypeResume {
		w.handlePersonResume(connection, frame)
		return
	}
	if code := w.RefuseRelayWrite(ctx, record, connection, frame); code != "" {
		connection.Send(workspacesession.ErrorFrame(frame.Channel, frame.Stream, code, relayCodeMessage(code)))
		return
	}
	// The terminal's authority is asked before a stream is allocated, so a
	// refused open never costs the person a stream slot and never reaches a
	// runner.
	if code := w.RefuseTerminal(ctx, record, connection, frame); code != "" {
		connection.Send(workspacesession.ErrorFrame(frame.Channel, frame.Stream, code, relayCodeMessage(code)))
		return
	}
	stream, code := w.resolvePersonStream(connection, record, frame)
	if stream == nil {
		// The pointer is what decides, not the code: a refusal without a code
		// is still a refusal, and guarding on the thing about to be
		// dereferenced is the only guard that stays true if either side grows
		// a case.
		if code == "" {
			code = workspacesession.CodeInvalidFrame
		}
		connection.Send(workspacesession.ErrorFrame(frame.Channel, frame.Stream, code, relayCodeMessage(code)))
		return
	}
	if frame.Type == workspacesession.TypeClose {
		// A close is answered before the runner is even looked up, and the
		// slot goes back to the connection either way. A person who is done
		// with a stream must be able to say so whatever the runner is doing;
		// refusing the close because nothing is serving the workspace would
		// hold the slot for a stream that has no owner left on either side.
		w.Relay.CloseStream(ctx, connection.WorkspaceID, stream.ID)
		connection.Send(workspacesession.Frame{Channel: frame.Channel, Stream: stream.ID, Type: workspacesession.TypeClosed})
	}
	runner := w.Relay.RunnerFor(connection.WorkspaceID)
	if runner == nil {
		if frame.Type == workspacesession.TypeClose {
			// The stream is already gone and the person has been told; there
			// is nobody to forward the close to.
			return
		}
		// Nothing is serving the workspace right now. The person is told
		// rather than left waiting: an unanswered request is indistinguishable
		// from a slow one, and only the hub knows which this is.
		connection.Send(workspacesession.ErrorFrame(frame.Channel, stream.ID, workspacesession.CodeStaleExecution,
			"No runner is serving this workspace"))
		return
	}
	if frame.Channel == workspacesession.ChannelExec && frame.Type == workspacesession.TypeExecRun {
		// The run row is claimed before the frame is forwarded, so a frame the
		// hub refuses is never executed and a run can only be started once.
		if code := w.StartExecRun(ctx, connection, runner, record, stream, frame); code != "" {
			connection.Send(workspacesession.ErrorFrame(frame.Channel, stream.ID, code, relayCodeMessage(code)))
			return
		}
	}
	if frame.Channel == workspacesession.ChannelTerminal {
		switch frame.Type {
		case workspacesession.TypeTerminalOpen:
			// refuseTerminal already parsed and refused anything this cannot
			// read, so the error here is unreachable rather than merely
			// unlikely. It is still handled: a recording is the only record a
			// closed tab leaves behind, and one started at a window nobody
			// asked for would be a recording that plays back wrong. No
			// recording at all, with a line saying so, is the honest answer.
			open, err := decodeTerminalOpen(frame)
			if err != nil {
				w.Logger.Warn("workspace.terminal_open_not_recorded",
					"workspace_id", record.ID, "stream", stream.ID, "error", err)
				break
			}
			w.Relay.BeginTerminalRecording(w.beginTerminalRecording(record, connection, stream, open))
		default:
			w.recordPersonTerminalFrame(connection, stream, frame)
		}
	}
	forwarded := frame
	forwarded.Stream = stream.ID
	forwarded.Seq = w.Relay.NextToRunner(stream)
	// The actor is stamped by the hub and never supplied by the client, so a
	// runner can audit what it was asked to do and by whom.
	actor := connection.Actor
	forwarded.Actor = &actor
	runner.Send(forwarded)
}

// resolvePersonStream finds or opens the stream a person frame belongs to.
//
// A terminal opens explicitly and every open is its own PTY, so a terminal
// frame with no stream always allocates: the relay protocol says a second tab that
// wants the same shell opens its own stream.
//
// Files, diff and preview are request/response and open on their first
// request. A later request with no stream is the same conversation continuing,
// not a second one, so it reuses the stream this connection already has open
// on that channel. Allocating instead would let an ordinary client exhaust its
// own budget -- eight listings and the ninth is refused with stream_limit, on
// a connection holding eight streams nobody is using -- which is exactly what
// the sixth dogfood run hit.
func (w *HTTP) resolvePersonStream(connection *Connection, record workspacehub.Record, frame workspacesession.Frame) (*Stream, string) {
	if !w.ChannelPermitted(record, frame.Channel) {
		return nil, workspacesession.CodeForbidden
	}
	// The exec channel defines exactly one person-originated frame,
	// so this is where its vocabulary is checked: before a stream is
	// allocated, because a channel that allocated for anything it was sent
	// would spend a connection's whole budget on typos. close is a shared
	// control rather than a channel frame, and ack and resume never reach
	// here, so only the channel's own types need naming.
	if frame.Channel == workspacesession.ChannelExec && frame.Type != workspacesession.TypeClose &&
		!workspacesession.ValidExecRequest(frame.Type) {
		return nil, workspacesession.CodeUnknownFrame
	}
	if frame.Stream != "" {
		if stream := w.Relay.LookupStream(connection, frame.Stream); stream != nil {
			return stream, ""
		}
		// A stream this connection does not own does not exist for it.
		return nil, workspacesession.CodeResumeFailed
	}
	if frame.Type == workspacesession.TypeClose {
		return nil, workspacesession.CodeInvalidFrame
	}
	// Terminal and exec allocate per request instead of reusing. Every
	// terminal open is its own PTY, and every exec run is its own process with
	// its own seq space: two runs sharing a stream would share
	// one sequence, and a reader could not tell whose output it was reading or
	// which run the exited frame ended.
	if frame.Channel != workspacesession.ChannelTerminal && frame.Channel != workspacesession.ChannelExec {
		if stream := w.Relay.ChannelStream(connection, frame.Channel); stream != nil {
			return stream, ""
		}
	}
	stream, code := w.Relay.OpenStream(connection, frame.Channel)
	if stream == nil {
		if code == "" {
			code = workspacesession.CodeStreamLimit
		}
		return nil, code
	}
	return stream, ""
}

// channelPermitted reports whether a workspace may carry this channel: the
// runner reported the capability, and a read-only workspace refuses a terminal
// whatever the runner can do.
func (w *HTTP) ChannelPermitted(record workspacehub.Record, channel string) bool {
	if record.Capabilities == nil {
		return false
	}
	if channel == workspacesession.ChannelTerminal {
		// refuseTerminal asks both of these first and answers with a code that
		// says which it was, so a reader is normally told read_only rather than
		// forbidden. They stay here because this is the check on the path a
		// stream is actually allocated on: a gate that only existed in the
		// refusal above would be one edit away from being bypassed.
		if record.ReadOnly || !w.TerminalConfig().Enabled {
			return false
		}
	}
	if channel == workspacesession.ChannelExec && record.ReadOnly {
		// A read-only workspace is one whose attempt is still running, and an
		// action's command writes into the worktree the model is editing.
		// The run endpoint refuses one too; this is the check
		// that holds when the frame is the last moment before something
		// executes, and it is where the relay's own answer has to come from.
		return false
	}
	return record.Capabilities.Has(workspacesession.ChannelCapability(channel))
}

// handlePersonAck records an acknowledgement, which is what releases the
// replay buffer.
func (w *HTTP) handlePersonAck(connection *Connection, frame workspacesession.Frame) {
	stream := w.Relay.LookupStream(connection, frame.Stream)
	if stream == nil {
		return
	}
	var payload workspacesession.AckPayload
	if err := json.Unmarshal(frame.Payload, &payload); err != nil {
		return
	}
	w.Relay.Acknowledge(stream, payload.Through)
}

// handlePersonResume moves a parked stream onto this connection and replays
// what the person missed.
func (w *HTTP) handlePersonResume(connection *Connection, frame workspacesession.Frame) {
	var payload workspacesession.ResumePayload
	if err := json.Unmarshal(frame.Payload, &payload); err != nil {
		connection.Send(workspacesession.ErrorFrame(frame.Channel, frame.Stream, workspacesession.CodeResumeFailed, "resume is malformed"))
		return
	}
	stream, replay, code := w.Relay.ResumeStream(connection, payload.Stream, payload.LastSeq, w.now())
	if stream == nil {
		if code == "" {
			code = workspacesession.CodeResumeFailed
		}
		connection.Send(workspacesession.ErrorFrame(frame.Channel, payload.Stream, code, relayCodeMessage(code)))
		return
	}
	resumed, err := workspacesession.Encode(map[string]any{"stream": stream.ID, "last_seq": payload.LastSeq})
	if err != nil {
		connection.Send(workspacesession.ErrorFrame(frame.Channel, payload.Stream, workspacesession.CodeResumeFailed, "resume could not be answered"))
		return
	}
	connection.Send(workspacesession.Frame{Channel: stream.Channel, Stream: stream.ID, Type: workspacesession.TypeResumed, Payload: resumed})
	for _, buffered := range replay {
		connection.Send(buffered)
	}
}

// relayCodeMessage is the sentence that goes with a relay error code. It is one
// table so two handlers cannot describe the same refusal differently.
func relayCodeMessage(code string) string {
	switch code {
	case workspacesession.CodeStreamLimit:
		return "The stream limit for this connection or workspace has been reached"
	case workspacesession.CodeRelayBusy:
		return "The relay is at its memory limit; try again shortly"
	case workspacesession.CodeResumeFailed:
		return "The stream cannot be resumed"
	case workspacesession.CodeForbidden:
		// Several refusals share this code: a channel the workspace does not
		// serve, a git write by a person who holds read and not write, and
		// every one of the terminal's own gates --- the runners grant, the
		// enabled setting, the isolation rule and the support window.
		// The code is what a client switches on, so the sentence has to
		// cover them all; naming only the channel would be a lie on the rest.
		// A client that wants to say more shows the reason it already knows
		// from the workspace and the capability, which is where its own
		// disabled sentences come from.
		return "This workspace does not serve that channel, or your authority on the project does not allow it"
	case workspacesession.CodeReadOnly:
		return "The workspace is read-only while its attempt is still running"
	case workspacesession.CodeGitFailed:
		// The hub only reaches this sentence when it has to describe a git
		// failure it did not run. A runner-originated git_failed carries
		// git's own stderr and its own message, which is the whole point of
		// the code, and that frame is forwarded unchanged.
		return "The git command failed"
	case workspacesession.CodeWorkspaceClosed:
		return "The workspace has ended"
	case workspacesession.CodeOverflow:
		return "The stream fell too far behind and was closed"
	case workspacesession.CodeInvalidFrame:
		return "The frame is not valid on this channel"
	case workspacesession.CodeUnknownFrame:
		return "The channel does not define that frame type"
	case workspacesession.CodeAlreadyRunning:
		return "This run is already being executed; read it back through the run"
	default:
		return "The relay refused this frame"
	}
}

// touchWorkspace records person-originated activity, which is the only thing
// that resets the idle clock. The write is best effort: losing one touch
// shortens a workspace's life by nothing a person would notice, and failing a
// frame over it would be worse.
func (w *HTTP) TouchWorkspace(ctx context.Context, record workspacehub.Record) {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	now := w.now()
	err := w.AccessWrite(ctx, record.OrganizationID, func(writer tenantstore.Writer) error {
		owner, ok := writer.(postgres.WorkspaceWriter)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		return owner.TouchWorkspaceActivity(ctx, record.ProjectID, record.ID, now)
	})
	if err != nil {
		w.Logger.Warn("workspace.activity_not_recorded", "workspace_id", record.ID, "error", err)
	}
}

// The runner side.

// openWorkspaceWorkerRelay implements GET .../workspaces/:workspace/worker/relay.
// It is fenced by the workspace tuple, carried in headers because a GET has no
// body: the same shape the conversation controls poll uses.
func (w *HTTP) OpenWorkspaceWorkerRelay(c echo.Context) error {
	identity, err := WorkerIdentityFromHeaders(c)
	if err != nil {
		return w.NativeAPIError(c, err)
	}
	scope := hubscope.NativeRequestScope(c)
	ctx := c.Request().Context()
	var record workspacehub.Record
	err = w.AccessRead(ctx, scope.Organization, func(reader tenantstore.Reader) error {
		var err error
		record, err = w.ReadRelayWorker(ctx, reader, scope, c.Param("workspace"), identity)
		if err == nil && !workspacesession.Bound(record.State) {
			return hubapi.NativeStaleExecution("The workspace is not bound to a runner")
		}
		return err
	})
	if err != nil {
		return w.NativeAPIError(c, err)
	}
	socket, err := acceptRelaySocket(c)
	if err != nil {
		return w.NativeAPIError(c, err)
	}
	connection := &Connection{
		ID: hubapi.NewID("relayrunner"), WorkspaceID: record.ID, Socket: socket, Runner: true, Scope: w.AuthInput(scope), Owner: record.Owner(),
		PrincipalID: scope.Credential.ID, SessionID: string(record.LeaseID),
		Out: make(chan Outbound, WriteQueue), Done: make(chan struct{}),
	}
	previous, err := w.Relay.AttachRunner(connection)
	if err != nil {
		w.closeRelaySocket(socket, "server_shutdown")
		return nil
	}
	if previous != nil {
		// A second runner connection replaces the first. Two runners
		// answering one workspace would each be serving a different worktree,
		// so the old one is told exactly why it lost the workspace.
		previous.SendFinal(workspacesession.ErrorFrame("", "", workspacesession.CodeSuperseded, "Another runner connection took this workspace"),
			workspacesession.CodeSuperseded)
		// The PTYs being recorded were on the connection that just lost the
		// workspace, so their recordings are complete.
		w.Relay.FailWorkspaceExecRuns(ctx, record.ID, workspacesession.RunReasonStreamClosed)
	}
	w.CloseIfEndedSinceUpgrade(ctx, connection)
	socketCtx, cancel := context.WithCancel(ctx)
	var helpers sync.WaitGroup
	defer helpers.Wait()
	defer cancel()
	helpers.Go(func() { connection.WriteLoop(socketCtx) })
	helpers.Go(func() {
		select {
		case <-connection.Done:
			cancel()
		case <-socketCtx.Done():
		}
	})
	w.readRunner(socketCtx, connection)
	connection.Close("runner_closed")
	// detachRunner fails whatever runs this connection was still serving, so a
	// runner that went away mid-build leaves a failed run rather than one that
	// says running for ever.
	w.Relay.DetachRunner(ctx, connection)
	w.closeRelaySocket(socket, connection.Reason())
	return nil
}

// readRunner is the runner side's read loop. Runner frames are routed to the
// one person connection that owns the stream they name; there is no broadcast,
// because a stream belongs to one connection.
func (w *HTTP) readRunner(ctx context.Context, connection *Connection) {
	for {
		frame, ok := w.readFrame(ctx, connection)
		if !ok {
			return
		}
		w.HandleRunnerFrame(ctx, connection, frame)
	}
}

func (w *HTTP) HandleRunnerFrame(ctx context.Context, connection *Connection, frame workspacesession.Frame) {
	if frame.Type == "" {
		return
	}
	if frame.Stream == "" {
		// A runner frame with no stream has no audience: the hub does not
		// know who asked, and guessing would show one person another's
		// output.
		return
	}
	stream, person := w.Relay.RunnerStream(connection, frame.Stream)
	if stream == nil {
		return
	}
	if frame.Type == workspacesession.TypeAck {
		return
	}
	outbound := frame
	outbound.Actor = nil
	outbound.Seq = w.Relay.NextToPerson(stream)
	// A hub stream has no person to deliver to and none to acknowledge, so
	// nothing is buffered on it for a replay nobody can ask for.
	// Buffering would make a long build overflow a stream with no
	// reader behind it and kill the very run the hub started.
	overflow := false
	if !stream.HubOwned {
		overflow = w.Relay.BufferForPerson(connection.WorkspaceID, stream, outbound, outbound.Seq)
	}
	if stream.Channel == workspacesession.ChannelExec && frame.Type == workspacesession.TypeExecOutput {
		// A person can close the tab as soon as this output reaches them.
		// Collect it before delivery so teardown cannot finalize the run
		// without output the person already received.
		w.RecordExecFrame(ctx, connection, stream, frame)
	}
	if person != nil {
		person.Send(outbound)
	}
	if overflow {
		// The reader stopped acknowledging and the buffer passed its cap.
		// Closing the stream is the contract's answer: the alternative is
		// the hub growing without bound for one reader's convenience.
		w.Relay.CloseStream(ctx, connection.WorkspaceID, stream.ID)
		if person != nil {
			person.Send(workspacesession.ErrorFrame(stream.Channel, stream.ID, workspacesession.CodeOverflow, relayCodeMessage(workspacesession.CodeOverflow)))
		}
		connection.Send(workspacesession.Frame{Channel: stream.Channel, Stream: stream.ID, Type: workspacesession.TypeClose})
		return
	}
	if stream.Channel == workspacesession.ChannelTerminal {
		// The hub is not only forwarding a terminal stream, it is recording
		// it. A recording is the only thing a closed tab
		// leaves behind, and an owner auditing who was in a worktree has
		// nothing else to read.
		w.recordRunnerTerminalFrame(connection, stream, frame)
	}
	if stream.Channel == workspacesession.ChannelExec && frame.Type != workspacesession.TypeExecOutput {
		// The hub is not only forwarding an exec stream, it is recording
		// it: the run row is what a person comes back to after the tab is
		// closed, and it is the only record a run nobody watched leaves.
		w.RecordExecFrame(ctx, connection, stream, frame)
	}
	if frame.Type == workspacesession.TypeClosed || frame.Type == workspacesession.TypeClose {
		w.Relay.CloseStream(ctx, connection.WorkspaceID, stream.ID)
	}
}

// Recording action runs from relayed frames.
//
// The hub forwards the exec channel like any other, and it also reads it. That
// is the whole reason a run is a row before it is a process: the person who
// asked may close the tab, and a run-on-worktree-creation run has no person at
// all, so the record cannot be the client's.

// startExecRun claims the run a person's run frame names and marks it running.
// It answers "" when the frame may be forwarded, and a relay error code
// otherwise.
//
// Every field of the frame is checked against the row rather than trusted: the
// run must belong to this workspace, name the action the frame names, still be
// queued, and carry the command the hub stored. The last one is the one worth
// spelling out -- the runner validates the command against the action it was
// handed, and this is the check that makes "the action it was handed" mean the
// project's command rather than one a client composed.
func (w *HTTP) StartExecRun(ctx context.Context, connection, runner *Connection, record workspacehub.Record, stream *Stream, frame workspacesession.Frame) string {
	var payload workspacesession.ExecRun
	if err := json.Unmarshal(frame.Payload, &payload); err != nil {
		return workspacesession.CodeInvalidFrame
	}
	state, _, code := w.ClaimRelayExecRun(ctx, record, runner, payload, stream.ID)
	if code != "" {
		return code
	}
	if !w.Relay.BeginExecRun(connection.WorkspaceID, stream.ID, state, runner) {
		w.FailExecRuns(ctx, []*ExecRun{state}, workspacesession.RunReasonStreamClosed)
		w.Relay.CloseStream(ctx, connection.WorkspaceID, stream.ID)
		return workspacesession.CodeStaleExecution
	}
	return ""
}

// recordExecFrame reads one runner-originated exec frame into the run's
// record: output accumulates in memory, and exited is what writes the terminal
// row.
func (w *HTTP) RecordExecFrame(ctx context.Context, connection *Connection, stream *Stream, frame workspacesession.Frame) {
	switch frame.Type {
	case workspacesession.TypeExecOutput:
		var payload workspacesession.ExecOutput
		if err := json.Unmarshal(frame.Payload, &payload); err != nil {
			return
		}
		data := []byte(payload.Data)
		if payload.Encoding == "base64" {
			// A command that writes binary to its stdout sends its spans
			// encoded, and the stored output is the bytes rather than the
			// envelope: a reader downloading the output wants what the command
			// wrote.
			decoded, err := base64.StdEncoding.DecodeString(payload.Data)
			if err != nil {
				return
			}
			data = decoded
		}
		w.Relay.AppendExecOutput(connection.WorkspaceID, stream.ID, data)
	case workspacesession.TypeExecExited:
		var payload workspacesession.ExecExited
		if err := json.Unmarshal(frame.Payload, &payload); err != nil {
			return
		}
		state, ok := w.Relay.TakeExecRun(connection.WorkspaceID, stream.ID)
		if !ok {
			// The run was already ended by a teardown path, or this stream
			// never carried one. Either way there is nothing to write, and
			// writing over a terminal status is exactly what is forbidden.
			return
		}
		code := payload.Code
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), WriteTimeout)
		defer cancel()
		w.finishExecRun(writeCtx, state, workspacesession.RunStatusForExit(code), &code, "")
	}
}

// failExecRuns ends every run it is given with one reason and the output
// collected so far. It is what every stream and connection teardown calls: a
// run left running for ever is the bug this function exists to prevent.
//
// The write deliberately drops the caller's cancellation and keeps only its
// values. The events that reveal a run can never exit -- a socket closing, a
// request finishing, a workspace ending -- are the same events that cancel the
// contexts they arrive on, so a write that honoured the cancellation would be
// abandoned exactly when it mattered and the run would stay running for ever.
// It keeps a timeout of its own so a wedged database cannot hold the teardown.
func (w *HTTP) FailExecRuns(ctx context.Context, states []*ExecRun, reason string) {
	for _, state := range states {
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), WriteTimeout)
		w.finishExecRun(writeCtx, state, workspacesession.RunFailed, nil, reason)
		cancel()
	}
}

// finishExecRun writes a run's terminal status and its accumulated output.
//
// A run already in a terminal status is left alone: two teardown paths may
// race for the same run -- the person's socket closing while the workspace
// ends -- and the first answer is the true one.
func (w *HTTP) finishExecRun(ctx context.Context, state *ExecRun, status string, exitCode *int, reason string) {
	if state == nil {
		return
	}
	output, err := w.PrepareRelayOutput(ctx, state.Organization, state.Output)
	if err != nil {
		w.Logger.Warn("action_run.not_recorded", "run_id", state.RunID, "status", status, "error", err)
		return
	}
	input := postgres.FinishRelayActionRunInput{OrganizationID: state.Organization, ProjectID: state.Project, ID: state.RunID, WorkspaceID: state.WorkspaceID,
		RunnerID: &state.RunnerID, LeaseID: state.LeaseID, FencingToken: state.FencingToken, Claimant: state.Claimant,
		ExpectedRevision: state.Revision, ExpectedOutputSequence: state.OutputSequence, Status: status, Reason: reason,
		FinishedAt: w.now(), Truncated: state.Truncated}
	input.ExitCode, err = ActionRunExitCode(exitCode)
	if err != nil {
		w.Logger.Warn("action_run.not_recorded", "run_id", state.RunID, "status", status, "error", err)
		return
	}
	if output.Key != "" {
		input.OutputObjectKey = &output.Key
		input.OutputSha256 = output.SHA256
		input.OutputBytes = output.CompressedSize
		input.OutputContentBytes = output.UncompressedSize
	}
	var finished postgres.ActionRun
	err = w.AccessWrite(ctx, state.Organization, func(writer tenantstore.Writer) error {
		owner, ok := writer.(postgres.RelayActionWriter)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		if reason == "" {
			if _, err := w.ReadRelayRunnerAuthority(ctx, writer, hubscope.ScopeFromAuthInput(state.RunnerScope), state.LeaseID, w.now()); err != nil {
				var refusal *hubapi.NativeError
				if !errors.Is(err, corestore.ErrNotFound) && (!errors.As(err, &refusal) || refusal.Status >= 500) {
					return err
				}
				input.Status, input.ExitCode, input.Reason = workspacesession.RunFailed, pgtype.Int4{}, workspacesession.RunReasonStreamClosed
			}
		}
		var err error
		finished, err = owner.FinishRelayActionRun(ctx, input)
		if errors.Is(err, corestore.ErrRevisionConflict) && input.Reason == "" {
			current, readErr := owner.ActionRun(ctx, postgres.ActionRunInput{OrganizationID: state.Organization, ProjectID: state.Project, ID: state.RunID})
			if readErr != nil {
				return readErr
			}
			if current.Status != "running" || current.Revision != state.Revision || current.OutputSequence != state.OutputSequence || current.WorkspaceID != state.WorkspaceID || current.ClaimedBy != state.Claimant || current.LeaseID != state.LeaseID || current.FencingToken != state.FencingToken {
				return err
			}
			lease, readErr := writer.Lease(ctx, state.LeaseID)
			if readErr != nil {
				return readErr
			}
			workspaces, ok := writer.(postgres.WorkspaceReader)
			if !ok {
				return hubapi.NativeWorkItemStorageUnavailable()
			}
			workspace, readErr := workspaces.WorkspaceSession(ctx, state.Project, state.WorkspaceID)
			if readErr != nil {
				return readErr
			}
			if lease.ReleasedAt != nil || !lease.ExpiresAt.After(w.now()) {
				input.Reason = workspacesession.RunReasonLeaseLost
			} else if workspacesession.Terminal(workspace.State) {
				input.Reason = w.ActionRunReason(workspace.Reason)
			} else {
				return err
			}
			input.Status, input.ExitCode = workspacesession.RunFailed, pgtype.Int4{}
			finished, err = owner.FinishRelayActionRun(ctx, input)
		}
		if err != nil {
			return err
		}
		if output.Key != "" {
			return RetainRelayArchive(ctx, writer, state.Organization, state.Project, state.RunID, "project-action-run-output", output, input.FinishedAt)
		}
		return nil
	})
	if errors.Is(err, corestore.ErrRevisionConflict) {
		return
	}
	if err != nil {
		w.Logger.Warn("action_run.not_recorded", "run_id", state.RunID, "status", status, "error", err)
		return
	}
	w.Logger.Info("action_run.finished", "run_id", finished.ID, "action_id", finished.ActionID,
		"workspace_id", finished.WorkspaceID, "status", finished.Status, "reason", finished.Reason,
		"output_bytes", finished.OutputContentBytes, "truncated", finished.Truncated)
}

// Audit rows.

// openRelayAudit writes the connection's workspace_relay_sessions row.
func (w *HTTP) OpenRelayAudit(ctx context.Context, record workspacehub.Record, connection *Connection, principal Principal) error {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	connection.AuditID = hubapi.NewID("relaysession")
	err := w.AccessWrite(ctx, record.OrganizationID, func(writer tenantstore.Writer) error {
		owner, ok := writer.(postgres.RelayActionWriter)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		_, err := owner.CreateRelaySession(ctx, postgres.CreateRelaySessionInput{
			OrganizationID: record.OrganizationID, ProjectID: record.ProjectID, WorkspaceID: record.ID,
			ID: connection.AuditID, PrincipalID: principal.PrincipalID, Subject: principal.Subject,
			ConnectionID: connection.ID, Channels: []string{}, OpenedAt: w.now(), SupportReason: principal.SupportReason,
		})
		return err
	})
	if err != nil {
		return fmt.Errorf("open relay session audit: %w", err)
	}
	return nil
}

// closeRelayAudit completes the row with the reason and the byte counters.
func (w *HTTP) CloseRelayAudit(ctx context.Context, record workspacehub.Record, connection *Connection, reason string) {
	if connection.AuditID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	in, out := connection.Counters()
	now := w.now()
	err := w.AccessWrite(ctx, record.OrganizationID, func(writer tenantstore.Writer) error {
		owner, ok := writer.(postgres.RelayActionWriter)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		return owner.CloseRelaySession(ctx, postgres.CloseRelaySessionInput{
			OrganizationID: record.OrganizationID, ProjectID: record.ProjectID,
			ID: connection.AuditID, ConnectionID: connection.ID, ClosedAt: &now,
			CloseReason: reason, BytesIn: in, BytesOut: out,
		})
	})
	if err != nil && !errors.Is(err, corestore.ErrNotFound) {
		w.Logger.Warn("workspace.relay_audit_not_closed", "connection_id", connection.ID, "error", err)
	}
}

func WorkspaceRecord(row postgres.WorkspaceSession) workspacehub.Record {
	var capabilities *workspacesession.Capabilities
	if row.Capabilities != nil {
		value := workspacesession.CapabilitiesFrom(row.Capabilities)
		capabilities = &value
	}
	return workspacehub.Record{ID: row.ID, OrganizationID: row.OrganizationID, ProjectID: row.ProjectID, SubjectWorkItemID: row.SubjectWorkItemID,
		AttemptID: row.AttemptID, Ref: row.Ref, HeadSHA: row.HeadSHA, RunnerID: row.RunnerID, MachineID: tracker.MachineID(row.MachineID),
		MachineHostname: row.MachineHostname, WorktreePath: row.WorktreePath, LeaseID: tracker.LeaseID(row.LeaseID), FencingToken: tracker.FencingToken(row.FencingToken),
		State: row.State, Reason: row.Reason, Requires: row.Requires, Capabilities: capabilities, Isolation: row.Isolation, Worktree: row.Worktree, ReadOnly: row.ReadOnly,
		IdleTimeoutSeconds: int(row.IdleTimeoutSeconds), ExpiresAt: row.ExpiresAt, RequestedExpiresAt: row.RequestedExpiresAt, OpenedAt: row.OpenedAt,
		LastActivityAt: row.LastActivityAt, LastHeartbeatAt: row.LastHeartbeatAt, RebindDeadline: row.RebindDeadline, CreatedBy: row.CreatedBy,
		Revision: row.Revision, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
}

func (w *HTTP) ReadRelayRunnerAuthority(ctx context.Context, reader tenantstore.Reader, scope hubscope.NativeScope, leaseID string, now time.Time) (runnerstore.Identity, error) {
	if scope.Credential.EntryKey != nil || scope.Credential.Runner.RunnerID == "" || scope.Credential.Runner.OrganizationID != scope.Organization {
		return runnerstore.Identity{}, hubscope.RunnerUnauthorized()
	}
	credential, err := reader.AccessCredential(ctx, scope.Credential.ID)
	if err != nil {
		return runnerstore.Identity{}, err
	}
	identity, err := w.RunnerForLease(ctx, reader, leaseID)
	if err != nil {
		return runnerstore.Identity{}, err
	}
	if credential.OrganizationID != scope.Organization || credential.Scope != string(hubapi.APIScopeWorker) || credential.Kind != "service" || !credential.Valid(scope.Credential.Hash, now) || !w.RunnerMatchesLease(identity, scope.Organization, scope.Credential.Runner.RunnerID, credential.ID, scope.Credential.Runner.MachineID, now) {
		return runnerstore.Identity{}, hubscope.RunnerUnauthorized()
	}
	allowed, err := reader.RunnerProjectAuthorized(ctx, identity.RunnerID, scope.Project, now)
	if err != nil {
		return runnerstore.Identity{}, err
	}
	if !allowed {
		return runnerstore.Identity{}, hubapi.NativeNotFound()
	}
	if identity.State == "disabled" {
		return runnerstore.Identity{}, w.RunnerExcluded([]runnerauth.Exclusion{{Code: "runner_disabled", Message: "Runner is disabled"}})
	}
	return identity, nil
}

func (w *HTTP) ReadRelayWorker(ctx context.Context, reader tenantstore.Reader, scope hubscope.NativeScope, id string, claim WorkerIdentity) (workspacehub.Record, error) {
	if workspacesession.ValidateID(id) != nil {
		return workspacehub.Record{}, hubapi.NativeNotFound()
	}
	if strings.TrimSpace(string(claim.LeaseID)) == "" || claim.FencingToken <= 0 {
		return workspacehub.Record{}, hubapi.NativePublicInvalid("A lease and fencing token are required")
	}
	workspaces, ok := reader.(postgres.WorkspaceReader)
	if !ok {
		return workspacehub.Record{}, hubapi.NativeWorkItemStorageUnavailable()
	}
	found, err := workspaces.WorkspaceSession(ctx, scope.Project, id)
	if err != nil {
		return workspacehub.Record{}, hubapi.ProjectRecordError(err)
	}
	identity, err := w.ReadRelayRunnerAuthority(ctx, reader, scope, string(claim.LeaseID), w.now())
	if err != nil {
		return workspacehub.Record{}, err
	}
	lease, err := reader.Lease(ctx, string(claim.LeaseID))
	if err != nil {
		return workspacehub.Record{}, hubapi.ProjectRecordError(err)
	}
	if lease.FencingToken != int64(claim.FencingToken) || lease.ReleasedAt != nil || !lease.ExpiresAt.After(w.now()) {
		return workspacehub.Record{}, hubapi.NativeStaleExecution("The workspace lease is no longer current")
	}
	if identity.MachineID != tracker.MachineID(lease.MachineID) {
		return workspacehub.Record{}, hubapi.NativeNotFoundAt("lease_runner_credential")
	}

	if found.RunnerID != identity.RunnerID {
		return workspacehub.Record{}, hubapi.NativeStaleExecution("The workspace belongs to another runner")
	}
	owner, ok := reader.(postgres.RelayActionReader)
	if !ok {
		return workspacehub.Record{}, hubapi.NativeWorkItemStorageUnavailable()
	}
	found, err = owner.WorkspaceRelayOwner(ctx, postgres.WorkspaceRelayOwnerInput{OrganizationID: scope.Organization, ProjectID: scope.Project, ID: id, RunnerID: identity.RunnerID, MachineID: string(identity.MachineID), LeaseID: lease.LeaseID, FencingToken: lease.FencingToken, Now: w.now()})
	if errors.Is(err, corestore.ErrNotFound) {
		return workspacehub.Record{}, hubapi.NativeStaleExecution("The lease does not hold this workspace's work item")
	}
	return WorkspaceRecord(found), err
}

func (w *HTTP) ClaimRelayExecRun(ctx context.Context, record workspacehub.Record, runner *Connection, payload workspacesession.ExecRun, claimant string) (*ExecRun, string, string) {
	if runner == nil || runner.Scope.Organization != record.OrganizationID || runner.Scope.Project != record.ProjectID {
		return nil, "", workspacesession.CodeStaleExecution
	}
	var run postgres.ActionRun
	var workspace workspacehub.Record
	err := w.AccessRead(ctx, record.OrganizationID, func(reader tenantstore.Reader) error {
		var err error
		workspace, err = w.ReadRelayWorker(ctx, reader, hubscope.ScopeFromAuthInput(runner.Scope), record.ID, WorkerIdentity{LeaseID: tracker.LeaseID(runner.SessionID), FencingToken: tracker.FencingToken(runner.Owner.FencingToken)})
		if err != nil {
			return err
		}
		if !workspacesession.Bound(workspace.State) {
			return hubapi.NativeStaleExecution("The workspace is not bound to a runner")
		}
		owner, ok := reader.(postgres.RelayActionReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		run, err = owner.ActionRun(ctx, postgres.ActionRunInput{OrganizationID: record.OrganizationID, ProjectID: record.ProjectID, ID: strings.TrimSpace(payload.RunID)})
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) || err == nil && run.WorkspaceID != record.ID {
		return nil, "", workspacesession.CodeNotFound
	}
	if err != nil {
		w.Logger.Warn("action_run.not_started", "workspace_id", record.ID, "run_id", payload.RunID, "error", err)
		return nil, "", workspacesession.CodeStaleExecution
	}
	ref := attachment.ArchiveReference{Key: run.CommandObjectKey, SHA256: run.CommandSha256, CompressedSize: run.CommandBytes, UncompressedSize: run.CommandContentBytes, Codec: "gzip", SchemaVersion: 1, Kind: "archive", Retention: attachment.ArchiveRetention}
	if ref.Validate(string(record.OrganizationID)) != nil || w.RunDetailStore() == nil {
		return nil, "", workspacesession.CodeStaleExecution
	}
	command, err := w.RunDetailStore().ReadDetail(ctx, string(record.OrganizationID), ref)
	if err != nil {
		w.Logger.Warn("action_run.command_not_read", "workspace_id", record.ID, "run_id", payload.RunID, "error", err)
		return nil, "", workspacesession.CodeStaleExecution
	}
	if run.ActionID != strings.TrimSpace(payload.ActionID) || string(command) != payload.Command {
		return nil, "", workspacesession.CodeForbidden
	}
	if run.Status != workspacesession.RunQueued || run.ClaimedBy != "" {
		return nil, "", workspacesession.CodeAlreadyRunning
	}
	err = w.AccessWrite(ctx, record.OrganizationID, func(writer tenantstore.Writer) error {
		if _, err := w.ReadRelayRunnerAuthority(ctx, writer, hubscope.ScopeFromAuthInput(runner.Scope), string(workspace.LeaseID), w.now()); err != nil {
			return err
		}
		owner, ok := writer.(postgres.RelayActionWriter)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		var err error
		id := run.ID
		run, err = owner.ClaimActionRun(ctx, postgres.ClaimActionRunInput{OrganizationID: record.OrganizationID, ProjectID: record.ProjectID, ID: run.ID, RunnerID: workspace.RunnerID, LeaseID: string(workspace.LeaseID), FencingToken: int64(workspace.FencingToken), StartedAt: new(w.now()), Claimant: claimant})
		if errors.Is(err, corestore.ErrRevisionConflict) {
			current, readErr := owner.ActionRun(ctx, postgres.ActionRunInput{OrganizationID: record.OrganizationID, ProjectID: record.ProjectID, ID: id})
			if readErr != nil {
				return readErr
			}
			if current.Status != workspacesession.RunQueued || current.ClaimedBy != "" {
				return w.ActionRunClaimed
			}
		}
		return err
	})
	if errors.Is(err, w.ActionRunClaimed) {
		return nil, "", workspacesession.CodeAlreadyRunning
	}
	if err != nil {
		w.Logger.Warn("action_run.not_started", "workspace_id", record.ID, "run_id", payload.RunID, "error", err)
		return nil, "", workspacesession.CodeStaleExecution
	}
	return &ExecRun{RunID: run.ID, ActionID: run.ActionID, Organization: run.OrganizationID, Project: run.ProjectID, WorkspaceID: run.WorkspaceID, RunnerID: workspace.RunnerID, LeaseID: run.LeaseID, FencingToken: run.FencingToken, Claimant: run.ClaimedBy, Revision: run.Revision, OutputSequence: run.OutputSequence, RunnerScope: runner.Scope}, string(command), ""
}

func (w *HTTP) ReadRelayWorkspaceForActor(ctx context.Context, reader tenantstore.Reader, scope hubscope.NativeScope, id string) (workspacehub.Record, error) {
	if err := w.AuthorizeAccessScope(ctx, reader, scope, false); err != nil {
		return workspacehub.Record{}, err
	}
	if workspacesession.ValidateID(id) != nil {
		return workspacehub.Record{}, hubapi.NativeNotFound()
	}
	owner, ok := reader.(postgres.WorkspaceReader)
	if !ok {
		return workspacehub.Record{}, hubapi.NativeWorkItemStorageUnavailable()
	}
	row, err := owner.WorkspaceSession(ctx, scope.Project, id)
	if err != nil {
		return workspacehub.Record{}, hubapi.ProjectRecordError(err)
	}
	if err := workspacehub.ReadableSubject(ctx, reader, scope, row.SubjectWorkItemID); err != nil {
		return workspacehub.Record{}, hubapi.ProjectRecordError(err)
	}
	return WorkspaceRecord(row), nil
}

func (w *HTTP) PrepareRelayOutput(ctx context.Context, organization tracker.OrganizationID, data []byte) (attachment.ArchiveReference, error) {
	if len(data) == 0 {
		return attachment.ArchiveReference{}, nil
	}
	outputCtx := hubcontent.IsolateContentContext(w.ContentContext(ctx))
	if content, ok := outputCtx.Value(hubcontent.ChangeContentKey{}).(*hubcontent.ChangeContent); ok {
		content.Mu.Lock()
		content.Completion = true
		content.Mu.Unlock()
		if content.Call != nil {
			if err := hubmaintenance.ReserveArchiveWrite(ctx, string(organization), int64(len(data))); err != nil {
				return attachment.ArchiveReference{}, err
			}
		}
	}
	return w.PrepareMeteredContent(outputCtx, string(organization), data, false)
}

func ActionRunExitCode(value *int) (pgtype.Int4, error) {
	var code pgtype.Int4
	if value != nil {
		if err := code.Scan(int64(*value)); err != nil {
			return code, hubapi.NativePublicInvalid("exit_code exceeds storage bounds")
		}
	}
	return code, nil
}

func RetainRelayArchive(ctx context.Context, writer tenantstore.Writer, organization tracker.OrganizationID, project tracker.ProjectID, id, kind string, ref attachment.ArchiveReference, at time.Time) error {
	if ref.Validate(string(organization)) != nil {
		return artifact.ErrIntegrity
	}
	if err := writer.LockArchiveAccounting(ctx); err != nil {
		return err
	}
	object, err := writer.RegisterArchiveObject(ctx, artifactstore.ArchiveObject{OrganizationID: organization, ID: hubapi.NewID("archive"), Reference: ref, CreatedAt: at})
	if err != nil {
		return err
	}
	if kind == "project-action-run-output" || kind == "workspace-terminal-recording" {
		usage, err := writer.ChargeArchiveObject(ctx, object.ID, math.MaxInt64)
		if errors.Is(err, corestore.ErrNotFound) {
			return hubapi.AttachmentAccountingUnavailable()
		}
		if err != nil {
			return err
		}
		if !usage.Allowed {
			return &hostedplans.LimitError{Resource: "collaboration_bytes", Allowance: math.MaxInt64, Consumption: usage.Used}
		}
	}
	return writer.AddArchiveOwner(ctx, artifactstore.ArchiveOwner{OrganizationID: organization, ArchiveID: object.ID, Kind: kind, ID: actionstore.ArchiveOwnerID(project, id), CreatedAt: at})
}

func (w *HTTP) RelayArchiveReference(ctx context.Context, organization tracker.OrganizationID, expected attachment.ArchiveReference) (attachment.ArchiveReference, error) {
	var actual attachment.ArchiveReference
	err := w.AccessRead(ctx, organization, func(reader tenantstore.Reader) error {
		owner, ok := reader.(hubrecords.AttemptArchiveReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		object, err := owner.ArchiveObjectByKey(ctx, expected.Key)
		if err != nil {
			return err
		}
		actual = object.Reference
		if object.OrganizationID != organization || actual.Validate(string(organization)) != nil || actual.Key != expected.Key || actual.SHA256 != expected.SHA256 || actual.CompressedSize != expected.CompressedSize || actual.UncompressedSize != expected.UncompressedSize {
			return artifact.ErrIntegrity
		}
		return nil
	})
	return actual, err
}

func WorkerIdentityFromHeaders(c echo.Context) (WorkerIdentity, error) {
	lease := strings.TrimSpace(c.Request().Header.Get("X-Detent-Lease"))
	token := strings.TrimSpace(c.Request().Header.Get("X-Detent-Fencing-Token"))
	if lease == "" || token == "" {
		return WorkerIdentity{}, hubapi.NativePublicInvalid("A lease and fencing token header are required")
	}
	var fencing int64
	if _, err := fmt.Sscanf(token, "%d", &fencing); err != nil || fencing <= 0 {
		return WorkerIdentity{}, hubapi.NativePublicInvalid("The fencing token header is invalid")
	}
	return WorkerIdentity{LeaseID: tracker.LeaseID(lease), FencingToken: tracker.FencingToken(fencing)}, nil
}

// relayEndReason reports whether a workspace in this state can no longer serve
// a relay connection, and the code the connection is closed with.
//
// A terminal workspace serves nobody. An unreachable or re-requested one has
// lost the runner that was serving its streams, which ends the runner's
// connection and every stream a person held on it; a person may still connect
// to one afterwards and is answered per frame that no runner is serving.
func EndReason(state string, servingRunner bool) (string, bool) {
	switch {
	case workspacesession.Terminal(state):
		return workspacesession.CodeWorkspaceClosed, true
	case servingRunner && (state == workspacesession.StateUnreachable || state == workspacesession.StateRequested):
		return workspacesession.CodeStaleExecution, true
	}
	return "", false
}

// closeIfEndedSinceUpgrade closes a connection whose workspace ended between
// the upgrade's state check and the connection joining the relay.
//
// committed() closes a workspace's connections after the ending transaction
// commits, and it can only close the connections already attached. A
// connection that passed its check before the commit and attached after the
// close would otherwise sit on a workspace that is over, with nothing ever
// telling it so. Re-reading the row after attaching closes that window: either
// the close ran after the attach and found the connection, or the attach ran
// after the commit and this read sees the ended state.
func (w *HTTP) CloseIfEndedSinceUpgrade(ctx context.Context, connection *Connection) {
	var record postgres.WorkspaceSession
	err := w.AccessRead(ctx, connection.Scope.Organization, func(reader tenantstore.Reader) error {
		owner, ok := reader.(postgres.WorkspaceReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		var err error
		record, err = owner.WorkspaceSession(ctx, connection.Scope.Project, connection.WorkspaceID)
		return err
	})
	if err != nil {
		// The periodic authority re-check reads the same row and closes the
		// connection when it can, so a failed read here is logged, not fatal.
		w.Logger.Warn("workspace.relay_attach_recheck_failed", "workspace_id", connection.WorkspaceID,
			"connection_id", connection.ID, "error", err)
		return
	}
	if reason, ends := EndReason(record.State, connection.Runner); ends {
		w.Relay.CloseWorkspace(ctx, record.ID, reason)
	}
}
