package hubserver

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
)

const hostedSharedClaimsKey = "hosted_shared_claims"

type HostedSharedEntry struct {
	Issuer     string
	PublicKeys []ed25519.PublicKey
	Generation int64

	verifier *cloudassert.Verifier
}

func (e *HostedSharedEntry) validate(organization string) error {
	if e == nil {
		return nil
	}
	if !hostedSafeID(e.Issuer) || e.Generation < 1 || len(e.PublicKeys) == 0 || len(e.PublicKeys) > 4 {
		return errors.New("shared entry requires an issuer, allocation generation and one to four public keys")
	}
	for _, key := range e.PublicKeys {
		if len(key) != ed25519.PublicKeySize {
			return errors.New("shared entry public key is invalid")
		}
	}
	e.verifier = &cloudassert.Verifier{PublicKeys: e.PublicKeys, Issuer: e.Issuer, Audience: organization, Generation: e.Generation}
	return nil
}

func (s *Service) hostedShared() bool {
	return s.config.Hosted != nil && s.config.Hosted.SharedEntry != nil
}

func (s *Service) hostedBase() string {
	if s.hostedShared() {
		return "/organizations/" + s.config.Hosted.OrganizationID
	}
	return ""
}

func (s *Service) hostedPath(path string) string {
	return s.hostedBase() + path
}

func (s *Service) hostedSignInPath() string {
	if s.hostedShared() {
		return "/organizations"
	}
	return "/login"
}

var hostedSharedEntryRoutes = map[string]bool{
	"/login": true, "/auth/oidc/start": true, "/auth/oidc/callback": true, "/invite": true, "/logout": true,
	"/support": true, "/support/start": true, "/webhooks/stripe": true,
	"/organization/switch": true, "/organization/create": true, "/organization/join": true,
}

func (s *Service) hostedSharedEntry(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		request := c.Request()
		entry := s.config.Hosted.SharedEntry
		if !cloudassert.CanonicalPath(request.URL) {
			return s.nativeAPIError(c, nativeNotFound())
		}
		body, err := io.ReadAll(io.LimitReader(request.Body, cloudassert.MaxBodyBytes+1))
		if err != nil {
			return c.JSON(http.StatusBadRequest, apiErrorResponse{Code: "invalid_request", Message: "Request body could not be read"})
		}
		if len(body) > cloudassert.MaxBodyBytes {
			return c.JSON(http.StatusRequestEntityTooLarge, apiErrorResponse{Code: "payload_too_large", Message: "Request body is too large"})
		}
		request.Body = io.NopCloser(bytes.NewReader(body))
		claims, err := entry.verifier.Verify(request.Header.Get(cloudassert.Header), request.Method, request.URL.RequestURI(), body)
		if err != nil {
			return c.JSON(http.StatusUnauthorized, apiErrorResponse{Code: "entry_assertion_required", Message: "Requests must arrive through the authenticated shared entry"})
		}
		request.Header.Del(cloudassert.Header)
		bearer := request.Header.Get(echo.HeaderAuthorization) != ""
		if claims.Kind == cloudassert.KindBrowser && bearer || claims.Kind == cloudassert.KindMachine && !bearer {
			return c.JSON(http.StatusUnauthorized, apiErrorResponse{Code: "entry_assertion_required", Message: "Requests must arrive through the authenticated shared entry"})
		}
		request.Header.Del("Cookie")
		organization := s.config.Hosted.OrganizationID
		prefix := "/organizations/" + organization
		path := request.URL.Path
		internal := strings.HasPrefix(path, "/internal/")
		switch {
		case internal:
		case path == prefix || strings.HasPrefix(path, prefix+"/"):
			path = strings.TrimPrefix(path, prefix)
			if path == "" {
				path = "/"
			}
			if strings.HasPrefix(path, "/organizations/") || strings.HasPrefix(path, "/internal/") || hostedSharedEntryRoutes[path] {
				return s.nativeAPIError(c, nativeNotFound())
			}
			request.URL.Path, request.URL.RawPath = path, ""
		case strings.HasPrefix(path, "/api/v2/organizations/"+organization+"/"):
		default:
			return s.nativeAPIError(c, nativeNotFound())
		}
		if internal != (claims.Kind == cloudassert.KindService) {
			return s.nativeAPIError(c, nativeNotFound())
		}
		c.Set(hostedSharedClaimsKey, claims)
		return next(c)
	}
}

