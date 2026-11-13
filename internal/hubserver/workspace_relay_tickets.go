package hubserver

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

// Relay tickets.
//
// Browsers cannot set headers on a WebSocket upgrade, so the hosted JSON API's CSRF rule
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
	credential hubapi.APICredential
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
	credential, ok := c.Get("hub_api_credential").(hubapi.APICredential)
	if !ok {
		return relayPrincipal{}, hubapi.NativeNotFound()
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
		return &hubapi.NativeError{Code: "forbidden", Message: "The relay refuses this origin", Status: http.StatusForbidden, PublicMessage: true}
	}
	return nil
}

// relayTicketRequest reports whether a request mints a relay ticket. Minting
// is a POST only because it must carry the CSRF header; it grants nothing
// beyond the project read the relay upgrade checks, so it needs read rather
// than write. The writes a relay can carry -- a git commit or push and a
// terminal -- are refused per frame against the person's grant.
func relayTicketRequest(c echo.Context) bool {
	return c.Request().Method == http.MethodPost && c.Path() == hubapi.NativeBase+"/workspaces/:workspace/relay-tickets"
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
	scope := hubscope.NativeRequestScope(c)
	ctx := c.Request().Context()
	ticket, err := newRelayTicket()
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	err = s.accessWrite(ctx, scope.Organization, func(writer tenantstore.Writer) error {
		current := scope
		current.Credential = principal.credential
		record, err := service.readRelayWorkspaceForActor(ctx, writer, current, c.Param("workspace"))
		if err != nil {
			return err
		}
		if workspacesession.Terminal(record.State) {
			return hubapi.NativeStaleExecution("The workspace has ended")
		}
		owner, ok := writer.(postgres.RelayActionWriter)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		now := s.config.now().UTC()
		_, err = owner.CreateRelayTicket(ctx, postgres.CreateRelayTicketInput{OrganizationID: scope.Organization, ProjectID: scope.Project, Digest: relayTicketDigest(ticket), WorkspaceID: record.ID, SessionID: principal.sessionID, PrincipalID: principal.principalID, Subject: principal.subject, SupportReason: principal.supportReason, CreatedAt: now, ExpiresAt: now.Add(workspacesession.TicketLifetime)})
		return err
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusCreated, workspaceRelayTicketResponse{
		Ticket: ticket, ExpiresIn: int(workspacesession.TicketLifetime / time.Second),
	})
}

// sweepRelayTickets drops spent and expired tickets. A ticket is worth 30
// seconds, so the table is only ever a handful of rows; the sweep is here so
// that stays true on a hub nobody restarts.
func (w *workspaceService) sweepRelayTickets(ctx context.Context, now time.Time) error {
	organization := tracker.OrganizationID(w.server.appOrganizationID())
	w.relay.mu.Lock()
	after := w.relay.ticketSweepAfter
	w.relay.mu.Unlock()
	var page []postgres.RelayTicket
	err := w.server.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(reader tenantstore.Reader) error {
		owner, ok := reader.(postgres.RelayActionReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		var err error
		page, err = owner.RelayTicketPage(ctx, after, hubmaintenance.ItemLimit)
		return err
	})
	if err != nil {
		return err
	}
	if len(page) == 0 {
		w.relay.mu.Lock()
		w.relay.ticketSweepAfter = ""
		w.relay.mu.Unlock()
		return nil
	}
	cutoff := now.Add(-workspacesession.TicketLifetime)
	for _, ticket := range page {
		if !ticket.ExpiresAt.After(now) || ticket.RedeemedAt != nil && !ticket.RedeemedAt.After(cutoff) {
			if err := w.server.config.TenantRepository.WriteTenant(ctx, organization, corestore.Background, func(writer tenantstore.Writer) error {
				owner, ok := writer.(postgres.RelayActionWriter)
				if !ok {
					return hubapi.NativeWorkItemStorageUnavailable()
				}
				return owner.DeleteExpiredRelayTicket(ctx, ticket.Digest, now, cutoff)
			}); err != nil {
				return err
			}
		}
		w.relay.mu.Lock()
		w.relay.ticketSweepAfter = ticket.Digest
		w.relay.mu.Unlock()
	}
	return nil
}
