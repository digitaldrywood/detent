package hubserver

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

// Relay tickets (decisions section 18.2).
//
// Browsers cannot set headers on a WebSocket upgrade, so section 12's CSRF rule
// cannot be met on the socket itself. The ticket moves the check to a request
// that can carry it: an ordinary POST with the CSRF header and a checked
// Origin mints a single-use secret, bound to one workspace and to the session
// that minted it, and the socket presents that instead. A stolen ticket is
// worth 30 seconds, one connection, and only to a caller who can also present
// the same session.
//
// Only the digest is stored, for the same reason an API token's is: a database
// read must not yield a usable credential.

// workspaceRelayTicketResponse is what a mint answers with.
type workspaceRelayTicketResponse struct {
	Ticket    string `json:"ticket"`
	ExpiresIn int    `json:"expires_in"`
}

// relayTicketDigest is the stored form of a ticket.
func relayTicketDigest(ticket string) string {
	sum := sha256.Sum256([]byte("detent-workspace-relay-ticket:" + ticket))
	return hex.EncodeToString(sum[:])
}

// newRelayTicket returns a fresh ticket secret.
func newRelayTicket() (string, error) {
	buf := make([]byte, workspacesession.TicketBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate relay ticket: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// relayPrincipal is who is on the person side of a relay. It is resolved from
// a hosted session on a hosted hub and from an operator token elsewhere, so the
// relay has one notion of "the person" whichever way the hub is deployed.
type relayPrincipal struct {
	credential apiCredential
	// SessionID binds a ticket and a resumed stream. For a hosted session it
	// is the provider's session id; for a token principal it is the token,
	// which is the closest thing that credential has to a session.
	sessionID     string
	subject       string
	principalID   string
	supportReason string
}

// relayPrincipalFor resolves the person behind a request. A hosted session is
// preferred; a bearer token is accepted so a single-tenant hub and the tests
// can use the relay without inventing a cookie.
func (s *Service) relayPrincipalFor(c echo.Context) (relayPrincipal, error) {
	return s.relayPrincipal(c.Request().Context(), c)
}

// relayPrincipal is relayPrincipalFor on a caller's own context. The periodic
// re-check runs on the connection's context rather than the original request's,
// so it needs to say which one it means: a re-check that outlived the
// connection it was checking would be checking nothing.
func (s *Service) relayPrincipal(ctx context.Context, c echo.Context) (relayPrincipal, error) {
	if s.config.Hosted != nil && c.Request().Header.Get(echo.HeaderAuthorization) == "" {
		// hostedSession authenticates on the request's own context, which is
		// the wrong one for a re-check that runs on a timer long after the
		// upgrade: the cookie read and the authentication are therefore done
		// here, on the caller's context.
		cookie, err := c.Cookie(hostedCookie)
		if err != nil || s.hostedSessions == nil {
			return relayPrincipal{}, auth.ErrInvalidSession
		}
		session, err := s.hostedSessions.Authenticate(ctx, cookie.Value)
		if err != nil {
			return relayPrincipal{}, auth.ErrInvalidSession
		}
		credential, _, err := s.hostedSessionCredential(ctx, session, apikey.HashToken(cookie.Value))
		if err != nil {
			return relayPrincipal{}, err
		}
		principal := relayPrincipal{
			credential: credential, subject: credential.Hosted.Subject,
			sessionID: credential.Hosted.SessionID, principalID: credential.ID,
		}
		if credential.Hosted.SupportActor != "" {
			// A support actor's row carries the reason, because a surface
			// never widens an audience silently.
			principal.supportReason = credential.Hosted.SupportReason
			principal.subject = credential.Hosted.SupportActor
		}
		return principal, nil
	}
	credential, ok := c.Get("hub_api_credential").(apiCredential)
	if !ok {
		return relayPrincipal{}, nativeNotFound()
	}
	return relayPrincipal{
		credential: credential, subject: credential.Name,
		sessionID: credential.ID, principalID: credential.ID,
	}, nil
}

// requireRelayOrigin refuses a cross-origin mint or upgrade. A browser sends
// Origin on both, and the hub's public URL is the only one that may open a
// relay: without this the ticket's CSRF protection would be complete and its
// redemption would not be.
func (s *Service) requireRelayOrigin(c echo.Context) error {
	origin := strings.TrimSpace(c.Request().Header.Get("Origin"))
	if origin == "" {
		// A non-browser client sends no Origin. It also cannot be made to
		// send one by a hostile page, which is the attack Origin defends
		// against, so an absent header is not itself a failure.
		return nil
	}
	if s.config.Hosted == nil || strings.TrimSpace(s.config.Hosted.PublicURL) == "" {
		return nil
	}
	allowed, err := url.Parse(s.config.Hosted.PublicURL)
	if err != nil {
		return fmt.Errorf("parse hub public url: %w", err)
	}
	presented, err := url.Parse(origin)
	if err != nil || presented.Scheme != allowed.Scheme || presented.Host != allowed.Host {
		return &nativeError{Code: "forbidden", Message: "The relay refuses this origin", status: http.StatusForbidden}
	}
	return nil
}

// mintWorkspaceRelayTicket implements POST .../workspaces/:workspace/relay-tickets.
func (s *Service) mintWorkspaceRelayTicket(c echo.Context) error {
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
	scope := nativeRequestScope(c)
	ctx := c.Request().Context()
	ticket, err := newRelayTicket()
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	err = s.hubTransact(ctx, func(tx *sql.Tx, now time.Time) error {
		record, err := service.readWorkspaceForActor(ctx, tx, scope, c.Param("workspace"))
		if err != nil {
			return err
		}
		if workspacesession.Terminal(record.State) {
			return nativeStaleExecution("The workspace has ended")
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO workspace_relay_tickets
 (digest, workspace_id, session_id, principal_id, subject, support_reason, created_at, expires_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, relayTicketDigest(ticket), record.ID, principal.sessionID,
			principal.principalID, principal.subject, principal.supportReason,
			formatHubTime(now), formatHubTime(now.Add(workspacesession.TicketLifetime)))
		if err != nil {
			return fmt.Errorf("store relay ticket: %w", err)
		}
		return nil
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusCreated, workspaceRelayTicketResponse{
		Ticket: ticket, ExpiresIn: int(workspacesession.TicketLifetime / time.Second),
	})
}

// redeemedRelayTicket is a ticket that has just been spent.
type redeemedRelayTicket struct {
	WorkspaceID   string
	SessionID     string
	PrincipalID   string
	Subject       string
	SupportReason string
}

// redeemRelayTicket spends a ticket exactly once. The update is the check: a
// second connection presenting the same ticket changes no rows and is refused,
// with no window between reading and marking it spent.
func redeemRelayTicket(ctx context.Context, tx *sql.Tx, ticket, workspaceID string, now time.Time) (redeemedRelayTicket, error) {
	digest := relayTicketDigest(ticket)
	result, err := tx.ExecContext(ctx, `UPDATE workspace_relay_tickets SET redeemed_at = ?
WHERE digest = ? AND workspace_id = ? AND redeemed_at IS NULL AND julianday(expires_at) > julianday(?)`,
		formatHubTime(now), digest, workspaceID, formatHubTime(now))
	if err != nil {
		return redeemedRelayTicket{}, fmt.Errorf("redeem relay ticket: %w", err)
	}
	spent, err := result.RowsAffected()
	if err != nil {
		return redeemedRelayTicket{}, fmt.Errorf("count redeemed relay ticket: %w", err)
	}
	if spent != 1 {
		return redeemedRelayTicket{}, nativeNotFound()
	}
	var redeemed redeemedRelayTicket
	err = tx.QueryRowContext(ctx, `SELECT workspace_id, session_id, principal_id, subject, support_reason
FROM workspace_relay_tickets WHERE digest = ?`, digest).Scan(&redeemed.WorkspaceID, &redeemed.SessionID,
		&redeemed.PrincipalID, &redeemed.Subject, &redeemed.SupportReason)
	if errors.Is(err, sql.ErrNoRows) {
		return redeemedRelayTicket{}, nativeNotFound()
	}
	if err != nil {
		return redeemedRelayTicket{}, fmt.Errorf("read redeemed relay ticket: %w", err)
	}
	return redeemed, nil
}

// sweepRelayTickets drops spent and expired tickets. A ticket is worth 30
// seconds, so the table is only ever a handful of rows; the sweep is here so
// that stays true on a hub nobody restarts.
func (w *workspaceService) sweepRelayTickets(ctx context.Context, now time.Time) error {
	cutoff := formatHubTime(now.Add(-workspacesession.TicketLifetime))
	if _, err := w.server.database.db.ExecContext(ctx, `DELETE FROM workspace_relay_tickets
WHERE julianday(expires_at) <= julianday(?) OR (redeemed_at IS NOT NULL AND julianday(redeemed_at) <= julianday(?))`,
		formatHubTime(now), cutoff); err != nil {
		return fmt.Errorf("sweep relay tickets: %w", err)
	}
	return nil
}