func hostedSharedClaims(c echo.Context) (cloudassert.Claims, bool) {
	claims, ok := c.Get(hostedSharedClaimsKey).(cloudassert.Claims)
	return claims, ok
}

func (s *Service) sharedHostedSession(c echo.Context) (auth.Session, string, error) {
	claims, ok := hostedSharedClaims(c)
	if !ok || claims.Kind != cloudassert.KindBrowser {
		return auth.Session{}, "", auth.ErrInvalidSession
	}
	identity := auth.HostedIdentity{
		Subject: claims.Subject, OrganizationID: claims.ProviderOrganization, SessionID: claims.ProviderSession,
		CreatedAt: claims.SessionCreatedAt, ExpiresAt: claims.SessionExpiresAt, SupportActor: claims.SupportActor, SupportReason: claims.SupportReason,
	}
	encoded, err := json.Marshal(identity)
	if err != nil {
		return auth.Session{}, "", auth.ErrInvalidSession
	}
	ctx := c.Request().Context()
	if identity.SupportActor != "" && (!hostedEmailListed(s.config.Hosted.StaffEmails, identity.SupportActor) || !hostedEmailListed(s.config.Hosted.SupportActors, identity.SupportActor)) {
		return auth.Session{}, "", auth.ErrInvalidSession
	}
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return auth.Session{}, "", auth.ErrInvalidSession
	}
	defer tx.Rollback()
	inserted, err := tx.ExecContext(ctx, "INSERT INTO hosted_sessions (token_hash,email,identity_json,expires_at,created_at) VALUES (?,?,?,?,?) ON CONFLICT(token_hash) DO NOTHING", claims.Binding, claims.Email, string(encoded), formatHubTime(claims.SessionExpiresAt), formatHubTime(s.config.now()))
	if err != nil {
		return auth.Session{}, "", auth.ErrInvalidSession
	}
	if rows, err := inserted.RowsAffected(); err != nil || rows == 1 && identity.SupportActor != "" && s.hostedAuditWith(ctx, tx, &identity, "session_started", "/auth/oidc/callback", "", http.StatusOK) != nil {
		return auth.Session{}, "", auth.ErrInvalidSession
	}
	if err := tx.Commit(); err != nil {
		return auth.Session{}, "", auth.ErrInvalidSession
	}
	session, err := s.WebSession(ctx, claims.Binding, s.config.now())
	if err != nil || session.Identity == nil || session.Identity.Subject != identity.Subject || session.Identity.SessionID != identity.SessionID || session.Identity.OrganizationID != identity.OrganizationID || !strings.EqualFold(session.Email, claims.Email) {
		return auth.Session{}, "", auth.ErrInvalidSession
	}
	c.Set("hosted_session", session)
	return session, claims.Binding, nil
}

func (s *Service) hostedSharedCSRF(c echo.Context) string {
	if claims, ok := hostedSharedClaims(c); ok && claims.Kind == cloudassert.KindBrowser {
		return claims.CSRF
	}
	return ""
}

func (s *Service) hostedSharedCSRFValid(c echo.Context, value string) bool {
	expected := s.hostedSharedCSRF(c)
	return expected != "" && subtle.ConstantTimeCompare([]byte(value), []byte(expected)) == 1
}

func (s *Service) registerHostedSharedRoutes(e *echo.Echo) {
	e.POST("/internal/v1/sessions/revoke", s.revokeHostedSharedSessions)
	e.POST("/internal/v1/invitations/accept", s.acceptHostedSharedInvitation)
	e.POST("/internal/v1/health", s.hostedSharedHealth)
	e.POST("/internal/v1/owner/bootstrap", s.bootstrapHostedSharedOwner)
	e.POST("/internal/v1/billing/binding", s.hostedSharedBillingBinding)
	e.POST("/internal/v1/billing/events", s.hostedSharedBillingEvent)
}

type hostedSharedBilling struct {
	Enabled         bool      `json:"enabled"`
	AccountID       string    `json:"account_id,omitempty"`
	Mode            string    `json:"mode,omitempty"`
	CustomerID      string    `json:"customer_id,omitempty"`
	Status          string    `json:"status"`
	AccessUntil     time.Time `json:"access_until,omitzero"`
	CheckoutPending bool      `json:"checkout_pending"`
}

type hostedSharedBillingQuery struct {
	Reconcile bool `json:"reconcile"`
}

