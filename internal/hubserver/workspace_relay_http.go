package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/coder/websocket"
	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

// The relay's two sockets (decisions section 18.2): the person's, opened with a
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
	socket.SetReadLimit(relayReadLimit)
	return socket, nil
}

// openWorkspaceRelay implements GET .../workspaces/:workspace/relay?ticket=.
func (s *Service) openWorkspaceRelay(c echo.Context) error {
	service, err := s.requireWorkspaces()
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if err := s.requireRelayOrigin(c); err != nil {
		return s.nativeAPIError(c, err)
	}
	principal, err := s.relayPrincipalFor(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	ticket := strings.TrimSpace(c.QueryParam("ticket"))
	if ticket == "" {
		return s.nativeAPIError(c, nativeInvalid("A relay ticket is required"))
	}
	scope := nativeRequestScope(c)
	ctx := c.Request().Context()
	var record workspaceRecord
	err = s.hubTransact(ctx, func(tx *sql.Tx, now time.Time) error {
		found, err := service.readWorkspaceForActor(ctx, tx, scope, c.Param("workspace"))
		if err != nil {
			return err
		}
		if workspacesession.Terminal(found.State) {
			return nativeStaleExecution("The workspace has ended")
		}
		redeemed, err := redeemRelayTicket(ctx, tx, ticket, found.ID, now)
		if err != nil {
			return err
		}
		// The ticket is bound to the session that minted it, so a ticket
		// carried to a different browser buys nothing: the connection must
		// present both.
		if redeemed.SessionID != principal.sessionID || redeemed.PrincipalID != principal.principalID {
			return nativeNotFound()
		}
		record = found
		return nil
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	socket, err := acceptRelaySocket(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	connection := &relayConnection{
		id: newNativeID("relayconn"), workspaceID: record.ID, socket: socket,
		principalID: principal.principalID, sessionID: principal.sessionID,
		out: make(chan relayOutbound, relayWriteQueue), done: make(chan struct{}),
	}
	if principal.credential.Hosted != nil {
		connection.subject = principal.credential.Hosted.Subject
	}
	connection.sessionHash = principal.credential.SessionHash
	connection.supportReason = principal.supportReason
	connection.hostedRole = principal.credential.HostedRole
	// The actor is resolved once here rather than per frame. It changes only
	// when the principal does, and a principal change closes the connection
	// (revalidatePerson), so a per-frame lookup would be a database read per
	// keystroke that could never return a different answer.
	connection.actor = s.relayActor(ctx, connection, principal)
	return service.servePerson(c, scope, record, connection, principal)
}

// relayActor is the stamp every person-originated frame carries: who asked,
// from which connection, and -- for the git channel -- who a commit is
// authored as (section 18.13).
//
// The name and the email are the hub's to supply for the same reason the rest
// of the tuple is: a client that could name its own author could attribute a
// commit to anybody, and a commit's author line is permanent in the
// repository's history in a way a relay audit row is not.
func (s *Service) relayActor(ctx context.Context, connection *relayConnection, principal relayPrincipal) workspacesession.Actor {
	actor := workspacesession.Actor{
		PrincipalID: principal.principalID, Subject: principal.subject, ConnectionID: connection.id,
	}
	if principal.credential.Hosted == nil {
		// An operator token is not a person. It gets the token's name so a
		// runner's log says which credential acted, and no email, which is
		// what makes the runner refuse a commit from it with forbidden. That
		// is the intended outcome rather than a gap: a commit authored by a
		// machine credential names nobody who could answer for it.
		actor.Name = principal.credential.Name
		return actor
	}
	// principal.subject, not credential.Hosted.Subject: on a support session
	// the two differ, and the author has to be whoever is actually acting.
	// A support actor holds no hosted_members row, so the lookup finds
	// nothing, the email stays empty and the runner refuses the commit --
	// which is the only correct answer, because attributing the commit to the
	// person being stood in for would put a name in the repository's history
	// that never typed it.
	var email string
	err := s.database.db.QueryRowContext(ctx,
		"SELECT email FROM hosted_members WHERE user_id = ? AND active = 1", principal.subject).Scan(&email)
	switch {
	case err == nil:
		actor.Email = strings.TrimSpace(email)
		actor.Name = relayActorName(actor.Email)
	case !errors.Is(err, sql.ErrNoRows):
		// A missing row is the support actor's expected answer, so only a
		// real failure is worth a line. Either way the connection opens: a
		// status is a read and must not depend on an author line.
		s.config.Logger.Warn("workspace.relay_actor_unresolved",
			"connection_id", connection.id, "error", err)
	}
	if actor.Name == "" {
		actor.Name = principal.subject
	}
	return actor
}

// relayActorName derives the display name a commit is authored under from the
// acting person's email address.
//
// It is derived rather than read because there is nothing to read: the hosted
// schema carries user_id, email, membership_id and role and no display name
// anywhere (migration 00017_hosted_identity), and /app/bootstrap's actor
// serves the email as the person's only human-readable name. The rejected
// alternative was to fabricate one -- a constant like "Detent user", or the
// opaque hosted subject -- and a fabricated name is worse here than a derived
// one, because it lands in a commit's author line and stays in the
// repository's history naming somebody who does not exist.
//
// The derivation is deliberately shallow: the local part, without the +tag a
// person added for their own routing, with its separators read as word breaks.
func relayActorName(email string) string {
	address := strings.TrimSpace(email)
	if address == "" {
		return ""
	}
	local := address
	if at := strings.LastIndex(address, "@"); at > 0 {
		local = address[:at]
	}
	if plus := strings.IndexByte(local, '+'); plus > 0 {
		local = local[:plus]
	}
	words := strings.FieldsFunc(local, func(r rune) bool { return r == '.' || r == '_' || r == '-' })
	parts := make([]string, 0, len(words))
	for _, word := range words {
		first, size := utf8.DecodeRuneInString(word)
		if first == utf8.RuneError {
			parts = append(parts, word)
			continue
		}
		parts = append(parts, string(unicode.ToUpper(first))+word[size:])
	}
	if len(parts) == 0 {
		// A local part of nothing but separators. The address itself is the
		// only honest answer left; an empty name would make the runner refuse
		// a commit this person is entitled to make.
		return address
	}
	return strings.Join(parts, " ")
}

// servePerson runs one person connection for its whole life: the audit row, the
// writer, the authority re-check and the read loop.
func (w *workspaceService) servePerson(c echo.Context, scope nativeScope, record workspaceRecord, connection *relayConnection, principal relayPrincipal) error {
	ctx := c.Request().Context()
	if err := w.openRelayAudit(ctx, record, connection, principal); err != nil {
		// The upgrade already happened, so there is no HTTP response left to
		// write an error into: the connection has been handed to the
		// WebSocket. Answering with nativeAPIError here would write a second
		// set of headers onto a hijacked connection, which the server reports
		// as a superfluous WriteHeader and the client never sees. Closing the
		// socket is the only way left to say no.
		//
		// And it does have to say no. Every person connection writes an audit
		// row (section 18.2); one that could not is a connection nobody could
		// later account for, which is exactly what the row exists to prevent.
		w.logger.Warn("workspace.relay_audit_unavailable", "workspace_id", record.ID,
			"connection_id", connection.id, "error", err)
		w.closeRelaySocket(connection.socket, "audit_unavailable")
		return nil
	}
	if err := w.relay.attachPerson(connection); err != nil {
		w.closeRelayAudit(context.WithoutCancel(ctx), connection, "server_shutdown")
		w.closeRelaySocket(connection.socket, "server_shutdown")
		return nil
	}
	socketCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go connection.writeLoop(socketCtx)
	go w.watchPersonAuthority(socketCtx, c, scope, record, connection)
	go func() {
		<-connection.done
		cancel()
	}()
	w.readPerson(socketCtx, connection, record)
	connection.close("client_closed")
	w.relay.detachPerson(ctx, connection, w.now())
	reason := connection.reason()
	w.closeRelayAudit(context.WithoutCancel(ctx), connection, reason)
	w.closeRelaySocket(connection.socket, reason)
	return nil
}

// closeRelaySocket ends a socket and records a close that failed. Every caller
// has already answered its own caller, so there is nobody left to return an
// error to; a close that fails still matters, because it means the peer never
// learned why its connection went away.
func (w *workspaceService) closeRelaySocket(socket *websocket.Conn, reason string) {
	if err := socket.Close(relayStatusFor(reason), relayCloseText(reason)); err != nil {
		w.logger.Debug("workspace.relay_socket_not_closed", "reason", reason, "error", err)
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

// watchPersonAuthority is section 18.2's 30-second periodic re-check. Together
// with the re-check on every person-originated frame it is how a membership,
// role, grant or session change closes a connection.
func (w *workspaceService) watchPersonAuthority(ctx context.Context, c echo.Context, scope nativeScope, record workspaceRecord, connection *relayConnection) {
	ticker := time.NewTicker(workspacesession.AuthorityRecheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-connection.done:
			return
		case <-ticker.C:
			if err := w.revalidatePerson(ctx, c, scope, record, connection); err != nil {
				connection.revoke()
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
func (w *workspaceService) revalidatePerson(ctx context.Context, c echo.Context, scope nativeScope, record workspaceRecord, connection *relayConnection) error {
	principal, err := w.server.relayPrincipal(ctx, c)
	if err != nil {
		return err
	}
	if principal.principalID != connection.principalID || principal.sessionID != connection.sessionID {
		// The cookie now names someone else, or a different session of the
		// same person. Either way this connection's authority is gone.
		return errors.New("the relay principal changed")
	}
	current := scope
	current.credential = principal.credential
	if err := w.server.requireHostedProject(ctx, w.server.database.db, current, false); err != nil {
		return err
	}
	live, err := readWorkspaceByID(ctx, w.server.database.db, record.ID)
	if err != nil {
		return err
	}
	if workspacesession.Terminal(live.State) {
		return errors.New("the workspace has ended")
	}
	if _, _, err := readNativeIssue(ctx, w.server.database.db, current, live.SubjectWorkItemID); err != nil {
		return err
	}
	return nil
}

// revalidatePersonLocally is the per-frame half of section 18.2's authority
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
func (w *workspaceService) revalidatePersonLocally(ctx context.Context, record workspaceRecord, connection *relayConnection) error {
	live, err := readWorkspaceByID(ctx, w.server.database.db, record.ID)
	if err != nil {
		return fmt.Errorf("read workspace: %w", err)
	}
	if !workspacesession.Bound(live.State) {
		return errors.New("the workspace is no longer bound to a runner")
	}
	if live.LeaseID != "" {
		var fencing tracker.FencingToken
		var expires string
		var released sql.NullString
		err := w.server.database.db.QueryRowContext(ctx,
			"SELECT fencing_token, expires_at, released_at FROM leases WHERE lease_id = ?",
			live.LeaseID).Scan(&fencing, &expires, &released)
		if err != nil {
			return fmt.Errorf("read workspace lease: %w", err)
		}
		expiresAt, err := parseTimeValue(expires)
		if err != nil {
			return fmt.Errorf("decode workspace lease expiry: %w", err)
		}
		if released.Valid || fencing != live.FencingToken || !expiresAt.After(w.now()) {
			// The workspace tuple no longer names the current lease, so
			// anything this frame asked for would run under a dead
			// generation.
			return errors.New("the workspace lease is no longer current")
		}
	}
	if connection.sessionHash == "" {
		// A token principal has no hosted session row; its authority is the
		// token, which the middleware already proved on the upgrade and which
		// is revoked by deleting it rather than by a row this check can read.
		return nil
	}
	var active int
	err = w.server.database.db.QueryRowContext(ctx, `SELECT count(*) FROM hosted_sessions s
JOIN hosted_members m ON m.user_id = ? AND m.active = 1
WHERE s.token_hash = ? AND s.revoked_at IS NULL AND julianday(s.expires_at) > julianday(?)`,
		connection.subject, connection.sessionHash, formatHubTime(w.now())).Scan(&active)
	if err != nil {
		return fmt.Errorf("read hosted session: %w", err)
	}
	if active == 0 {
		return errors.New("the hosted session or membership is no longer active")
	}
	var granted int
	err = w.server.database.db.QueryRowContext(ctx, `SELECT count(*) FROM hosted_project_grants g
JOIN hosted_members m ON m.user_id = g.user_id AND m.active = 1
WHERE g.user_id = ? AND g.organization_id = ? AND g.project_id = ?`,
		connection.subject, live.OrganizationID, live.ProjectID).Scan(&granted)
	if err != nil {
		return fmt.Errorf("read project grant: %w", err)
	}
	if granted == 0 {
		return errors.New("the project grant has been withdrawn")
	}
	return nil
}

// refuseTerminal is the terminal's whole per-frame authority (section 18.3),
// and it is longer than any other channel's because a terminal is the runner
// account's shell handed to a person and nothing else on this relay is.
//
// Section 18.3 lists the gate in order and this follows it in order:
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
// the reason section 18.2 gives about everything else: a socket that lives for
// an hour would otherwise outlive the grant that opened it, and for a terminal
// "outlive" means a person keeps typing into a worktree after their access was
// withdrawn. The `runners` grant is the one that is checked at creation too
// (authorizeRequires), and checking it again here is not redundant: a grant can
// be withdrawn while the shell is open.
//
// It answers "" when the frame may be forwarded and a relay error code
// otherwise.
func (w *workspaceService) refuseTerminal(ctx context.Context, record workspaceRecord, connection *relayConnection, frame workspacesession.Frame) string {
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
		// is what a recording's header declares as the window (section 18.3) ---
		// so an open it cannot parse is one it must refuse rather than forward:
		// forwarding it would spend a stream and start a recording claiming a
		// window nobody asked for, and the runner would refuse the frame a
		// moment later anyway.
		if _, err := decodeTerminalOpen(frame); err != nil {
			return workspacesession.CodeInvalidFrame
		}
	}
	if !w.config.Terminal.Enabled {
		return workspacesession.CodeForbidden
	}
	if record.ReadOnly {
		// A read-only workspace is one whose subject attempt is still running,
		// and section 18.1 refuses a terminal there outright: a person must not
		// type into a worktree the model is editing. read_only rather than
		// forbidden, because the request was never allowed rather than the
		// person being the wrong person.
		return workspacesession.CodeReadOnly
	}
	if connection.sessionHash == "" {
		// A token principal has no hosted session, no membership role and no
		// project grant row, so none of the checks below can be asked of it.
		// Its authority is the token, which the middleware already proved on
		// the upgrade against the same scope this frame needs --- the same
		// reasoning refuseGitWrite applies to a commit, which is the other
		// frame on this relay that changes a worktree.
		//
		// Section 18.3's "a terminal requires a hosted session" is enforced
		// where it can actually be enforced: authorizeRequires refuses a token
		// that asks for `requires: ["terminal"]`, so no token can open a
		// workspace with a terminal on it. What is left here is a token acting
		// on a workspace a person opened, on a project the token holds write
		// on, which is an organization-level secret acting within its scope.
		return ""
	}
	if w.config.Terminal.Isolation == workspacesession.IsolationUser &&
		connection.hostedRole != "owner" && connection.hostedRole != "admin" {
		// `user` isolation runs the shell as the runner's own account, with its
		// credential store and its other checkouts in reach. Section 18.3
		// allows it "only when the organization has set
		// workspaces.terminal.isolation: user explicitly and the person is an
		// owner or admin with the runners grant".
		return workspacesession.CodeForbidden
	}
	if connection.supportReason != "" && w.config.Terminal.Isolation != workspacesession.IsolationContainer {
		// A support actor gets a terminal "only with a support reason recorded
		// and only at container" (section 18.3). The reason is recorded on the
		// audit row and on the recording already; this is the other half.
		return workspacesession.CodeForbidden
	}
	var count int
	err := w.server.database.db.QueryRowContext(ctx, `SELECT count(*) FROM hosted_project_grants g
JOIN hosted_members m ON m.user_id = g.user_id AND m.active = 1
WHERE g.user_id = ? AND g.organization_id = ? AND g.project_id = ?
 AND g.can_write = 1 AND g.manage_runner = 1 AND m.role != 'viewer'`,
		connection.subject, record.OrganizationID, record.ProjectID).Scan(&count)
	if err != nil {
		// The grant cannot be read, so neither `write` nor the runners flag can
		// be proved. Forwarding would be handing out a shell on an authority
		// nobody established, so the refusal stands and the failure is logged
		// rather than resolved in the person's favour.
		w.logger.Warn("workspace.relay_terminal_grant_unreadable", "workspace_id", record.ID,
			"connection_id", connection.id, "error", err)
		return workspacesession.CodeForbidden
	}
	if count == 0 {
		// Either the grant is read-only, or it lacks the runners flag, or the
		// role is viewer. The viewer half mirrors requireHostedProject: a role
		// demotion must not be survivable by holding an old grant row, and
		// section 18.3 says outright that viewers never get a terminal.
		return workspacesession.CodeForbidden
	}
	return ""
}

// decodeTerminalOpen reads an open frame's window and working directory.
//
// An absent payload is not an error: section 18.3 lets a client that has not
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

// refuseGitWrite is the per-frame half of section 18.2's `write` rule, and it
// exists because the git channel is the first channel that writes. It reports
// the code the frame is refused with, or "" when it may be forwarded.
//
// channelPermitted already gates the channel on the capability the runner
// reported, and that check cannot answer this one: the capability says the
// worktree is a repository, not that this person may change it. A `status` is
// a read and needs nothing beyond the project read the connection proved on
// the upgrade; a `commit` or a `push` needs `write`, which is a different
// question asked per frame because a grant can be withdrawn under a live
// socket.
func (w *workspaceService) refuseGitWrite(ctx context.Context, record workspaceRecord, connection *relayConnection, frame workspacesession.Frame) string {
	if frame.Channel != workspacesession.ChannelGit || !workspacesession.GitWriteRequest(frame.Type) {
		return ""
	}
	if record.ReadOnly {
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
	if connection.sessionHash == "" {
		// A token principal has no hosted session and no project grant row;
		// its authority is the token, which the middleware already proved on
		// the upgrade against the same `write` scope this frame needs. There
		// is nothing here that could answer the question better, so the frame
		// is forwarded and the runner's own lease check is the next gate.
		return ""
	}
	var count int
	err := w.server.database.db.QueryRowContext(ctx, `SELECT count(*) FROM hosted_project_grants g
JOIN hosted_members m ON m.user_id = g.user_id AND m.active = 1
WHERE g.user_id = ? AND g.organization_id = ? AND g.project_id = ? AND g.can_write = 1 AND m.role != 'viewer'`,
		connection.subject, record.OrganizationID, record.ProjectID).Scan(&count)
	if err != nil {
		// The grant cannot be read, so `write` cannot be proved. Forwarding
		// the frame would be writing to a repository on an authority nobody
		// established, so the refusal stands and the failure is logged rather
		// than resolved in the person's favour.
		w.logger.Warn("workspace.relay_write_grant_unreadable", "workspace_id", record.ID,
			"connection_id", connection.id, "error", err)
		return workspacesession.CodeForbidden
	}
	if count == 0 {
		// The grant is read-only, or the role is viewer. The viewer half
		// mirrors requireHostedProject, which refuses a write to a viewer
		// however generous the grant row is: a role demotion must not be
		// survivable by holding an old can_write row.
		return workspacesession.CodeForbidden
	}
	return ""
}

// readPerson is the person side's read loop.
func (w *workspaceService) readPerson(ctx context.Context, connection *relayConnection, record workspaceRecord) {
	for {
		frame, ok := w.readFrame(ctx, connection)
		if !ok {
			return
		}
		w.handlePersonFrame(ctx, connection, record, frame)
	}
}

// readFrame reads and decodes one frame, answering a malformed one rather than
// closing: a client bug on one frame must not cost a person their session.
func (w *workspaceService) readFrame(ctx context.Context, connection *relayConnection) (workspacesession.Frame, bool) {
	kind, data, err := connection.socket.Read(ctx)
	if err != nil {
		connection.close("client_closed")
		return workspacesession.Frame{}, false
	}
	connection.countIn(len(data))
	if kind != websocket.MessageText {
		// Binary data rides inside a frame as base64 (section 18.2), so a
		// binary message is not a frame at all.
		connection.send(workspacesession.ErrorFrame("", "", workspacesession.CodeInvalidFrame, "Frames are JSON text"))
		return workspacesession.Frame{}, true
	}
	var frame workspacesession.Frame
	if err := json.Unmarshal(data, &frame); err != nil {
		connection.send(workspacesession.ErrorFrame("", "", workspacesession.CodeInvalidFrame, "Frame is not valid JSON"))
		return workspacesession.Frame{}, true
	}
	if err := workspacesession.ValidateFrame(frame); err != nil {
		connection.send(workspacesession.ErrorFrame(frame.Channel, frame.Stream, workspacesession.CodeInvalidFrame, err.Error()))
		return workspacesession.Frame{}, true
	}
	return frame, true
}

// handlePersonFrame applies the authority re-check and routes one frame.
func (w *workspaceService) handlePersonFrame(ctx context.Context, connection *relayConnection, record workspaceRecord, frame workspacesession.Frame) {
	if frame.Type == "" {
		return
	}
	if frame.Type == workspacesession.TypeAck {
		// An acknowledgement is not activity. Section 18.1 resets the idle
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
	if err := w.revalidatePersonLocally(ctx, record, connection); err != nil {
		w.logger.Info("workspace.relay_revoked", "workspace_id", record.ID,
			"connection_id", connection.id, "reason", err)
		connection.revoke()
		return
	}
	w.touchWorkspace(ctx, record.ID)
	if frame.Type == workspacesession.TypeResume {
		w.handlePersonResume(connection, frame)
		return
	}
	if code := w.refuseGitWrite(ctx, record, connection, frame); code != "" {
		connection.send(workspacesession.ErrorFrame(frame.Channel, frame.Stream, code, relayCodeMessage(code)))
		return
	}
	// The terminal's authority is asked before a stream is allocated, so a
	// refused open never costs the person a stream slot and never reaches a
	// runner (section 18.3).
	if code := w.refuseTerminal(ctx, record, connection, frame); code != "" {
		connection.send(workspacesession.ErrorFrame(frame.Channel, frame.Stream, code, relayCodeMessage(code)))
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
		connection.send(workspacesession.ErrorFrame(frame.Channel, frame.Stream, code, relayCodeMessage(code)))
		return
	}
	if frame.Type == workspacesession.TypeClose {
		// A close is answered before the runner is even looked up, and the
		// slot goes back to the connection either way. A person who is done
		// with a stream must be able to say so whatever the runner is doing;
		// refusing the close because nothing is serving the workspace would
		// hold the slot for a stream that has no owner left on either side.
		w.relay.closeStream(ctx, connection.workspaceID, stream.id)
		connection.send(workspacesession.Frame{Channel: frame.Channel, Stream: stream.id, Type: workspacesession.TypeClosed})
	}
	runner := w.relay.runnerFor(connection.workspaceID)
	if runner == nil {
		if frame.Type == workspacesession.TypeClose {
			// The stream is already gone and the person has been told; there
			// is nobody to forward the close to.
			return
		}
		// Nothing is serving the workspace right now. The person is told
		// rather than left waiting: an unanswered request is indistinguishable
		// from a slow one, and only the hub knows which this is.
		connection.send(workspacesession.ErrorFrame(frame.Channel, stream.id, workspacesession.CodeStaleExecution,
			"No runner is serving this workspace"))
		return
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
				w.logger.Warn("workspace.terminal_open_not_recorded",
					"workspace_id", record.ID, "stream", stream.id, "error", err)
				break
			}
			w.relay.beginTerminalRecording(w.beginTerminalRecording(record, connection, stream, open))
		default:
			w.recordPersonTerminalFrame(connection, stream, frame)
		}
	}
	forwarded := frame
	forwarded.Stream = stream.id
	forwarded.Seq = w.relay.nextToRunner(stream)
	// The actor is stamped by the hub and never supplied by the client, so a
	// runner can audit what it was asked to do and by whom.
	actor := connection.actor
	forwarded.Actor = &actor
	runner.send(forwarded)
}

// resolvePersonStream finds or opens the stream a person frame belongs to.
//
// A terminal opens explicitly and every open is its own PTY, so a terminal
// frame with no stream always allocates: section 18.2 says a second tab that
// wants the same shell opens its own stream.
//
// Files, diff and preview are request/response and open on their first
// request. A later request with no stream is the same conversation continuing,
// not a second one, so it reuses the stream this connection already has open
// on that channel. Allocating instead would let an ordinary client exhaust its
// own budget -- eight listings and the ninth is refused with stream_limit, on
// a connection holding eight streams nobody is using -- which is exactly what
// the sixth dogfood run hit.
func (w *workspaceService) resolvePersonStream(connection *relayConnection, record workspaceRecord, frame workspacesession.Frame) (*relayStream, string) {
	if !w.channelPermitted(record, frame.Channel) {
		return nil, workspacesession.CodeForbidden
	}
	if frame.Stream != "" {
		if stream := w.relay.lookupStream(connection, frame.Stream); stream != nil {
			return stream, ""
		}
		// A stream this connection does not own does not exist for it.
		return nil, workspacesession.CodeResumeFailed
	}
	if frame.Type == workspacesession.TypeClose {
		return nil, workspacesession.CodeInvalidFrame
	}
	// A terminal allocates per request instead of reusing: every terminal
	// open is its own PTY.
	if frame.Channel != workspacesession.ChannelTerminal {
		if stream := w.relay.channelStream(connection, frame.Channel); stream != nil {
			return stream, ""
		}
	}
	stream, code := w.relay.openStream(connection, frame.Channel)
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
func (w *workspaceService) channelPermitted(record workspaceRecord, channel string) bool {
	if record.Capabilities == nil {
		return false
	}
	if channel == workspacesession.ChannelTerminal {
		// refuseTerminal asks both of these first and answers with a code that
		// says which it was, so a reader is normally told read_only rather than
		// forbidden. They stay here because this is the check on the path a
		// stream is actually allocated on: a gate that only existed in the
		// refusal above would be one edit away from being bypassed.
		if record.ReadOnly || !w.config.Terminal.Enabled {
			return false
		}
	}
	if channel == workspacesession.ChannelExec {
		// The exec channel runs project actions (section 18.12), which this
		// hub does not serve.
		return false
	}
	return record.Capabilities.Has(workspacesession.ChannelCapability(channel))
}

// handlePersonAck records an acknowledgement, which is what releases the
// replay buffer.
func (w *workspaceService) handlePersonAck(connection *relayConnection, frame workspacesession.Frame) {
	stream := w.relay.lookupStream(connection, frame.Stream)
	if stream == nil {
		return
	}
	var payload workspacesession.AckPayload
	if err := json.Unmarshal(frame.Payload, &payload); err != nil {
		return
	}
	w.relay.acknowledge(stream, payload.Through)
}

// handlePersonResume moves a parked stream onto this connection and replays
// what the person missed.
func (w *workspaceService) handlePersonResume(connection *relayConnection, frame workspacesession.Frame) {
	var payload workspacesession.ResumePayload
	if err := json.Unmarshal(frame.Payload, &payload); err != nil {
		connection.send(workspacesession.ErrorFrame(frame.Channel, frame.Stream, workspacesession.CodeResumeFailed, "resume is malformed"))
		return
	}
	stream, replay, code := w.relay.resumeStream(connection, payload.Stream, payload.LastSeq, w.now())
	if stream == nil {
		if code == "" {
			code = workspacesession.CodeResumeFailed
		}
		connection.send(workspacesession.ErrorFrame(frame.Channel, payload.Stream, code, relayCodeMessage(code)))
		return
	}
	resumed, err := workspacesession.Encode(map[string]any{"stream": stream.id, "last_seq": payload.LastSeq})
	if err != nil {
		connection.send(workspacesession.ErrorFrame(frame.Channel, payload.Stream, workspacesession.CodeResumeFailed, "resume could not be answered"))
		return
	}
	connection.send(workspacesession.Frame{Channel: stream.channel, Stream: stream.id, Type: workspacesession.TypeResumed, Payload: resumed})
	for _, buffered := range replay {
		connection.send(buffered)
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
		// enabled setting, the isolation rule and the support window (section
		// 18.3). The code is what a client switches on, so the sentence has to
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
func (w *workspaceService) touchWorkspace(ctx context.Context, workspaceID string) {
	now := w.now()
	if _, err := w.server.database.db.ExecContext(ctx, `UPDATE workspace_sessions SET last_activity_at = ?
WHERE id = ? AND state IN `+openWorkspaceStates, formatHubTime(now), workspaceID); err != nil {
		w.logger.Warn("workspace.activity_not_recorded", "workspace_id", workspaceID, "error", err)
	}
}

// The runner side.

// openWorkspaceWorkerRelay implements GET .../workspaces/:workspace/worker/relay.
// It is fenced by the workspace tuple, carried in headers because a GET has no
// body: the same shape the conversation controls poll uses.
func (s *Service) openWorkspaceWorkerRelay(c echo.Context) error {
	service, err := s.requireWorkspaces()
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	identity, err := workerRelayIdentity(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	scope := nativeRequestScope(c)
	ctx := c.Request().Context()
	var record workspaceRecord
	err = s.hubTransact(ctx, func(tx *sql.Tx, now time.Time) error {
		found, _, err := service.loadWorkspaceForWorker(ctx, tx, scope, c.Param("workspace"), identity, now)
		if err != nil {
			return err
		}
		if !workspacesession.Bound(found.State) {
			return nativeStaleExecution("The workspace is not bound to a runner")
		}
		if found.RunnerID != scope.credential.Runner.RunnerID {
			return nativeStaleExecution("The workspace belongs to another runner")
		}
		record = found
		return nil
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	socket, err := acceptRelaySocket(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	connection := &relayConnection{
		id: newNativeID("relayrunner"), workspaceID: record.ID, socket: socket, runner: true,
		principalID: scope.credential.ID, sessionID: string(record.LeaseID),
		out: make(chan relayOutbound, relayWriteQueue), done: make(chan struct{}),
	}
	previous, err := service.relay.attachRunner(connection)
	if err != nil {
		service.closeRelaySocket(socket, "server_shutdown")
		return nil
	}
	if previous != nil {
		// A second runner connection replaces the first. Two runners
		// answering one workspace would each be serving a different worktree,
		// so the old one is told exactly why it lost the workspace.
		previous.sendFinal(workspacesession.ErrorFrame("", "", workspacesession.CodeSuperseded, "Another runner connection took this workspace"),
			workspacesession.CodeSuperseded)
		// The PTYs being recorded were on the connection that just lost the
		// workspace, so their recordings are complete.
		service.relay.finishWorkspaceTerminalRecordings(ctx, record.ID)
	}
	socketCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go connection.writeLoop(socketCtx)
	go func() {
		<-connection.done
		cancel()
	}()
	service.readRunner(socketCtx, connection)
	connection.close("runner_closed")
	service.relay.detachRunner(ctx, connection)
	service.closeRelaySocket(socket, connection.reason())
	return nil
}

// readRunner is the runner side's read loop. Runner frames are routed to the
// one person connection that owns the stream they name; there is no broadcast,
// because a stream belongs to one connection.
func (w *workspaceService) readRunner(ctx context.Context, connection *relayConnection) {
	for {
		frame, ok := w.readFrame(ctx, connection)
		if !ok {
			return
		}
		if frame.Type == "" {
			continue
		}
		if frame.Stream == "" {
			// A runner frame with no stream has no audience: the hub does not
			// know who asked, and guessing would show one person another's
			// output.
			continue
		}
		stream, person := w.relay.runnerStream(connection, frame.Stream)
		if stream == nil {
			continue
		}
		if frame.Type == workspacesession.TypeAck {
			continue
		}
		outbound := frame
		outbound.Actor = nil
		outbound.Seq = w.relay.nextToPerson(stream)
		overflow := w.relay.bufferForPerson(connection.workspaceID, stream, outbound, outbound.Seq)
		if person != nil {
			person.send(outbound)
		}
		if overflow {
			// The reader stopped acknowledging and the buffer passed its cap.
			// Closing the stream is the contract's answer: the alternative is
			// the hub growing without bound for one reader's convenience.
			w.relay.closeStream(ctx, connection.workspaceID, stream.id)
			if person != nil {
				person.send(workspacesession.ErrorFrame(stream.channel, stream.id, workspacesession.CodeOverflow, relayCodeMessage(workspacesession.CodeOverflow)))
			}
			connection.send(workspacesession.Frame{Channel: stream.channel, Stream: stream.id, Type: workspacesession.TypeClose})
			continue
		}
		if stream.channel == workspacesession.ChannelTerminal {
			// The hub is not only forwarding a terminal stream, it is recording
			// it (section 18.3). A recording is the only thing a closed tab
			// leaves behind, and an owner auditing who was in a worktree has
			// nothing else to read.
			w.recordRunnerTerminalFrame(connection, stream, frame)
		}
		if frame.Type == workspacesession.TypeClosed || frame.Type == workspacesession.TypeClose {
			w.relay.closeStream(ctx, connection.workspaceID, stream.id)
		}
	}
}

// Audit rows.

// openRelayAudit writes the connection's workspace_relay_sessions row.
func (w *workspaceService) openRelayAudit(ctx context.Context, record workspaceRecord, connection *relayConnection, principal relayPrincipal) error {
	connection.auditID = newNativeID("relaysession")
	channels, err := json.Marshal([]string{})
	if err != nil {
		return fmt.Errorf("encode relay session channels: %w", err)
	}
	_, err = w.server.database.db.ExecContext(ctx, `INSERT INTO workspace_relay_sessions
 (id, workspace_id, organization_id, principal_id, subject, connection_id, channels_json, opened_at, support_reason)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, connection.auditID, record.ID, record.OrganizationID,
		principal.principalID, principal.subject, connection.id, string(channels),
		formatHubTime(w.now()), principal.supportReason)
	if err != nil {
		return fmt.Errorf("open relay session audit: %w", err)
	}
	return nil
}

// closeRelayAudit completes the row with the reason and the byte counters.
func (w *workspaceService) closeRelayAudit(ctx context.Context, connection *relayConnection, reason string) {
	if connection.auditID == "" {
		return
	}
	in, out := connection.counters()
	if _, err := w.server.database.db.ExecContext(ctx, `UPDATE workspace_relay_sessions
SET closed_at = ?, close_reason = ?, bytes_in = ?, bytes_out = ? WHERE id = ?`,
		formatHubTime(w.now()), reason, in, out, connection.auditID); err != nil {
		w.logger.Warn("workspace.relay_audit_not_closed", "connection_id", connection.id, "error", err)
	}
}