func (s *Service) hostedSharedBillingBinding(c echo.Context) error {
	cfg := s.config.Hosted.Billing
	result := hostedSharedBilling{Status: "free"}
	if cfg != nil {
		result.Enabled, result.AccountID, result.Mode = true, cfg.AccountID, cfg.mode()
		binding, err := s.database.hostedBillingBinding(c.Request().Context(), cfg)
		if err != nil && !errors.Is(err, errHostedBillingUnbound) {
			return s.internalAPIError(c, "billing_unavailable", "Billing binding is unavailable", err)
		}
		result.CustomerID = binding.CustomerID
		var query hostedSharedBillingQuery
		if err := decodeAPIJSON(c, &query); err != nil {
			return invalidAPIRequest(c, err)
		}
		if query.Reconcile && binding.CustomerID != "" && s.billing != nil {
			s.billing.mu.Lock()
			reconcileErr := s.billing.reconcile(c.Request().Context())
			s.billing.mu.Unlock()
			if reconcileErr != nil {
				return c.JSON(http.StatusServiceUnavailable, apiErrorResponse{Code: "billing_unavailable", Message: "Billing could not be reconciled"})
			}
		}
		var raw string
		if err := s.database.db.QueryRowContext(c.Request().Context(), "SELECT checkout_json FROM hosted_billing_accounts WHERE organization_id=?", s.config.Hosted.OrganizationID).Scan(&raw); err == nil {
			var checkout hostedCheckout
			result.CheckoutPending = json.Unmarshal([]byte(raw), &checkout) == nil && checkout.ExpiresAt.After(s.config.now())
		}
		state, err := s.database.readHostedBilling(c.Request().Context())
		if err != nil {
			return s.internalAPIError(c, "billing_unavailable", "Billing binding is unavailable", err)
		}
		result.Status, result.AccessUntil = state.Status, state.AccessUntil
	}
	return c.JSON(http.StatusOK, result)
}

type hostedSharedBillingEventRequest struct {
	EventID    string `json:"event_id"`
	EventType  string `json:"event_type"`
	CustomerID string `json:"customer_id"`
}

func (s *Service) hostedSharedBillingEvent(c echo.Context) error {
	cfg := s.config.Hosted.Billing
	var request hostedSharedBillingEventRequest
	if err := decodeAPIJSON(c, &request); err != nil || cfg == nil || !strings.HasPrefix(request.EventID, "evt_") || !hostedSafeID(request.EventID) || request.EventType == "" || len(request.EventType) > 128 {
		return invalidAPIRequest(c, errors.New("invalid billing event"))
	}
	binding, err := s.database.hostedBillingBinding(c.Request().Context(), cfg)
	if err != nil || binding.CustomerID != request.CustomerID {
		return c.JSON(http.StatusConflict, apiErrorResponse{Code: "customer_mismatch", Message: "The event customer is not bound to this organization"})
	}
	if _, err := s.database.db.ExecContext(c.Request().Context(), "INSERT INTO hosted_billing_events(event_id,event_type,received_at) VALUES(?,?,?) ON CONFLICT(event_id) DO NOTHING", request.EventID, request.EventType, formatHubTime(s.config.now())); err != nil {
		return s.internalAPIError(c, "billing_unavailable", "Billing event could not be recorded", err)
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Service) hostedSharedHealth(c echo.Context) error {
	if !s.ready.Load() || s.database.health(c.Request().Context()) != nil {
		return c.JSON(http.StatusServiceUnavailable, apiErrorResponse{Code: "not_ready", Message: "Tenant is not ready"})
	}
	return c.NoContent(http.StatusNoContent)
}

type hostedSharedOwner struct {
	OrganizationName string `json:"organization_name"`
}

func (s *Service) bootstrapHostedSharedOwner(c echo.Context) error {
	claims, _ := hostedSharedClaims(c)
	var request hostedSharedOwner
	name := ""
	if err := decodeAPIJSON(c, &request); err == nil {
		name = strings.TrimSpace(request.OrganizationName)
	}
	if claims.Subject == "" || claims.Email == "" || name == "" || len(name) > 120 || hostedEmailListed(s.config.Hosted.StaffEmails, claims.Email) {
		return invalidAPIRequest(c, errors.New("invalid owner bootstrap"))
	}
	ctx := c.Request().Context()
	s.hostedMutationMu.Lock()
	defer s.hostedMutationMu.Unlock()
	var members int
	var owner string
	if err := s.database.db.QueryRowContext(ctx, "SELECT count(*), COALESCE(max(CASE WHEN role = 'owner' AND active = 1 THEN user_id END), '') FROM hosted_members").Scan(&members, &owner); err != nil {
		return s.internalAPIError(c, "bootstrap_unavailable", "Owner bootstrap is unavailable", err)
	}
	if members != 0 {
		if members == 1 && owner == claims.Subject {
			return c.NoContent(http.StatusNoContent)
		}
		return c.JSON(http.StatusConflict, apiErrorResponse{Code: "already_bootstrapped", Message: "This organization already has members"})
	}
	provider, err := s.hostedProviderOrganization(ctx)
	if err != nil || provider == "" {
		return c.JSON(http.StatusConflict, apiErrorResponse{Code: "unallocated", Message: "The tenant has no provider organization"})
	}
	membership, err := s.hostedMembership(ctx, &auth.HostedIdentity{Subject: claims.Subject, OrganizationID: provider})
	if err != nil || membership.Role.Slug != "owner" {
		return c.JSON(http.StatusForbidden, apiErrorResponse{Code: "owner_unverified", Message: "The provider does not report this identity as an owner"})
	}
	if err := s.storeHostedMember(ctx, auth.Identity{Subject: claims.Subject, Email: claims.Email, EmailVerified: true}, membership, name); err != nil {
		return s.internalAPIError(c, "bootstrap_unavailable", "Owner bootstrap is unavailable", err)
	}
	return c.NoContent(http.StatusNoContent)
}

type hostedSharedRevocation struct {
	Bindings []string `json:"bindings"`
}

func (s *Service) revokeHostedSharedSessions(c echo.Context) error {
	var request hostedSharedRevocation
	if err := decodeAPIJSON(c, &request); err != nil || len(request.Bindings) == 0 || len(request.Bindings) > 100 {
		return invalidAPIRequest(c, errors.New("invalid revocation"))
	}
	ctx := c.Request().Context()
	now := formatHubTime(s.config.now())
	for _, binding := range request.Bindings {
		if len(binding) != 64 {
			return invalidAPIRequest(c, errors.New("invalid binding"))
		}
		if err := s.revokeHostedSharedBinding(ctx, binding, now); err != nil {
			return s.internalAPIError(c, "revocation_unavailable", "Sessions could not be revoked", err)
		}
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Service) revokeHostedSharedBinding(ctx context.Context, binding, now string) error {
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var encoded string
	var identity auth.HostedIdentity
	active := tx.QueryRowContext(ctx, "SELECT identity_json FROM hosted_sessions WHERE token_hash = ? AND revoked_at IS NULL", binding).Scan(&encoded) == nil
	if _, err := tx.ExecContext(ctx, "INSERT INTO hosted_sessions (token_hash,email,identity_json,expires_at,created_at,revoked_at) VALUES (?,'','{}',?,?,?) ON CONFLICT(token_hash) DO UPDATE SET revoked_at = COALESCE(revoked_at, excluded.revoked_at)", binding, now, now, now); err != nil {
		return err
	}
	if active && json.Unmarshal([]byte(encoded), &identity) == nil && identity.SupportActor != "" {
		if err := s.hostedAuditWith(ctx, tx, &identity, "session_ended", "/logout", "", http.StatusOK); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type hostedSharedInvitation struct {
	Token string `json:"token"`
}

func (s *Service) acceptHostedSharedInvitation(c echo.Context) error {
	claims, _ := hostedSharedClaims(c)
	var request hostedSharedInvitation
	if err := decodeAPIJSON(c, &request); err != nil || claims.Subject == "" || claims.Email == "" {
		return invalidAPIRequest(c, errors.New("invalid invitation"))
	}
	if hostedEmailListed(s.config.Hosted.StaffEmails, claims.Email) {
		return c.JSON(http.StatusForbidden, apiErrorResponse{Code: "invitation_unavailable", Message: "This invitation is unavailable"})
	}
	identity := auth.Identity{Subject: claims.Subject, Email: claims.Email, EmailVerified: true, Hosted: &auth.HostedIdentity{Subject: claims.Subject, SessionID: claims.ProviderSession}}
	s.hostedMutationMu.Lock()
	err := s.acceptHostedInvitationFor(c.Request().Context(), identity, request.Token)
	s.hostedMutationMu.Unlock()
	if err != nil {
		return c.JSON(http.StatusForbidden, apiErrorResponse{Code: "invitation_unavailable", Message: "This invitation is expired, already used, or intended for another account or organization"})
	}
	return c.NoContent(http.StatusNoContent)
}
